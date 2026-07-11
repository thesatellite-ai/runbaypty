// session.go — one PTY session: spawn, reader pump, ring, write lock,
// resize, kill-tree, exit capture.
//
// Concurrency model: exactly one reader goroutine per session pumps the PTY
// fd into the ring and signals a sync.Cond. Subscribers are PULL-based: they
// loop WaitOutput(afterSeq) → ReplayFrom(lastSeen) at their own pace, which
// makes per-subscriber backpressure natural (a slow subscriber only delays
// itself; the truncated flag tells it when it fell behind the ring). The
// fanout/disconnect policy layers on top in the daemon (M3), not here.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/ids"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// readBufSize is the PTY read chunk. 32 KiB matches the default batch cap —
// one full read can become one OUTPUT frame without re-slicing.
const readBufSize = 32 * 1024

// veof is the EOF control byte (^D) written by InputEOF. A PTY has no
// half-close; the terminal way to signal EOF is the VEOF character, which
// the line discipline turns into a zero-length read for the child.
const veof = 0x04

// signalMap translates wire signal names (proto.Signal*) to the platform
// signal. Closed set — KILL validates against it.
var signalMap = map[string]syscall.Signal{
	proto.SignalTERM: syscall.SIGTERM,
	proto.SignalKILL: syscall.SIGKILL,
	proto.SignalINT:  syscall.SIGINT,
	proto.SignalHUP:  syscall.SIGHUP,
}

// SpawnConfig is the engine-side spawn request (the daemon maps proto.Spawn
// onto it after validation).
type SpawnConfig struct {
	Cmd  string
	Args []string
	Cwd  string
	// Env entries are appended to the daemon's environment (KEY=VALUE).
	Env  []string
	Cols uint16
	Rows uint16
	Name string
	Meta map[string]string
	// RingBytes ≤ 0 uses constants.DefaultRingBytes.
	RingBytes int
	// LogPath enables the durable (ts,bytes) log at this path ("" = off).
	LogPath string
	// SilenceAfter tunes the silence/activity threshold (0 = default).
	SilenceAfter time.Duration
	// Linger=false kills the session when the last subscriber detaches
	// (daemon-enforced); default true.
	Linger bool
}

// Session is one live (or retained-after-exit) PTY session.
type Session struct {
	id   string
	ring *Ring
	// bus receives this session's lifecycle events (registry-owned).
	bus *EventBus
	// mon watches activity/silence/bell (always set; cheap when idle).
	mon *Monitor
	// subscribers counts active attaches (daemon-maintained; linger policy
	// and INFO read it).
	subscribers atomic.Int64

	// cond guards the mutable fields below AND signals "ring advanced or
	// state changed" to WaitOutput waiters. cond.L is &s.mu.
	mu   sync.Mutex
	cond *sync.Cond

	cfg     SpawnConfig
	name    string
	meta    map[string]string
	state   proto.SessionState
	ptmx    *os.File
	cmd     *exec.Cmd
	pid     int
	cols    uint16
	rows    uint16
	bytesIn uint64

	// writeLockHolder is the client id holding the single write lock
	// ("" = unheld). Enforcement of readOnly attaches lives in the daemon;
	// the session enforces "INPUT requires the lock".
	writeLockHolder string

	startedAt time.Time
	exitCode  int    // valid once state == StateExited
	exitSig   string // wire signal name when killed by signal, else ""
	exitedAt  time.Time

	// logw is the optional durable log; logBroken flips once on the first
	// append failure (stop-and-warn, never crash the pump over a full disk).
	logw      *SessionLogWriter
	logBroken bool

	// paused: the read pump exited for handover WITHOUT finishing the
	// session; readerDone closes each time a pump goroutine exits.
	paused     bool
	readerDone chan struct{}
	// adopted: this session came through a handover — the process is not
	// our child, so finish() must not cmd.Wait (see AdoptSession).
	adopted bool

	// done closes when the reader pump has fully drained and Wait returned.
	done chan struct{}
}

