package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTail_LogHistoryThenLiveStitch(t *testing.T) {
	sock := startDaemonT(t)
	logPath := filepath.Join(t.TempDir(), "s.log")

	// Phase 1 output lands in the log while nobody watches.
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--log", logPath, "--", "/bin/sh", "-c",
		"echo history-line-one; echo history-line-two; sleep 0.3; echo live-line-three; exit 0"))

	// Wait for the session to finish so history + live are both present.
	deadline := time.Now().Add(testTimeout)
	for !strings.Contains(mustCLI(t, sock, "info", id, "--json"), `"exited"`) {
		if time.Now().After(deadline) {
			t.Fatal("session never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}

	out, err := runCLI(t, sock, "", "tail", id)
	if err != nil {
		t.Fatalf("tail: %v\n%s", err, out)
	}
	// All three lines exactly once — the log/ring stitch must not duplicate
	// the seam bytes or drop any.
	for _, want := range []string{"history-line-one", "history-line-two", "live-line-three"} {
		if got := strings.Count(out, want); got != 1 {
			t.Errorf("%q appears %d times, want exactly 1 (seam dup/gap)\n%s", want, got, out)
		}
	}
	if !strings.Contains(out, "exited: code 0") {
		t.Errorf("tail did not report exit:\n%s", out)
	}
}

func TestTail_NoLogFallsBackToRing(t *testing.T) {
	sock := startDaemonT(t)
	id := strings.TrimSpace(mustCLI(t, sock, "run", "--", "/bin/sh", "-c", "echo ring-only-history; exit 0"))
	deadline := time.Now().Add(testTimeout)
	for !strings.Contains(mustCLI(t, sock, "info", id, "--json"), `"exited"`) {
		if time.Now().After(deadline) {
			t.Fatal("session never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := runCLI(t, sock, "", "tail", id)
	if err != nil {
		t.Fatalf("tail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ring-only-history") {
		t.Errorf("ring fallback missing history:\n%s", out)
	}
}
