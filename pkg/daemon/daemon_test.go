package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// TestMain: no goroutine may outlive its test — pumps, accept loops,
// housekeeping, all of it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ── handshake gate ──────────────────────────────────────────────────

func TestHandshake_ControlBeforeHelloRefused(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialRaw(t, sock)
	c.send(proto.TypeList, proto.List{ReqID: "r1"}, nil)
	c.expectErrCode(string(errcodes.HandshakeFirst))
	// The daemon closes the connection after the gate violation.
	if _, ok := c.next(); ok {
		t.Fatal("connection stayed open after handshake violation")
	}
}

func TestHandshake_ProtocolMajorMismatchRefused(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialRaw(t, sock)
	f := c.hello(99)
	if f.Type != proto.TypeError {
		t.Fatalf("got %s, want ERROR", f.Type)
	}
	var em proto.ErrorMsg
	if err := f.DecodeHeader(&em); err != nil {
		t.Fatal(err)
	}
	if em.Code != errcodes.ProtocolMismatch {
		t.Fatalf("code = %s, want E_PROTOCOL_MISMATCH", em.Code)
	}
}

func TestHandshake_HappyPathMintsClientID(t *testing.T) {
	sock, _ := startServer(t, Options{Version: "9.9.9-test"})
	c := dialRaw(t, sock)
	f := c.hello(proto.ProtocolVersion)
	if f.Type != proto.TypeHelloAck {
		t.Fatalf("got %s, want HELLO_ACK", f.Type)
	}
	var ack proto.HelloAck
	if err := f.DecodeHeader(&ack); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ack.ClientID, "cli_") {
		t.Errorf("ClientID = %q, want cli_…", ack.ClientID)
	}
	if ack.DaemonVersion != "9.9.9-test" {
		t.Errorf("DaemonVersion = %q", ack.DaemonVersion)
	}
}

// ── the core lifecycle ──────────────────────────────────────────────

func TestLifecycle_SpawnAttachInputOutputKill(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialT(t, sock)

	ok := spawnShell(c, "cat") // cat echoes stdin via the PTY line discipline
	if !strings.HasPrefix(ok.SessionID, "ses_") || ok.Pid <= 0 {
		t.Fatalf("SpawnOK = %+v", ok)
	}

	att := c.attach(ok.SessionID, nil, false)
	if att.SessionID != ok.SessionID || att.Truncated {
		t.Fatalf("AttachOK = %+v", att)
	}

	c.input(ok.SessionID, []byte("echo-me-back\r"))
	data, _ := c.collectOutput(ok.SessionID, "echo-me-back")
	if len(data) == 0 {
		t.Fatal("no output")
	}

	c.send(proto.TypeKill, proto.Kill{ReqID: c.nextReqID(), SessionID: ok.SessionID, Signal: proto.SignalKILL}, nil)
	c.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)
	exit := c.expectSkipping(proto.TypeExit, proto.TypeOutput, proto.TypeEvent)
	var ex proto.Exit
	if err := exit.DecodeHeader(&ex); err != nil {
		t.Fatal(err)
	}
	if ex.SessionID != ok.SessionID || ex.Signal != proto.SignalKILL {
		t.Errorf("Exit = %+v", ex)
	}
}

// ── THE product test: zero-gap reconnect ────────────────────────────

// lineRe matches the generator's output lines.
var lineRe = regexp.MustCompile(`line-(\d+)\n`)

