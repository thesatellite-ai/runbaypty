package host

import (
	"fmt"
	"sync"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// killAll reaps every session in r so goleak stays clean.
func killAll(t *testing.T, r *Registry) {
	t.Helper()
	for _, s := range r.List() {
		_ = s.Kill(proto.SignalKILL)
		waitExit(t, s)
	}
}

func TestRegistry_LookupByIDAndName(t *testing.T) {
	r := NewRegistry(0)
	t.Cleanup(func() { killAll(t, r) })

	cfg := shCfg("sleep 300")
	cfg.Name = "dev-server"
	s, err := r.Spawn(cfg)
	if err != nil {
		t.Fatal(err)
	}

	byID, err := r.Lookup(s.ID())
	if err != nil || byID != s {
		t.Errorf("Lookup(id) = (%v, %v), want the session", byID, err)
	}
	byName, err := r.Lookup("dev-server")
	if err != nil || byName != s {
		t.Errorf("Lookup(name) = (%v, %v), want the session", byName, err)
	}
	if _, err := r.Lookup("nope"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("expected E_SESSION_NOT_FOUND, got %v", err)
	}
	// A well-formed but unknown id also 404s with the id-flavored message.
	if _, err := r.Lookup("ses_018f3b2c9a8e7891b4f51234567890ab"); !errcodes.IsCode(err, errcodes.SessionNotFound) {
		t.Errorf("expected E_SESSION_NOT_FOUND for unknown id, got %v", err)
	}
}

func TestRegistry_NameUniqueness(t *testing.T) {
	r := NewRegistry(0)
	t.Cleanup(func() { killAll(t, r) })

	cfg := shCfg("sleep 300")
	cfg.Name = "taken"
	if _, err := r.Spawn(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Spawn(cfg); !errcodes.IsCode(err, errcodes.NameTaken) {
		t.Errorf("expected E_NAME_TAKEN, got %v", err)
	}
}

func TestRegistry_InvalidNamesRefused(t *testing.T) {
	r := NewRegistry(0)
	for _, name := range []string{"-leading-dash", "has space", "has/slash", "x" + string(make([]byte, 100)), ".hidden"} {
		cfg := shCfg("true")
		cfg.Name = name
		if _, err := r.Spawn(cfg); !errcodes.IsCode(err, errcodes.InvalidName) {
			t.Errorf("name %q: expected E_INVALID_NAME, got %v", name, err)
		}
	}
	if r.Count() != 0 {
		t.Errorf("invalid names left %d registry entries", r.Count())
	}
}

func TestRegistry_RenameFreesOldName(t *testing.T) {
	r := NewRegistry(0)
	t.Cleanup(func() { killAll(t, r) })

	cfg := shCfg("sleep 300")
	cfg.Name = "before"
	s, err := r.Spawn(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Rename("before", "after"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := r.Lookup("before"); err == nil {
		t.Error("old name still resolves after rename")
	}
	if got, _ := r.Lookup("after"); got != s {
		t.Error("new name does not resolve")
	}

	// The freed name is reusable.
	cfg2 := shCfg("sleep 300")
	cfg2.Name = "before"
	if _, err := r.Spawn(cfg2); err != nil {
		t.Errorf("freed name not reusable: %v", err)
	}
	// Renaming to a taken name is refused.
	if err := r.Rename("after", "before"); !errcodes.IsCode(err, errcodes.NameTaken) {
		t.Errorf("expected E_NAME_TAKEN, got %v", err)
	}
	// Clearing a name works and unindexes it.
	if err := r.Rename("after", ""); err != nil {
		t.Fatalf("clear name: %v", err)
	}
	if s.Name() != "" {
		t.Errorf("name not cleared: %q", s.Name())
	}
}

func TestRegistry_MaxSessionsCap(t *testing.T) {
	r := NewRegistry(2)
	t.Cleanup(func() { killAll(t, r) })

	for range 2 {
		if _, err := r.Spawn(shCfg("sleep 300")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := r.Spawn(shCfg("sleep 300"))
	if !errcodes.IsCode(err, errcodes.LimitExceeded) {
		t.Fatalf("expected E_LIMIT_EXCEEDED, got %v", err)
	}
	// Existing sessions unharmed; reaping one frees a slot.
	victims := r.List()
	if len(victims) != 2 {
		t.Fatalf("List = %d sessions, want 2", len(victims))
	}
	_ = victims[0].Kill(proto.SignalKILL)
	waitExit(t, victims[0])
	r.Remove(victims[0].ID())
	if _, err := r.Spawn(shCfg("sleep 300")); err != nil {
		t.Errorf("slot not freed after Remove: %v", err)
	}
}

func TestRegistry_ConcurrentSpawnsRaceClean(t *testing.T) {
	r := NewRegistry(64)
	t.Cleanup(func() { killAll(t, r) })

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := shCfg("sleep 300")
			cfg.Name = fmt.Sprintf("conc-%d", i)
			if _, err := r.Spawn(cfg); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent spawn: %v", err)
	}
	if got := len(r.List()); got != 32 {
		t.Errorf("List = %d, want 32", got)
	}
	// Same-name race: exactly one of N wins.
	var okCount int
	var mu sync.Mutex
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := shCfg("sleep 300")
			cfg.Name = "same-name"
			if _, err := r.Spawn(cfg); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if okCount != 1 {
		t.Errorf("same-name race: %d spawns succeeded, want exactly 1", okCount)
	}
}

func TestRegistry_ListSortedChronologically(t *testing.T) {
	r := NewRegistry(0)
	t.Cleanup(func() { killAll(t, r) })

	for range 5 {
		if _, err := r.Spawn(shCfg("sleep 300")); err != nil {
			t.Fatal(err)
		}
	}
	list := r.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].ID() >= list[i].ID() {
			t.Errorf("List not sorted at %d: %s >= %s", i, list[i-1].ID(), list[i].ID())
		}
	}
}

func TestSession_InfoSnapshot(t *testing.T) {
	r := NewRegistry(0)
	t.Cleanup(func() { killAll(t, r) })

	cfg := shCfg("echo info-test; sleep 300")
	cfg.Name = "info-target"
	cfg.Meta = map[string]string{"app": "runbay", "run": "42"}
	s, err := r.Spawn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitOutputContains(t, s, "info-test")
	s.TakeWrite("cli_me")

	info := s.Info()
	switch {
	case info.ID != s.ID():
		t.Errorf("info.ID = %q", info.ID)
	case info.Name != "info-target":
		t.Errorf("info.Name = %q", info.Name)
	case info.State != proto.StateRunning:
		t.Errorf("info.State = %q", info.State)
	case info.Pid <= 0:
		t.Errorf("info.Pid = %d", info.Pid)
	case info.LastSeq == 0:
		t.Error("info.LastSeq = 0 after output")
	case info.BytesOut != info.LastSeq:
		t.Errorf("BytesOut %d != LastSeq %d", info.BytesOut, info.LastSeq)
	case info.WriteLockHolder != "cli_me":
		t.Errorf("WriteLockHolder = %q", info.WriteLockHolder)
	case info.Meta["run"] != "42":
		t.Errorf("Meta = %v", info.Meta)
	case info.ExitCode != nil:
		t.Error("ExitCode set while running")
	}
}

func TestRegistry_GlobalRingCapRefusesAndReleases(t *testing.T) {
	r := NewRegistry(0)
	r.SetRingTotalCap(3 * 1024 * 1024) // room for three 1MiB rings
	t.Cleanup(func() { killAll(t, r) })

	cfg := shCfg("sleep 300")
	cfg.RingBytes = 1024 * 1024
	var first *Session
	for i := range 3 {
		s, err := r.Spawn(cfg)
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		if first == nil {
			first = s
		}
	}
	if _, err := r.Spawn(cfg); !errcodes.IsCode(err, errcodes.LimitExceeded) {
		t.Fatalf("expected E_LIMIT_EXCEEDED at the ring cap, got %v", err)
	}
	// Reaping one session releases its reservation.
	_ = first.Kill(proto.SignalKILL)
	waitExit(t, first)
	r.Remove(first.ID())
	if _, err := r.Spawn(cfg); err != nil {
		t.Fatalf("reservation not released on Remove: %v", err)
	}
	// A failed spawn must release its reservation too.
	bad := cfg
	bad.Cmd = "/nonexistent/xyz"
	for range 5 {
		_, _ = r.Spawn(bad)
	}
	if _, err := r.Spawn(cfg); !errcodes.IsCode(err, errcodes.LimitExceeded) {
		t.Fatalf("cap accounting drifted after failed spawns: %v", err)
	}
}
