// zero-downtime-upgrade replaces the daemon binary while a session is running
// and producing output — and shows not a single byte is lost across the swap.
//
// This is runbaypty's answer to "how do I upgrade the thing that holds all my
// sessions without killing them?" A new daemon started with `serve --takeover`
// connects to the running one and receives, over a private Unix socket via
// SCM_RIGHTS: the PTY master file descriptors, the ring-buffer state (so the
// sequence axis is continuous), and the flock fd (so the single-daemon lock
// never drops). The old daemon then exits. The socket path itself is re-bound,
// not handed over, so a client sees a brief reconnect — but the seq numbers
// never skip, so client.Follow resumes with zero gap and the process behind
// the stream was swapped underneath without losing a byte.
//
// To keep the demo self-contained and safe, it runs its OWN dedicated daemon on
// a private socket and home — it never touches your real daemon. It:
//
//  1. starts daemon A and spawns a session that prints an incrementing counter,
//  2. Follows that session (client.Follow = auto-reconnect + zero-gap resume),
//  3. starts daemon B with --takeover, which adopts A's session and ends A,
//  4. shows the counter the Follower read is perfectly contiguous across the
//     swap, while the daemon PID changed and the session id did not.
//
// Run:
//
//	go run ./examples/zero-downtime-upgrade
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

// The counter session prints "tick N" every 200ms. A steady, numbered stream is
// what makes zero-gap visible: if the Follower's ticks are contiguous end to
// end, nothing was dropped or duplicated across the daemon swap.
const counterScript = `i=0; while :; do echo "tick $i"; i=$((i+1)); sleep 0.2; done`

// tickPrefix is the literal each counter line starts with, parsed back into the
// integer we audit for contiguity.
const tickPrefix = "tick "

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zero-downtime-upgrade:", err)
		os.Exit(1)
	}
}

func run() error {
	bin, err := runbayptyBinary()
	if err != nil {
		return err
	}

	// A private socket (kept short — macOS caps UDS paths near 104 bytes) and a
	// private home, so this demo's daemon is fully isolated from your real one.
	sock := fmt.Sprintf("/tmp/rpty-zdu-%d.sock", os.Getpid())
	home, err := os.MkdirTemp("", "rpty-zdu-home-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(home) }()
	defer func() { _ = os.Remove(sock) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── 1. Start daemon A and wait for it to listen ───────────────────────────
	daemonA, err := startDaemon(ctx, bin, sock, home, false)
	if err != nil {
		return fmt.Errorf("start daemon A: %w", err)
	}
	pidA := daemonA.Process.Pid
	if err := waitForSocket(sock, 5*time.Second); err != nil {
		return fmt.Errorf("daemon A never listened: %w", err)
	}
	fmt.Printf("daemon A up (pid %d) on %s\n", pidA, sock)

	// Spawn the counter session on daemon A.
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	sessionID, _, err := c.Spawn(ctx, client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", counterScript}})
	if err != nil {
		return err
	}
	_ = c.Close() // the Follower opens its own connection; we're done with this one
	fmt.Printf("counter session %s spawned\n\n", sessionID)

	// ── 2. Follow the session across whatever happens to the daemon ───────────
	// Follow reconnects automatically and resumes at the exact seq it last read.
	// It does not know or care that the reconnect will land on a DIFFERENT
	// daemon process — from its side it's the same socket, same session, same
	// seq axis. That obliviousness is the proof: nothing about the client had to
	// participate in the upgrade.
	f, err := client.Follow(ctx, sock, sessionID, client.FollowOpts{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	rec := &recorder{}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if n, ok := parseTick(sc.Text()); ok {
				rec.add(n)
			}
		}
	}()

	// Let the counter run on daemon A so we have a solid "before" run of ticks.
	time.Sleep(2 * time.Second)
	lastBeforeSwap := rec.last()
	fmt.Printf("read %d ticks from daemon A (last: tick %d)\n", rec.count(), lastBeforeSwap)

	// ── 3. Upgrade: start daemon B with --takeover ────────────────────────────
	// B connects to A, receives the PTY fds + ring state + flock fd, and A exits
	// (the socket path is re-bound by B, not handed over).
	fmt.Println("\n── upgrading: starting daemon B with --takeover ──")
	daemonB, err := startDaemon(ctx, bin, sock, home, true)
	if err != nil {
		return fmt.Errorf("start daemon B: %w", err)
	}
	pidB := daemonB.Process.Pid

	// A hands off and exits on its own once B has adopted everything. Waiting for
	// A to exit is how we know the handover completed.
	if err := daemonA.Wait(); err != nil {
		// A exiting non-zero would be a real failure; a clean handover exits 0.
		fmt.Printf("(note) daemon A exited: %v\n", err)
	}
	fmt.Printf("daemon A (pid %d) handed off and exited; daemon B (pid %d) now serving\n", pidA, pidB)

	// ── 4. Keep reading — the Follower rides straight through the swap ─────────
	time.Sleep(2 * time.Second)
	ticks := rec.snapshot()
	fmt.Printf("read %d ticks total (last: tick %d)\n\n", len(ticks), rec.last())

	// Stop the session and daemon B; the demo's measurement is done.
	stopDaemonSession(sock, sessionID)
	_ = daemonB.Process.Signal(os.Interrupt)
	_, _ = daemonB.Process.Wait()

	// ── Verdict ───────────────────────────────────────────────────────────────
	gap := firstGap(ticks)
	fmt.Println("── verdict ──")
	fmt.Printf("daemon pid changed:     %d → %d   (%v)\n", pidA, pidB, pidA != pidB)
	fmt.Printf("session id unchanged:   %s   (survived the swap)\n", sessionID)
	if gap < 0 {
		fmt.Printf("tick stream contiguous: yes — %d ticks, no gap, no duplicate across the upgrade\n", len(ticks))
	} else {
		return fmt.Errorf("GAP DETECTED after tick %d — bytes were lost across the swap", gap)
	}
	fmt.Println()
	fmt.Println("The binary running the daemon was replaced mid-stream. The session")
	fmt.Println("kept running, the sequence numbers kept counting, and the Follower")
	fmt.Println("never knew a thing. That is a zero-downtime upgrade.")
	return nil
}

// recorder collects the tick integers the Follower reads, guarded for the
// reader goroutine.
type recorder struct {
	mu    sync.Mutex
	ticks []int
}

func (r *recorder) add(n int) {
	r.mu.Lock()
	r.ticks = append(r.ticks, n)
	r.mu.Unlock()
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ticks)
}

func (r *recorder) last() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ticks) == 0 {
		return -1
	}
	return r.ticks[len(r.ticks)-1]
}

