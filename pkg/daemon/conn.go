// conn.go — one client connection: handshake gate, frame dispatch, and the
// per-attachment output pumps.
//
// Threading: one decode loop per connection (this file's handle method);
// one pump goroutine per attached session; one pump for the event
// subscription. All frames out share the connection's proto.Encoder, whose
// mutex guarantees frames never interleave. Disconnect cleanup releases
// every subscription and write lock the connection held.
package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/host"
	"github.com/thesatellite-ai/runbaypty/pkg/ids"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// outputChunkBytes caps one OUTPUT frame's payload. Replays of a large ring
// split into chunks so a single frame never approaches MaxFrameLen and the
// receiver can render progressively.
const outputChunkBytes = 64 * 1024

// conn is one connected client.
type conn struct {
	srv      *Server
	netConn  net.Conn
	enc      *proto.Encoder
	dec      *proto.Decoder
	clientID string

	// needsToken marks WS connections: HELLO must carry a valid token
	// (UDS connections skip this — file permissions are their auth).
	needsToken bool
	// scope is what this connection may do (control on UDS; token-derived
	// on WS). Guarded by mu after the handshake sets it.
	scope connScope

	mu        sync.Mutex
	helloDone bool
	// attachments maps sessionID → the pump's cancel + readOnly flag.
	attachments map[string]*attachment
	// eventCancel tears down the event pump (nil = not subscribed).
	eventCancel func()
	// watches maps watch_id → its scanner registration.
	watches   map[string]*watchReg
	closeOnce sync.Once
}

// attachment is one session subscription on this connection.
type attachment struct {
	cancel   context.CancelFunc
	readOnly bool
	// pumpDone closes when the pump goroutine has fully exited.
	pumpDone chan struct{}
}

func newConn(s *Server, nc net.Conn) *conn {
	return &conn{
		srv:         s,
		netConn:     nc,
		enc:         proto.NewEncoder(nc),
		dec:         proto.NewDecoder(nc),
		clientID:    ids.MustNew(ids.PrefixClient),
		attachments: make(map[string]*attachment),
		watches:     make(map[string]*watchReg),
	}
}

// close tears the connection down (idempotent; safe from any goroutine).
func (c *conn) close() {
	c.closeOnce.Do(func() { _ = c.netConn.Close() })
}

// handle runs the decode loop until the peer disconnects or errs, then
// cleans up everything the connection owned.
func (c *conn) handle() {
	defer c.cleanup()
	for {
		f, err := c.dec.Read()
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrUnexpectedEOF) {
				c.srv.log.Warn("conn read", "client", c.clientID, "err", err)
				// Protocol-level decode errors get a parting ERROR frame.
				var cli *errcodes.CLIError
				if errors.As(err, &cli) {
					_ = c.sendErr("", cli)
				}
			}
			return
		}
		if !c.handshaken() && f.Type != proto.TypeHello {
			_ = c.sendErr("", errcodes.Newf(errcodes.HandshakeFirst, "%s before HELLO", f.Type))
			return
		}
		if !c.dispatch(f) {
			return
		}
	}
}

// cleanup releases every resource the connection held: pumps, event
// subscription, write locks. Runs exactly once, from handle's defer.
func (c *conn) cleanup() {
	c.close()
	c.srv.log.Debug("conn cleanup start", "client", c.clientID)

	c.mu.Lock()
	atts := c.attachments
	c.attachments = make(map[string]*attachment)
	eventCancel := c.eventCancel
	c.eventCancel = nil
	c.mu.Unlock()

	for sessionID, att := range atts {
		att.cancel()
		c.srv.log.Debug("conn cleanup: waiting for pump", "client", c.clientID, "session", sessionID)
		<-att.pumpDone
		c.releaseAttachment(sessionID)
	}
	if eventCancel != nil {
		eventCancel()
	}
	c.cancelWatches()
	// Release any write locks this client still holds (auto-release on
	// disconnect — a dead client must never wedge a session's input).
	for _, sess := range c.srv.reg.List() {
		holder := sess.WriteLockHolder()
		c.srv.log.Debug("cleanup lock scan", "client", c.clientID, "session", sess.ID(), "holder", holder)
		if holder == c.clientID {
			sess.ReleaseWrite(c.clientID)
		}
	}
	c.srv.log.Debug("conn cleanup done", "client", c.clientID)
	c.srv.dropConn(c)
}

