package client

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/thesatellite-ai/runbaypty/pkg/daemon"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testTimeout = 10 * time.Second

// startDaemon runs a real daemon on a short-path socket for SDK tests.
func startDaemon(t *testing.T) string {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "rptyc-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	srv, err := daemon.New(daemon.Options{
		HomeDir:    t.TempDir(),
		SocketPath: sockDir + "/d.sock",
		Version:    "sdk-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("daemon shutdown hung")
		}
	})
	select {
	case <-srv.Ready():
	case <-time.After(testTimeout):
		t.Fatal("daemon never ready")
	}
	return sockDir + "/d.sock"
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// readUntil reads from st until the accumulated bytes contain want.
func readUntil(t *testing.T, st *Stream, want string) []byte {
	t.Helper()
	var acc []byte
	buf := make([]byte, 4096)
	deadline := time.Now().Add(testTimeout)
	for !bytes.Contains(acc, []byte(want)) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q; have %q", want, acc)
		}
		n, err := st.Read(buf)
		acc = append(acc, buf[:n]...)
		if err != nil {
			if bytes.Contains(acc, []byte(want)) {
				break
			}
			t.Fatalf("stream ended before %q: %v (have %q)", want, err, acc)
		}
	}
	return acc
}

func TestSDK_SpawnAttachReadKill(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if !strings.HasPrefix(c.ClientID(), "cli_") {
		t.Errorf("ClientID = %q", c.ClientID())
	}

	id, pid, err := c.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo sdk-hello; sleep 300"}, Name: "sdk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "ses_") || pid <= 0 {
		t.Fatalf("Spawn = (%s, %d)", id, pid)
	}

	// Attach by NAME — the SDK resolves to the canonical id.
	st, err := c.Attach(ctxT(t), "sdk-test", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionID() != id {
		t.Errorf("stream session = %s, want %s", st.SessionID(), id)
	}
	readUntil(t, st, "sdk-hello")

	if err := c.Kill(ctxT(t), id, proto.SignalKILL); err != nil {
		t.Fatal(err)
	}
	// Stream drains then EOFs; Exit reports the signal.
	if _, err := io.ReadAll(st); err != nil {
		t.Fatalf("drain after kill: %v", err)
	}
	code, sig, exited := st.Exit()
	if !exited || code != -1 || sig != proto.SignalKILL {
		t.Errorf("Exit = (%d, %q, %v)", code, sig, exited)
	}
}

func TestSDK_ZeroGapResumeAcrossConnections(t *testing.T) {
	sock := startDaemon(t)

	boss, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()
	id, _, err := boss.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", `i=0; while :; do echo n-$i; i=$((i+1)); sleep 0.005; done`}})
	if err != nil {
		t.Fatal(err)
	}

	// Connection 1 reads a while, remembers LastSeq, dies without detach.
	c1, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	st1, err := c1.Attach(ctxT(t), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	part1 := readUntil(t, st1, "n-15\r\n")
	resumeAt := st1.LastSeq()
	_ = c1.Close()

	time.Sleep(200 * time.Millisecond) // stream runs on with nobody attached

	// Connection 2 resumes exactly at LastSeq — the SDK's zero-gap contract.
	c2, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	st2, err := c2.Attach(ctxT(t), id, &resumeAt, false)
	if err != nil {
		t.Fatal(err)
	}
	part2 := readUntil(t, st2, "n-60\r\n")

	joined := strings.ReplaceAll(string(part1)+string(part2), "\r\n", "\n")
	next := 0
	for _, line := range strings.Split(joined, "\n") {
		if line == "" {
			continue
		}
		var n int
		if _, err := fmtSscanf(line, &n); err != nil {
			t.Fatalf("unparseable line %q", line)
		}
		if n != next {
			t.Fatalf("gap/dup at the resume seam: got n-%d, want n-%d", n, next)
		}
		next++
	}
	if next < 60 {
		t.Fatalf("only %d lines audited", next)
	}
	_ = boss.Kill(ctxT(t), id, proto.SignalKILL)
}

func fmtSscanf(line string, n *int) (int, error) {
	// strconv over fmt.Sscanf: strict, no surprise whitespace tolerance.
	rest, ok := strings.CutPrefix(line, "n-")
	if !ok {
		return 0, errcodes.Newf(errcodes.InvalidInput, "line %q", line)
	}
	v, err := strconvAtoi(rest)
	if err != nil {
		return 0, err
	}
	*n = v
	return 1, nil
}

func strconvAtoi(s string) (int, error) {
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errcodes.Newf(errcodes.InvalidInput, "non-digit in %q", s)
		}
		v = v*10 + int(r-'0')
	}
	return v, nil
}

