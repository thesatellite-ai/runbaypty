package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// asMap decodes a patch for structural comparison (order-independent).
func asMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("patch not an object: %v (%s)", err, b)
	}
	return m
}

func TestBuildMetaPatch_OperatorDecidesType(t *testing.T) {
	t.Parallel()
	patch, err := buildMetaPatch("", []string{
		"name=deploy",       // string
		"id=007",            // string, leading zero preserved
		"count:=5",          // JSON number
		"active:=true",      // JSON bool
		`tags:=["ci","fa"]`, // JSON array, atomic
	}, strings.NewReader(""))
	if err != nil {
		t.Fatalf("buildMetaPatch: %v", err)
	}
	m := asMap(t, patch)
	if m["name"] != "deploy" || m["id"] != "007" {
		t.Errorf("string fields wrong: %v", m)
	}
	if f, ok := m["count"].(float64); !ok || f != 5 {
		t.Errorf("count should be JSON number 5: %v (%T)", m["count"], m["count"])
	}
	if m["active"] != true {
		t.Errorf("active should be JSON true: %v", m["active"])
	}
	if arr, ok := m["tags"].([]any); !ok || len(arr) != 2 || arr[0] != "ci" {
		t.Errorf("tags should be an atomic array: %v", m["tags"])
	}
}

func TestBuildMetaPatch_DottedNesting(t *testing.T) {
	t.Parallel()
	patch, err := buildMetaPatch("", []string{"task.id:=5", "task.name=deploy"}, strings.NewReader(""))
	if err != nil {
		t.Fatalf("buildMetaPatch: %v", err)
	}
	m := asMap(t, patch)
	task, ok := m["task"].(map[string]any)
	if !ok {
		t.Fatalf("task should nest: %v", m)
	}
	if task["name"] != "deploy" {
		t.Errorf("task.name wrong: %v", task)
	}
	if f, ok := task["id"].(float64); !ok || f != 5 {
		t.Errorf("task.id should be 5: %v", task["id"])
	}
}

func TestBuildMetaPatch_EscapedDotIsLiteral(t *testing.T) {
	t.Parallel()
	patch, err := buildMetaPatch("", []string{`foo\.bar=1`}, strings.NewReader(""))
	if err != nil {
		t.Fatalf("buildMetaPatch: %v", err)
	}
	m := asMap(t, patch)
	if m["foo.bar"] != "1" {
		t.Errorf("literal-dot key lost: %v", m)
	}
}

func TestBuildMetaPatch_JSONBaseThenPairsOverride(t *testing.T) {
	t.Parallel()
	// --json supplies the base; a pair overrides one field.
	patch, err := buildMetaPatch(`{"a":"1","b":"2"}`, []string{"b=override"}, strings.NewReader(""))
	if err != nil {
		t.Fatalf("buildMetaPatch: %v", err)
	}
	if !reflect.DeepEqual(asMap(t, patch), map[string]any{"a": "1", "b": "override"}) {
		t.Errorf("json base + override wrong: %s", patch)
	}
}

func TestBuildMetaPatch_JSONFromStdin(t *testing.T) {
	t.Parallel()
	patch, err := buildMetaPatch("-", nil, strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("buildMetaPatch: %v", err)
	}
	if f := asMap(t, patch)["x"].(float64); f != 1 {
		t.Errorf("stdin json not read: %s", patch)
	}
}

func TestParseMetaPair_Errors(t *testing.T) {
	t.Parallel()
	// noeq: no operator. =noKey: empty key (string). :=5: empty key (JSON).
	// k:=notjson: invalid JSON after :=.
	for _, bad := range []string{"noeq", "=noKey", ":=5", "k:=notjson"} {
		if _, _, err := parseMetaPair(bad); err == nil {
			t.Errorf("parseMetaPair(%q) should error", bad)
		}
	}
}

