package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// TestMain enforces zero leaked goroutines across the whole host package —
// every reader pump and cond waiter must have a clean exit path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testTimeout = 10 * time.Second

// shCfg builds a SpawnConfig running sh -c script with sane defaults.
func shCfg(script string) SpawnConfig {
	return SpawnConfig{Cmd: "/bin/sh", Args: []string{"-c", script}, Cols: 80, Rows: 24, Linger: true}
}

// waitExit fails the test if the session hasn't exited within the timeout.
func waitExit(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(testTimeout):
		t.Fatalf("session %s did not exit within %v", s.ID(), testTimeout)
	}
}

// waitOutputContains polls via WaitOutput until the ring contains want.
func waitOutputContains(t *testing.T, s *Session, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var seen uint64
	for {
		data, _, _ := s.ReplayFrom(0)
		if bytes.Contains(data, []byte(want)) {
			return
		}
		last, exited := s.WaitOutput(ctx, seen)
		if ctx.Err() != nil {
			t.Fatalf("timeout waiting for %q; ring: %q", want, data)
		}
		if exited && last == seen {
			data, _, _ = s.ReplayFrom(0)
			if !bytes.Contains(data, []byte(want)) {
				t.Fatalf("session exited without emitting %q; ring: %q", want, data)
			}
			return
		}
		seen = last
	}
}

// spawnT spawns through a fresh registry and reaps the process at cleanup.
func spawnT(t *testing.T, cfg SpawnConfig) *Session {
	t.Helper()
	r := NewRegistry(0)
	s, err := r.Spawn(cfg)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Kill(proto.SignalKILL)
		waitExit(t, s)
	})
	return s
}

func TestSession_OutputLandsInRing(t *testing.T) {
	s := spawnT(t, shCfg("echo hello-runbaypty"))
	waitExit(t, s) // fast exit: final bytes must still be captured (flush-before-exit)
	waitOutputContains(t, s, "hello-runbaypty")
	if code, sig, ok := s.ExitCode(); !ok || code != 0 || sig != "" {
		t.Errorf("ExitCode = (%d, %q, %v), want (0, \"\", true)", code, sig, ok)
	}
	if s.State() != proto.StateExited {
		t.Errorf("State = %s, want exited", s.State())
	}
}

