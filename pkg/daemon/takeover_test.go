package daemon

// takeover_test.go — the M9 flagship gates.
//
// t1: a generator streams numbered lines while the daemon is REPLACED
//     underneath it; the consumer (client.Follow, reconnecting through the
//     same socket path) must see a contiguous stream — zero gap, zero dup,
//     same session pid — across the upgrade.
// t2: a takeover that fails before ACK leaves the OLD daemon fully
//     functional (rollback) and every session unharmed.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestTakeover_ZeroGapMidStream(t *testing.T) {
	home := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)
	sock := sockDir + "/d.sock"

	// Daemon A (the "old version").
	oldSrv, err := New(Options{HomeDir: home, SocketPath: sock, Version: "old"})
	if err != nil {
		t.Fatal(err)
	}
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	oldDone := make(chan error, 1)
	go func() { oldDone <- oldSrv.Serve(oldCtx) }()
	<-oldSrv.Ready()

	// A generator session + its original pid.
	boss, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	id, pidBefore, err := boss.Spawn(ctxT(t), client.SpawnOpts{
		Cmd: "/bin/sh", Args: []string{"-c", `i=0; while :; do echo h-$i; i=$((i+1)); sleep 0.004; done`},
		Name: "survivor", Meta: map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = boss.Close() // the follower is the only client from here

	// The consumer: Follow through the SAME socket path — its reconnect
	// loop is exactly how real clients ride out the upgrade.
	fctx, fcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer fcancel()
	fl, err := client.Follow(fctx, sock, id, client.FollowOpts{ReadOnly: true, InitialBackoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()

	var acc []byte
	buf := make([]byte, 8192)
	readLines := func() int { return strings.Count(string(acc), "\n") }
	for readLines() < 30 { // warm stream on the OLD daemon
		n, rerr := fl.Read(buf)
		acc = append(acc, buf[:n]...)
		if rerr != nil {
			t.Fatalf("pre-takeover read: %v", rerr)
		}
	}

	// ── THE UPGRADE ── daemon B takes over mid-stream.
	adopted, err := RequestTakeover(sock, home)
	if err != nil {
		t.Fatalf("RequestTakeover: %v", err)
	}
	if len(adopted.Sessions) != 1 || adopted.Sessions[0].State.Name != "survivor" {
		t.Fatalf("adopted = %+v", adopted.Sessions)
	}
	newSrv, err := New(Options{HomeDir: home, SocketPath: sock, Version: "new", Adopted: adopted})
	if err != nil {
		t.Fatal(err)
	}
	newCtx, newCancel := context.WithCancel(context.Background())
	newDone := make(chan error, 1)
	go func() { newDone <- newSrv.Serve(newCtx) }()
	t.Cleanup(func() {
		newCancel()
		select {
		case <-newDone:
		case <-time.After(testTimeout):
			t.Error("new daemon shutdown hung")
		}
	})
	<-newSrv.Ready()

	// The old daemon must retire CLEANLY via the handover path (nil, and
	// crucially: without killing the session).
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("old daemon exit: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("old daemon never retired")
	}

	// Keep reading through the seam until well past the takeover.
	for readLines() < 150 {
		n, rerr := fl.Read(buf)
		acc = append(acc, buf[:n]...)
		if rerr != nil {
			t.Fatalf("post-takeover read: %v (%d lines)", rerr, readLines())
		}
	}

	// Audit: h-0 … h-149 contiguous across the upgrade. THE guarantee.
	next := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(acc), "\r\n", "\n"), "\n") {
		rest, ok := strings.CutPrefix(line, "h-")
		if !ok || rest == "" {
			continue
		}
		n, aerr := strconvAtoiT(strings.TrimSpace(rest))
		if aerr != nil {
			continue // torn tail line
		}
		if n != next {
			t.Fatalf("upgrade broke the stream: got h-%d, want h-%d", n, next)
		}
		next++
	}
	if next < 150 {
		t.Fatalf("only %d lines audited", next)
	}

	// Same PROCESS survived: pid unchanged, name + meta intact, adopted
	// daemon serves INFO for it.
	c2, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	info, err := c2.Info(ctxT(t), "survivor")
	if err != nil {
		t.Fatal(err)
	}
	if info.Pid != pidBefore {
		t.Errorf("pid changed across takeover: %d → %d", pidBefore, info.Pid)
	}
	if info.Meta["k"] != "v" {
		t.Errorf("meta lost: %v", info.Meta)
	}
	if !processAlive(pidBefore) {
		t.Error("session process died during takeover")
	}
	_ = c2.Kill(ctxT(t), id, proto.SignalKILL)
}

func strconvAtoiT(s string) (int, error) {
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, context.DeadlineExceeded // any error will do for the filter
		}
		v = v*10 + int(r-'0')
	}
	return v, nil
}

func TestTakeover_FailureRollsBackCleanly(t *testing.T) {
	sock, srv := startServer(t, Options{})
	c := dialT(t, sock)
	ok := spawnShell(c, "echo alive; sleep 300")
	c.collectOutputAfterAttach(t, ok.SessionID)

	// A takeover request pointing at a reply path nobody listens on: the
	// old daemon must roll back — resume readers, re-listen, keep serving.
	c.send(proto.TypeHandoverReq, proto.HandoverReq{ReqID: c.nextReqID(), ReplyPath: "/tmp/definitely-not-listening.sock"}, nil)
	c.expect(proto.TypeOK) // accepted; failure happens async

	// The handover FIRST tears down every client conn (including ours) and
	// unlinks the socket; only then does the rollback re-listen. Wait for
	// our conn to drop before polling, or the poll can hit the pre-teardown
	// listener and "succeed" into the gap.
	for {
		if _, open := <-c.frames; !open {
			break
		}
	}

	// Poll with raw dials (t.Fatal inside dialT cannot be retried).
	deadline := time.Now().Add(testTimeout)
	for {
		nc, derr := net.DialTimeout("unix", sock, time.Second)
		if derr == nil {
			_ = nc.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never recovered from failed takeover: %v", derr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	c2 := dialT(t, sock)

	// Full service restored: the old session is intact AND still streams.
	att := c2.attach(ok.SessionID, nil, false)
	if att.SessionID != ok.SessionID {
		t.Fatalf("attach after rollback: %+v", att)
	}
	data, _ := c2.collectOutput(ok.SessionID, "alive")
	if len(data) == 0 {
		t.Fatal("no replay after rollback")
	}
	// And the daemon still spawns fresh sessions.
	ok2 := spawnShell(c2, "echo rollback-fresh; sleep 300")
	c2.attach(ok2.SessionID, nil, false)
	c2.collectOutput(ok2.SessionID, "rollback-fresh")

	_ = srv
}

// collectOutputAfterAttach is a tiny helper: attach + wait for first bytes.
func (c *testClient) collectOutputAfterAttach(t *testing.T, sessionID string) {
	t.Helper()
	c.attach(sessionID, nil, false)
	c.collectOutput(sessionID, "alive")
}