func (r *recorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.ticks))
	copy(out, r.ticks)
	return out
}

// parseTick turns a "tick N" line into N. Returns ok=false for any other line
// (the shell prompt, a partial line) so the audit only counts real ticks.
func parseTick(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, tickPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(line, tickPrefix))
	if err != nil {
		return 0, false
	}
	return n, true
}

// firstGap returns the tick value AFTER which contiguity broke (a skip or a
// duplicate), or -1 if the whole sequence is perfectly consecutive. This is the
// zero-gap test: the counter increments by exactly 1 forever, so any deviation
// in what the Follower read means the daemon swap lost or repeated bytes.
func firstGap(ticks []int) int {
	for i := 1; i < len(ticks); i++ {
		if ticks[i] != ticks[i-1]+1 {
			return ticks[i-1]
		}
	}
	return -1
}

// runbayptyBinary locates the CLI: PATH first, then the repo's ./bin build so
// the example runs straight from a checkout.
func runbayptyBinary() (string, error) {
	if p, err := exec.LookPath(constants.BinaryName); err == nil {
		return p, nil
	}
	local := filepath.Join("bin", constants.BinaryName)
	if _, err := os.Stat(local); err == nil {
		return filepath.Abs(local)
	}
	return "", fmt.Errorf("runbaypty binary not found on PATH or in ./bin — run: go build -o bin/runbaypty ./cmd/runbaypty")
}

// startDaemon launches `serve` (optionally with --takeover) on a private socket
// and home. It returns the started process; the caller owns its lifetime. The
// daemon's own logs go to the home dir so they don't clutter the demo output.
func startDaemon(ctx context.Context, bin, sock, home string, takeover bool) (*exec.Cmd, error) {
	args := []string{"serve"}
	if takeover {
		args = append(args, "--takeover")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// Isolate this daemon: only OUR socket/home, never the parent's env, so the
	// demo can't accidentally point at (or take over) your real daemon.
	cmd.Env = append(daemonEnv(), constants.EnvSock+"="+sock, constants.EnvHome+"="+home)
	logPath := filepath.Join(home, fmt.Sprintf("serve-takeover=%v.log", takeover))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	return cmd, nil
}

// daemonEnv returns the parent environment with any runbaypty socket/home
// overrides stripped, so startDaemon's explicit values are the only ones set.
func daemonEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, constants.EnvSock+"=") || strings.HasPrefix(kv, constants.EnvHome+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// waitForSocket polls until the daemon's socket accepts a connection or the
// deadline passes.
func waitForSocket(sock string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if c, err := client.Dial(sock); err == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s not ready within %s", sock, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stopDaemonSession best-effort kills the demo's counter so it doesn't linger
// past the run. Errors are ignored — the daemon is about to be stopped anyway.
func stopDaemonSession(sock, sessionID string) {
	c, err := client.Dial(sock)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Kill(ctx, sessionID, "")
}