func TestReconnect_ZeroGapWithSinceSeq(t *testing.T) {
	sock, _ := startServer(t, Options{})
	boss := dialT(t, sock)

	// A generator printing numbered lines forever.
	ok := spawnShell(boss, `i=0; while :; do echo line-$i; i=$((i+1)); sleep 0.005; done`)

	// First client: read until we've seen at least line-20, then vanish
	// mid-stream (no DETACH — simulating a crash/rebuild).
	c1 := dialT(t, sock)
	c1.attach(ok.SessionID, nil, false)
	data1, next1 := c1.collectOutput(ok.SessionID, "line-20\r\n")
	c1.close()

	// Let the session stream more while nobody is attached.
	time.Sleep(300 * time.Millisecond)

	// Second client resumes from exactly where the first stopped.
	c2 := dialT(t, sock)
	since := next1
	att := c2.attach(ok.SessionID, &since, false)
	if att.Truncated {
		t.Fatal("replay truncated — ring should easily hold this window")
	}
	data2, _ := c2.collectOutput(ok.SessionID, "line-90\r\n")

	// Audit: the concatenation must be a contiguous numbered stream with
	// no gap and no duplicate at the seam.
	all := append(append([]byte{}, data1...), data2...)
	normalized := bytes.ReplaceAll(all, []byte("\r\n"), []byte("\n"))
	matches := lineRe.FindAllSubmatch(normalized, -1)
	if len(matches) < 90 {
		t.Fatalf("only %d lines parsed", len(matches))
	}
	for i, m := range matches {
		n, _ := strconv.Atoi(string(m[1]))
		if n != i {
			t.Fatalf("line sequence broken at index %d: got line-%d (gap or duplicate at the reconnect seam)", i, n)
		}
	}

	// Kill the generator so shutdown is quick.
	boss.send(proto.TypeKill, proto.Kill{ReqID: boss.nextReqID(), SessionID: ok.SessionID, Signal: proto.SignalKILL}, nil)
	boss.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)
}

// ── multi-client + write control ────────────────────────────────────

func TestMultiClient_IdenticalStreams(t *testing.T) {
	sock, _ := startServer(t, Options{})
	boss := dialT(t, sock)
	ok := spawnShell(boss, "sleep 0.2; echo shared-output-payload; sleep 300")

	c1 := dialT(t, sock)
	c2 := dialT(t, sock)
	c1.attach(ok.SessionID, nil, false)
	c2.attach(ok.SessionID, nil, true) // read-only viewer

	d1, _ := c1.collectOutput(ok.SessionID, "shared-output-payload")
	d2, _ := c2.collectOutput(ok.SessionID, "shared-output-payload")
	if !bytes.Equal(d1, d2) {
		t.Errorf("streams diverge: %d vs %d bytes", len(d1), len(d2))
	}

	boss.send(proto.TypeKill, proto.Kill{ReqID: boss.nextReqID(), SessionID: ok.SessionID, Signal: proto.SignalKILL}, nil)
	boss.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)
}

func TestReadOnlyAttach_InputAndTakeWriteRefused(t *testing.T) {
	sock, _ := startServer(t, Options{})
	boss := dialT(t, sock)
	ok := spawnShell(boss, "sleep 300")

	viewer := dialT(t, sock)
	viewer.attach(ok.SessionID, nil, true)

	viewer.input(ok.SessionID, []byte("nope"))
	viewer.expectErrCode(string(errcodes.ReadOnlyAttach))

	viewer.send(proto.TypeTakeWrite, proto.TakeWrite{ReqID: viewer.nextReqID(), SessionID: ok.SessionID}, nil)
	viewer.expectErrCode(string(errcodes.ReadOnlyAttach))
}

