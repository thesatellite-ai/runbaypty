package client

// follow_test.go — the reconnect guarantee, proven with a connection-killing
// proxy: the daemon stays healthy while the client's transport dies
// repeatedly; the Follower must deliver a byte-exact, gap-free, dup-free
// stream across every seam.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// chaosProxy pipes UDS connections to the real daemon socket and can kill
// every active pipe on demand (the client sees a dropped connection; the
// daemon sees a disconnect — exactly a client-side crash/restart).
type chaosProxy struct {
	t        *testing.T
	front    string
	backend  string
	ln       net.Listener
	mu       sync.Mutex
	active   []net.Conn
	closed   bool
	loopDone chan struct{}
}

func newChaosProxy(t *testing.T, backend string) *chaosProxy {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rptypx-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	front := dir + "/proxy.sock"
	ln, err := net.Listen("unix", front)
	if err != nil {
		t.Fatal(err)
	}
	p := &chaosProxy{t: t, front: front, backend: backend, ln: ln, loopDone: make(chan struct{})}
	go p.acceptLoop()
	t.Cleanup(p.shutdown)
	return p
}

func (p *chaosProxy) acceptLoop() {
	defer close(p.loopDone)
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		back, err := net.Dial("unix", p.backend)
		if err != nil {
			_ = conn.Close()
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.Close()
			_ = back.Close()
			return
		}
		p.active = append(p.active, conn, back)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(back, conn); _ = back.Close() }()
		go func() { _, _ = io.Copy(conn, back); _ = conn.Close() }()
	}
}

// killConnections drops every live pipe (new connections still accepted).
func (p *chaosProxy) killConnections() {
	p.mu.Lock()
	conns := p.active
	p.active = nil
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (p *chaosProxy) shutdown() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_ = p.ln.Close()
	p.killConnections()
	<-p.loopDone
}

func TestFollow_ZeroGapAcrossRepeatedConnectionLoss(t *testing.T) {
	sock := startDaemon(t)
	proxy := newChaosProxy(t, sock)

	// Direct (un-proxied) control connection spawns the generator.
	boss, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()
	id, _, err := boss.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", `i=0; while :; do echo f-$i; i=$((i+1)); sleep 0.004; done`}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fl, err := Follow(ctx, proxy.front, id, FollowOpts{ReadOnly: true, InitialBackoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()

	// Read continuously while the proxy assassinates the connection three
	// times. The consumer-visible stream must stay contiguous.
	var acc []byte
	buf := make([]byte, 4096)
	kills := 0
	for {
		n, rerr := fl.Read(buf)
		acc = append(acc, buf[:n]...)
		if rerr != nil {
			t.Fatalf("Follower.Read: %v (after %d bytes, %d kills)", rerr, len(acc), kills)
		}
		lines := strings.Count(string(acc), "\n")
		if kills < 3 && lines > (kills+1)*25 {
			proxy.killConnections()
			kills++
		}
		if lines > 120 && kills == 3 {
			break
		}
	}

	// Audit: f-0 … f-N contiguous, no dup, no gap, across three seams.
	next := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(acc), "\r\n", "\n"), "\n") {
		rest, ok := strings.CutPrefix(line, "f-")
		if !ok || rest == "" {
			continue
		}
		n, err := strconvAtoi(strings.TrimSpace(rest))
		if err != nil {
			continue // torn final line in the buffer — fine
		}
		if n != next {
			t.Fatalf("seam broke the stream: got f-%d, want f-%d (kills=%d)", n, next, kills)
		}
		next++
	}
	if next < 100 {
		t.Fatalf("only %d lines audited", next)
	}
	_ = boss.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestFollow_SessionExitEndsWithEOFAndOutcome(t *testing.T) {
	sock := startDaemon(t)
	fl := func() *Follower {
		boss, err := Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		defer boss.Close()
		id, _, err := boss.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo done-output; exit 4"}})
		if err != nil {
			t.Fatal(err)
		}
		fl, err := Follow(ctxT(t), sock, id, FollowOpts{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		return fl
	}()
	defer fl.Close()

	data, err := io.ReadAll(fl)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(data), "done-output") {
		t.Errorf("missing output: %q", data)
	}
	if code, _, exited := fl.Exit(); !exited || code != 4 {
		t.Errorf("Exit = (%d, exited=%v), want (4, true)", code, exited)
	}
}

func TestFollow_SessionGoneIsDefinitiveNotRetryForever(t *testing.T) {
	sock := startDaemon(t)
	_, err := Follow(ctxT(t), sock, "ses_018f3b2c9a8e7891b4f51234567890ab", FollowOpts{})
	if err == nil {
		t.Fatal("Follow of a nonexistent session must fail fast")
	}
}

func TestFollow_ReconnectGivesUpOnContextCancel(t *testing.T) {
	// A follower whose daemon never returns must exit on ctx cancel, not
	// spin forever — the reconnect loop's only unbounded wait.
	sock := startDaemon(t)
	proxy := newChaosProxy(t, sock)

	boss, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()
	id, _, err := boss.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", `while :; do echo x; sleep 0.01; done`}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	fl, err := Follow(ctx, proxy.front, id, FollowOpts{ReadOnly: true, InitialBackoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	buf := make([]byte, 1024)
	if _, err := fl.Read(buf); err != nil {
		t.Fatal(err)
	}

	// Kill the transport permanently, then cancel: Read must return the
	// context error rather than retrying forever.
	proxy.shutdown()
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	deadline := time.After(testTimeout)
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, rerr := fl.Read(buf); rerr != nil {
				errCh <- rerr
				return
			}
		}
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Read after permanent transport loss = %v, want context.Canceled", err)
		}
	case <-deadline:
		t.Fatal("Follower.Read never gave up after ctx cancel")
	}
	_ = boss.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestFollow_ReconnectSurfacesDefinitiveDaemonAnswer(t *testing.T) {
	// If the daemon comes back but the SESSION is gone, Follow must surface
	// that rather than retry: a reaped session never returns.
	sock := startDaemon(t)
	boss, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer boss.Close()
	id, _, err := boss.Spawn(ctxT(t), SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo hi; sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	fl, err := Follow(ctxT(t), sock, id, FollowOpts{ReadOnly: true, InitialBackoff: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	buf := make([]byte, 1024)
	if _, err := fl.Read(buf); err != nil {
		t.Fatal(err)
	}
	// Kill the session, then force a reconnect by closing the SDK's conn.
	_ = boss.Kill(ctxT(t), id, proto.SignalKILL)
	// The stream ends cleanly with EOF (retention replays the death) — the
	// definitive answer, not an infinite retry.
	deadline := time.Now().Add(testTimeout)
	for {
		_, rerr := fl.Read(buf)
		if rerr == io.EOF {
			if _, _, exited := fl.Exit(); !exited {
				t.Error("EOF without an exit outcome")
			}
			return
		}
		if rerr != nil {
			t.Fatalf("Read = %v, want io.EOF", rerr)
		}
		if time.Now().After(deadline) {
			t.Fatal("stream never ended after the session was killed")
		}
	}
}
