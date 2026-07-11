package daemon

// tmpdir_test.go — short-path temp dirs for UDS sockets. sun_path is capped
// at ~104 bytes on macOS / 108 on Linux, and Go's t.TempDir() lands under
// /var/folders/… which plus a socket name can breach it. Sockets get a dir
// under /tmp instead; everything else stays in t.TempDir().

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

// contextWithCancel is context.WithCancel with a test-friendly name kept
// separate so call sites read clearly next to t.Context().
func contextWithCancel(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

// processAlive probes a pid with signal 0.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func mkdirTempShort() (string, error) {
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = os.TempDir() // non-Unix-y environment fallback
	}
	dir, err := os.MkdirTemp(base, "rpty-*")
	if err != nil {
		return "", fmt.Errorf("mkdir short temp: %w", err)
	}
	return dir, nil
}

func removeAllQuiet(dir string) error {
	return os.RemoveAll(dir)
}