func TestReadJSONSource(t *testing.T) {
	t.Parallel()
	// Inline literal.
	if b, err := readJSONSource(`{"a":1}`, strings.NewReader("")); err != nil || string(b) != `{"a":1}` {
		t.Errorf("inline: %s %v", b, err)
	}
	// Stdin.
	if b, err := readJSONSource("-", strings.NewReader(`{"b":2}`)); err != nil || string(b) != `{"b":2}` {
		t.Errorf("stdin: %s %v", b, err)
	}
	// File.
	dir := t.TempDir()
	p := filepath.Join(dir, "m.json")
	if err := os.WriteFile(p, []byte(`{"c":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readJSONSource("@"+p, strings.NewReader("")); err != nil || string(b) != `{"c":3}` {
		t.Errorf("file: %s %v", b, err)
	}
	// Missing file errors.
	if _, err := readJSONSource("@/no/such/file.json", strings.NewReader("")); err == nil {
		t.Error("missing file should error")
	}
	// A stdin read error propagates.
	if _, err := readJSONSource("-", errReader{}); err == nil {
		t.Error("stdin read error should propagate")
	}
}

// errReader always fails, to exercise the stdin read-error branch.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBuildMetaPatch_Errors(t *testing.T) {
	t.Parallel()
	// --json that is not an object.
	if _, err := buildMetaPatch(`[1,2]`, nil, strings.NewReader("")); err == nil {
		t.Error("non-object --json should error")
	}
	// --json from a missing file.
	if _, err := buildMetaPatch("@/no/such.json", nil, strings.NewReader("")); err == nil {
		t.Error("missing --json file should error")
	}
	// A bad pair propagates the parse error.
	if _, err := buildMetaPatch("", []string{"noeq"}, strings.NewReader("")); err == nil {
		t.Error("bad pair should error")
	}
}

func TestMetaMatches_InvalidDocNeverMatches(t *testing.T) {
	t.Parallel()
	// A malformed meta doc decodes to nil, so no real filter matches.
	if ok, _ := metaMatches([]byte(`{bad`), []string{"a=1"}); ok {
		t.Error("invalid doc should not match")
	}
	// A JSON null value renders as empty and never equals a real filter value,
	// but matches an explicit empty target.
	doc := []byte(`{"n":null}`)
	if ok, _ := metaMatches(doc, []string{"n=x"}); ok {
		t.Error("null should not equal x")
	}
	if ok, _ := metaMatches(doc, []string{"n="}); !ok {
		t.Error("null renders empty, should match empty target")
	}
}

func TestBuildIncrPatch(t *testing.T) {
	t.Parallel()
	patch, err := buildIncrPatch([]string{"tokens=200", "task.retries=1"})
	if err != nil {
		t.Fatalf("buildIncrPatch: %v", err)
	}
	m := asMap(t, patch)
	if f := m["tokens"].(float64); f != 200 {
		t.Errorf("tokens delta wrong: %v", m["tokens"])
	}
	task := m["task"].(map[string]any)
	if f := task["retries"].(float64); f != 1 {
		t.Errorf("task.retries delta wrong: %v", task["retries"])
	}
	// Non-number delta is rejected.
	if _, err := buildIncrPatch([]string{"tokens=lots"}); err == nil {
		t.Error("non-number delta should error")
	}
	if _, err := buildIncrPatch([]string{"noeq"}); err == nil {
		t.Error("missing = should error")
	}
}

func TestMetaMatches(t *testing.T) {
	t.Parallel()
	doc := []byte(`{"agent":"a1","step":3,"active":true,"paused":false,"task":{"name":"deploy"}}`)
	cases := []struct {
		filters []string
		want    bool
	}{
		{nil, true},                             // no filter matches everything
		{[]string{"agent=a1"}, true},            // string
		{[]string{"agent=a2"}, false},           // string mismatch
		{[]string{"step=3"}, true},              // number rendered as digits
		{[]string{"active=true"}, true},         // bool true
		{[]string{"paused=false"}, true},        // bool false
		{[]string{"active=false"}, false},       // bool mismatch
		{[]string{"task.name=deploy"}, true},    // dotted path
		{[]string{"task.name=rollback"}, false}, // dotted mismatch
		{[]string{"agent=a1", "step=3"}, true},  // AND, both match
		{[]string{"agent=a1", "step=9"}, false}, // AND, one fails
		{[]string{"missing=x"}, false},          // absent path
		{[]string{"task=deploy"}, false},        // object has no scalar form
	}
	for _, c := range cases {
		got, err := metaMatches(doc, c.filters)
		if err != nil {
			t.Fatalf("metaMatches(%v): %v", c.filters, err)
		}
		if got != c.want {
			t.Errorf("metaMatches(%v) = %v, want %v", c.filters, got, c.want)
		}
	}
	if _, err := metaMatches(doc, []string{"noeq"}); err == nil {
		t.Error("filter without = should error")
	}
	// Empty meta never matches a real filter.
	if ok, _ := metaMatches(nil, []string{"agent=a1"}); ok {
		t.Error("empty meta should not match")
	}
}

func TestBuildUnsetPatch_NullsForDelete(t *testing.T) {
	t.Parallel()
	patch, err := buildUnsetPatch([]string{"a", "task.id"})
	if err != nil {
		t.Fatalf("buildUnsetPatch: %v", err)
	}
	m := asMap(t, patch)
	if v, ok := m["a"]; !ok || v != nil {
		t.Errorf("a should be null: %v", m)
	}
	task, ok := m["task"].(map[string]any)
	if !ok {
		t.Fatalf("task should nest: %v", m)
	}
	if v, ok := task["id"]; !ok || v != nil {
		t.Errorf("task.id should be null: %v", task)
	}
}
