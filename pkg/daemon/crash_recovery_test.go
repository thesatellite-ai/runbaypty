package daemon

// crash_recovery_test.go — task-m6-crashloop: a daemon that died by kill -9
// leaves a socket file, a discovery file with a dead pid, and a lock file.
// The next daemon must recover all three without human help — that is what
// KeepAlive/Restart supervision depends on.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

func TestCrashRecovery_StaleFilesFromDeadDaemon(t *testing.T) {
	home := t.TempDir()
	sockDir, err := mkdirTempShort()
	if err != nil {
		t.Fatal(err)
	}
	defer removeAllQuiet(sockDir)
	sock := sockDir + "/d.sock"

	// Fabricate the corpse: a bound-then-abandoned socket file, a discovery
	// file pointing at a pid that no longer exists, and a leftover lock file
	// with no live flock holder (kill -9 releases flocks automatically).
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // Close removes the socket file — recreate it as a dead husk.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		// A plain file at the socket path is the worst-case stale state.
		t.Fatal(err)
	}
	deadPid := 99999
	for processAlive(deadPid) {
		deadPid--
	}
	stale, _ := json.Marshal(Discovery{SocketPath: sock, Pid: deadPid, Version: "corpse"})
	if err := os.WriteFile(filepath.Join(home, constants.DiscoveryFilename), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, constants.LockFilename), []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The next daemon must boot cleanly over the corpse.
	srv, err := New(Options{HomeDir: home, SocketPath: sock, Version: "phoenix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	select {
	case <-srv.Ready():
	case err := <-done:
		t.Fatalf("daemon failed to recover from stale state: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("daemon hung during recovery")
	}

	// Fresh discovery replaced the corpse's.
	raw, err := os.ReadFile(filepath.Join(home, constants.DiscoveryFilename))
	if err != nil {
		t.Fatal(err)
	}
	var d Discovery
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Pid != os.Getpid() || d.Version != "phoenix" {
		t.Errorf("discovery not rewritten: %+v", d)
	}

	// And it actually serves.
	c := dialT(t, sock)
	ok := spawnShell(c, "echo recovered")
	c.attach(ok.SessionID, nil, false)
	c.collectOutput(ok.SessionID, "recovered")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
