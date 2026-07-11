package daemon

// lifecycle_test.go — Server construction branches and the shutdown paths
// the happy-path suites skip: option defaults, bad home, and the SIGTERM →
// deadline → SIGKILL escalation for a session that ignores TERM.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestNew_AppliesDefaultsAndEnvPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv(constants.EnvHome, home)
	t.Setenv(constants.EnvSock, filepath.Join(home, "custom.sock"))

	srv, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if srv.opts.HomeDir != home {
		t.Errorf("HomeDir = %q, want the env value", srv.opts.HomeDir)
	}
	if srv.opts.SocketPath != filepath.Join(home, "custom.sock") {
		t.Errorf("SocketPath = %q", srv.opts.SocketPath)
	}
	if srv.opts.RetentionTTL != constants.DefaultRetentionTTL {
		t.Errorf("RetentionTTL = %v, want the default", srv.opts.RetentionTTL)
	}
	if srv.opts.Logger == nil {
		t.Error("nil Logger not defaulted")
	}
	if srv.Registry() == nil || srv.Ready() == nil {
		t.Error("Registry/Ready not initialized")
	}
}

func TestServe_UnwritableHomeFailsCleanly(t *testing.T) {
	// A home dir that cannot be created must error out, not panic.
	srv, err := New(Options{HomeDir: "/proc/nonexistent-root/home", SocketPath: "/tmp/never.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(t.Context()); err == nil {
		t.Fatal("Serve with an uncreatable home should error")
	}
}

func TestShutdown_EscalatesToSIGKILLForStubbornSession(t *testing.T) {
	home := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)

	srv, err := New(Options{HomeDir: home, SocketPath: sockDir + "/d.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-srv.Ready()

	c, err := client.Dial(sockDir + "/d.sock")
	if err != nil {
		t.Fatal(err)
	}
	// A session that CANNOT die from SIGTERM: it BLOCKS the signal, so TERM
	// stays pending forever and only SIGKILL (unblockable) ends it. That is
	// the escalation path, deterministically. (`trap "" TERM` in a shell and
	// SIGSTOP both fail here: inside a PTY the group TERM still takes the
	// shell down, and macOS terminates stopped processes on a default-action
	// signal rather than leaving it pending.)
	perl, lookErr := exec.LookPath("perl")
	if lookErr != nil {
		t.Skip("perl unavailable — cannot build a TERM-blocking session")
	}
	const blockTerm = `use POSIX; my $s = POSIX::SigSet->new(POSIX::SIGTERM); sigprocmask(SIG_BLOCK, $s); sleep 300`
	_, pid, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: perl, Args: []string{"-e", blockTerm}})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	time.Sleep(300 * time.Millisecond) // let perl install the signal mask

	if n := len(srv.Registry().List()); n != 1 {
		t.Fatalf("registry has %d sessions before shutdown", n)
	}
	if !processAlive(pid) {
		t.Fatal("session gone before shutdown")
	}
	select {
	case <-srv.Registry().List()[0].Done():
		t.Fatal("session reports Done() before shutdown — nothing to escalate")
	default:
	}

	// Shorten the grace period so the test doesn't wait the production 5s.
	origDeadline := shutdownDeadline
	shutdownDeadline = 400 * time.Millisecond
	t.Cleanup(func() { shutdownDeadline = origDeadline })

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * shutdownDeadline):
		t.Fatal("shutdown never completed — escalation missing?")
	}
	// TERM got its full grace period before the KILL escalation.
	if elapsed := time.Since(start); elapsed < shutdownDeadline {
		t.Errorf("shutdown took %v — TERM should have been given the full %v grace", elapsed, shutdownDeadline)
	}
	if processAlive(pid) {
		t.Error("stubborn session survived shutdown — SIGKILL escalation failed")
	}
}

func TestShutdown_EscalatesToSIGKILLForEveryStubbornSession(t *testing.T) {
	// Regression: SIGKILL escalation must fire for EVERY straggler, not just
	// the first. A single-shot deadline channel fires once, so a second
	// TERM-ignoring session would block shutdown forever — this spawns three.
	home := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)

	srv, err := New(Options{HomeDir: home, SocketPath: sockDir + "/d.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-srv.Ready()

	c, err := client.Dial(sockDir + "/d.sock")
	if err != nil {
		t.Fatal(err)
	}
	perl, lookErr := exec.LookPath("perl")
	if lookErr != nil {
		t.Skip("perl unavailable — cannot build TERM-blocking sessions")
	}
	const blockTerm = `use POSIX; my $s = POSIX::SigSet->new(POSIX::SIGTERM); sigprocmask(SIG_BLOCK, $s); sleep 300`
	pids := make([]int, 0, 3)
	for range 3 {
		_, pid, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: perl, Args: []string{"-e", blockTerm}})
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	_ = c.Close()
	time.Sleep(300 * time.Millisecond) // let perl install the signal masks

	if n := len(srv.Registry().List()); n != 3 {
		t.Fatalf("registry has %d sessions before shutdown, want 3", n)
	}

	origDeadline := shutdownDeadline
	shutdownDeadline = 400 * time.Millisecond
	t.Cleanup(func() { shutdownDeadline = origDeadline })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(4 * shutdownDeadline):
		t.Fatal("shutdown never completed — a straggler beyond the first was not SIGKILL-escalated")
	}
	for _, pid := range pids {
		if processAlive(pid) {
			t.Errorf("stubborn session pid %d survived shutdown — SIGKILL escalation failed", pid)
		}
	}
}

func TestDaemonStopping_EventReachesSubscribers(t *testing.T) {
	home := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)
	srv, err := New(Options{HomeDir: home, SocketPath: sockDir + "/d.sock", Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	<-srv.Ready()

	// Subscribe directly on the bus (a client conn would be torn down first).
	subID, events := srv.Registry().Events().Subscribe("")
	defer srv.Registry().Events().Unsubscribe(subID)

	cancel()
	<-done

	deadline := time.After(testTimeout)
	for {
		select {
		case ev := <-events:
			if ev.Type == proto.EventDaemonStopping {
				return
			}
		case <-deadline:
			t.Fatal("daemon-stopping event never emitted")
		}
	}
}

func TestDiscovery_RemovedAndTokensOnlyWithWS(t *testing.T) {
	// Without --ws-port there are no tokens and no ws_port in discovery.
	sock, srv := startServer(t, Options{})
	_ = sock
	raw, err := os.ReadFile(filepath.Join(srv.opts.HomeDir, constants.DiscoveryFilename))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, "ws_port") || strings.Contains(body, "token_path") {
		t.Errorf("discovery leaks WS fields with WS disabled:\n%s", body)
	}
	for _, name := range []string{constants.TokenFilename, tokenROFilename} {
		if _, err := os.Stat(filepath.Join(srv.opts.HomeDir, name)); !os.IsNotExist(err) {
			t.Errorf("token %s minted with WS disabled", name)
		}
	}
}
