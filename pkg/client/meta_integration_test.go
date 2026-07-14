package client

import (
	"encoding/json"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// metaOf spawns nothing; it decodes a session's meta document for assertions.
func metaOf(t *testing.T, c *Client, id string) map[string]any {
	t.Helper()
	info, err := c.Info(ctxT(t), id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(info.Annotations) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(info.Annotations, &m); err != nil {
		t.Fatalf("meta not an object: %v", err)
	}
	return m
}

func TestSDK_MetaJSON_FullPath(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Spawn with an initial JSON meta document (exercises onSpawn validation
	// and the SpawnOpts.Annotations wire field).
	id, _, err := c.Spawn(ctxT(t), SpawnOpts{
		Cmd:         "/bin/cat",
		Name:        "meta-sdk",
		Annotations: json.RawMessage(`{"agent":"a1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Merge (default mode).
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"status":"running"}`), SetMetaOpts{}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// Increment a counter.
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"tokens":5}`), SetMetaOpts{Mode: proto.MetaModeIncr}); err != nil {
		t.Fatalf("incr: %v", err)
	}
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"tokens":5}`), SetMetaOpts{Mode: proto.MetaModeIncr}); err != nil {
		t.Fatalf("incr2: %v", err)
	}
	m := metaOf(t, c, id)
	if m["agent"] != "a1" || m["status"] != "running" {
		t.Errorf("merge lost fields: %v", m)
	}
	if f, _ := m["tokens"].(float64); f != 10 {
		t.Errorf("tokens should be 10: %v", m["tokens"])
	}

	// CAS: a stale version is rejected; the current version succeeds.
	info, err := c.Info(ctxT(t), id)
	if err != nil {
		t.Fatal(err)
	}
	stale := uint64(0)
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"x":1}`), SetMetaOpts{IfVersion: &stale}); !errcodes.IsCode(err, errcodes.MetaConflict) {
		t.Fatalf("stale CAS: want E_META_CONFLICT, got %v", err)
	}
	cur := info.MetaVersion
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"x":1}`), SetMetaOpts{IfVersion: &cur}); err != nil {
		t.Fatalf("current CAS: %v", err)
	}

	// Replace is wholesale.
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"only":"this"}`), SetMetaOpts{Mode: proto.MetaModeReplace}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if m := metaOf(t, c, id); len(m) != 1 || m["only"] != "this" {
		t.Errorf("replace not wholesale: %v", m)
	}

	// Reserved namespace is rejected over the wire.
	if err := c.SetMetaJSON(ctxT(t), id, json.RawMessage(`{"rpty.pid":1}`), SetMetaOpts{}); !errcodes.IsCode(err, errcodes.ReservedMetaKey) {
		t.Fatalf("reserved write: want E_RESERVED_META_KEY, got %v", err)
	}

	// Legacy flat SetMeta still projects into Info.Meta.
	if err := c.SetMeta(ctxT(t), id, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("legacy SetMeta: %v", err)
	}
	info, err = c.Info(ctxT(t), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Meta["k"] != "v" {
		t.Errorf("legacy projection missing: %v", info.Meta)
	}

	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestSDK_SpawnRejectsReservedAnnotations(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A reserved key in the spawn blob fails the spawn loudly (onSpawn
	// ValidateSpawnAnnotations reject path).
	_, _, err = c.Spawn(ctxT(t), SpawnOpts{
		Cmd:         "/bin/cat",
		Annotations: json.RawMessage(`{"rpty.x":1}`),
	})
	if !errcodes.IsCode(err, errcodes.ReservedMetaKey) {
		t.Fatalf("want E_RESERVED_META_KEY, got %v", err)
	}
	// A non-object blob is likewise rejected.
	_, _, err = c.Spawn(ctxT(t), SpawnOpts{
		Cmd:         "/bin/cat",
		Annotations: json.RawMessage(`5`),
	})
	if !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Fatalf("want E_INVALID_INPUT, got %v", err)
	}
}
