// takeover.go — zero-downtime daemon upgrade (HANDOVER.md).
//
// The NEW daemon (`serve --takeover`) asks the OLD daemon to hand over: the
// old one freezes readers, unlinks its socket, and streams every session's
// state + ptmx fd (SCM_RIGHTS) plus the flock fd over a private unix
// connection; the new daemon rebuilds and ACKs; the old exits without
// touching a single child process.
//
// The private channel speaks a deliberately tiny internal protocol (NOT the
// public wire protocol — it lives and dies inside one machine, same
// version on both ends is guaranteed by the flow):
//
//	ctrl byte 'S' (+ oob ptmx fd)  session follows (live)
//	ctrl byte 'X'                  session follows (exited-retained, no fd)
//	blob stateJSON · blob ringData
//	ctrl byte 'L' (+ oob lock fd)  finale: blob finJSON{count}
//	reply byte 'A'                 new daemon ACKs; old exits
//
// Blobs are u32-length-prefixed (rings exceed the public 1 MiB frame cap).
package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/host"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// takeover ctrl bytes (closed set; see package doc).
const (
	ctrlSessionLive   = 'S'
	ctrlSessionExited = 'X'
	ctrlFinale        = 'L'
	ctrlAck           = 'A'
)

// takeoverTimeout bounds each side's patience; past it the old daemon
// rolls back and the new one gives up. takeoverDialTimeout is the shorter
// bound for the initial "is anyone there" dial.
const (
	takeoverTimeout     = 15 * time.Second
	takeoverDialTimeout = 2 * time.Second
)

// Reply-channel filesystem names. The dir lives under shortTmpBase (NOT the
// home dir: sun_path caps ~104 bytes and home paths routinely exceed it).
const (
	shortTmpBase    = "/tmp"
	replyDirPattern = "rptyto-*"
	replySockName   = "t.sock"
)

// maxBlob sanity-caps a blob (a ring is ≤ MaxRingBytes; state JSON is tiny).
const maxBlob = 128 << 20

// handoverFin is the finale blob.
type handoverFin struct {
	Count    int    `json:"count"`
	LockPath string `json:"lock_path"`
}

// AdoptedSession is one received session awaiting host.AdoptSession.
type AdoptedSession struct {
	State host.HandoverState
	Ring  []byte
	Ptmx  *os.File // nil for exited-retained sessions
}

// Adopted is everything RequestTakeover received.
type Adopted struct {
	Sessions []AdoptedSession
	LockPath string
	LockFile *os.File
}

// ── blob + fd primitives ────────────────────────────────────────────

func writeBlob(c net.Conn, data []byte) error {
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(data)))
	if _, err := c.Write(head[:]); err != nil {
		return err
	}
	_, err := c.Write(data)
	return err
}

func readBlob(c net.Conn) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > maxBlob {
		return nil, fmt.Errorf("takeover: blob %d exceeds sanity cap", n)
	}
	data := make([]byte, n)
	_, err := io.ReadFull(c, data)
	return data, err
}

// writeCtrl sends one ctrl byte, attaching an fd via SCM_RIGHTS when given.
func writeCtrl(c *net.UnixConn, ctrl byte, fd int) error {
	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	_, _, err := c.WriteMsgUnix([]byte{ctrl}, oob, nil)
	return err
}

// readCtrl receives one ctrl byte and at most one fd.
func readCtrl(c *net.UnixConn) (ctrl byte, file *os.File, err error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return 0, nil, err
	}
	if n != 1 {
		return 0, nil, fmt.Errorf("takeover: ctrl read %d bytes", n)
	}
	if oobn > 0 {
		msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
		if perr == nil && len(msgs) > 0 {
			if fds, perr := unix.ParseUnixRights(&msgs[0]); perr == nil && len(fds) > 0 {
				_ = unix.SetNonblock(fds[0], true) // pollability for the adopting daemon
				file = os.NewFile(uintptr(fds[0]), "takeover-fd")
			}
		}
	}
	return buf[0], file, nil
}

// ── old-daemon side ─────────────────────────────────────────────────

// onHandoverReq handles HANDOVER_REQ: UDS-only (a token-authed WS peer is
// still remote-ish in spirit — the requester must be the same-UID local
// binary), then the handover proceeds asynchronously.
func (c *conn) onHandoverReq(f proto.Frame) {
	var m proto.HandoverReq
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	if c.needsToken {
		_ = c.sendErr(m.ReqID, errcodes.New(errcodes.Unsupported, "takeover only over the unix socket"))
		return
	}
	c.sendOK(m.ReqID)
	go c.srv.performHandover(m.ReplyPath)
}

