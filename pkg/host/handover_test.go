package host

// handover_test.go — the engine half of the zero-downtime upgrade, tested
// WITHOUT a daemon: pause a live reader, snapshot state, rebuild the session
// around the same ptmx in a fresh registry, and prove the seq axis and the
// process both survived. Plus the rollback path (ResumeReader).

import (
	"bytes"
	"github.com/creack/pty"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestRing_SnapshotRestoreKeepsSeqAxis(t *testing.T) {
	t.Parallel()
	r := NewRing(256)
	fill(r, 100, 100, 100) // 300 bytes through a 256 ring: wrapped + truncated

	data, endSeq := r.Snapshot()
	if endSeq != r.LastSeq() || len(data) != r.Len() {
		t.Fatalf("Snapshot = (%d bytes, end %d), ring = (%d, %d)", len(data), endSeq, r.Len(), r.LastSeq())
	}
	if r.capBytes() != 256 {
		t.Errorf("capBytes = %d", r.capBytes())
	}

	// Restored ring continues the ABSOLUTE axis — replay arithmetic must
	// stay valid across a daemon handover.
	restored := NewRingFromSnapshot(256, data, endSeq)
	if restored.LastSeq() != endSeq {
		t.Fatalf("restored LastSeq = %d, want %d", restored.LastSeq(), endSeq)
	}
	oldest := endSeq - uint64(restored.Len())
	got, from, truncated := restored.ReplayFrom(oldest)
	if from != oldest || truncated {
		t.Fatalf("restored replay = (from %d, truncated %v)", from, truncated)
	}
	if want := refChunk(oldest, int(endSeq-oldest)); !bytes.Equal(got, want) {
		t.Error("restored ring bytes diverge from the pre-snapshot stream")
	}
	// New writes continue the axis, not restart it.
	restored.Write(refChunk(endSeq, 10))
	if restored.LastSeq() != endSeq+10 {
		t.Errorf("post-restore write broke the axis: %d", restored.LastSeq())
	}
}

func TestSession_PauseResumeReaderKeepsSessionAlive(t *testing.T) {
	s := spawnT(t, shCfg("i=0; while :; do echo p-$i; i=$((i+1)); sleep 0.01; done"))
	waitOutputContains(t, s, "p-0")

	if err := s.PauseReader(); err != nil {
		t.Fatalf("PauseReader: %v", err)
	}
	// Paused: the ring must stop advancing even though the child keeps
	// writing (its bytes wait in the kernel PTY buffer).
	frozen := s.LastSeq()
	time.Sleep(80 * time.Millisecond)
	if s.LastSeq() != frozen {
		t.Fatalf("ring advanced while paused: %d → %d", frozen, s.LastSeq())
	}
	if s.State() != proto.StateRunning {
		t.Fatalf("pause changed state to %s — the session must stay alive", s.State())
	}

	// Rollback path: resume, and the buffered bytes flow.
	if err := s.ResumeReader(); err != nil {
		t.Fatalf("ResumeReader: %v", err)
	}
	deadline := time.Now().Add(testTimeout)
	for s.LastSeq() == frozen {
		if time.Now().After(deadline) {
			t.Fatal("ring never advanced after resume")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Pausing an exited session is a no-op, not an error.
	_ = s.Kill(proto.SignalKILL)
	waitExit(t, s)
	if err := s.PauseReader(); err != nil {
		t.Errorf("PauseReader on exited session: %v", err)
	}
	if err := s.ResumeReader(); err != nil {
		t.Errorf("ResumeReader on exited session: %v", err)
	}
}

func TestSession_HandoverStateAndAdoptRoundTrip(t *testing.T) {
	oldReg := NewRegistry(0)
	cfg := shCfg("i=0; while :; do echo h-$i; i=$((i+1)); sleep 0.01; done")
	cfg.Name = "adoptee"
	cfg.Meta = map[string]string{"k": "v"}
	src, err := oldReg.Spawn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitOutputContains(t, src, "h-1")
	pidBefore := src.Info().Pid

	// Freeze, then snapshot: the ring must be quiescent for an exact axis.
	if err := src.PauseReader(); err != nil {
		t.Fatal(err)
	}
	state, ring, ptmx := src.HandoverState()
	if state.ID != src.ID() || state.Name != "adoptee" || state.Meta["k"] != "v" || state.Pid != pidBefore {
		t.Fatalf("HandoverState = %+v", state)
	}
	if state.RingEndSeq != src.LastSeq() || len(ring) != src.RingLen() {
		t.Fatalf("state ring (%d, end %d) vs session (%d, %d)", len(ring), state.RingEndSeq, src.RingLen(), src.LastSeq())
	}

	// Rebuild in a NEW registry around the same PTY — the "new daemon".
	newReg := NewRegistry(0)
	adopted := AdoptSession(state, ring, ptmx, newReg.Events())
	t.Cleanup(func() {
		_ = adopted.Kill(proto.SignalKILL)
		waitExit(t, adopted)
	})
	if err := newReg.Adopt(adopted); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got, err := newReg.Lookup("adoptee"); err != nil || got != adopted {
		t.Fatalf("adopted session not resolvable by name: %v", err)
	}

	// Identity + axis survived; the process is the SAME one.
	info := adopted.Info()
	if info.Pid != pidBefore || info.ID != state.ID || info.Meta["k"] != "v" {
		t.Fatalf("adopted info = %+v", info)
	}
	if adopted.LastSeq() != state.RingEndSeq {
		t.Fatalf("adopted axis restarted: %d != %d", adopted.LastSeq(), state.RingEndSeq)
	}
	if !adopted.Linger() {
		t.Error("linger policy lost across adoption")
	}

	// And it keeps streaming: the axis advances from where it left off.
	deadline := time.Now().Add(testTimeout)
	for adopted.LastSeq() <= state.RingEndSeq {
		if time.Now().After(deadline) {
			t.Fatal("adopted session never produced new output")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Adopting the same id twice is refused; so is a duplicate name.
	if err := newReg.Adopt(adopted); err == nil {
		t.Error("duplicate Adopt accepted")
	}
}

func TestAdoptedSession_ExitReportsUnknownStatus(t *testing.T) {
	// An adopted process is not our child: exit detection works (PTY EOF)
	// but the wait status is unavailable — code -1, honestly.
	reg := NewRegistry(0)
	src, err := reg.Spawn(shCfg("sleep 300"))
	if err != nil {
		t.Fatal(err)
	}
	if err := src.PauseReader(); err != nil {
		t.Fatal(err)
	}
	state, ring, ptmx := src.HandoverState()

	newReg := NewRegistry(0)
	adopted := AdoptSession(state, ring, ptmx, newReg.Events())
	if err := newReg.Adopt(adopted); err != nil {
		t.Fatal(err)
	}
	if err := adopted.Kill(proto.SignalKILL); err != nil {
		t.Fatal(err)
	}
	waitExit(t, adopted)
	code, _, exited := adopted.ExitCode()
	if !exited || code != -1 {
		t.Errorf("adopted exit = (%d, exited=%v), want (-1, true) — wait status is unavailable", code, exited)
	}
}

func TestSession_SmallAccessors(t *testing.T) {
	s := spawnT(t, shCfg("sleep 300"))
	if s.Linger() != true {
		t.Error("Linger() should mirror cfg")
	}
	if got := s.AddSubscriber(); got != 1 {
		t.Errorf("AddSubscriber = %d", got)
	}
	if got := s.AddSubscriber(); got != 2 {
		t.Errorf("AddSubscriber = %d", got)
	}
	if got := s.RemoveSubscriber(); got != 1 {
		t.Errorf("RemoveSubscriber = %d", got)
	}
	if s.Info().Subscribers != 1 {
		t.Errorf("Info().Subscribers = %d", s.Info().Subscribers)
	}
	if s.RingLen() < 0 {
		t.Error("RingLen negative")
	}
	s.SetMeta(map[string]string{"a": "1"})
	if s.Info().Meta["a"] != "1" {
		t.Error("SetMeta not reflected in Info")
	}
	if s.Monitor() == nil {
		t.Error("Monitor() nil")
	}
}

func TestPollableFile_EnablesDeadlines(t *testing.T) {
	// The invariant PauseReader depends on: a rewrapped PTY master accepts
	// SetReadDeadline (plain creack masters do not on macOS).
	s := spawnT(t, shCfg("sleep 300"))
	if err := s.ptmx.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("spawned master rejects deadlines — pollableFile regression: %v", err)
	}
	if err := s.ptmx.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestIsTransientSpawnErr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"ENXIO retries", errnoErr(6 /* ENXIO on darwin/linux */), true},
		{"bogus negative errno retries", errnoErr(-6), true},
		{"ENOENT does not", errnoErr(2), false},
		{"non-errno does not", errNotErrno{}, false},
		{"nil does not", nil, false},
	} {
		if got := isTransientSpawnErr(tc.err); got != tc.want {
			t.Errorf("%s: isTransientSpawnErr(%v) = %v", tc.name, tc.err, got)
		}
	}
}

type errNotErrno struct{}

func (errNotErrno) Error() string { return "not an errno" }

// errnoErr builds a syscall.Errno for the table above (ENXIO is 6 on both
// darwin and linux; a negative value models the bogus runtime artifact).
func errnoErr(n int) error { return syscall.Errno(uintptr(n)) }

func TestStartPTYWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	// The retry loop must survive a transient errno storm and hand back the
	// *rebuilt* Cmd (a failed Start poisons its Cmd — reusing it segfaults).
	orig := ptyStart
	t.Cleanup(func() { ptyStart = orig })

	var attempts int
	ptyStart = func(c *exec.Cmd, ws *pty.Winsize) (*os.File, error) {
		attempts++
		if attempts < 3 {
			return nil, syscall.Errno(uintptr(6)) // ENXIO: transient
		}
		return orig(c, ws)
	}

	r := NewRegistry(0)
	s, err := r.Spawn(shCfg("echo retried-ok"))
	if err != nil {
		t.Fatalf("Spawn should have survived 2 transient failures: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Kill(proto.SignalKILL)
		waitExit(t, s)
	})
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	// The session is fully functional — the returned Cmd is the started one.
	waitExit(t, s)
	waitOutputContains(t, s, "retried-ok")
	if code, _, ok := s.ExitCode(); !ok || code != 0 {
		t.Errorf("exit = (%d, %v) — Wait() ran against the wrong Cmd?", code, ok)
	}
}

func TestStartPTYWithRetry_GivesUpAndRefusesNonTransient(t *testing.T) {
	orig := ptyStart
	t.Cleanup(func() { ptyStart = orig })

	// Permanent transient failure → exhausts retries, surfaces E_SPAWN_FAILED.
	var attempts int
	ptyStart = func(*exec.Cmd, *pty.Winsize) (*os.File, error) {
		attempts++
		return nil, syscall.Errno(uintptr(6))
	}
	r := NewRegistry(0)
	if _, err := r.Spawn(shCfg("true")); !errcodes.IsCode(err, errcodes.SpawnFailed) {
		t.Errorf("exhausted retries = %v, want E_SPAWN_FAILED", err)
	}
	if attempts != ptySpawnRetries+1 {
		t.Errorf("attempts = %d, want %d", attempts, ptySpawnRetries+1)
	}

	// A NON-transient failure must not retry at all — fail fast.
	attempts = 0
	ptyStart = func(*exec.Cmd, *pty.Winsize) (*os.File, error) {
		attempts++
		return nil, syscall.ENOENT
	}
	if _, err := r.Spawn(shCfg("true")); !errcodes.IsCode(err, errcodes.SpawnFailed) {
		t.Errorf("non-transient = %v", err)
	}
	if attempts != 1 {
		t.Errorf("non-transient retried %d times, want 1 attempt", attempts)
	}
}

func TestSession_BrokenLogDisablesItselfAndWarns(t *testing.T) {
	// A durable log that starts failing must NOT take down a healthy
	// session: it disables itself once and emits a warning event.
	path := filepath.Join(t.TempDir(), "broken.log")
	cfg := shCfg("i=0; while :; do echo line-$i; i=$((i+1)); sleep 0.01; done")
	cfg.LogPath = path

	r := NewRegistry(0)
	subID, events := r.Events().Subscribe("")
	t.Cleanup(func() { r.Events().Unsubscribe(subID) })

	s, err := r.Spawn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Kill(proto.SignalKILL)
		waitExit(t, s)
	})
	waitOutputContains(t, s, "line-0")

	// Break the log out from under the writer (close its fd).
	s.mu.Lock()
	logw := s.logw
	s.mu.Unlock()
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}

	// A meta-changed event carrying log_broken must arrive, and the session
	// must keep streaming afterwards.
	deadline := time.After(testTimeout)
	for {
		select {
		case ev := <-events:
			if ev.Type == proto.EventMetaChanged && ev.Data[proto.DataKeyLogBroken] != "" {
				// The session must keep streaming after the log breaks. Poll
				// for the seq to advance rather than asserting it moved within a
				// fixed window — under a loaded CI runner (race detector, many
				// parallel packages) the shell subprocess can miss a short
				// window and flake, even though streaming never actually stopped.
				before := s.LastSeq()
				streamDeadline := time.After(testTimeout)
				for s.LastSeq() <= before {
					select {
					case <-streamDeadline:
						t.Error("session stopped streaming after its log broke")
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
				return
			}
		case <-deadline:
			t.Fatal("no log_broken event after the log failed")
		}
	}
}