// spawn starts the PTY process. Callers go through Registry.Spawn (which
// mints the id, enforces caps, indexes the name, and provides the bus).
func spawn(id string, cfg SpawnConfig, bus *EventBus) (*Session, error) {
	if cfg.Cols == 0 || cfg.Rows == 0 {
		return nil, errcodes.Newf(errcodes.InvalidInput, "spawn %q: cols/rows must be non-zero", cfg.Cmd)
	}
	ringBytes := cfg.RingBytes
	if ringBytes <= 0 {
		ringBytes = constants.DefaultRingBytes
	}

	var logw *SessionLogWriter
	if cfg.LogPath != "" {
		var err error
		if logw, err = NewSessionLogWriter(cfg.LogPath, time.Now()); err != nil {
			return nil, err
		}
	}

	cmd := exec.Command(cfg.Cmd, cfg.Args...)
	cmd.Dir = cfg.Cwd
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	// pty.StartWithSize runs the child with Setsid+Setctty: the child is a
	// session leader whose pgid == pid, so signaling -pid reaches the whole
	// process group — that IS the kill-tree mechanism.
	rawPtmx, cmd, err := startPTYWithRetry(cmd, &pty.Winsize{Cols: cfg.Cols, Rows: cfg.Rows})
	if err != nil {
		if logw != nil {
			_ = logw.Close()
		}
		return nil, errcodes.Newf(errcodes.SpawnFailed, "spawn %q: %v", cfg.Cmd, err).WithCause(err)
	}
	// Rewrap the master as a POLLABLE file (nonblock + netpoller): read
	// deadlines then work, which is what lets a handover pause the reader
	// without closing the fd (handover.go). Plain creack ptmx files are
	// blocking-mode on some platforms and refuse SetReadDeadline.
	ptmx, err := pollableFile(rawPtmx)
	if err != nil {
		_ = rawPtmx.Close()
		if logw != nil {
			_ = logw.Close()
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil, errcodes.Newf(errcodes.SpawnFailed, "spawn %q: pollable master: %v", cfg.Cmd, err).WithCause(err)
	}

	s := &Session{
		id:        id,
		ring:      NewRing(ringBytes),
		bus:       bus,
		mon:       NewMonitor(id, bus, cfg.SilenceAfter),
		cfg:       cfg,
		name:      cfg.Name,
		meta:      cfg.Meta,
		state:     proto.StateRunning,
		ptmx:      ptmx,
		cmd:       cmd,
		pid:       cmd.Process.Pid,
		cols:      cfg.Cols,
		rows:      cfg.Rows,
		startedAt: time.Now().UTC(),
		logw:      logw,
		done:      make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	s.readerDone = make(chan struct{})

	go s.readPump()
	return s, nil
}

// ptySpawnRetries / ptySpawnRetryDelay bound the transient-failure retry in
// startPTYWithRetry.
const (
	ptySpawnRetries    = 3
	ptySpawnRetryDelay = 10 * time.Millisecond
)

// ptyStart is the PTY-spawn seam. Production points at creack's
// StartWithSize; tests swap it to exercise the transient-failure retry
// (real ENXIO storms are not reproducible on demand).
var ptyStart = pty.StartWithSize

// startPTYWithRetry wraps pty.StartWithSize with a bounded retry: macOS
// transiently fails PTY allocation with ENXIO ("device not configured") —
// or a bogus negative errno — under concurrent open load (observed at
// 32-way parallel spawns). The failure clears within milliseconds;
// retrying beats surfacing a spurious E_SPAWN_FAILED to the client.
//
// Returns the *exec.Cmd that actually STARTED: a failed Start poisons its
// Cmd (Go forbids reuse), so retries run a rebuilt copy — callers must use
// the returned Cmd, never the one they passed in (its .Process is nil).
func startPTYWithRetry(cmd *exec.Cmd, ws *pty.Winsize) (*os.File, *exec.Cmd, error) {
	var lastErr error
	for attempt := 0; attempt <= ptySpawnRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(ptySpawnRetryDelay << (attempt - 1)) // 10/20/40ms
		}
		ptmx, err := ptyStart(cmd, ws)
		if err == nil {
			return ptmx, cmd, nil
		}
		lastErr = err
		if !isTransientSpawnErr(err) {
			return nil, nil, err // real failure (bad command, perms) — no retry
		}
		// exec.Cmd cannot be reused after a failed Start: rebuild it.
		fresh := exec.Command(cmd.Path, cmd.Args[1:]...)
		fresh.Dir, fresh.Env = cmd.Dir, cmd.Env
		cmd = fresh
	}
	return nil, nil, lastErr
}

