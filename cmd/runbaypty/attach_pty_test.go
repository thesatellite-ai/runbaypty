package main

// attach_pty_test.go — task-m4-attach-t1: the interactive raw-mode attach,
// driven through a REAL controlling terminal. The CLI binary runs as a
// child inside a creack/pty PTY; we type into the master side, watch the
// echo, hit the detach key, and verify the session survived.

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

var (
	builtBinOnce sync.Once
	builtBinPath string
	builtBinErr  error
)

// buildCLI compiles the binary once per test run.
func buildCLI(t *testing.T) string {
	t.Helper()
	builtBinOnce.Do(func() {
		dir, err := filepath.Abs("../../bin")
		if err != nil {
			builtBinErr = err
			return
		}
		builtBinPath = filepath.Join(dir, "runbaypty.test-cli")
		out, err := exec.Command("go", "build", "-o", builtBinPath, ".").CombinedOutput()
		if err != nil {
			builtBinErr = err
			builtBinPath = string(out)
		}
	})
	if builtBinErr != nil {
		t.Fatalf("build CLI: %v — %s", builtBinErr, builtBinPath)
	}
	return builtBinPath
}

func TestAttach_InteractiveRawModeDetach(t *testing.T) {
	bin := buildCLI(t)
	sock := startDaemonT(t)

	// A cat session: everything typed echoes back via the line discipline.
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--name", "tty-target", "--", "/bin/cat"))

	// Run `attach` as a child WITH a real controlling terminal.
	cmd := exec.Command(bin, "--sock", sock, "attach", "tty-target")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close() }()

	// Reader goroutine accumulates everything the attach prints.
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	waitOut := func(want string) {
		t.Helper()
		deadline := time.Now().Add(testTimeout)
		for {
			mu.Lock()
			ok := strings.Contains(out.String(), want)
			mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				mu.Lock()
				t.Fatalf("timeout waiting for %q; attach output:\n%s", want, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Type through the attach → session cat echoes → back through attach.
	if _, err := ptmx.Write([]byte("typed-through-tty\r")); err != nil {
		t.Fatal(err)
	}
	waitOut("typed-through-tty")

	// The interactive path took the write lock and resized to our PTY size.
	var info proto.SessionInfo
	deadline := time.Now().Add(testTimeout)
	for {
		infoJSON := mustCLI(t, sock, "info", id, "--json")
		if err := jsonUnmarshal(infoJSON, &info); err != nil {
			t.Fatal(err)
		}
		if info.WriteLockHolder != "" && info.Cols == 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock/resize not applied: holder=%q size=%dx%d", info.WriteLockHolder, info.Cols, info.Rows)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Detach key (ctrl-\): the CLI exits 0, prints the detach notice, and
	// the SESSION KEEPS RUNNING — the entire point of the product.
	if _, err := ptmx.Write([]byte{0x1c}); err != nil {
		t.Fatal(err)
	}
	waitOut("keeps running")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("attach exit after detach: %v", err)
	}
	if err := jsonUnmarshal(mustCLI(t, sock, "info", id, "--json"), &info); err != nil {
		t.Fatal(err)
	}
	if info.State != proto.StateRunning {
		t.Fatalf("session state after detach = %s, want running", info.State)
	}
	// The lock releases when the daemon processes the disconnect — async
	// relative to the CLI process exiting, so poll rather than assert once.
	// FRESH struct each poll: write_lock_holder is omitempty, so once the
	// lock releases the field is ABSENT from the JSON and unmarshaling into
	// a reused struct would keep showing the stale holder forever.
	deadline = time.Now().Add(testTimeout)
	for {
		var fresh proto.SessionInfo
		if err := jsonUnmarshal(mustCLI(t, sock, "info", id, "--json"), &fresh); err != nil {
			t.Fatal(err)
		}
		if fresh.WriteLockHolder == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write lock survived detach: %q", fresh.WriteLockHolder)
		}
		time.Sleep(20 * time.Millisecond)
	}

	mustCLI(t, sock, "kill", "--signal", "KILL", id)
}

func jsonUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
