package daemon

import (
	"encoding/json"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// infoOf sends INFO and returns the decoded snapshot.
func infoOf(c *testClient, sessionID string) proto.SessionInfo {
	c.t.Helper()
	c.send(proto.TypeInfo, proto.Info{ReqID: c.nextReqID(), SessionID: sessionID}, nil)
	f := c.expect(proto.TypeInfoOK)
	var ok proto.InfoOK
	if err := f.DecodeHeader(&ok); err != nil {
		c.t.Fatal(err)
	}
	return ok.Session
}

// setMetaJSON drives one SET_META_JSON frame.
func (c *testClient) setMetaJSON(sessionID string, mode proto.MetaMode, patch string, ifVersion *uint64) {
	c.send(proto.TypeSetMetaJSON, proto.SetMetaJSON{
		ReqID: c.nextReqID(), SessionID: sessionID, Mode: mode,
		IfVersion: ifVersion, Patch: json.RawMessage(patch),
	}, nil)
}

func TestConn_SetMetaJSON_OverTheWire(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialT(t, sock)

	ok := c.spawn(proto.Spawn{Cmd: "/bin/cat", Annotations: json.RawMessage(`{"agent":"a1"}`)})
	id := ok.SessionID

	// Merge.
	c.setMetaJSON(id, proto.MetaModeMerge, `{"status":"running"}`, nil)
	c.expect(proto.TypeOK)
	// Increment.
	c.setMetaJSON(id, proto.MetaModeIncr, `{"tokens":5}`, nil)
	c.expect(proto.TypeOK)

	info := infoOf(c, id)
	var m map[string]any
	if err := json.Unmarshal(info.Annotations, &m); err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if m["agent"] != "a1" || m["status"] != "running" {
		t.Errorf("merge lost fields: %v", m)
	}
	if f, _ := m["tokens"].(float64); f != 5 {
		t.Errorf("tokens = %v, want 5", m["tokens"])
	}
	if info.MetaVersion == 0 {
		t.Error("meta_version should have advanced")
	}

	// Reserved namespace rejected.
	c.setMetaJSON(id, proto.MetaModeMerge, `{"rpty.pid":1}`, nil)
	c.expectErrCode(string(errcodes.ReservedMetaKey))

	// Stale compare-and-swap rejected.
	stale := uint64(0)
	c.setMetaJSON(id, proto.MetaModeMerge, `{"x":1}`, &stale)
	c.expectErrCode(string(errcodes.MetaConflict))

	// Correct compare-and-swap accepted.
	cur := info.MetaVersion
	c.setMetaJSON(id, proto.MetaModeMerge, `{"x":1}`, &cur)
	c.expect(proto.TypeOK)

	// Unknown session is rejected (the Lookup error arm).
	c.setMetaJSON("ses_deadbeef", proto.MetaModeMerge, `{"a":1}`, nil)
	c.expectErrCode(string(errcodes.SessionNotFound))
}

func TestConn_SpawnRejectsBadAnnotations(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialT(t, sock)

	// Reserved key in the spawn blob fails the spawn (onSpawn validation).
	c.send(proto.TypeSpawn, proto.Spawn{
		ReqID: c.nextReqID(), Cmd: "/bin/cat", Cols: 80, Rows: 24,
		Annotations: json.RawMessage(`{"rpty.x":1}`),
	}, nil)
	c.expectErrCode(string(errcodes.ReservedMetaKey))

	// Non-object blob fails too.
	c.send(proto.TypeSpawn, proto.Spawn{
		ReqID: c.nextReqID(), Cmd: "/bin/cat", Cols: 80, Rows: 24,
		Annotations: json.RawMessage(`5`),
	}, nil)
	c.expectErrCode(string(errcodes.InvalidInput))
}