// performHandover is the old daemon's exit ritual. Any failure before the
// ACK rolls everything back — sessions never blink either way.
func (s *Server) performHandover(replyPath string) {
	s.log.Info("takeover requested", "reply", replyPath)

	// 1. Stop the world (listener + clients), but keep sessions alive.
	s.mu.Lock()
	s.closed = true
	openConns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		openConns = append(openConns, c)
	}
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close() // also unlinks the socket path (Go unlinks on Close)
	}
	for _, c := range openConns {
		c.close()
	}

	sessions := s.reg.List()
	rollback := func(stage string, err error) {
		s.log.Warn("takeover failed — rolling back", "stage", stage, "err", err)
		for _, sess := range sessions {
			if rerr := sess.ResumeReader(); rerr != nil {
				s.log.Warn("rollback resume", "session", sess.ID(), "err", rerr)
			}
		}
		if rerr := s.relisten(); rerr != nil {
			s.log.Error("rollback re-listen failed — daemon is headless", "err", rerr)
		}
	}

	// 2. Freeze every live reader so the ring snapshots are exact.
	for _, sess := range sessions {
		if err := sess.PauseReader(); err != nil {
			rollback("pause", err)
			return
		}
	}

	// 3. Stream state to the new daemon.
	nc, err := net.DialTimeout(constants.NetworkUnix, replyPath, takeoverTimeout)
	if err != nil {
		rollback("dial", err)
		return
	}
	defer func() { _ = nc.Close() }()
	_ = nc.SetDeadline(time.Now().Add(takeoverTimeout))
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		// net.Dial("unix") always yields *net.UnixConn; anything else means
		// the transport changed under us — fail the handover, not the daemon.
		rollback("conn type", fmt.Errorf("dial returned %T, need *net.UnixConn for SCM_RIGHTS", nc))
		return
	}

	for _, sess := range sessions {
		state, ring, ptmx := sess.HandoverState()
		ctrl := byte(ctrlSessionExited)
		fd := -1
		if sess.State() == proto.StateRunning && ptmx != nil {
			ctrl = ctrlSessionLive
			fd = int(ptmx.Fd())
		}
		stateJSON, err := json.Marshal(state)
		if err == nil {
			err = writeCtrl(uc, ctrl, fd)
		}
		if err == nil {
			err = writeBlob(nc, stateJSON)
		}
		if err == nil {
			err = writeBlob(nc, ring)
		}
		if err != nil {
			rollback("stream "+sess.ID(), err)
			return
		}
	}

	// 4. Finale: the flock fd — the lock transfers WITH the fd, so there is
	// never a moment where no one holds it.
	fin := handoverFin{Count: len(sessions), LockPath: s.lock.File().Name()}
	finJSON, _ := json.Marshal(fin)
	if err := writeCtrl(uc, ctrlFinale, int(s.lock.File().Fd())); err != nil {
		rollback("lock fd", err)
		return
	}
	if err := writeBlob(nc, finJSON); err != nil {
		rollback("fin", err)
		return
	}

	// 5. Wait for the ACK, then leave without touching the children.
	ack := make([]byte, 1)
	if _, err := io.ReadFull(nc, ack); err != nil || ack[0] != ctrlAck {
		rollback("ack", fmt.Errorf("ack read: %v (byte %v)", err, ack))
		return
	}
	s.log.Info("takeover complete — this daemon retires", "sessions", len(sessions))
	close(s.handedOver) // Serve returns via the handover path (no session kill)
}

