package host

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// liveSession spawns a cat that blocks on stdin, so the session stays running
// for meta writes; spawnT kills it at cleanup.
func liveSession(t *testing.T) *Session {
	t.Helper()
	return spawnT(t, SpawnConfig{Cmd: "/bin/cat", Cols: 80, Rows: 24, Linger: true})
}

// metaDoc decodes the session's current meta document for comparison.
func metaDoc(t *testing.T, s *Session) map[string]any {
	t.Helper()
	raw := s.Info().Annotations
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("meta not an object: %v (%s)", err, raw)
	}
	return m
}

func TestSetMetaJSON_MergeDisjointFieldsSurvive(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	// Two independent writers touching different fields: both must survive,
	// which is the whole reason merge exists.
	if _, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"status":"running"}`), nil); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	if _, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"progress":60}`), nil); err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	got := metaDoc(t, s)
	if got["status"] != "running" {
		t.Errorf("status lost: %v", got)
	}
	if f, ok := got["progress"].(float64); !ok || f != 60 {
		t.Errorf("progress wrong: %v (%T)", got["progress"], got["progress"])
	}
}

func TestSetMetaJSON_ReplaceIsWholesale(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	_, _ = s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"a":"1","b":"2"}`), nil)
	if _, err := s.SetMetaJSON(proto.MetaModeReplace, []byte(`{"c":"3"}`), nil); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got := metaDoc(t, s)
	if !reflect.DeepEqual(got, map[string]any{"c": "3"}) {
		t.Errorf("replace not wholesale: %v", got)
	}
}

func TestSetMetaJSON_NullDeletes(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	_, _ = s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"a":"1","b":"2"}`), nil)
	_, _ = s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"a":null}`), nil)
	got := metaDoc(t, s)
	if _, ok := got["a"]; ok {
		t.Errorf("a should be deleted: %v", got)
	}
	if got["b"] != "2" {
		t.Errorf("b should remain: %v", got)
	}
}

func TestSetMetaJSON_ReservedKeyRejected(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	for _, patch := range []string{`{"rpty.pid":1}`, `{"rpty":{"x":1}}`} {
		_, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(patch), nil)
		if !errcodes.IsCode(err, errcodes.ReservedMetaKey) {
			t.Errorf("patch %s: want E_RESERVED_META_KEY, got %v", patch, err)
		}
	}
}

func TestSetMetaJSON_SizeCapEnforced(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	big := `{"blob":"` + strings.Repeat("x", MaxMetaBytes+1) + `"}`
	_, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(big), nil)
	if !errcodes.IsCode(err, errcodes.MetaTooLarge) {
		t.Fatalf("want E_META_TOO_LARGE, got %v", err)
	}
	// The failed write must not have mutated the document.
	if len(metaDoc(t, s)) != 0 {
		t.Errorf("oversize write mutated meta: %v", metaDoc(t, s))
	}
}

func TestSetMetaJSON_NonObjectRejected(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	_, err := s.SetMetaJSON(proto.MetaModeReplace, []byte(`5`), nil)
	if !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Fatalf("want E_INVALID_INPUT for non-object, got %v", err)
	}
}

func TestSetMetaJSON_CompareAndSwap(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	v, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"n":1}`), nil)
	if err != nil || v != 1 {
		t.Fatalf("first write: v=%d err=%v", v, err)
	}
	// Stale version is rejected.
	stale := uint64(0)
	if _, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"n":2}`), &stale); !errcodes.IsCode(err, errcodes.MetaConflict) {
		t.Fatalf("want E_META_CONFLICT, got %v", err)
	}
	// Correct version succeeds and bumps.
	cur := uint64(1)
	v, err = s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"n":2}`), &cur)
	if err != nil || v != 2 {
		t.Fatalf("CAS write: v=%d err=%v", v, err)
	}
}

func TestInitialMetaDoc_MetaAndAnnotationsMerge(t *testing.T) {
	t.Parallel()
	s := spawnT(t, SpawnConfig{
		Cmd: "/bin/cat", Cols: 80, Rows: 24, Linger: true,
		Meta:        map[string]string{"agent": "a1"},
		Annotations: []byte(`{"task":{"id":5}}`),
	})
	info := s.Info()
	// Legacy flat projection sees the string field only.
	if !reflect.DeepEqual(info.Meta, map[string]string{"agent": "a1"}) {
		t.Errorf("projection wrong: %v", info.Meta)
	}
	// Full document carries both.
	doc := metaDoc(t, s)
	if doc["agent"] != "a1" {
		t.Errorf("agent missing from doc: %v", doc)
	}
	if _, ok := doc["task"].(map[string]any); !ok {
		t.Errorf("task nesting missing: %v", doc)
	}
	if info.MetaVersion != 0 {
		t.Errorf("fresh session should be meta_version 0, got %d", info.MetaVersion)
	}
}

