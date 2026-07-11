package main

// interactive_test.go — the raw-mode attach path driven IN-PROCESS through a
// real PTY pair, plus run()'s exit-code contract and `daemon install` against
// a fake supervisor on PATH. These close the last coverage gaps that a
// subprocess test cannot (subprocess coverage is invisible to the profiler).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// ptyPair returns a connected (master, slave) pair. The slave is a REAL
// terminal, so `term.IsTerminal(slave.Fd())` is true and attachInteractive
// takes its raw-mode path.
func ptyPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = s.Close() })
	return m, s
}

// drain accumulates everything written to w until the test ends.
type drain struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (d *drain) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *drain) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

func (d *drain) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !strings.Contains(d.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q; got:\n%s", want, d.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAttachInteractive_RawModeEchoResizeDetach(t *testing.T) {
	sock := startDaemonT(t)
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	id, _, err := c.Spawn(context.Background(), client.SpawnOpts{Cmd: "/bin/cat", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Attach(context.Background(), id, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TakeWrite(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	master, slave := ptyPair(t)
	// Size the local "terminal" so the interactive path's initial resize has
	// something distinctive to propagate.
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 111, Rows: 41}); err != nil {
		t.Fatal(err)
	}

	out := &drain{}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- attachInteractive(ctx, c, st, false, slave, out) }()

	// The interactive path resizes the session to OUR terminal on entry.
	deadline := time.Now().Add(testTimeout)
	for {
		info, ierr := c.Info(context.Background(), id)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if info.Cols == 111 && info.Rows == 41 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initial resize never applied: %dx%d", info.Cols, info.Rows)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Keystrokes flow master → stdin pump → INPUT → cat echo → OUTPUT → out.
	if _, err := master.Write([]byte("raw-mode-echo\r")); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "raw-mode-echo")

	// The detach key (ctrl-\, 0x1c) ends the attach WITHOUT killing the
	// session — the whole point of the feature. Bytes before the key in the
	// same chunk are still forwarded.
	if _, err := master.Write([]byte{'x', detachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "detached") {
			t.Fatalf("attachInteractive returned %v, want errDetached", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("detach key did not end the interactive attach")
	}

	// Session survived the detach.
	info, err := c.Info(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != proto.StateRunning {
		t.Fatalf("session state after detach = %s", info.State)
	}
	_ = c.Kill(context.Background(), id, proto.SignalKILL)
}

func TestAttachInteractive_ReadOnlyNeitherTypesNorResizes(t *testing.T) {
	sock := startDaemonT(t)
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	id, _, err := c.Spawn(context.Background(), client.SpawnOpts{Cmd: "/bin/cat", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Attach(context.Background(), id, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	master, slave := ptyPair(t)
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 123, Rows: 45}); err != nil {
		t.Fatal(err)
	}
	out := &drain{}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- attachInteractive(ctx, c, st, true /* read-only */, slave, out) }()

	if _, err := master.Write([]byte("must-not-reach-session\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	info, err := c.Info(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.BytesIn != 0 {
		t.Errorf("read-only interactive attach leaked %d input bytes", info.BytesIn)
	}
	if info.Cols == 123 {
		t.Error("read-only interactive attach resized the session")
	}
	// Detach still works from a read-only attach.
	if _, err := master.Write([]byte{detachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errCh:
	case <-time.After(testTimeout):
		t.Fatal("read-only attach did not detach")
	}
	_ = c.Kill(context.Background(), id, proto.SignalKILL)
}

func TestAttachInteractive_EndsWhenSessionExits(t *testing.T) {
	sock := startDaemonT(t)
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, _, err := c.Spawn(context.Background(), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo bye; exit 5"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Attach(context.Background(), id, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_, slave := ptyPair(t)
	out := &drain{}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// The output pump ends when the session exits and the stream drains:
	// attachInteractive returns nil (not errDetached).
	if err := attachInteractive(ctx, c, st, true, slave, out); err != nil {
		t.Fatalf("attachInteractive on an exiting session = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "bye") {
		t.Errorf("missed final output: %q", out.String())
	}
	if code, _, exited := st.Exit(); !exited || code != 5 {
		t.Errorf("exit = (%d, %v)", code, exited)
	}
}

func TestRun_ExitCodeContract(t *testing.T) {
	var stdout, stderr bytes.Buffer

	// 0: a self-contained verb succeeds.
	if got := run([]string{"version"}, &stdout, &stderr); got != exitOK {
		t.Errorf("run(version) = %d, want %d", got, exitOK)
	}
	if !strings.Contains(stdout.String(), "protocol v") {
		t.Errorf("version output: %q", stdout.String())
	}

	// 3: daemon unreachable is distinguishable from a generic failure.
	stdout.Reset()
	stderr.Reset()
	if got := run([]string{"--sock", "/tmp/definitely-no-daemon.sock", "ls"}, &stdout, &stderr); got != exitDaemonUnreachable {
		t.Errorf("run(ls, dead socket) = %d, want %d", got, exitDaemonUnreachable)
	}
	if !strings.Contains(stderr.String(), "ERROR:") {
		t.Errorf("stderr lacks the error envelope: %q", stderr.String())
	}

	// 1: any other failure.
	stdout.Reset()
	stderr.Reset()
	sock := startDaemonT(t)
	if got := run([]string{"--sock", sock, "info", "ghost"}, &stdout, &stderr); got != exitError {
		t.Errorf("run(info ghost) = %d, want %d", got, exitError)
	}
	// The --color flag is honored (never emits ANSI when told never).
	stdout.Reset()
	stderr.Reset()
	if got := run([]string{"--color", "never", "version"}, &stdout, &stderr); got != exitOK {
		t.Errorf("run(--color never) = %d", got)
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Error("--color never still emitted ANSI")
	}
}

// fakeSupervisorPath builds a temp dir containing stub launchctl/systemctl
// executables that log their argv, and prepends it to PATH.
func fakeSupervisorPath(t *testing.T) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$(basename $0) $@\" >> " + logFile + "\nexit 0\n"
	for _, name := range []string{supervisorLaunchctl, supervisorSystemctl} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func TestDaemonInstall_WritesUnitAndCallsSupervisor(t *testing.T) {
	if runtime.GOOS != goosDarwin && runtime.GOOS != goosLinux {
		t.Skipf("install unsupported on %s", runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv(constants.EnvHome, home)
	// Redirect the unit file into a temp HOME so we never touch the real
	// LaunchAgents / systemd user dir.
	t.Setenv("HOME", t.TempDir())
	calls := fakeSupervisorPath(t)

	if _, err := runCLI(t, "/tmp/unused.sock", "", "daemon", "install"); err != nil {
		t.Fatalf("daemon install: %v", err)
	}

	p, err := resolveInstallPaths()
	if err != nil {
		t.Fatal(err)
	}
	// The binary was copied to the stable path…
	if fi, err := os.Stat(p.stableBin); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("stable binary: %v", err)
	}
	// …the unit file exists and names that binary…
	unit, err := os.ReadFile(p.unitPath)
	if err != nil {
		t.Fatalf("unit file: %v", err)
	}
	if !strings.Contains(string(unit), p.stableBin) {
		t.Error("unit file does not point at the stable binary")
	}
	// …and the right supervisor was invoked.
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("supervisor never called: %v", err)
	}
	got := string(logged)
	if runtime.GOOS == goosDarwin {
		if !strings.Contains(got, launchctlBootstrap) || !strings.Contains(got, launchctlBootout) {
			t.Errorf("launchctl calls = %q (want bootout-then-bootstrap: install must be idempotent)", got)
		}
	} else if !strings.Contains(got, "daemon-reload") || !strings.Contains(got, "enable") {
		t.Errorf("systemctl calls = %q", got)
	}

	// start / stop / uninstall all shell out cleanly too.
	for _, sub := range []string{"start", "stop", "uninstall"} {
		if _, err := runCLI(t, "/tmp/unused.sock", "", "daemon", sub); err != nil {
			t.Errorf("daemon %s: %v", sub, err)
		}
	}
	if _, err := os.Stat(p.unitPath); !os.IsNotExist(err) {
		t.Error("uninstall left the unit file behind")
	}
}

func TestDaemonStatus_AgainstLiveDaemon(t *testing.T) {
	home := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "rptystat-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Setenv(constants.EnvHome, home)

	// A real daemon publishes discovery; `daemon status` must read it.
	srv, err := newTestDaemon(t, home, sockDir+"/d.sock")
	if err != nil {
		t.Fatal(err)
	}
	_ = srv

	out := mustCLI(t, "/tmp/unused.sock", "daemon", "status")
	for _, want := range []string{"running:", "pid", "socket:"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	// --json carries the alive flag + the whole discovery record.
	raw := mustCLI(t, "/tmp/unused.sock", "daemon", "status", "--json")
	if !strings.Contains(raw, `"alive": true`) {
		t.Errorf("status --json = %s", raw)
	}
}

func TestServeCommand_RunsAndShutsDown(t *testing.T) {
	// Exercise serve's RunE end to end (flag plumbing → daemon.New →
	// Serve → graceful stop) without a supervisor.
	home := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "rptyserve-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Setenv(constants.EnvHome, home)
	t.Setenv(constants.EnvSock, filepath.Join(sockDir, "s.sock"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		root := newRootCommand()
		// ring-total must exceed 4 × the default per-session ring (2 MiB),
		// or the ring cap — not --max-sessions — is what refuses the spawns.
		root.SetArgs([]string{"serve", "--log-json", "--max-sessions", "4", "--retention-ttl", "1m", "--ring-total", "16777216"})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		if err := root.ExecuteContext(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
		done <- 0
	}()

	// The daemon is up once the socket accepts a connection.
	deadline := time.Now().Add(testTimeout)
	var c *client.Client
	for {
		c, err = client.Dial(filepath.Join(sockDir, "s.sock"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never listened: %v", err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	// The flags took effect: max-sessions 4 is enforced.
	for range 4 {
		if _, _, err := c.Spawn(context.Background(), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.Spawn(context.Background(), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}}); err == nil {
		t.Error("--max-sessions 4 not enforced")
	}
	_ = c.Close()

	cancel()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("serve did not shut down on context cancel")
	}
	// Graceful shutdown removed its runtime files.
	if _, err := os.Stat(filepath.Join(sockDir, "s.sock")); !os.IsNotExist(err) {
		t.Error("serve left its socket behind")
	}
}

func TestServeCommand_TakeoverWithoutDaemonColdBoots(t *testing.T) {
	// `serve --takeover` with nobody to take over from must cold-boot, not
	// fail: that is what makes it safe as the default upgrade command.
	home := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "rptytk-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Setenv(constants.EnvHome, home)
	t.Setenv(constants.EnvSock, filepath.Join(sockDir, "s.sock"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		root := newRootCommand()
		root.SetArgs([]string{"serve", "--takeover"})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		if err := root.ExecuteContext(ctx); err != nil {
			t.Errorf("serve --takeover cold boot: %v", err)
		}
	}()

	deadline := time.Now().Add(testTimeout)
	for {
		c, derr := client.Dial(filepath.Join(sockDir, "s.sock"))
		if derr == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve --takeover never listened: %v", derr)
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("serve --takeover did not shut down")
	}
}