// releaseAttachment decrements the subscriber count and applies the linger
// policy: a linger:false session dies with its last subscriber.
func (c *conn) releaseAttachment(sessionID string) {
	sess, err := c.srv.reg.Lookup(sessionID)
	if err != nil {
		return // already reaped
	}
	remaining := sess.RemoveSubscriber()
	c.srv.reg.Events().EmitSession(proto.EventDetached, sessionID, map[string]string{proto.DataKeyClient: c.clientID})
	if remaining == 0 && !sess.Linger() && sess.State() == proto.StateRunning {
		c.srv.log.Info("linger=false: killing session on last detach", "session", sessionID)
		_ = sess.Kill(proto.SignalTERM)
	}
}

func (c *conn) handshaken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.helloDone
}

// dispatch routes one frame. Returns false when the connection must close
// (fatal protocol errors). Per-request failures send ERROR and keep going.
func (c *conn) dispatch(f proto.Frame) bool {
	if c.scopeReadOnly() && controlOnlyTypes[f.Type] {
		// Read-only scope: refuse control verbs with the reqId echoed so
		// the client's pending request settles instead of timing out.
		var probe struct {
			ReqID string `json:"req_id"`
		}
		_ = f.DecodeHeader(&probe)
		_ = c.sendErr(probe.ReqID, errcodes.Newf(errcodes.ReadOnlyScope, "%s requires the control token", f.Type))
		return true
	}
	switch f.Type {
	case proto.TypeHello:
		return c.onHello(f)
	case proto.TypeSpawn:
		c.onSpawn(f)
	case proto.TypeInput:
		c.onInput(f)
	case proto.TypeInputEOF:
		c.onInputEOF(f)
	case proto.TypeResize:
		c.onResize(f)
	case proto.TypeKill:
		c.onKill(f)
	case proto.TypeList:
		c.onList(f)
	case proto.TypeInfo:
		c.onInfo(f)
	case proto.TypeAttach:
		c.onAttach(f)
	case proto.TypeDetach:
		c.onDetach(f)
	case proto.TypeRename:
		c.onRename(f)
	case proto.TypeSetMeta:
		c.onSetMeta(f)
	case proto.TypeTakeWrite:
		c.onTakeWrite(f)
	case proto.TypeReleaseWrite:
		c.onReleaseWrite(f)
	case proto.TypeSubEvents:
		c.onSubscribeEvents(f)
	case proto.TypeReplayCommand:
		c.onReplayCommand(f)
	case proto.TypeHandoverReq:
		c.onHandoverReq(f)
	case proto.TypeWatch:
		c.onWatch(f)
	default:
		// Unknown or server-push types from a client: skip with a warning —
		// additive forward-compat, never a connection kill.
		c.srv.log.Warn("skipping unexpected frame", "client", c.clientID, "type", f.Type.String())
	}
	return true
}

// controlOnlyTypes are refused on read-only-scoped connections. LIST /
// INFO / ATTACH / DETACH / SUBSCRIBE_EVENTS stay allowed — that's the
// share-a-view-of-my-session feature.
var controlOnlyTypes = map[proto.FrameType]bool{
	proto.TypeHandoverReq:  true,
	proto.TypeSpawn:        true,
	proto.TypeInput:        true,
	proto.TypeInputEOF:     true,
	proto.TypeResize:       true,
	proto.TypeKill:         true,
	proto.TypeRename:       true,
	proto.TypeSetMeta:      true,
	proto.TypeTakeWrite:    true,
	proto.TypeReleaseWrite: true,
}

func (c *conn) scopeReadOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scope == scopeReadOnly
}