// isTransientSpawnErr classifies PTY-spawn failures worth retrying:
// ENXIO/EAGAIN/EINTR, plus BOGUS NEGATIVE errnos — macOS's fork/exec path
// under concurrent /dev/ptmx opens has been observed returning errno -6
// (prints as "errno -6"; a real Errno is never negative), which clears on
// retry like ENXIO does.
func isTransientSpawnErr(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if errno == syscall.ENXIO || errno == syscall.EAGAIN || errno == syscall.EINTR {
		return true
	}
	return int64(errno) < 0 // bogus negative errno = runtime artifact, transient
}

// readPump is the one goroutine that owns reads from the PTY fd. It exits
// when the fd reaches EOF/EIO (child exited and the slave side closed) —
// that is its clearly-defined exit condition.
func (s *Session) readPump() {
	buf := make([]byte, readBufSize)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			now := time.Now()
			s.ring.Write(buf[:n])
			s.mon.Feed(buf[:n], now)
			s.appendLog(now, buf[:n])
			s.cond.L.Lock()
			s.cond.Broadcast()
			s.cond.L.Unlock()
		}
		if err != nil {
			// Handover pause: a read deadline unblocked us — exit WITHOUT
			// finishing; the session lives on (in the adopting daemon, or
			// here again after a rollback ResumeReader).
			s.mu.Lock()
			paused := s.paused
			s.mu.Unlock()
			if paused && errors.Is(err, os.ErrDeadlineExceeded) {
				close(s.readerDone)
				return
			}
			// EIO (Linux) / EOF (macOS) when the child side closes — the
			// normal end of a PTY stream, not an operational error.
			break
		}
	}
	close(s.readerDone)
	s.finish()
}

// appendLog writes to the durable log; the first failure disables further
// writes and emits a warning-shaped event (stop-and-warn, per MISSION —
// a broken log must never take down a healthy session).
func (s *Session) appendLog(now time.Time, p []byte) {
	if s.logw == nil || s.logBroken {
		return
	}
	if err := s.logw.Append(now, p); err != nil {
		s.logBroken = true
		s.bus.EmitSession(proto.EventMetaChanged, s.id, map[string]string{
			proto.DataKeyLogBroken: err.Error(),
		})
	}
}

// finish reaps the child, records the exit, and wakes every waiter.
// Runs exactly once, from readPump.
func (s *Session) finish() {
	var err error
	if s.adopted {
		// Not our child: no Wait status. Exit detection (PTY EOF) got us
		// here; the code is honestly unknown (HANDOVER.md).
		err = errcodes.New(errcodes.Unsupported, "adopted session: wait status unavailable")
	} else {
		err = s.cmd.Wait()
	}
	// ptmx.Close happens BELOW the state flip: Info() touches ptmx.Fd()
	// only while state==running under s.mu, so closing before the guarded
	// transition raced Info's fd use (caught by -race via task-m2-fgproc).

	code := 0
	sig := ""
	if err != nil {
		code = -1
	}
	if ps := procState(s.cmd); ps != nil {
		code = ps.ExitCode() // -1 when signal-killed
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			for name, sysSig := range signalMap {
				if ws.Signal() == sysSig {
					sig = name
					break
				}
			}
			if sig == "" {
				sig = ws.Signal().String()
			}
		}
	}

	s.mu.Lock()
	s.state = proto.StateExited
	s.exitCode = code
	s.exitSig = sig
	s.exitedAt = time.Now().UTC()
	s.writeLockHolder = ""
	logw := s.logw
	s.cond.Broadcast()
	s.mu.Unlock()
	_ = s.ptmx.Close() // reader is done with it; error carries no info here

	if logw != nil {
		// Close error means the log tail may be lost; surface it as an
		// event — the session's own exit still proceeds normally.
		if err := logw.Close(); err != nil {
			s.bus.EmitSession(proto.EventMetaChanged, s.id, map[string]string{proto.DataKeyLogBroken: err.Error()})
		}
	}
	s.bus.EmitExited(s.id, code, sig)
	close(s.done)
}

// procState nil-safely fetches the wait status (adopted sessions have no cmd).
func procState(cmd *exec.Cmd) *os.ProcessState {
	if cmd == nil {
		return nil
	}
	return cmd.ProcessState
}

// ExitedAt returns when the process exited (zero time + false if running).
func (s *Session) ExitedAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != proto.StateExited {
		return time.Time{}, false
	}
	return s.exitedAt, true
}

// Linger reports whether the session outlives its last subscriber.
func (s *Session) Linger() bool { return s.cfg.Linger }

