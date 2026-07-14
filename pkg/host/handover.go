// handover.go — the engine side of zero-downtime daemon upgrade
// (HANDOVER.md): freeze a session's reader, serialize its state, and
// rebuild it in a new daemon process around the SAME PTY master fd.
package host

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/metajson"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// HandoverState is one session's transferable state. The ptmx fd rides
// alongside via SCM_RIGHTS — fds cannot live in JSON.
type HandoverState struct {
	ID   string   `json:"id"`
	Name string   `json:"name,omitempty"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	Cols uint16   `json:"cols"`
	Rows uint16   `json:"rows"`
	// Meta is the legacy flat projection (top-level string fields), kept so a
	// pre-upgrade daemon on the receiving end still restores tags. MetaDoc is
	// the full JSON meta document; MetaVersion the write counter.
	Meta        map[string]string `json:"meta,omitempty"`
	MetaDoc     []byte            `json:"meta_doc,omitempty"`
	MetaVersion uint64            `json:"meta_version,omitempty"`
	Pid         int               `json:"pid"`
	StartedAtMs int64             `json:"started_at_ms"`
	RingCap     int               `json:"ring_cap"`
	RingEndSeq  uint64            `json:"ring_end_seq"`
	// RingData travels as the handover frame's PAYLOAD, not JSON (binary).
	BytesIn   uint64 `json:"bytes_in"`
	LogPath   string `json:"log_path,omitempty"`
	Linger    bool   `json:"linger"`
	SilenceMs int64  `json:"silence_ms"`
	// StreamPos re-seeds the monitor's OSC scanner axis.
	StreamPos uint64 `json:"stream_pos"`
}

// pollableFile dups fd, flips it to non-blocking, and rewraps via
// os.NewFile — which registers non-blocking fds with the runtime poller,
// enabling SetReadDeadline on PTY masters. The original file is closed by
// the caller; the dup keeps the PTY open.
func pollableFile(f *os.File) (*os.File, error) {
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return nil, fmt.Errorf("dup: %w", err)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("set nonblock: %w", err)
	}
	nf := os.NewFile(uintptr(fd), f.Name())
	_ = f.Close() // dup holds the PTY open from here
	return nf, nil
}

// PauseReader stops the read pump WITHOUT closing the PTY: a read deadline
// unblocks the pending Read, and the pump exits when it sees the pause flag.
// Bytes the child writes while paused wait in the kernel PTY buffer and
// flow to the adopting daemon — ordering is preserved by the fd itself.
func (s *Session) PauseReader() error {
	s.mu.Lock()
	if s.state != proto.StateRunning {
		s.mu.Unlock()
		return nil // exited sessions have no live reader to pause
	}
	s.paused = true
	ptmx := s.ptmx
	s.mu.Unlock()

	if err := ptmx.SetReadDeadline(time.Now()); err != nil {
		return fmt.Errorf("handover: pause %s: %w", s.id, err)
	}
	<-s.readerDone // pump exited (without finish() — see readPump)
	return nil
}

// ResumeReader restarts the pump after a failed handover (rollback path).
func (s *Session) ResumeReader() error {
	s.mu.Lock()
	if s.state != proto.StateRunning || !s.paused {
		s.mu.Unlock()
		return nil
	}
	s.paused = false
	s.readerDone = make(chan struct{})
	ptmx := s.ptmx
	s.mu.Unlock()

	if err := ptmx.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("handover: resume %s: %w", s.id, err)
	}
	go s.readPump()
	return nil
}

// HandoverState snapshots the session for transfer. Call ONLY after
// PauseReader — the ring must be quiescent for the seq axis to be exact.
func (s *Session) HandoverState() (HandoverState, []byte, *os.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ringData, endSeq := s.ring.Snapshot()
	return HandoverState{
		ID: s.id, Name: s.name, Cmd: s.cfg.Cmd, Args: s.cfg.Args, Cwd: s.cfg.Cwd,
		Cols: s.cols, Rows: s.rows,
		Meta: metajson.Project(s.metaDoc), MetaDoc: s.metaDoc, MetaVersion: s.metaVersion,
		Pid:         s.pid,
		StartedAtMs: s.startedAt.UnixMilli(),
		RingCap:     s.ring.capBytes(), RingEndSeq: endSeq,
		BytesIn: s.bytesIn, LogPath: s.cfg.LogPath, Linger: s.cfg.Linger,
		SilenceMs: s.mon.silenceAfter.Milliseconds(), StreamPos: endSeq,
	}, ringData, s.ptmx
}

// AdoptSession rebuilds a session in the NEW daemon around a transferred
// ptmx. The process is NOT our child: exit detection works (EOF on the
// master when the child dies) but the wait status is unavailable — so an
// adopted session's exit reports code -1 (HANDOVER.md; Linux subreaper
// support to recover the real code is a follow-up).
func AdoptSession(st HandoverState, ringData []byte, rawPtmx *os.File, bus *EventBus) *Session {
	// Transferred fds arrive blocking-mode; rewrap pollable so the NEXT
	// handover can pause this daemon's reader too. If that rewrap fails (rare)
	// the session still streams normally; the only degradation is that a FUTURE
	// takeover of THIS session could not pause its reader for a clean snapshot.
	ptmx, err := pollableFile(rawPtmx)
	if err != nil {
		ptmx = rawPtmx
	} else {
		_ = ptmx.SetReadDeadline(time.Time{})
	}
	ringCap := st.RingCap
	if ringCap <= 0 {
		ringCap = constants.DefaultRingBytes
	}
	s := &Session{
		id:   st.ID,
		ring: NewRingFromSnapshot(ringCap, ringData, st.RingEndSeq),
		bus:  bus,
		mon:  NewMonitor(st.ID, bus, time.Duration(st.SilenceMs)*time.Millisecond),
		cfg: SpawnConfig{
			Cmd: st.Cmd, Args: st.Args, Cwd: st.Cwd, Cols: st.Cols, Rows: st.Rows,
			Name: st.Name, Meta: st.Meta, RingBytes: st.RingCap,
			LogPath: st.LogPath, Linger: st.Linger,
		},
		name:        st.Name,
		metaDoc:     adoptMetaDoc(st),
		metaVersion: st.MetaVersion,
		state:       proto.StateRunning,
		ptmx:        ptmx,
		cmd:         nil, // not our child — finish() takes the adopted path
		pid:         st.Pid,
		cols:        st.Cols,
		rows:        st.Rows,
		bytesIn:     st.BytesIn,
		startedAt:   time.UnixMilli(st.StartedAtMs).UTC(),
		adopted:     true,
		done:        make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	s.readerDone = make(chan struct{})
	s.mon.SetStreamPos(st.StreamPos)
	if st.LogPath != "" {
		if logw, err := ReopenSessionLogWriter(st.LogPath); err == nil {
			s.logw = logw
		} else {
			bus.EmitSession(proto.EventMetaChanged, st.ID, map[string]string{proto.DataKeyLogBroken: err.Error()})
		}
	}
	go s.readPump()
	return s
}

// SetStreamPos re-seeds the monitor's absolute stream position (adoption).
func (m *Monitor) SetStreamPos(pos uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamPos = pos
	m.lastOutput = time.Now()
	m.started = true
}

// capBytes returns the ring's capacity (handover state needs it).
func (r *Ring) capBytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.buf)
}