func TestSession_CwdAndEnvPropagate(t *testing.T) {
	dir, err := os.MkdirTemp("", "rpty-cwd-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	cfg := shCfg(`echo "cwd=$(pwd) foo=$FOO"`)
	cfg.Cwd = dir
	cfg.Env = []string{"FOO=bar-value"}
	s := spawnT(t, cfg)
	waitExit(t, s)
	waitOutputContains(t, s, "foo=bar-value")
	// macOS: /tmp is a symlink; pwd may print either form. Match the tail.
	data, _, _ := s.ReplayFrom(0)
	base := dir[strings.LastIndex(dir, "/"):]
	if !bytes.Contains(data, []byte(base)) {
		t.Errorf("cwd not honored; ring: %q", data)
	}
}

func TestSession_InitialSizeApplied(t *testing.T) {
	cfg := shCfg("stty size")
	cfg.Cols, cfg.Rows = 120, 40
	s := spawnT(t, cfg)
	waitExit(t, s)
	waitOutputContains(t, s, "40 120") // stty prints "rows cols"
}

func TestSession_LiveResize(t *testing.T) {
	// A shell reading commands from the PTY: resize live, then ask.
	s := spawnT(t, SpawnConfig{Cmd: "/bin/sh", Cols: 80, Rows: 24})
	s.TakeWrite("cli_test")
	if err := s.Resize(132, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := s.WriteInput("cli_test", []byte("stty size; exit\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	waitOutputContains(t, s, "50 132")
	waitExit(t, s)
}

func TestSession_KillTreeReachesGrandchildren(t *testing.T) {
	// The background sleep is a grandchild; TERM on the group must end it.
	s := spawnT(t, shCfg("sleep 300 & echo ready; wait"))
	waitOutputContains(t, s, "ready")
	pid := s.Info().Pid

	if err := s.Kill(proto.SignalTERM); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitExit(t, s)

	// The whole process group must be gone (signal 0 probe → ESRCH).
	deadline := time.Now().Add(testTimeout)
	for {
		err := syscall.Kill(-pid, 0)
		if err == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still alive after kill-tree", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSession_ExitCodes(t *testing.T) {
	t.Run("clean exit 3", func(t *testing.T) {
		s := spawnT(t, shCfg("exit 3"))
		waitExit(t, s)
		if code, sig, _ := s.ExitCode(); code != 3 || sig != "" {
			t.Errorf("ExitCode = (%d, %q), want (3, \"\")", code, sig)
		}
	})
	t.Run("signal death reports wire signal name", func(t *testing.T) {
		s := spawnT(t, shCfg("echo up; sleep 300"))
		waitOutputContains(t, s, "up")
		if err := s.Kill(proto.SignalKILL); err != nil {
			t.Fatal(err)
		}
		waitExit(t, s)
		if code, sig, _ := s.ExitCode(); code != -1 || sig != proto.SignalKILL {
			t.Errorf("ExitCode = (%d, %q), want (-1, %q)", code, sig, proto.SignalKILL)
		}
	})
	t.Run("unknown signal name refused", func(t *testing.T) {
		s := spawnT(t, shCfg("sleep 300"))
		if err := s.Kill("SIGPONY"); !errcodes.IsCode(err, errcodes.InvalidInput) {
			t.Errorf("expected E_INVALID_INPUT, got %v", err)
		}
	})
}

func TestSession_WriteLock(t *testing.T) {
	s := spawnT(t, SpawnConfig{Cmd: "/bin/cat", Cols: 80, Rows: 24})

	// Unheld: session permits (auto-claim policy is the daemon's decision).
	if err := s.WriteInput("cli_a", []byte("x")); err != nil {
		t.Fatalf("unheld write: %v", err)
	}

	s.TakeWrite("cli_a")
	if err := s.WriteInput("cli_b", []byte("y")); !errcodes.IsCode(err, errcodes.NoWriteLock) {
		t.Errorf("expected E_NO_WRITE_LOCK for non-holder, got %v", err)
	}
	// Takeover moves the lock; the old holder is now refused.
	s.TakeWrite("cli_b")
	if err := s.WriteInput("cli_a", []byte("z")); !errcodes.IsCode(err, errcodes.NoWriteLock) {
		t.Errorf("expected E_NO_WRITE_LOCK after takeover, got %v", err)
	}
	if err := s.WriteInput("cli_b", []byte("ok")); err != nil {
		t.Errorf("holder write refused: %v", err)
	}
	// Release by a non-holder is a no-op; by the holder it frees the lock.
	s.ReleaseWrite("cli_a")
	if got := s.WriteLockHolder(); got != "cli_b" {
		t.Errorf("non-holder release changed holder to %q", got)
	}
	s.ReleaseWrite("cli_b")
	if got := s.WriteLockHolder(); got != "" {
		t.Errorf("holder release left %q", got)
	}
}

func TestSession_InputEOFEndsCat(t *testing.T) {
	s := spawnT(t, SpawnConfig{Cmd: "/bin/cat", Cols: 80, Rows: 24})
	if err := s.InputEOF("cli_a"); err != nil {
		t.Fatalf("InputEOF: %v", err)
	}
	waitExit(t, s) // cat exits on EOF — proves ^D reached the line discipline
	if code, _, _ := s.ExitCode(); code != 0 {
		t.Errorf("cat exit code = %d, want 0", code)
	}
}

func TestSession_InputAfterExitRefused(t *testing.T) {
	s := spawnT(t, shCfg("true"))
	waitExit(t, s)
	if err := s.WriteInput("cli_a", []byte("x")); !errcodes.IsCode(err, errcodes.SessionExited) {
		t.Errorf("expected E_SESSION_EXITED, got %v", err)
	}
	if err := s.Resize(10, 10); !errcodes.IsCode(err, errcodes.SessionExited) {
		t.Errorf("expected E_SESSION_EXITED for resize, got %v", err)
	}
	if err := s.Kill(proto.SignalTERM); err != nil {
		t.Errorf("kill after exit should be idempotent nil, got %v", err)
	}
}

func TestSession_WaitOutputWakesOnCtxCancel(t *testing.T) {
	s := spawnT(t, shCfg("sleep 300"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, exited := s.WaitOutput(ctx, s.LastSeq())
	if exited {
		t.Error("session reported exited on ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("WaitOutput ignored ctx for %v", elapsed)
	}
}

func TestSession_WaitOutputWakesOnExit(t *testing.T) {
	s := spawnT(t, shCfg("sleep 0.2; echo done"))
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var seen uint64
	for {
		last, exited := s.WaitOutput(ctx, seen)
		if exited {
			return // woke on exit — the waiter never hangs on a dead session
		}
		if ctx.Err() != nil {
			t.Fatal("WaitOutput never reported exit")
		}
		seen = last
	}
}

func TestSession_LockAutoReleasesOnExit(t *testing.T) {
	s := spawnT(t, shCfg("sleep 300"))
	s.TakeWrite("cli_a")
	if err := s.Kill(proto.SignalKILL); err != nil {
		t.Fatal(err)
	}
	waitExit(t, s)
	if got := s.WriteLockHolder(); got != "" {
		t.Errorf("write lock survived exit: %q", got)
	}
}

func TestSpawn_BadCommandFails(t *testing.T) {
	r := NewRegistry(0)
	_, err := r.Spawn(SpawnConfig{Cmd: "/nonexistent/binary/xyz", Cols: 80, Rows: 24})
	if !errcodes.IsCode(err, errcodes.SpawnFailed) {
		t.Errorf("expected E_SPAWN_FAILED, got %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("failed spawn left %d registry entries", r.Count())
	}
}

func TestSpawn_ZeroSizeRefused(t *testing.T) {
	r := NewRegistry(0)
	if _, err := r.Spawn(SpawnConfig{Cmd: "/bin/sh", Cols: 0, Rows: 24}); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("expected E_INVALID_INPUT, got %v", err)
	}
}

func TestSession_NoLeaksAfterManyCycles(t *testing.T) {
	// 30 spawn/exit cycles; TestMain's goleak verifies zero stragglers.
	r := NewRegistry(0)
	for i := range 30 {
		s, err := r.Spawn(shCfg(fmt.Sprintf("echo cycle-%d", i)))
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		waitExit(t, s)
		r.Remove(s.ID())
	}
	if r.Count() != 0 {
		t.Errorf("registry not empty: %d", r.Count())
	}
}

func TestSession_KillIsIdempotentUnderRace(t *testing.T) {
	// Regression (found by the WS smoke check): a second KILL arriving in
	// the window where the child is a zombie (killed, not yet reaped) hit
	// macOS's EPERM-on-zombie-group quirk and surfaced E_INTERNAL. Every
	// racing KILL must return nil.
	s := spawnT(t, shCfg("sleep 300"))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Kill(proto.SignalKILL); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("racing Kill: %v", err)
	}
	waitExit(t, s)
	// And killing the fully-exited session stays nil too.
	if err := s.Kill(proto.SignalKILL); err != nil {
		t.Errorf("kill after exit: %v", err)
	}
}
