package daemon

// verbs_test.go — coverage for the control verbs and error branches the
// happy-path suites never reach: INPUT_EOF, RESIZE, RELEASE_WRITE, DETACH
// misuse, REPLAY_COMMAND / SUBSCRIBE_EVENTS refusals, and the SDK methods
// that wrap them.

import (
	"io"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// dialSDK returns a connected SDK client closed at cleanup.
func dialSDK(t *testing.T, sock string) *client.Client {
	t.Helper()
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestVerbs_InputEOFEndsCatViaSDK(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)

	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Attach(ctxT(t), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TakeWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}
	// ^D through the wire: cat sees EOF and exits 0.
	if err := c.InputEOF(ctxT(t), id); err != nil {
		t.Fatalf("InputEOF: %v", err)
	}
	if _, err := io.ReadAll(st); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if code, _, exited := st.Exit(); !exited || code != 0 {
		t.Errorf("cat exit = (%d, %v), want (0, true)", code, exited)
	}
	// INPUT_EOF on a dead session is refused with the typed code.
	if err := c.InputEOF(ctxT(t), id); !errcodes.IsCode(err, errcodes.SessionExited) {
		t.Errorf("InputEOF after exit = %v, want E_SESSION_EXITED", err)
	}
	// …and on a session that never existed.
	if err := c.InputEOF(ctxT(t), "ghost"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("InputEOF(ghost) = %v", err)
	}
}

func TestVerbs_ResizeAppliesAndValidates(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Resize(ctxT(t), id, 132, 43); err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(ctxT(t), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Cols != 132 || info.Rows != 43 {
		t.Errorf("size = %dx%d, want 132x43", info.Cols, info.Rows)
	}
	// Zero dimensions are refused (a zero grid wedges the child).
	if err := c.Resize(ctxT(t), id, 0, 24); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("Resize(0 cols) = %v, want E_INVALID_INPUT", err)
	}
	if err := c.Resize(ctxT(t), "ghost", 80, 24); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("Resize(ghost) = %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestVerbs_ReleaseWriteFreesTheLock(t *testing.T) {
	sock, srv := startServer(t, Options{})
	a := dialSDK(t, sock)
	b := dialSDK(t, sock)

	id, _, err := a.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.TakeWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}
	sess, err := srv.Registry().Lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.WriteLockHolder() != a.ClientID() {
		t.Fatalf("lock holder = %q, want %q", sess.WriteLockHolder(), a.ClientID())
	}

	// Release by a NON-holder is a no-op (idempotent, never an error).
	if err := b.ReleaseWrite(ctxT(t), id); err != nil {
		t.Fatalf("non-holder ReleaseWrite: %v", err)
	}
	if sess.WriteLockHolder() != a.ClientID() {
		t.Error("non-holder release stole the lock")
	}
	// Release by the holder frees it; a second release stays a no-op.
	if err := a.ReleaseWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}
	if got := sess.WriteLockHolder(); got != "" {
		t.Errorf("lock still held by %q after release", got)
	}
	if err := a.ReleaseWrite(ctxT(t), id); err != nil {
		t.Errorf("double ReleaseWrite: %v", err)
	}
	if err := a.ReleaseWrite(ctxT(t), "ghost"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("ReleaseWrite(ghost) = %v", err)
	}
	_ = a.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestVerbs_DetachMisuseAndRepeatAttach(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}

	// DETACH without an ATTACH → typed refusal.
	raw := dialT(t, sock)
	raw.send(proto.TypeDetach, proto.Detach{ReqID: raw.nextReqID(), SessionID: id}, nil)
	raw.expectErrCode(string(errcodes.NotAttached))

	// Double ATTACH on one connection → refused.
	st, err := c.Attach(ctxT(t), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach(ctxT(t), id, nil, false); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("duplicate Attach = %v, want E_INVALID_INPUT", err)
	}
	// Detach then re-attach on the same connection works.
	if err := st.Detach(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach(ctxT(t), id, nil, false); err != nil {
		t.Fatalf("re-attach after detach: %v", err)
	}
	// ATTACH to nothing.
	if _, err := c.Attach(ctxT(t), "ghost", nil, false); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("Attach(ghost) = %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestVerbs_SubscribeEventsTwiceRefused(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	if _, err := c.SubscribeEvents(ctxT(t), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscribeEvents(ctxT(t), ""); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("double SubscribeEvents = %v, want E_INVALID_INPUT", err)
	}
}

func TestVerbs_ReplayCommandUnknownSession(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	if _, _, _, err := c.LastCommandOutput(ctxT(t), "ghost"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("LastCommandOutput(ghost) = %v", err)
	}
}

func TestVerbs_KillUnknownSignalAndSession(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Kill(ctxT(t), id, "SIGPONY"); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("Kill(bad signal) = %v", err)
	}
	if err := c.Kill(ctxT(t), "ghost", proto.SignalTERM); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("Kill(ghost) = %v", err)
	}
	// Empty signal name defaults to TERM and succeeds.
	if err := c.Kill(ctxT(t), id, ""); err != nil {
		t.Errorf("Kill(default signal) = %v", err)
	}
}