func TestWriteLock_HandoffAndDisconnectRelease(t *testing.T) {
	sock, srv := startServer(t, Options{})
	a := dialT(t, sock)
	ok := spawnShell(a, "cat")

	// A takes the lock; B's input is refused with the holder in the message.
	a.send(proto.TypeTakeWrite, proto.TakeWrite{ReqID: a.nextReqID(), SessionID: ok.SessionID}, nil)
	a.expect(proto.TypeOK)

	b := dialT(t, sock)
	b.input(ok.SessionID, []byte("refused"))
	b.expectErrCode(string(errcodes.NoWriteLock))

	// A disconnects → the lock auto-releases → B can write.
	a.close()
	deadline := time.Now().Add(testTimeout)
	for {
		sess, err := srv.Registry().Lookup(ok.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if sess.WriteLockHolder() == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("write lock not released on disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.attach(ok.SessionID, nil, false)
	b.input(ok.SessionID, []byte("accepted-now\r"))
	b.collectOutput(ok.SessionID, "accepted-now")
}

// ── events ──────────────────────────────────────────────────────────

func TestEvents_CreatedExitedOverTheWire(t *testing.T) {
	sock, _ := startServer(t, Options{})
	watcher := dialT(t, sock)
	watcher.send(proto.TypeSubEvents, proto.SubscribeEvents{ReqID: watcher.nextReqID()}, nil)
	watcher.expect(proto.TypeOK)

	actor := dialT(t, sock)
	ok := spawnShell(actor, "exit 5")

	sawCreated, sawExited := false, false
	deadline := time.After(testTimeout)
	for !sawCreated || !sawExited {
		select {
		case f, open := <-watcher.frames:
			if !open {
				t.Fatal("watcher connection closed early")
			}
			if f.Type != proto.TypeEvent {
				continue
			}
			var ev proto.Event
			if err := f.DecodeHeader(&ev); err != nil {
				t.Fatal(err)
			}
			if ev.SessionID != ok.SessionID {
				continue
			}
			switch ev.Type {
			case proto.EventCreated:
				sawCreated = true
			case proto.EventExited:
				sawExited = true
				if ev.Data["exit_code"] != "5" {
					t.Errorf("exited data = %v", ev.Data)
				}
			}
		case <-deadline:
			t.Fatalf("events missing: created=%v exited=%v", sawCreated, sawExited)
		}
	}
}

// ── list / info / rename / meta over the wire ───────────────────────

func TestControlPlane_ListInfoRenameMeta(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialT(t, sock)
	ok := c.spawn(proto.Spawn{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}, Name: "wired", Meta: map[string]string{"k": "v"}})

	c.send(proto.TypeList, proto.List{ReqID: c.nextReqID()}, nil)
	var list proto.ListOK
	if err := c.expect(proto.TypeListOK).DecodeHeader(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Name != "wired" || list.Sessions[0].Meta["k"] != "v" {
		t.Fatalf("ListOK = %+v", list)
	}

	// INFO resolves by name too.
	c.send(proto.TypeInfo, proto.Info{ReqID: c.nextReqID(), SessionID: "wired"}, nil)
	var info proto.InfoOK
	if err := c.expect(proto.TypeInfoOK).DecodeHeader(&info); err != nil {
		t.Fatal(err)
	}
	if info.Session.ID != ok.SessionID || info.Session.State != proto.StateRunning {
		t.Fatalf("InfoOK = %+v", info.Session)
	}

	c.send(proto.TypeRename, proto.Rename{ReqID: c.nextReqID(), SessionID: ok.SessionID, Name: "rewired"}, nil)
	c.expect(proto.TypeOK)
	c.send(proto.TypeSetMeta, proto.SetMeta{ReqID: c.nextReqID(), SessionID: "rewired", Meta: map[string]string{"k2": "v2"}}, nil)
	c.expect(proto.TypeOK)
	c.send(proto.TypeInfo, proto.Info{ReqID: c.nextReqID(), SessionID: "rewired"}, nil)
	// Fresh struct: json.Unmarshal MERGES into a pre-populated map, so
	// decoding into the earlier `info` would false-fail the wholesale check.
	var info2 proto.InfoOK
	if err := c.expect(proto.TypeInfoOK).DecodeHeader(&info2); err != nil {
		t.Fatal(err)
	}
	if info2.Session.Meta["k2"] != "v2" || info2.Session.Meta["k"] != "" {
		t.Errorf("meta not replaced wholesale: %v", info2.Session.Meta)
	}

	// Unknown session errors cleanly.
	c.send(proto.TypeInfo, proto.Info{ReqID: c.nextReqID(), SessionID: "ghost"}, nil)
	c.expectErrCode(string(errcodes.SessionNotFound))
}

// ── linger ──────────────────────────────────────────────────────────

func TestLinger_FalseKillsOnLastDetach(t *testing.T) {
	sock, srv := startServer(t, Options{})
	c := dialT(t, sock)
	linger := false
	ok := c.spawn(proto.Spawn{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}, Linger: &linger})
	c.attach(ok.SessionID, nil, false)

	c.send(proto.TypeDetach, proto.Detach{ReqID: c.nextReqID(), SessionID: ok.SessionID}, nil)
	c.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)

	sess, err := srv.Registry().Lookup(ok.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(testTimeout):
		t.Fatal("linger:false session survived last detach")
	}
}

// ── exit retention over the wire ────────────────────────────────────

func TestRetention_LateAttachSeesDeathAndScrollback(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c := dialT(t, sock)

	// Subscribe to events BEFORE spawning. The command exits almost instantly,
	// so subscribing after spawn would race the exit: EventExited can fire
	// before the subscription is registered, and the watcher would wait forever
	// (10s timeout under CI). Subscribe-then-act — same rule the examples teach.
	watcher := dialT(t, sock)
	watcher.send(proto.TypeSubEvents, proto.SubscribeEvents{ReqID: watcher.nextReqID(), SessionID: ""}, nil)
	watcher.expect(proto.TypeOK)

	ok := spawnShell(c, "echo famous-last-words; exit 3")

	// Wait for THIS session's exit via events on the watcher (no one attached).
	deadline := time.After(testTimeout)
	for exited := false; !exited; {
		select {
		case f, open := <-watcher.frames:
			if open && f.Type == proto.TypeEvent {
				var ev proto.Event
				_ = f.DecodeHeader(&ev)
				exited = ev.Type == proto.EventExited && ev.SessionID == ok.SessionID
			}
		case <-deadline:
			t.Fatal("never saw exited event")
		}
	}

	// LATE attach: session already dead, record retained → scrollback + EXIT.
	late := dialT(t, sock)
	late.attach(ok.SessionID, nil, false)
	data, _ := late.collectOutput(ok.SessionID, "famous-last-words")
	if len(data) == 0 {
		t.Fatal("no scrollback for exited session")
	}
	exit := late.expectSkipping(proto.TypeExit, proto.TypeOutput, proto.TypeEvent)
	var ex proto.Exit
	if err := exit.DecodeHeader(&ex); err != nil {
		t.Fatal(err)
	}
	if ex.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", ex.ExitCode)
	}
}

