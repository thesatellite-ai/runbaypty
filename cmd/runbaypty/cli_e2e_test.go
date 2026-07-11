package main

// cli_e2e_test.go — every client verb driven in-process against a real
// daemon: fresh cobra tree per invocation, --sock pointing at a per-test
// socket, golden-ish assertions on output. The interactive raw-mode attach
// path needs a real controlling TTY and is exercised by the piped path here
// (same pumps, no termios).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/daemon"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

const testTimeout = 15 * time.Second

// startDaemonT runs a daemon for one test and returns its socket path.
func startDaemonT(t *testing.T) string {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "rptye2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := daemon.New(daemon.Options{HomeDir: t.TempDir(), SocketPath: sockDir + "/d.sock", Version: "e2e", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
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

// newTestDaemon starts a daemon on an explicit home+socket (for tests that
// need the discovery file at a known place, e.g. `daemon status`).
func newTestDaemon(t *testing.T, home, sock string) (*daemon.Server, error) {
	t.Helper()
	srv, err := daemon.New(daemon.Options{HomeDir: home, SocketPath: sock, Version: "status-test"})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("daemon shutdown hung")
		}
	})
	select {
	case <-srv.Ready():
	case <-time.After(testTimeout):
		return nil, context.DeadlineExceeded
	}
	return srv, nil
}

