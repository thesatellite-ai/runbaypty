// Package processlock provides advisory file-locking so exactly one runbaypty
// daemon owns a given home directory at a time.
//
// The daemon holds a single lock file (e.g. ~/.runbaypty/daemon.lock) beside
// its socket. The lock is:
//
//   - Advisory (flock-style) on Unix — not kernel-enforced against
//     non-cooperating processes, which is fine: the only contender is another
//     runbaypty daemon, and daemons cooperate by design.
//   - Self-healing: a stale lock left by a killed daemon is detected via a
//     pid-alive probe of the recorded owner and reclaimed.
//   - Transferable across a zero-downtime upgrade: flock ownership belongs to
//     the OPEN FILE DESCRIPTION, so passing the lock fd over SCM_RIGHTS hands
//     the lock to the new daemon with no held-by-nobody gap (see HANDOVER.md).
//
// Usage:
//
//	lock, err := processlock.Acquire(filepath.Join(home, "daemon.lock"))
//	if err != nil { return err }
//	defer lock.Release()
//	// ... run the daemon ...
package processlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrLockHeld is returned when another live process holds the lock.
var ErrLockHeld = errors.New("processlock: another runbaypty daemon holds the lock")

// Lock represents a held lock. Call Release exactly once.
type Lock struct {
	path  string
	file  *os.File
	owned bool
}

// File exposes the lock's open file. flock ownership belongs to the OPEN
// FILE DESCRIPTION, so passing this fd over SCM_RIGHTS transfers the lock
// itself — the handover never has a held-by-nobody gap (HANDOVER.md).
func (l *Lock) File() *os.File { return l.file }

// Adopt wraps an fd received over SCM_RIGHTS as a held Lock. The flock
// arrived WITH the fd; no locking syscall happens here.
func Adopt(path string, file *os.File) *Lock {
	return &Lock{path: path, file: file, owned: true}
}

// Acquire takes an exclusive lock at path. Steps:
//
//  1. Ensure the parent dir exists (mkdir -p).
//  2. Try to open + flock the file. If it fails because another process holds
//     the lock, read its pid; if dead, reclaim and retry once; if alive, return
//     ErrLockHeld.
//  3. Write our own pid + start time to the file content for forensics.
func Acquire(path string) (*Lock, error) {
	// #nosec G301 -- lock parent dir is user-facing (e.g. ~/.runbaypty)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("processlock: mkdir parent: %w", err)
	}

	// Attempt acquisition. If it fails AND the existing pid is dead, reclaim.
	for attempt := 0; attempt < 2; attempt++ {
		l, err := tryAcquire(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, errLockBusy) {
			return nil, err
		}

		// Lock busy — check if owner is alive.
		if alive, _ := ownerAlive(path); !alive {
			// Stale lock; remove and retry once.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("processlock: reclaim stale: %w", err)
			}
			continue
		}
		return nil, ErrLockHeld
	}
	return nil, ErrLockHeld
}

// Release drops the lock. Idempotent, and a no-op on a nil receiver — safe to
// `defer lock.Release()` even when Acquire returned (nil, err).
func (l *Lock) Release() error {
	if l == nil || !l.owned {
		return nil
	}
	l.owned = false
	// Close releases the OS-level flock automatically on Unix.
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("processlock: close: %w", err)
	}
	// Best-effort cleanup of the lock file content. We DON'T delete the file
	// itself because doing so creates a TOCTOU window for the next Acquirer.
	// Subsequent Acquire calls will re-truncate and rewrite content.
	return nil
}

// ownerAlive reads the lock file content and checks if the owning pid is alive.
// Returns (alive, error). On parse errors, treats as alive (safe default — better
// to refuse than to incorrectly reclaim).
func ownerAlive(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 0 {
		return true, nil
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 0 {
		return true, nil
	}
	return pidAlive(pid), nil
}

// writeContent writes pid + timestamp to an open lock file.
// Format: "<pid> <iso8601-timestamp>\n"
// Read by ownerAlive() and by `runbaypty daemon status` for diagnostics.
func writeContent(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err := fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