// relisten restores the UDS listener after a failed takeover.
func (s *Server) relisten() error {
	ln, err := net.Listen(constants.NetworkUnix, s.opts.SocketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.opts.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.mu.Lock()
	s.closed = false
	s.ln = ln
	s.mu.Unlock()
	go s.acceptLoop(ln)
	return nil
}

// ── new-daemon side ─────────────────────────────────────────────────

// RequestTakeover asks the daemon at oldSock to hand everything over.
// Returns the adopted state for Options.Adopted. The caller becomes the
// daemon; on error, the old daemon (if any) keeps running — safe to fall
// back to a normal boot only when the error is DaemonUnreachable.
func RequestTakeover(oldSock, homeDir string) (*Adopted, error) {
	// Private reply listener. Short /tmp dir, NOT the home dir: sun_path
	// caps at ~104 bytes and home dirs (or test tempdirs) routinely exceed
	// it. Mode 0700 dir = same-UID-only, matching the UDS auth model.
	replyDir, err := os.MkdirTemp(shortTmpBase, replyDirPattern)
	if err != nil {
		return nil, fmt.Errorf("takeover: reply dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(replyDir) }()
	_ = homeDir // reserved for future policy (e.g. same-home assertion)
	replyPath := replyDir + string(os.PathSeparator) + replySockName
	replyLn, err := net.Listen(constants.NetworkUnix, replyPath)
	if err != nil {
		return nil, fmt.Errorf("takeover: reply listener: %w", err)
	}
	defer func() { _ = replyLn.Close() }()

	// Ask over the public socket.
	nc, err := net.DialTimeout(constants.NetworkUnix, oldSock, takeoverDialTimeout)
	if err != nil {
		return nil, errcodes.Newf(errcodes.DaemonUnreachable, "no daemon to take over at %s: %v", oldSock, err).WithCause(err)
	}
	defer func() { _ = nc.Close() }()
	enc := proto.NewEncoder(nc)
	dec := proto.NewDecoder(nc)
	if err := enc.WriteMsg(proto.TypeHello, proto.Hello{ReqID: "t1", ProtocolVersion: proto.ProtocolVersion, ClientName: "takeover"}, nil); err != nil {
		return nil, err
	}
	if f, err := dec.Read(); err != nil || f.Type != proto.TypeHelloAck {
		return nil, fmt.Errorf("takeover: handshake with old daemon failed (%v, %v)", f.Type, err)
	}
	if err := enc.WriteMsg(proto.TypeHandoverReq, proto.HandoverReq{ReqID: "t2", ReplyPath: replyPath}, nil); err != nil {
		return nil, err
	}
	if f, err := dec.Read(); err != nil || f.Type != proto.TypeOK {
		return nil, fmt.Errorf("takeover: old daemon refused (%v, %v)", f.Type, err)
	}

	// Receive the state stream on the private channel.
	type deadliner interface{ SetDeadline(time.Time) error }
	if d, ok := replyLn.(deadliner); ok {
		_ = d.SetDeadline(time.Now().Add(takeoverTimeout))
	}
	rc, err := replyLn.Accept()
	if err != nil {
		return nil, fmt.Errorf("takeover: old daemon never called back: %w", err)
	}
	defer func() { _ = rc.Close() }()
	_ = rc.SetDeadline(time.Now().Add(takeoverTimeout))
	uc, ok := rc.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("takeover: accepted %T, need *net.UnixConn for SCM_RIGHTS", rc)
	}

	out := &Adopted{}
	for {
		ctrl, file, err := readCtrl(uc)
		if err != nil {
			return nil, fmt.Errorf("takeover: ctrl: %w", err)
		}
		if ctrl == ctrlFinale {
			finJSON, err := readBlob(rc)
			if err != nil {
				return nil, fmt.Errorf("takeover: fin: %w", err)
			}
			var fin handoverFin
			if err := json.Unmarshal(finJSON, &fin); err != nil {
				return nil, err
			}
			if fin.Count != len(out.Sessions) {
				return nil, fmt.Errorf("takeover: received %d sessions, old daemon sent %d", len(out.Sessions), fin.Count)
			}
			out.LockFile = file
			out.LockPath = fin.LockPath
			// ACK: the point of no return for the old daemon.
			if _, err := rc.Write([]byte{ctrlAck}); err != nil {
				return nil, err
			}
			return out, nil
		}
		stateJSON, err := readBlob(rc)
		if err != nil {
			return nil, fmt.Errorf("takeover: state: %w", err)
		}
		ring, err := readBlob(rc)
		if err != nil {
			return nil, fmt.Errorf("takeover: ring: %w", err)
		}
		var st host.HandoverState
		if err := json.Unmarshal(stateJSON, &st); err != nil {
			return nil, err
		}
		if ctrl == ctrlSessionLive && file == nil {
			return nil, fmt.Errorf("takeover: live session %s arrived without its fd", st.ID)
		}
		out.Sessions = append(out.Sessions, AdoptedSession{State: st, Ring: ring, Ptmx: file})
	}
}