func TestSetMetaJSON_IncrMode(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	_, _ = s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"tokens":900}`), nil)
	if _, err := s.SetMetaJSON(proto.MetaModeIncr, []byte(`{"tokens":200,"calls":1}`), nil); err != nil {
		t.Fatalf("incr: %v", err)
	}
	got := metaDoc(t, s)
	if f, _ := got["tokens"].(float64); f != 1100 {
		t.Errorf("tokens should be 1100: %v", got["tokens"])
	}
	if f, _ := got["calls"].(float64); f != 1 {
		t.Errorf("calls should start at 1: %v", got["calls"])
	}
	// Incrementing a non-number field is rejected.
	_, _ = s.SetMetaJSON(proto.MetaModeMerge, []byte(`{"name":"x"}`), nil)
	if _, err := s.SetMetaJSON(proto.MetaModeIncr, []byte(`{"name":1}`), nil); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("incr on string field: want E_INVALID_INPUT, got %v", err)
	}
}

func TestMetaChangeData(t *testing.T) {
	t.Parallel()
	d := metaChangeData(7, []string{"status", "progress"})
	if d[proto.DataKeyMetaKeys] != "progress,status" { // sorted
		t.Errorf("meta_keys wrong: %q", d[proto.DataKeyMetaKeys])
	}
	if d[proto.DataKeyMetaVersion] != "7" {
		t.Errorf("meta_version wrong: %q", d[proto.DataKeyMetaVersion])
	}
}

func TestSetMetaJSON_ReplaceInvalidPatchRejected(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	// Bad JSON in replace mode exercises the apply-error path (and modeName).
	if _, err := s.SetMetaJSON(proto.MetaModeReplace, []byte(`{bad`), nil); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Fatalf("want E_INVALID_INPUT, got %v", err)
	}
	// An unknown mode is rejected too.
	if _, err := s.SetMetaJSON(proto.MetaMode("bogus"), []byte(`{}`), nil); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Fatalf("unknown mode: want E_INVALID_INPUT, got %v", err)
	}
	// A merge-mode apply error (invalid patch JSON) exercises modeName's
	// default arm.
	if _, err := s.SetMetaJSON(proto.MetaModeMerge, []byte(`{bad`), nil); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Fatalf("merge bad patch: want E_INVALID_INPUT, got %v", err)
	}
}

func TestSetMetaJSON_EveryDeclaredModeHandled(t *testing.T) {
	t.Parallel()
	// Guards exhaustiveness: every mode in the canonical proto.MetaModeValues
	// must be wired into the SetMetaJSON switch. Adding a mode to the enum
	// without a case would surface here as an "unknown meta mode" error.
	s := liveSession(t)
	for _, mode := range proto.MetaModeValues {
		if _, err := s.SetMetaJSON(mode, []byte(`{"n":1}`), nil); err != nil {
			t.Errorf("mode %q not handled: %v", mode, err)
		}
	}
}

func TestValidateSpawnAnnotations(t *testing.T) {
	t.Parallel()
	if err := ValidateSpawnAnnotations(nil); err != nil {
		t.Errorf("empty should be valid: %v", err)
	}
	if err := ValidateSpawnAnnotations([]byte(`{"a":1}`)); err != nil {
		t.Errorf("object should be valid: %v", err)
	}
	if err := ValidateSpawnAnnotations([]byte(`5`)); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("non-object: want E_INVALID_INPUT, got %v", err)
	}
	if err := ValidateSpawnAnnotations([]byte(`{"rpty.pid":1}`)); !errcodes.IsCode(err, errcodes.ReservedMetaKey) {
		t.Errorf("reserved key: want E_RESERVED_META_KEY, got %v", err)
	}
	big := []byte(`{"blob":"` + strings.Repeat("x", MaxMetaBytes+1) + `"}`)
	if err := ValidateSpawnAnnotations(big); !errcodes.IsCode(err, errcodes.MetaTooLarge) {
		t.Errorf("oversize: want E_META_TOO_LARGE, got %v", err)
	}
}

func TestInitialMetaDoc_AllPaths(t *testing.T) {
	t.Parallel()
	// Neither set → empty document.
	if got := initialMetaDoc(SpawnConfig{}); got != nil {
		t.Errorf("no meta should be nil, got %s", got)
	}
	// Meta only → flat projection.
	if got := initialMetaDoc(SpawnConfig{Meta: map[string]string{"a": "1"}}); string(got) != `{"a":"1"}` {
		t.Errorf("meta-only doc = %s", got)
	}
	// Annotations only.
	if got := initialMetaDoc(SpawnConfig{Annotations: []byte(`{"b":2}`)}); string(got) != `{"b":2}` {
		t.Errorf("annotations-only doc = %s", got)
	}
	// Both merge (annotations on top of the flat base).
	got := initialMetaDoc(SpawnConfig{Meta: map[string]string{"a": "1"}, Annotations: []byte(`{"b":2}`)})
	if string(got) != `{"a":"1","b":2}` {
		t.Errorf("merged doc = %s", got)
	}
	// Bad annotations fall back to the flat base rather than failing.
	got = initialMetaDoc(SpawnConfig{Meta: map[string]string{"a": "1"}, Annotations: []byte(`{bad`)})
	if string(got) != `{"a":"1"}` {
		t.Errorf("bad-annotations fallback = %s", got)
	}
}

func TestAdoptMetaDoc_AllPaths(t *testing.T) {
	t.Parallel()
	// MetaDoc present wins.
	if got := adoptMetaDoc(HandoverState{MetaDoc: []byte(`{"x":1}`)}); string(got) != `{"x":1}` {
		t.Errorf("MetaDoc path = %s", got)
	}
	// Pre-upgrade daemon: only the flat Meta map, rebuilt.
	if got := adoptMetaDoc(HandoverState{Meta: map[string]string{"a": "1"}}); string(got) != `{"a":"1"}` {
		t.Errorf("rebuild-from-Meta path = %s", got)
	}
	// Neither → nil.
	if got := adoptMetaDoc(HandoverState{}); got != nil {
		t.Errorf("empty adopt = %s", got)
	}
}

func TestSetMeta_LegacyReplaceProjects(t *testing.T) {
	t.Parallel()
	s := liveSession(t)
	s.SetMeta(map[string]string{"k": "v"})
	if got := s.Info().Meta; !reflect.DeepEqual(got, map[string]string{"k": "v"}) {
		t.Errorf("legacy SetMeta projection: %v", got)
	}
	if s.Info().MetaVersion == 0 {
		t.Errorf("legacy SetMeta should bump version")
	}
}
