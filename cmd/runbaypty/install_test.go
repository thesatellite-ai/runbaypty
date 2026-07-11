package main

// install_test.go — the `daemon install` machinery below the supervisor
// call: path resolution, atomic binary copy, and the supervisor shell-out's
// error surfacing. The launchctl/systemctl invocations themselves are NOT
// exercised (they would register a real login service); the generated unit
// files are golden-tested in daemon_cmd_test.go.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

func TestResolveInstallPaths_HonorsHomeOverride(t *testing.T) {
	t.Setenv(constants.EnvHome, "/tmp/rpty-install-home")
	p, err := resolveInstallPaths()
	if err != nil {
		if errcodes.IsCode(err, errcodes.Unsupported) {
			t.Skipf("install unsupported on %s", runtime.GOOS)
		}
		t.Fatal(err)
	}
	if p.homeDir != "/tmp/rpty-install-home" {
		t.Errorf("homeDir = %q", p.homeDir)
	}
	// The stable binary lives INSIDE the home dir — one env var relocates
	// everything, and a supervisor never points at a moving PATH binary.
	wantBin := filepath.Join("/tmp/rpty-install-home", constants.StableBinDirname, constants.BinaryName)
	if p.stableBin != wantBin {
		t.Errorf("stableBin = %q, want %q", p.stableBin, wantBin)
	}
	if stableBinDir(p.homeDir) != filepath.Dir(wantBin) {
		t.Error("stableBinDir disagrees with resolveInstallPaths")
	}
	switch runtime.GOOS {
	case goosDarwin:
		if !strings.HasSuffix(p.unitPath, launchdLabel+".plist") || !strings.Contains(p.unitPath, "LaunchAgents") {
			t.Errorf("darwin unitPath = %q", p.unitPath)
		}
	case goosLinux:
		if !strings.HasSuffix(p.unitPath, systemdUnitName) || !strings.Contains(p.unitPath, "systemd") {
			t.Errorf("linux unitPath = %q", p.unitPath)
		}
	}
}

func TestCopyBinary_AtomicAndExecutable(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nested", "dir", constants.BinaryName)
	if err := copyBinary(dst); err != nil {
		t.Fatalf("copyBinary: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("copied binary not executable: %v", fi.Mode())
	}
	// The running test binary is what got copied — non-trivially sized.
	if fi.Size() < 1024 {
		t.Errorf("copied binary is %d bytes — copy truncated?", fi.Size())
	}
	// No tmp file left behind (rename, not write-in-place).
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file survived the atomic rename")
	}
	// Idempotent: a second copy over an existing binary succeeds.
	if err := copyBinary(dst); err != nil {
		t.Errorf("re-copy: %v", err)
	}
	// Unwritable destination surfaces an error rather than silently skipping.
	if err := copyBinary("/proc/nonexistent-root/x"); err == nil {
		t.Error("copyBinary to an unwritable path should error")
	}
}

func TestRunSupervisor_SurfacesCommandOutput(t *testing.T) {
	// Success path with a harmless command.
	if err := runSupervisor("true"); err != nil {
		t.Errorf("runSupervisor(true) = %v", err)
	}
	// Failure path: the command's own words are the useful part, so they
	// must reach the error message.
	err := runSupervisor("sh", "-c", "echo supervisor-said-this >&2; exit 3")
	if err == nil {
		t.Fatal("expected an error from a failing supervisor command")
	}
	if !strings.Contains(err.Error(), "supervisor-said-this") {
		t.Errorf("supervisor output not surfaced: %v", err)
	}
	if !errcodes.IsCode(err, errcodes.Internal) {
		t.Errorf("supervisor error code = %v", err)
	}
	// A missing binary is an error, not a panic.
	if err := runSupervisor("definitely-not-a-real-binary-xyz"); err == nil {
		t.Error("missing supervisor binary should error")
	}
}

func TestRenderErrorAndExitCode(t *testing.T) {
	// Typed errors render through the errcodes envelope (code + hint).
	typed := errcodes.New(errcodes.SessionNotFound, "no such session").WithHint("run ls")
	rendered := renderError(typed)
	for _, want := range []string{string(errcodes.SessionNotFound), "no such session", "run ls"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("renderError missing %q:\n%s", want, rendered)
		}
	}
	// Plain errors fall back to their message.
	if got := renderError(os.ErrNotExist); got != os.ErrNotExist.Error() {
		t.Errorf("renderError(plain) = %q", got)
	}
	// Exit-code contract: daemon-unreachable is distinguishable (3).
	if got := exitCodeFor(errcodes.New(errcodes.DaemonUnreachable, "down")); got != exitDaemonUnreachable {
		t.Errorf("exitCodeFor(unreachable) = %d, want %d", got, exitDaemonUnreachable)
	}
	if got := exitCodeFor(typed); got != exitError {
		t.Errorf("exitCodeFor(other) = %d, want %d", got, exitError)
	}
}