func TestVerbs_RenameAndSetMetaErrors(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}, Name: "taken"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Rename(ctxT(t), other, "taken"); !errcodes.IsCode(err, errcodes.NameTaken) {
		t.Errorf("Rename to taken name = %v", err)
	}
	if err := c.Rename(ctxT(t), other, "bad name"); !errcodes.IsCode(err, errcodes.InvalidName) {
		t.Errorf("Rename to invalid name = %v", err)
	}
	if err := c.Rename(ctxT(t), "ghost", "x"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("Rename(ghost) = %v", err)
	}
	if err := c.SetMeta(ctxT(t), "ghost", map[string]string{"a": "b"}); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("SetMeta(ghost) = %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
	_ = c.Kill(ctxT(t), other, proto.SignalKILL)
}

func TestVerbs_SpawnLimitsAndBadCommand(t *testing.T) {
	sock, _ := startServer(t, Options{MaxSessions: 1})
	c := dialSDK(t, sock)

	if _, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/nonexistent/xyz"}); !errcodes.IsCode(err, errcodes.SpawnFailed) {
		t.Errorf("bad command = %v, want E_SPAWN_FAILED", err)
	}
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}}); !errcodes.IsCode(err, errcodes.LimitExceeded) {
		t.Errorf("over max-sessions = %v, want E_LIMIT_EXCEEDED", err)
	}
	if _, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Name: "bad name"}); !errcodes.IsCode(err, errcodes.InvalidName) {
		t.Errorf("bad name = %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestVerbs_ClosedClientRefusesRequests(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil { // idempotent
		t.Errorf("double Close: %v", err)
	}
	if _, err := c.List(ctxT(t)); !errcodes.IsCode(err, errcodes.DaemonUnreachable) {
		t.Errorf("List on closed client = %v, want E_DAEMON_UNREACHABLE", err)
	}
}

func TestVerbs_RequestHonorsContextCancel(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialSDK(t, sock)
	ctx, cancel := contextWithCancel(t.Context())
	cancel() // already dead
	if _, err := c.List(ctx); err == nil {
		t.Error("List with a canceled context should fail")
	}
}

func TestVerbs_HandoverReqRefusedOverWS(t *testing.T) {
	// Takeover is UDS-only: a WS peer (even control-scoped) is refused.
	_, port, control, _ := startServerWS(t)
	c := dialWST(t, port, control)
	c.send(proto.TypeHandoverReq, proto.HandoverReq{ReqID: c.nextReqID(), ReplyPath: "/tmp/nope.sock"}, nil)
	// Control-only gate fires first for read-only; for control scope the
	// UDS check fires — either way it must be refused, never executed.
	f := c.expectSkipping(proto.TypeError, proto.TypeOutput, proto.TypeEvent)
	var em proto.ErrorMsg
	if err := f.DecodeHeader(&em); err != nil {
		t.Fatal(err)
	}
	if em.Code != errcodes.Unsupported && em.Code != errcodes.ReadOnlyScope {
		t.Errorf("HANDOVER_REQ over WS = %s, want a refusal", em.Code)
	}
}

// TestConn_MalformedHeadersRejectedNotFatal drives a malformed header into
// every control handler that decodes one. A JSON number in a string field
// makes DecodeHeader fail; the daemon must answer with E_BAD_FRAME and keep
// the connection alive — never crash, never wedge. This exercises the
// DecodeHeader error arm the happy-path suites (which only send valid frames)
// can't reach.
func TestConn_MalformedHeadersRejectedNotFatal(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialT(t, sock)

	bad := map[string]any{"req_id": 123, "session_id": 123} // numbers where strings are expected
	for _, ft := range []proto.FrameType{
		proto.TypeList, proto.TypeInfo, proto.TypeKill, proto.TypeResize,
		proto.TypeRename, proto.TypeSetMeta, proto.TypeSetMetaJSON, proto.TypeTakeWrite, proto.TypeReleaseWrite,
		proto.TypeReplayCommand, proto.TypeSubEvents, proto.TypeAttach, proto.TypeDetach,
		proto.TypeInputEOF, proto.TypeInput,
	} {
		c.send(ft, bad, nil)
		f := c.expect(proto.TypeError)
		var em proto.ErrorMsg
		if err := f.DecodeHeader(&em); err != nil {
			t.Fatalf("%s: decoding the ERROR frame: %v", ft, err)
		}
		if em.Code != errcodes.BadFrame {
			t.Errorf("%s: error code = %s, want %s", ft, em.Code, errcodes.BadFrame)
		}
	}

	// The connection survived every malformed frame: a valid LIST still works.
	c.send(proto.TypeList, proto.List{ReqID: c.nextReqID()}, nil)
	c.expect(proto.TypeListOK)
}
