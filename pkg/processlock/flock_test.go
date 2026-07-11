package processlock

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAcquire_BasicCycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l == nil {
		t.Fatal("nil lock")
	}

	// File exists with our pid in content
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if !strings.HasPrefix(string(data), "1") && !strings.Contains(string(data), " ") {
		// Lazy check — at least it's non-empty and has space-delimited fields
		t.Errorf("unexpected lock content: %q", string(data))
	}

	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}

	// Double release is fine
	if err := l.Release(); err != nil {
		t.Errorf("double release: %v", err)
	}
}

func TestAcquire_BlocksConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l1.Release()

	// Second acquire while first is held → ErrLockHeld
	l2, err := Acquire(path)
	if !errors.Is(err, ErrLockHeld) {
		t.Errorf("expected ErrLockHeld, got %v (lock=%v)", err, l2)
	}
	if l2 != nil {
		l2.Release()
	}
}

func TestAcquire_ReleasesAndReacquires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire #1: %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	defer l2.Release()
}

func TestAcquire_StaleLockReclaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	// Plant a stale lock file with a definitely-dead pid (PID 1 is init on
	// Unix, always alive — pick a high impossible pid).
	if err := os.WriteFile(path, []byte("9999999 stale\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("expected reclaim, got %v", err)
	}
	defer l.Release()
}

func TestAcquire_NilLockReleaseIsNoOp(t *testing.T) {
	// Release on a nil lock is a no-op so callers can `defer lock.Release()`
	// even on the error path where Acquire returned (nil, err).
	var l *Lock
	if err := l.Release(); err != nil {
		t.Errorf("nil-lock Release: %v", err)
	}
}

func TestAcquire_MakesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
}

func TestOwnerAlive_DeadPid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	if err := os.WriteFile(path, []byte("9999999 stale\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	alive, err := ownerAlive(path)
	if err != nil {
		t.Fatalf("ownerAlive: %v", err)
	}
	if alive {
		t.Error("expected stale pid to read as dead")
	}
}

func TestOwnerAlive_OurPid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	alive, err := ownerAlive(path)
	if err != nil {
		t.Fatalf("ownerAlive: %v", err)
	}
	if !alive {
		t.Error("expected our own pid to read as alive")
	}
}

// ── handover surface (fd transfer) ──────────────────────────────────

func TestLock_FileExposesOpenFileForTransfer(t *testing.T) {
	// The flock belongs to the OPEN FILE DESCRIPTION, so passing this fd
	// over SCM_RIGHTS transfers the lock itself — the daemon handover
	// depends on it (HANDOVER.md).
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	f := l.File()
	if f == nil {
		t.Fatal("File() = nil")
	}
	if f.Name() != path {
		t.Errorf("File().Name() = %q, want %q", f.Name(), path)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAdopt_WrapsTransferredFdAsHeldLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adopted.lock")

	// The "old daemon" holds the lock…
	orig, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	// …and hands its fd over (dup models the SCM_RIGHTS transfer).
	fd, err := syscall.Dup(int(orig.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	transferred := os.NewFile(uintptr(fd), path)

	adopted := Adopt(path, transferred)
	if adopted.File() != transferred {
		t.Error("Adopt did not retain the transferred file")
	}
	// The lock never had a held-by-nobody gap: while the dup is open, a
	// third party cannot take it even after the original releases.
	if err := orig.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Error("lock was acquirable while the transferred fd still holds it")
	}
	if err := adopted.Release(); err != nil {
		t.Fatal(err)
	}
	// Now it is free.
	l3, err := Acquire(path)
	if err != nil {
		t.Fatalf("lock not released by the adopted holder: %v", err)
	}
	_ = l3.Release()
}

func TestAcquire_ReclaimsLockFromDeadOwner(t *testing.T) {
	// A daemon killed by -9 leaves its lock file behind. flock itself is
	// released by the kernel, but the pid content lingers; the next daemon
	// must reclaim rather than refuse (this is the crash-recovery path).
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.lock")

	deadPID := 99999
	for pidAlive(deadPID) {
		deadPID--
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(deadPID)+"\n0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire over a dead owner's lock file: %v", err)
	}
	// The file now records OUR pid (forensics for the next daemon).
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), strconv.Itoa(os.Getpid())) {
		t.Errorf("lock content = %q, want our pid", body)
	}
	if alive, oerr := ownerAlive(path); oerr != nil || !alive {
		t.Errorf("ownerAlive() = (%v, %v) for our own live lock", alive, oerr)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// Release is idempotent.
	if err := l.Release(); err != nil {
		t.Errorf("double Release: %v", err)
	}
}

func TestOwnerAlive_MissingAndGarbageFiles(t *testing.T) {
	dir := t.TempDir()
	if alive, _ := ownerAlive(filepath.Join(dir, "absent.lock")); alive {
		t.Error("ownerAlive(missing) = true")
	}
	// A lock file we cannot parse is treated as HELD, deliberately: refusing
	// to start beats wrongly reclaiming a live daemon's lock.
	garbage := filepath.Join(dir, "garbage.lock")
	if err := os.WriteFile(garbage, []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	alive, err := ownerAlive(garbage)
	if err != nil || !alive {
		t.Errorf("ownerAlive(unparseable) = (%v, %v), want (true, nil) — the safe default", alive, err)
	}
	// A zero/negative pid is likewise treated as held.
	zero := filepath.Join(dir, "zero.lock")
	if err := os.WriteFile(zero, []byte("0 2026-07-10"), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, _ := ownerAlive(zero); !alive {
		t.Error("ownerAlive(pid 0) = false, want the safe default")
	}
}
