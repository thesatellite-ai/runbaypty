package constants

// constants_test.go — path resolution honors the env overrides that make
// isolated daemons (tests, parallel workspaces) possible, and the closed-set
// constants stay internally consistent.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHome_EnvOverrideWins(t *testing.T) {
	t.Setenv(EnvHome, "/tmp/custom-home")
	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/custom-home" {
		t.Errorf("Home() = %q, want the env override", got)
	}
}

func TestHome_DefaultsUnderUserHome(t *testing.T) {
	t.Setenv(EnvHome, "")
	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home on this machine")
	}
	if want := filepath.Join(userHome, ".runbaypty"); got != want {
		t.Errorf("Home() = %q, want %q", got, want)
	}
}

func TestSocketPath_EnvOverrideBypassesHome(t *testing.T) {
	// RUNBAYPTY_SOCK wins outright — it must not be joined onto the home.
	t.Setenv(EnvHome, "/tmp/ignored-home")
	t.Setenv(EnvSock, "/tmp/explicit.sock")
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/explicit.sock" {
		t.Errorf("SocketPath() = %q, want the env override verbatim", got)
	}
}

func TestSocketPath_DerivesFromHome(t *testing.T) {
	t.Setenv(EnvSock, "")
	t.Setenv(EnvHome, "/tmp/derived-home")
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/tmp/derived-home", SocketFilename); got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
}

func TestConstants_ShapeInvariants(t *testing.T) {
	t.Parallel()
	// The daemon must never bind a non-loopback host (MISSION invariant).
	if LoopbackHost != "127.0.0.1" {
		t.Errorf("LoopbackHost = %q — the WS listener may only bind loopback", LoopbackHost)
	}
	if !strings.HasPrefix(WSPathV1, "/") {
		t.Errorf("WSPathV1 = %q, want a rooted path", WSPathV1)
	}
	// Ring caps must be orderable: default ≤ per-session max ≤ global total.
	if DefaultRingBytes > MaxRingBytes {
		t.Errorf("DefaultRingBytes %d > MaxRingBytes %d", DefaultRingBytes, MaxRingBytes)
	}
	if int64(MaxRingBytes) > DefaultRingTotalBytes {
		t.Errorf("one session's max ring %d exceeds the global cap %d", MaxRingBytes, DefaultRingTotalBytes)
	}
	// Housekeeping must tick well inside the retention window, or exited
	// sessions linger far past their TTL.
	if TickInterval >= DefaultRetentionTTL {
		t.Errorf("TickInterval %v must be << DefaultRetentionTTL %v", TickInterval, DefaultRetentionTTL)
	}
	if DefaultBatchFlushMs <= 0 || DefaultBatchMaxBytes <= 0 || DefaultMaxSessions <= 0 {
		t.Error("defaults must be positive")
	}
	if TickInterval <= 0 || DefaultRetentionTTL <= time.Second {
		t.Error("durations must be sane")
	}
	// Every filename is a bare name, never a path (they are joined onto Home).
	for _, name := range []string{SocketFilename, DiscoveryFilename, LockFilename, TokenFilename, LogDirname, DaemonStdoutLog, DaemonStderrLog, StableBinDirname} {
		if strings.ContainsRune(name, os.PathSeparator) {
			t.Errorf("filename constant %q contains a path separator", name)
		}
	}
}