// runCLI executes one command in-process and returns stdout.
func runCLI(t *testing.T, sock string, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	if stdin != "" {
		root.SetIn(strings.NewReader(stdin))
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	root.SetArgs(append([]string{"--sock", sock}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// mustCLI is runCLI failing the test on error.
func mustCLI(t *testing.T, sock string, args ...string) string {
	t.Helper()
	out, err := runCLI(t, sock, "", args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func TestCLI_RunLsInfoLifecycle(t *testing.T) {
	sock := startDaemonT(t)

	id := strings.TrimSpace(mustCLI(t, sock, "run", "--name", "e2e-sleep", "--meta", "app=test", "--", "/bin/sh", "-c", "sleep 300"))
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("run printed %q, want a ses_ id", id)
	}

	ls := mustCLI(t, sock, "ls")
	if !strings.Contains(ls, "e2e-sleep") || !strings.Contains(ls, id) {
		t.Errorf("ls missing session:\n%s", ls)
	}

	// --json is machine-parseable and carries the meta.
	var sessions []proto.SessionInfo
	if err := json.Unmarshal([]byte(mustCLI(t, sock, "ls", "--json")), &sessions); err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Meta["app"] != "test" {
		t.Errorf("ls --json = %+v", sessions)
	}

	info := mustCLI(t, sock, "info", "e2e-sleep")
	for _, want := range []string{"id:", id, "state:", "running", "meta.app: test"} {
		if !strings.Contains(info, want) {
			t.Errorf("info missing %q:\n%s", want, info)
		}
	}

	mustCLI(t, sock, "rename", "e2e-sleep", "renamed-target")
	mustCLI(t, sock, "meta", "set", "renamed-target", "phase=two", "owner=cli")
	var meta map[string]string
	if err := json.Unmarshal([]byte(mustCLI(t, sock, "meta", "get", "renamed-target")), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["phase"] != "two" || meta["app"] != "" {
		t.Errorf("meta not replaced wholesale: %v", meta)
	}

	mustCLI(t, sock, "resize", "renamed-target", "132", "43")
	var one proto.SessionInfo
	if err := json.Unmarshal([]byte(mustCLI(t, sock, "info", "renamed-target", "--json")), &one); err != nil {
		t.Fatal(err)
	}
	if one.Cols != 132 || one.Rows != 43 {
		t.Errorf("resize not applied: %dx%d", one.Cols, one.Rows)
	}

	mustCLI(t, sock, "kill", "--signal", "KILL", "renamed-target")
	deadline := time.Now().Add(testTimeout)
	for {
		var after proto.SessionInfo
		if err := json.Unmarshal([]byte(mustCLI(t, sock, "info", id, "--json")), &after); err != nil {
			t.Fatal(err)
		}
		if after.State == proto.StateExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session never exited after kill")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCLI_AttachPipedStreamsAndForwardsStdin(t *testing.T) {
	sock := startDaemonT(t)
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/cat"))

	// Piped attach: stdin flows to cat, the PTY echoes it back to stdout,
	// then EOF-in-stdin leaves the session running; kill ends the stream.
	done := make(chan struct{})
	var attachOut string
	var attachErr error
	go func() {
		defer close(done)
		attachOut, attachErr = runCLI(t, sock, "piped-line\r", "attach", id)
	}()

	// Wait until the echo proves the input round-tripped, then kill.
	deadline := time.Now().Add(testTimeout)
	for {
		var info proto.SessionInfo
		if err := json.Unmarshal([]byte(mustCLI(t, sock, "info", id, "--json")), &info); err != nil {
			t.Fatal(err)
		}
		if info.BytesOut > 0 && info.BytesIn > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attach never moved bytes")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mustCLI(t, sock, "kill", "--signal", "KILL", id)
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("attach did not end after kill")
	}
	if attachErr != nil {
		t.Fatalf("attach: %v\n%s", attachErr, attachOut)
	}
	if !strings.Contains(attachOut, "piped-line") {
		t.Errorf("attach output missing echo:\n%q", attachOut)
	}
	if !strings.Contains(attachOut, "signal KILL") {
		t.Errorf("attach did not report exit outcome:\n%q", attachOut)
	}
}

func TestCLI_AttachReadOnlySeesButCannotType(t *testing.T) {
	sock := startDaemonT(t)
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/sh", "-c", "echo visible-to-viewers; sleep 300"))

	done := make(chan struct{})
	var out string
	go func() {
		defer close(done)
		out, _ = runCLI(t, sock, "ignored-input", "attach", "--read-only", id)
	}()
	deadline := time.Now().Add(testTimeout)
	for {
		var info proto.SessionInfo
		if err := json.Unmarshal([]byte(mustCLI(t, sock, "info", id, "--json")), &info); err != nil {
			t.Fatal(err)
		}
		if info.Subscribers > 0 && info.BytesOut > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("viewer never attached")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Read-only: stdin must NOT reach the session.
	var info proto.SessionInfo
	if err := json.Unmarshal([]byte(mustCLI(t, sock, "info", id, "--json")), &info); err != nil {
		t.Fatal(err)
	}
	if info.BytesIn != 0 {
		t.Errorf("read-only attach leaked %d input bytes", info.BytesIn)
	}
	mustCLI(t, sock, "kill", "--signal", "KILL", id)
	<-done
	if !strings.Contains(out, "visible-to-viewers") {
		t.Errorf("viewer missed output:\n%q", out)
	}
}

func TestCLI_ExitCodes(t *testing.T) {
	sock := startDaemonT(t)

	// Unknown session → exit code 1 shape (error return).
	if _, err := runCLI(t, sock, "", "info", "ghost"); err == nil {
		t.Error("info ghost should error")
	}
	// Dead socket → DaemonUnreachable → the CLI maps to exit 3 in main();
	// here we assert the typed error survives to the top.
	_, err := runCLI(t, "/tmp/nope-no-daemon.sock", "", "ls")
	if err == nil {
		t.Fatal("ls against dead socket should error")
	}
	if exitCodeFor(err) != exitDaemonUnreachable {
		t.Errorf("exit code = %d, want %d (err %v)", exitCodeFor(err), exitDaemonUnreachable, err)
	}
}

func TestCLI_VersionAndErrorsStillWork(t *testing.T) {
	out := mustCLI(t, "/tmp/unused.sock", "version")
	if !strings.Contains(out, "protocol v1") {
		t.Errorf("version output: %q", out)
	}
	out = mustCLI(t, "/tmp/unused.sock", "errors", "list")
	if !strings.Contains(out, "E_SESSION_NOT_FOUND") {
		t.Errorf("errors list output: %q", out)
	}
}

func TestCLI_HelpOnEveryVerb(t *testing.T) {
	for _, verb := range []string{"run", "ls", "info", "kill", "resize", "rename", "meta", "attach", "events", "serve", "version", "errors"} {
		out, err := runCLI(t, "/tmp/unused.sock", "", verb, "--help")
		if err != nil {
			t.Errorf("%s --help: %v", verb, err)
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("%s --help lacks Usage:\n%s", verb, out)
		}
	}
}