// ── frame senders ───────────────────────────────────────────────────

func (c *conn) sendErr(reqID string, err error) error {
	msg := proto.ErrorMsg{ReqID: reqID, Code: errcodes.Internal, Message: err.Error()}
	var cli *errcodes.CLIError
	if errors.As(err, &cli) {
		msg.Code = cli.Code
		msg.Message = cli.Message
	}
	return c.enc.WriteMsg(proto.TypeError, msg, nil)
}

func (c *conn) sendOK(reqID string) {
	_ = c.enc.WriteMsg(proto.TypeOK, proto.OK{ReqID: reqID}, nil)
}

// reply sends the typed response or the mapped error for a handler result.
func (c *conn) reply(reqID string, err error) {
	if err != nil {
		_ = c.sendErr(reqID, err)
		return
	}
	c.sendOK(reqID)
}

// ── handlers ────────────────────────────────────────────────────────

func (c *conn) onHello(f proto.Frame) bool {
	var m proto.Hello
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return false
	}
	if m.ProtocolVersion != proto.ProtocolVersion {
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.ProtocolMismatch,
			"client speaks protocol v%d, daemon v%d", m.ProtocolVersion, proto.ProtocolVersion))
		return false
	}
	scope := scopeControl
	if c.needsToken {
		var ok bool
		if scope, ok = c.srv.wsTokens.scopeFor(m.Token); !ok {
			_ = c.sendErr(m.ReqID, errcodes.New(errcodes.BadToken, "token missing or invalid"))
			return false
		}
	}
	c.mu.Lock()
	c.helloDone = true
	c.scope = scope
	c.mu.Unlock()
	_ = c.enc.WriteMsg(proto.TypeHelloAck, proto.HelloAck{
		ReqID:           m.ReqID,
		ProtocolVersion: proto.ProtocolVersion,
		DaemonVersion:   c.srv.opts.Version,
		ClientID:        c.clientID,
	}, nil)
	return true
}

func (c *conn) onSpawn(f proto.Frame) {
	var m proto.Spawn
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	ringBytes := min(m.RingBytes, constants.MaxRingBytes)
	linger := true
	if m.Linger != nil {
		linger = *m.Linger
	}
	sess, err := c.srv.reg.Spawn(host.SpawnConfig{
		Cmd:       m.Cmd,
		Args:      m.Args,
		Cwd:       m.Cwd,
		Env:       m.Env,
		Cols:      m.Cols,
		Rows:      m.Rows,
		Name:      m.Name,
		Meta:      m.Meta,
		RingBytes: ringBytes,
		LogPath:   m.LogPath,
		Linger:    linger,
	})
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	info := sess.Info()
	_ = c.enc.WriteMsg(proto.TypeSpawnOK, proto.SpawnOK{ReqID: m.ReqID, SessionID: sess.ID(), Pid: info.Pid}, nil)
}

// lookupForWrite resolves the session and enforces the read-only attach
// restriction for input-shaped operations.
func (c *conn) lookupForWrite(reqID, sessionID string) (*host.Session, bool) {
	sess, err := c.srv.reg.Lookup(sessionID)
	if err != nil {
		_ = c.sendErr(reqID, err)
		return nil, false
	}
	c.mu.Lock()
	att, attached := c.attachments[sess.ID()]
	c.mu.Unlock()
	if attached && att.readOnly {
		_ = c.sendErr(reqID, errcodes.Newf(errcodes.ReadOnlyAttach, "attached read-only to %s", sess.ID()))
		return nil, false
	}
	return sess, true
}

func (c *conn) onInput(f proto.Frame) {
	var m proto.InputHeader
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, ok := c.lookupForWrite(m.ReqID, m.SessionID)
	if !ok {
		return
	}
	// No per-keystroke ack (too chatty); ERROR only on refusal.
	if err := sess.WriteInput(c.clientID, f.Payload); err != nil {
		_ = c.sendErr(m.ReqID, err)
	}
}