func TestSDK_InputAndWriteRefusal(t *testing.T) {
	sock := startDaemon(t)
	a, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id, _, err := a.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	stA, err := a.Attach(ctxT(t), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// A takes the lock and types; the echo comes back.
	if err := a.TakeWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}
	if err := a.Input(id, []byte("hello-input\r")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, stA, "hello-input")

	// B attaches and tries to type — refused, surfaced via WriteRefusal.
	b, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	stB, err := b.Attach(ctxT(t), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Input(id, []byte("intruder")); err != nil {
		t.Fatal(err) // send itself succeeds; refusal is a push
	}
	deadline := time.Now().Add(testTimeout)
	for {
		if refusal := stB.WriteRefusal(); refusal != nil {
			if !errcodes.IsCode(refusal, errcodes.NoWriteLock) {
				t.Fatalf("refusal = %v, want E_NO_WRITE_LOCK", refusal)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("write refusal never surfaced")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = a.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestSDK_ListInfoRenameMetaEvents(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	events, err := c.SubscribeEvents(ctxT(t), "")
	if err != nil {
		t.Fatal(err)
	}

	id, _, err := c.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}, Name: "one", Meta: map[string]string{"a": "1"}})
	if err != nil {
		t.Fatal(err)
	}

	list, err := c.List(ctxT(t))
	if err != nil || len(list) != 1 || list[0].Name != "one" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := c.Rename(ctxT(t), id, "two"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMeta(ctxT(t), "two", map[string]string{"b": "2"}); err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(ctxT(t), "two")
	if err != nil || info.Meta["b"] != "2" || info.Meta["a"] != "" {
		t.Fatalf("Info = %+v, %v", info, err)
	}

	// Events observed: created + renamed at minimum.
	saw := map[proto.EventType]bool{}
	deadline := time.After(testTimeout)
	for !saw[proto.EventCreated] || !saw[proto.EventRenamed] {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event channel closed early")
			}
			if ev.SessionID == id {
				saw[ev.Type] = true
			}
		case <-deadline:
			t.Fatalf("events seen: %v", saw)
		}
	}

	// Unknown lookups map to the typed error.
	if _, err := c.Info(ctxT(t), "ghost"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("Info(ghost) = %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestSDK_DialFailsFastWithoutDaemon(t *testing.T) {
	_, err := Dial("/tmp/definitely-not-a-daemon.sock")
	if !errcodes.IsCode(err, errcodes.DaemonUnreachable) {
		t.Fatalf("expected E_DAEMON_UNREACHABLE, got %v", err)
	}
}

func TestSDK_DetachDrainsThenEOF(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, _, err := c.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo before-detach; sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Attach(ctxT(t), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, st, "before-detach")
	if err := st.Detach(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(st); err != nil {
		t.Fatalf("post-detach drain: %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

// ── coverage gap closers ────────────────────────────────────────────

func TestSDK_WatchOverSDK(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, _, err := c.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TakeWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}
	matches, err := c.Watch(ctxT(t), id, `SDK-[0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Input(id, []byte("hit SDK-42 here\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-matches:
		if ev.Match != "SDK-42" || ev.SessionID != id || ev.Seq == 0 {
			t.Errorf("watch event = %+v", ev)
		}
	case <-time.After(testTimeout):
		t.Fatal("no watch event")
	}
	// A bad pattern surfaces the typed error.
	if _, err := c.Watch(ctxT(t), id, "(["); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("bad pattern = %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestSDK_FollowAccessors(t *testing.T) {
	sock := startDaemon(t)
	boss, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()
	id, _, err := boss.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo followed; sleep 300"}, Name: "followee"})
	if err != nil {
		t.Fatal(err)
	}

	// Follow by NAME resolves to the canonical id, and LastSeq advances.
	fl, err := Follow(ctxT(t), sock, "followee", FollowOpts{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	if fl.SessionID() != id {
		t.Errorf("Follower.SessionID() = %q, want %q", fl.SessionID(), id)
	}
	if fl.LastSeq() != 0 {
		t.Errorf("LastSeq before any read = %d", fl.LastSeq())
	}
	buf := make([]byte, 4096)
	if _, err := fl.Read(buf); err != nil {
		t.Fatal(err)
	}
	if fl.LastSeq() == 0 {
		t.Error("LastSeq did not advance after a read")
	}
	if _, _, exited := fl.Exit(); exited {
		t.Error("Exit() reports exited on a live session")
	}
	_ = boss.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestSDK_StreamWriteRefusalCleared(t *testing.T) {
	sock := startDaemon(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, _, err := c.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Attach(ctxT(t), id, nil, true) // read-only attach
	if err != nil {
		t.Fatal(err)
	}
	if st.WriteRefusal() != nil {
		t.Error("fresh stream reports a refusal")
	}
	if err := c.Input(id, []byte("x")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testTimeout)
	var refusal error
	for refusal == nil {
		refusal = st.WriteRefusal()
		if time.Now().After(deadline) {
			t.Fatal("read-only input refusal never surfaced")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !errcodes.IsCode(refusal, errcodes.ReadOnlyAttach) {
		t.Errorf("refusal = %v, want E_READ_ONLY_ATTACH", refusal)
	}
	// WriteRefusal clears on read (one-shot).
	if again := st.WriteRefusal(); again != nil {
		t.Errorf("WriteRefusal not cleared: %v", again)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}
