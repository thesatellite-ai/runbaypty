package main

import (
	"strings"
	"testing"
)

// The generators are pure functions — golden-shape assertions here; the
// launchctl/systemctl paths are exercised manually (task-m6 integration).

func TestGenerateLaunchdPlist(t *testing.T) {
	plist := generateLaunchdPlist("/Users/x/.runbaypty/bin/runbaypty", "/Users/x/.runbaypty")
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.runbay.runbaypty</string>",
		"<string>/Users/x/.runbaypty/bin/runbaypty</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>RUNBAYPTY_HOME</key>",
		"daemon.err.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if strings.Contains(plist, "--foreground") {
		t.Error("plist must not pass --foreground; serve never forks anyway")
	}
}

func TestGenerateSystemdUnit(t *testing.T) {
	unit := generateSystemdUnit("/home/x/.runbaypty/bin/runbaypty", "/home/x/.runbaypty")
	for _, want := range []string{
		"ExecStart=/home/x/.runbaypty/bin/runbaypty serve",
		"Environment=RUNBAYPTY_HOME=/home/x/.runbaypty",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

func TestDaemonStatus_NoDaemon(t *testing.T) {
	t.Setenv("RUNBAYPTY_HOME", t.TempDir())
	_, err := runCLI(t, "/tmp/unused.sock", "", "daemon", "status")
	if err == nil {
		t.Fatal("status with no daemon should error")
	}
	if exitCodeFor(err) != exitDaemonUnreachable {
		t.Errorf("exit code = %d, want %d", exitCodeFor(err), exitDaemonUnreachable)
	}
}