func (c *conn) onInputEOF(f proto.Frame) {
	var m proto.InputEOF
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, ok := c.lookupForWrite(m.ReqID, m.SessionID)
	if !ok {
		return
	}
	c.reply(m.ReqID, sess.InputEOF(c.clientID))
}

func (c *conn) onResize(f proto.Frame) {
	var m proto.Resize
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	c.reply(m.ReqID, sess.Resize(m.Cols, m.Rows))
}

func (c *conn) onKill(f proto.Frame) {
	var m proto.Kill
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	c.reply(m.ReqID, sess.Kill(m.Signal))
}

func (c *conn) onList(f proto.Frame) {
	var m proto.List
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sessions := c.srv.reg.List()
	infos := make([]proto.SessionInfo, len(sessions))
	for i, sess := range sessions {
		infos[i] = sess.Info()
	}
	_ = c.enc.WriteMsg(proto.TypeListOK, proto.ListOK{ReqID: m.ReqID, Sessions: infos}, nil)
}

func (c *conn) onInfo(f proto.Frame) {
	var m proto.Info
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	_ = c.enc.WriteMsg(proto.TypeInfoOK, proto.InfoOK{ReqID: m.ReqID, Session: sess.Info()}, nil)
}

func (c *conn) onAttach(f proto.Frame) {
	var m proto.Attach
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}

	if c.scopeReadOnly() {
		m.ReadOnly = true // a read-only credential can never yield a writable attach
	}
	c.mu.Lock()
	if _, dup := c.attachments[sess.ID()]; dup {
		c.mu.Unlock()
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.InvalidInput, "already attached to %s", sess.ID()))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	att := &attachment{cancel: cancel, readOnly: m.ReadOnly, pumpDone: make(chan struct{})}
	c.attachments[sess.ID()] = att
	c.mu.Unlock()

	sess.AddSubscriber()
	c.srv.reg.Events().EmitSession(proto.EventAttached, sess.ID(), map[string]string{proto.DataKeyClient: c.clientID})

	since := uint64(0)
	if m.SinceSeq != nil {
		since = *m.SinceSeq
	}
	// Truncation is knowable now: does the ring still hold `since`?
	oldest := sess.LastSeq() - uint64(min(int(sess.LastSeq()), sess.RingLen()))
	_ = c.enc.WriteMsg(proto.TypeAttachOK, proto.AttachOK{
		ReqID:     m.ReqID,
		SessionID: sess.ID(),
		LastSeq:   sess.LastSeq(),
		Truncated: since < oldest,
	}, nil)

	go c.pumpOutput(ctx, sess, since, att.pumpDone)
}

// pumpOutput streams one session to this connection: replay from `since`,
// then follow live via WaitOutput. Pull-based — a slow connection paces
// itself here without touching the session or other subscribers. Exits on
// ctx cancel (detach/disconnect) or after the session exits and the stream
// is fully drained (then it sends EXIT).
func (c *conn) pumpOutput(ctx context.Context, sess *host.Session, since uint64, done chan<- struct{}) {
	defer close(done)
	lastSeen := since
	for {
		data, fromSeq, truncated := sess.ReplayFrom(lastSeen)
		if truncated && lastSeen > 0 {
			// This subscriber fell behind the ring window (or asked for
			// history the ring no longer holds). Tell it, then continue
			// from the oldest retained byte — the durable log has the rest.
			_ = c.sendErr("", errcodes.Newf(errcodes.RingGone,
				"session %s: bytes %d–%d fell out of the ring", sess.ID(), lastSeen, fromSeq))
		}
		for off := 0; off < len(data); off += outputChunkBytes {
			end := min(off+outputChunkBytes, len(data))
			if err := c.enc.WriteMsg(proto.TypeOutput, proto.OutputHeader{
				SessionID: sess.ID(),
				Seq:       fromSeq + uint64(off),
			}, data[off:end]); err != nil {
				return // connection gone; cleanup handles the rest
			}
		}
		if len(data) > 0 {
			lastSeen = fromSeq + uint64(len(data))
		}

		last, exited := sess.WaitOutput(ctx, lastSeen)
		if ctx.Err() != nil {
			return
		}
		if exited && last == lastSeen {
			code, sig, _ := sess.ExitCode()
			_ = c.enc.WriteMsg(proto.TypeExit, proto.Exit{SessionID: sess.ID(), ExitCode: code, Signal: sig}, nil)
			return
		}
	}
}