// AddSubscriber / RemoveSubscriber maintain the attach count (daemon calls
// these around subscriber pump lifetimes). RemoveSubscriber returns the
// remaining count so the daemon can apply the linger policy.
func (s *Session) AddSubscriber() int64    { return s.subscribers.Add(1) }
func (s *Session) RemoveSubscriber() int64 { return s.subscribers.Add(-1) }

// Monitor returns the session's activity monitor (for the daemon's ticker).
func (s *Session) Monitor() *Monitor { return s.mon }

// ID returns the session id (ses_…).
func (s *Session) ID() string { return s.id }

// Done closes when the process has exited and been reaped.
func (s *Session) Done() <-chan struct{} { return s.done }

// State returns the current lifecycle state.
func (s *Session) State() proto.SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// ExitCode returns (code, wireSignalName, true) once exited.
func (s *Session) ExitCode() (int, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != proto.StateExited {
		return 0, "", false
	}
	return s.exitCode, s.exitSig, true
}

// ReplayFrom delegates to the ring (see Ring.ReplayFrom for semantics).
func (s *Session) ReplayFrom(since uint64) ([]byte, uint64, bool) {
	return s.ring.ReplayFrom(since)
}

// LastSeq returns the seq after the newest output byte.
func (s *Session) LastSeq() uint64 { return s.ring.LastSeq() }

// RingLen returns how many bytes the ring currently retains (the oldest
// replayable seq is LastSeq()-RingLen()).
func (s *Session) RingLen() int { return s.ring.Len() }

// WaitOutput blocks until the ring advances past afterSeq, the session
// exits, or ctx is done. Returns the new LastSeq and whether the session has
// exited. This is the pull-fanout primitive: subscriber loops call it at
// their own pace (natural per-subscriber backpressure).
func (s *Session) WaitOutput(ctx context.Context, afterSeq uint64) (last uint64, exited bool) {
	// A context waiter goroutine wakes the cond when ctx fires; stop guards
	// it from outliving this call.
	stop := context.AfterFunc(ctx, func() {
		s.cond.L.Lock()
		s.cond.Broadcast()
		s.cond.L.Unlock()
	})
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for s.ring.LastSeq() <= afterSeq && s.state != proto.StateExited && ctx.Err() == nil {
		s.cond.Wait()
	}
	return s.ring.LastSeq(), s.state == proto.StateExited
}

// WriteInput writes keystrokes to the PTY on behalf of clientID, enforcing
// the write lock. Empty holder = unheld: the daemon decides auto-claim
// policy (task-dec-writelock); the session only enforces "held by someone
// else → refused".
func (s *Session) WriteInput(clientID string, p []byte) error {
	s.mu.Lock()
	if s.state == proto.StateExited {
		s.mu.Unlock()
		return errcodes.Newf(errcodes.SessionExited, "session %s: input after exit", s.id)
	}
	if s.writeLockHolder != "" && s.writeLockHolder != clientID {
		holder := s.writeLockHolder
		s.mu.Unlock()
		return errcodes.Newf(errcodes.NoWriteLock, "session %s: write lock held by %s", s.id, holder).
			WithHint("TAKE-WRITE to take over")
	}
	ptmx := s.ptmx
	s.bytesIn += uint64(len(p))
	s.mu.Unlock()

	if _, err := ptmx.Write(p); err != nil {
		return fmt.Errorf("session %s: pty write: %w", s.id, err)
	}
	return nil
}

// InputEOF sends the VEOF character (^D). A PTY has no half-close; this is
// the terminal-correct EOF signal for the foreground reader.
func (s *Session) InputEOF(clientID string) error {
	return s.WriteInput(clientID, []byte{veof})
}

// TakeWrite claims the write lock for clientID (stealing is allowed —
// explicit takeover is the agent↔human handoff).
func (s *Session) TakeWrite(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLockHolder = clientID
}

// ReleaseWrite releases the lock if clientID holds it (idempotent).
func (s *Session) ReleaseWrite(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeLockHolder == clientID {
		s.writeLockHolder = ""
	}
}

// WriteLockHolder returns the holding client id ("" = unheld).
func (s *Session) WriteLockHolder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLockHolder
}