// ── daemon plumbing ─────────────────────────────────────────────────

func TestDiscoveryFile_WrittenAndRemoved(t *testing.T) {
	dir := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)

	srv, err := New(Options{HomeDir: dir, SocketPath: sockDir + "/d.sock", Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := contextWithCancel(t.Context())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()
	<-srv.Ready()

	raw, err := os.ReadFile(filepath.Join(dir, constants.DiscoveryFilename))
	if err != nil {
		t.Fatalf("discovery file: %v", err)
	}
	var d Discovery
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Pid != os.Getpid() || d.Version != "1.2.3" || d.ProtocolVersion != proto.ProtocolVersion || d.SocketPath == "" {
		t.Errorf("Discovery = %+v", d)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, constants.DiscoveryFilename)); !os.IsNotExist(err) {
		t.Error("discovery file survived shutdown")
	}
	if _, err := os.Stat(d.SocketPath); !os.IsNotExist(err) {
		t.Error("socket file survived shutdown")
	}
}

func TestSecondDaemonOnSameHomeRefused(t *testing.T) {
	dir := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)

	first, err := New(Options{HomeDir: dir, SocketPath: sockDir + "/a.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := contextWithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Serve(ctx) }()
	<-first.Ready()
	defer func() { cancel(); <-done }()

	second, err := New(Options{HomeDir: dir, SocketPath: sockDir + "/b.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Serve(t.Context()); !errcodes.IsCode(err, errcodes.LockHeld) {
		t.Fatalf("expected E_LOCK_HELD, got %v", err)
	}
}

func TestGracefulShutdown_KillsSessionsNoOrphans(t *testing.T) {
	dir := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)

	srv, err := New(Options{HomeDir: dir, SocketPath: sockDir + "/d.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := contextWithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-srv.Ready()

	c := dialT(t, sockDir+"/d.sock")
	var pids []int
	for range 3 {
		ok := spawnShell(c, "sleep 300")
		pids = append(pids, ok.Pid)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("shutdown hung")
	}
	// Every session process must be gone.
	for _, pid := range pids {
		if processAlive(pid) {
			t.Errorf("pid %d survived graceful shutdown", pid)
		}
	}
}