func (c *conn) onDetach(f proto.Frame) {
	var m proto.Detach
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	c.mu.Lock()
	att, ok := c.attachments[sess.ID()]
	delete(c.attachments, sess.ID())
	c.mu.Unlock()
	if !ok {
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.NotAttached, "not attached to %s", sess.ID()))
		return
	}
	att.cancel()
	<-att.pumpDone
	c.releaseAttachment(sess.ID())
	c.sendOK(m.ReqID)
}

func (c *conn) onRename(f proto.Frame) {
	var m proto.Rename
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	c.reply(m.ReqID, c.srv.reg.Rename(m.SessionID, m.Name))
}

func (c *conn) onSetMeta(f proto.Frame) {
	var m proto.SetMeta
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	sess.SetMeta(m.Meta)
	c.sendOK(m.ReqID)
}

func (c *conn) onTakeWrite(f proto.Frame) {
	var m proto.TakeWrite
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, ok := c.lookupForWrite(m.ReqID, m.SessionID)
	if !ok {
		return
	}
	sess.TakeWrite(c.clientID)
	c.srv.log.Debug("write lock taken", "client", c.clientID, "session", sess.ID())
	c.sendOK(m.ReqID)
}

func (c *conn) onReleaseWrite(f proto.Frame) {
	var m proto.ReleaseWrite
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	sess.ReleaseWrite(c.clientID)
	c.sendOK(m.ReqID)
}

// onReplayCommand returns the last OSC 133-marked command's output window.
func (c *conn) onReplayCommand(f proto.Frame) {
	var m proto.ReplayCommand
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}
	start, end, ok := sess.Monitor().LastCommand()
	if !ok {
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.NotFound, "session %s has no completed OSC 133 command", sess.ID()).
			WithHint("the session's shell must emit OSC 133 marks (shell integration)"))
		return
	}
	data, fromSeq, truncated := sess.ReplayFrom(start)
	if end > fromSeq+uint64(len(data)) {
		end = fromSeq + uint64(len(data)) // window tail beyond ring end: clamp
	}
	if uint64(len(data)) > end-fromSeq {
		data = data[:end-fromSeq]
	}
	_ = c.enc.WriteMsg(proto.TypeReplayCommandOK, proto.ReplayCommandOK{
		ReqID: m.ReqID, SessionID: sess.ID(), StartSeq: fromSeq, EndSeq: end, Truncated: truncated,
	}, data)
}

func (c *conn) onSubscribeEvents(f proto.Frame) {
	var m proto.SubscribeEvents
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	c.mu.Lock()
	if c.eventCancel != nil {
		c.mu.Unlock()
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.InvalidInput, "already subscribed to events"))
		return
	}
	subID, ch := c.srv.reg.Events().Subscribe(m.SessionID)
	pumpDone := make(chan struct{})
	c.eventCancel = func() {
		c.srv.reg.Events().Unsubscribe(subID) // closes ch → pump drains out
		<-pumpDone
	}
	c.mu.Unlock()

	c.sendOK(m.ReqID)
	go func() {
		defer close(pumpDone)
		for ev := range ch {
			if err := c.enc.WriteMsg(proto.TypeEvent, ev, nil); err != nil {
				// Connection gone: drain until Unsubscribe closes the
				// channel so the bus never blocks on us.
				for range ch { //nolint:revive // deliberate drain
				}
				return
			}
		}
		if dropped := c.srv.reg.Events().Dropped(subID); dropped > 0 {
			c.srv.log.Warn("event subscriber dropped events", "client", c.clientID, "dropped", strconv.FormatUint(dropped, 10))
		}
	}()
}