// Resize applies the grid size (last-writer-wins; min-of-viewers is client
// policy per MISSION).
func (s *Session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errcodes.Newf(errcodes.InvalidInput, "session %s: resize to zero cols/rows", s.id)
	}
	s.mu.Lock()
	if s.state == proto.StateExited {
		s.mu.Unlock()
		return errcodes.Newf(errcodes.SessionExited, "session %s: resize after exit", s.id)
	}
	ptmx := s.ptmx
	s.cols, s.rows = cols, rows
	s.mu.Unlock()

	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		return fmt.Errorf("session %s: setsize: %w", s.id, err)
	}
	s.bus.EmitSession(proto.EventResized, s.id, map[string]string{
		proto.DataKeyCols: itoa16(cols), proto.DataKeyRows: itoa16(rows),
	})
	return nil
}

// itoa16 formats a grid dimension for event data.
func itoa16(v uint16) string { return fmt.Sprintf("%d", v) }

// Kill signals the whole process group (kill-tree). signalName "" defaults
// to TERM. Unknown names are E_INVALID_INPUT — the wire set is closed.
func (s *Session) Kill(signalName string) error {
	if signalName == "" {
		signalName = proto.SignalTERM
	}
	sig, ok := signalMap[signalName]
	if !ok {
		return errcodes.Newf(errcodes.InvalidInput, "session %s: unknown signal %q", s.id, signalName)
	}
	s.mu.Lock()
	if s.state == proto.StateExited {
		s.mu.Unlock()
		return nil // already dead — kill is idempotent
	}
	pid := s.pid
	s.mu.Unlock()

	// Negative pid = the process group (child is a session leader, pgid==pid).
	err := syscall.Kill(-pid, sig)
	if err == syscall.EPERM {
		// macOS quirk: signaling a process group whose leader is already a
		// zombie (killed but not yet reaped by our Wait) returns EPERM, not
		// ESRCH. A concurrent second KILL hits exactly this window. Fall
		// back to the direct pid — if that says ESRCH/EPERM too, the
		// process is dead or dying and kill's idempotency contract holds.
		err = syscall.Kill(pid, sig)
	}
	if err != nil && err != syscall.ESRCH && err != syscall.EPERM {
		return fmt.Errorf("session %s: kill pgid %d: %w", s.id, pid, err)
	}
	return nil
}

// Rename sets the human name. Uniqueness is the Registry's job — sessions
// are renamed only through Registry.Rename.
func (s *Session) setName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

// Name returns the human name ("" if unnamed).
func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// SetMeta replaces the client-owned KV map wholesale (daemon never merges).
func (s *Session) SetMeta(meta map[string]string) {
	s.mu.Lock()
	s.meta = meta
	s.mu.Unlock()
	s.bus.EmitSession(proto.EventMetaChanged, s.id, nil)
}

// Info snapshots the session for LIST_OK / INFO_OK.
func (s *Session) Info() proto.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Live introspection while running: foreground process via tcgetpgrp on
	// the master, live cwd where the platform exposes it (Linux /proc; the
	// spawn cwd is the macOS fallback — see procinfo_darwin.go).
	cwd := s.cfg.Cwd
	var fgPid int
	var fgComm string
	if s.state == proto.StateRunning {
		if live, ok := liveCwd(s.pid); ok {
			cwd = live
		}
		if pid, comm, ok := fgProcess(int(s.ptmx.Fd())); ok {
			fgPid, fgComm = pid, comm
		}
	}
	info := proto.SessionInfo{
		ID:              s.id,
		Name:            s.name,
		State:           s.state,
		Pid:             s.pid,
		Cmd:             s.cfg.Cmd,
		Args:            s.cfg.Args,
		Cwd:             cwd,
		Cols:            s.cols,
		Rows:            s.rows,
		StartedAtMs:     s.startedAt.UnixMilli(),
		LastSeq:         s.ring.LastSeq(),
		BytesOut:        s.ring.LastSeq(), // seq axis == total bytes out
		BytesIn:         s.bytesIn,
		Subscribers:     int(s.subscribers.Load()),
		WriteLockHolder: s.writeLockHolder,
		Meta:            s.meta,
		LogPath:         s.cfg.LogPath,
		FgPid:           fgPid,
		FgComm:          fgComm,
	}
	if s.state == proto.StateExited {
		code := s.exitCode
		at := s.exitedAt.UnixMilli()
		info.ExitCode = &code
		info.ExitedAtMs = &at
	}
	return info
}

// mintSessionID mints a new ses_… id (exposed for the Registry).
func mintSessionID() (string, error) {
	return ids.New(ids.PrefixSession)
}
