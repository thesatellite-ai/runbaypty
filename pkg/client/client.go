// Package client is the Go SDK for runbaypty: dial the daemon, spawn and
// control sessions, attach to byte streams with zero-gap resume, and follow
// lifecycle events. The CLI is built on this package; anything that embeds
// terminal sessions can be too.
//
// Concurrency model: one reader goroutine per Client demuxes incoming
// frames — responses are matched to waiting requests by reqID; pushes
// (OUTPUT / EXIT / EVENT) are routed to the owning Stream or event channel.
// All exported methods are safe for concurrent use.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// sdkClientName identifies this SDK in HELLO / daemon logs.
const sdkClientName = "go-sdk"

// dialTimeout bounds the UDS connect (a dead socket fails fast anyway; this
// guards weird filesystem states).
const dialTimeout = 5 * time.Second

// Client is one connection to the daemon.
type Client struct {
	nc  net.Conn
	enc *proto.Encoder

	mu       sync.Mutex
	nextReq  int
	pending  map[string]chan proto.Frame // reqID → response slot (buffered 1)
	streams  map[string]*Stream          // sessionID → active attach stream
	watchers map[string]chan WatchEvent  // watchID → match channel
	events   chan Event                  // nil until SubscribeEvents
	clientID string
	closed   bool
	done     chan struct{}
}

// Event is a lifecycle event with its wire fields re-exported so SDK users
// never import proto.
type Event struct {
	Type      proto.EventType
	SessionID string
	At        time.Time
	Data      map[string]string
}

// Dial connects to the daemon's UDS socket and completes the handshake.
func Dial(socketPath string) (*Client, error) {
	nc, err := net.DialTimeout(constants.NetworkUnix, socketPath, dialTimeout)
	if err != nil {
		return nil, errcodes.Newf(errcodes.DaemonUnreachable, "dial %s: %v", socketPath, err).
			WithHint("is the daemon running? try `runbaypty serve` or `runbaypty daemon status`").
			WithCause(err)
	}
	c := &Client{
		nc:       nc,
		enc:      proto.NewEncoder(nc),
		pending:  make(map[string]chan proto.Frame),
		streams:  make(map[string]*Stream),
		watchers: make(map[string]chan WatchEvent),
		done:     make(chan struct{}),
	}
	go c.readLoop()

	ack := proto.HelloAck{}
	f, err := c.request(context.Background(), proto.TypeHello, func(reqID string) any {
		return proto.Hello{ReqID: reqID, ProtocolVersion: proto.ProtocolVersion, ClientName: sdkClientName}
	}, nil, proto.TypeHelloAck)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := f.DecodeHeader(&ack); err != nil {
		_ = c.Close()
		return nil, err
	}
	c.mu.Lock()
	c.clientID = ack.ClientID
	c.mu.Unlock()
	return c, nil
}

// ClientID returns the daemon-minted cli_… id for this connection.
func (c *Client) ClientID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientID
}

// Close tears the connection down. Active streams end with io.EOF-shaped
// closure; pending requests fail.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.done
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	err := c.nc.Close()
	<-c.done
	return err
}

// readLoop is the demux: exactly one per client, exits when the connection
// dies (its clearly-defined exit condition), then fails everything pending.
func (c *Client) readLoop() {
	defer close(c.done)
	dec := proto.NewDecoder(c.nc)
	for {
		f, err := dec.Read()
		if err != nil {
			c.failAll(err)
			return
		}
		switch f.Type {
		case proto.TypeOutput:
			var h proto.OutputHeader
			if e := f.DecodeHeader(&h); e == nil {
				c.routeOutput(h, f.Payload)
			}
		case proto.TypeExit:
			var ex proto.Exit
			if e := f.DecodeHeader(&ex); e == nil {
				c.routeExit(ex)
			}
		case proto.TypeWatchEvent:
			var we proto.WatchEvent
			if e := f.DecodeHeader(&we); e == nil {
				c.routeWatchEvent(we)
			}
		case proto.TypeEvent:
			var ev proto.Event
			if e := f.DecodeHeader(&ev); e == nil {
				c.routeEvent(ev)
			}
		case proto.TypeError:
			var em proto.ErrorMsg
			if e := f.DecodeHeader(&em); e != nil {
				continue
			}
			if em.ReqID != "" {
				c.settle(em.ReqID, f)
			} else {
				c.routePushError(em)
			}
		default:
			// Everything else is a response — settle by reqID.
			var probe struct {
				ReqID string `json:"req_id"`
			}
			if e := f.DecodeHeader(&probe); e == nil && probe.ReqID != "" {
				c.settle(probe.ReqID, f)
			}
		}
	}
}

func (c *Client) settle(reqID string, f proto.Frame) {
	c.mu.Lock()
	slot, ok := c.pending[reqID]
	delete(c.pending, reqID)
	c.mu.Unlock()
	if ok {
		slot <- f // buffered 1; never blocks
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan proto.Frame)
	streams := c.streams
	c.streams = make(map[string]*Stream)
	events := c.events
	c.events = nil
	watchers := c.watchers
	c.watchers = make(map[string]chan WatchEvent)
	c.mu.Unlock()
	for _, ch := range watchers {
		close(ch)
	}

	for _, slot := range pending {
		close(slot) // receiver maps closed slot → connection error
	}
	for _, st := range streams {
		st.finish(errcodes.Newf(errcodes.DaemonUnreachable, "connection lost: %v", err))
	}
	if events != nil {
		close(events)
	}
}

// request sends one control message and waits for its response frame.
// build receives the minted reqID (so the message can embed it); wantType
// is the expected response ("OK" flows are wantType TypeOK).
func (c *Client) request(ctx context.Context, t proto.FrameType, build func(reqID string) any, payload []byte, wantType proto.FrameType) (proto.Frame, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return proto.Frame{}, errcodes.New(errcodes.DaemonUnreachable, "client closed")
	}
	c.nextReq++
	reqID := "c" + strconv.Itoa(c.nextReq)
	slot := make(chan proto.Frame, 1)
	c.pending[reqID] = slot
	c.mu.Unlock()

	if err := c.enc.WriteMsg(t, build(reqID), payload); err != nil {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return proto.Frame{}, fmt.Errorf("client: send %s: %w", t, err)
	}

	select {
	case f, ok := <-slot:
		if !ok {
			return proto.Frame{}, errcodes.Newf(errcodes.DaemonUnreachable, "connection lost awaiting %s", t)
		}
		if f.Type == proto.TypeError {
			var em proto.ErrorMsg
			if err := f.DecodeHeader(&em); err != nil {
				return proto.Frame{}, err
			}
			return proto.Frame{}, errcodes.New(em.Code, em.Message)
		}
		if f.Type != wantType {
			return proto.Frame{}, errcodes.Newf(errcodes.BadFrame, "got %s response, want %s", f.Type, wantType)
		}
		return f, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return proto.Frame{}, ctx.Err()
	}
}

// ── control-plane methods ───────────────────────────────────────────

// SpawnOpts mirrors proto.Spawn minus the wire plumbing.
type SpawnOpts struct {
	Cmd        string
	Args       []string
	Cwd        string
	Env        []string
	Cols, Rows uint16
	Name       string
	// Meta is the legacy flat KV; it seeds top-level string fields of the JSON
	// meta document. Prefer Annotations for structured data.
	Meta map[string]string
	// Annotations seeds the session's JSON meta document (arbitrary JSON
	// object). Merge/replace after spawn with SetMetaJSON.
	Annotations json.RawMessage
	RingBytes   int
	LogPath     string
	NoLinger    bool
}

// Spawn starts a session and returns its id + pid.
func (c *Client) Spawn(ctx context.Context, o SpawnOpts) (sessionID string, pid int, err error) {
	if o.Cols == 0 || o.Rows == 0 {
		o.Cols, o.Rows = 80, 24
	}
	var linger *bool
	if o.NoLinger {
		f := false
		linger = &f
	}
	resp, err := c.request(ctx, proto.TypeSpawn, func(reqID string) any {
		return proto.Spawn{
			ReqID: reqID, Cmd: o.Cmd, Args: o.Args, Cwd: o.Cwd, Env: o.Env,
			Cols: o.Cols, Rows: o.Rows, Name: o.Name, Meta: o.Meta, Annotations: o.Annotations,
			RingBytes: o.RingBytes, LogPath: o.LogPath, Linger: linger,
		}
	}, nil, proto.TypeSpawnOK)
	if err != nil {
		return "", 0, err
	}
	var ok proto.SpawnOK
	if err := resp.DecodeHeader(&ok); err != nil {
		return "", 0, err
	}
	return ok.SessionID, ok.Pid, nil
}

// List returns every session's snapshot.
func (c *Client) List(ctx context.Context) ([]proto.SessionInfo, error) {
	resp, err := c.request(ctx, proto.TypeList, func(reqID string) any {
		return proto.List{ReqID: reqID}
	}, nil, proto.TypeListOK)
	if err != nil {
		return nil, err
	}
	var ok proto.ListOK
	if err := resp.DecodeHeader(&ok); err != nil {
		return nil, err
	}
	return ok.Sessions, nil
}

// Info returns one session's snapshot (id or name).
func (c *Client) Info(ctx context.Context, idOrName string) (proto.SessionInfo, error) {
	resp, err := c.request(ctx, proto.TypeInfo, func(reqID string) any {
		return proto.Info{ReqID: reqID, SessionID: idOrName}
	}, nil, proto.TypeInfoOK)
	if err != nil {
		return proto.SessionInfo{}, err
	}
	var ok proto.InfoOK
	if err := resp.DecodeHeader(&ok); err != nil {
		return proto.SessionInfo{}, err
	}
	return ok.Session, nil
}

// Kill signals the session's process tree ("" = TERM).
func (c *Client) Kill(ctx context.Context, idOrName, signal string) error {
	_, err := c.request(ctx, proto.TypeKill, func(reqID string) any {
		return proto.Kill{ReqID: reqID, SessionID: idOrName, Signal: signal}
	}, nil, proto.TypeOK)
	return err
}

// Resize sets the grid.
func (c *Client) Resize(ctx context.Context, idOrName string, cols, rows uint16) error {
	_, err := c.request(ctx, proto.TypeResize, func(reqID string) any {
		return proto.Resize{ReqID: reqID, SessionID: idOrName, Cols: cols, Rows: rows}
	}, nil, proto.TypeOK)
	return err
}

// Rename changes the session's unique name.
func (c *Client) Rename(ctx context.Context, idOrName, newName string) error {
	_, err := c.request(ctx, proto.TypeRename, func(reqID string) any {
		return proto.Rename{ReqID: reqID, SessionID: idOrName, Name: newName}
	}, nil, proto.TypeOK)
	return err
}

// SetMeta replaces the session's legacy flat KV wholesale (folded into the
// JSON meta document as top-level string fields). Prefer SetMetaJSON.
func (c *Client) SetMeta(ctx context.Context, idOrName string, meta map[string]string) error {
	_, err := c.request(ctx, proto.TypeSetMeta, func(reqID string) any {
		return proto.SetMeta{ReqID: reqID, SessionID: idOrName, Meta: meta}
	}, nil, proto.TypeOK)
	return err
}

// SetMetaOpts tunes a SetMetaJSON call.
type SetMetaOpts struct {
	// Mode defaults to proto.MetaModeMerge (RFC 7386 merge patch); set
	// proto.MetaModeReplace to swap the whole document.
	Mode proto.MetaMode
	// IfVersion, when non-nil, makes the write conditional (compare-and-swap):
	// the daemon returns E_META_CONFLICT unless the current MetaVersion matches.
	IfVersion *uint64
}

// SetMetaJSON merges (default) or replaces the session's JSON meta document.
// The daemon applies it atomically under the session lock, so concurrent
// patches to different fields never clobber each other. patch is a JSON merge
// patch in merge mode, or the full document in replace mode.
func (c *Client) SetMetaJSON(ctx context.Context, idOrName string, patch json.RawMessage, o SetMetaOpts) error {
	_, err := c.request(ctx, proto.TypeSetMetaJSON, func(reqID string) any {
		return proto.SetMetaJSON{
			ReqID: reqID, SessionID: idOrName,
			Mode: o.Mode, IfVersion: o.IfVersion, Patch: patch,
		}
	}, nil, proto.TypeOK)
	return err
}

// TakeWrite claims the write lock; ReleaseWrite releases it.
func (c *Client) TakeWrite(ctx context.Context, idOrName string) error {
	_, err := c.request(ctx, proto.TypeTakeWrite, func(reqID string) any {
		return proto.TakeWrite{ReqID: reqID, SessionID: idOrName}
	}, nil, proto.TypeOK)
	return err
}

// ReleaseWrite releases the write lock if held by this client.
func (c *Client) ReleaseWrite(ctx context.Context, idOrName string) error {
	_, err := c.request(ctx, proto.TypeReleaseWrite, func(reqID string) any {
		return proto.ReleaseWrite{ReqID: reqID, SessionID: idOrName}
	}, nil, proto.TypeOK)
	return err
}

// InputEOF sends terminal EOF (^D semantics) to the session.
func (c *Client) InputEOF(ctx context.Context, idOrName string) error {
	_, err := c.request(ctx, proto.TypeInputEOF, func(reqID string) any {
		return proto.InputEOF{ReqID: reqID, SessionID: idOrName}
	}, nil, proto.TypeOK)
	return err
}

// Input writes keystrokes. Fire-and-forget by protocol design: refusals
// arrive as push errors on the session's Stream (or are dropped if not
// attached — callers who need certainty attach first).
func (c *Client) Input(sessionID string, p []byte) error {
	return c.enc.WriteMsg(proto.TypeInput, proto.InputHeader{SessionID: sessionID}, p)
}

// WatchEvent is one server-side regex match.
type WatchEvent struct {
	WatchID   string
	SessionID string
	Seq       uint64
	Match     string
}

// Watch registers a server-side RE2 watch on the session's future output.
// The channel closes when the connection dies; the watch itself lives until
// then (watches are per-connection).
func (c *Client) Watch(ctx context.Context, idOrName, pattern string) (<-chan WatchEvent, error) {
	resp, err := c.request(ctx, proto.TypeWatch, func(reqID string) any {
		return proto.Watch{ReqID: reqID, SessionID: idOrName, Pattern: pattern}
	}, nil, proto.TypeWatchOK)
	if err != nil {
		return nil, err
	}
	var ok proto.WatchOK
	if err := resp.DecodeHeader(&ok); err != nil {
		return nil, err
	}
	ch := make(chan WatchEvent, 64)
	c.mu.Lock()
	c.watchers[ok.WatchID] = ch
	c.mu.Unlock()
	return ch, nil
}

func (c *Client) routeWatchEvent(we proto.WatchEvent) {
	c.mu.Lock()
	ch := c.watchers[we.WatchID]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- WatchEvent{WatchID: we.WatchID, SessionID: we.SessionID, Seq: we.Seq, Match: we.Match}:
	default: // mirror the daemon's non-blocking push contract
	}
}

// LastCommandOutput returns the last completed OSC 133-marked command's
// output window (requires shell integration in the session). Returns the
// bytes plus the ring-axis boundaries.
func (c *Client) LastCommandOutput(ctx context.Context, idOrName string) (data []byte, startSeq, endSeq uint64, err error) {
	resp, err := c.request(ctx, proto.TypeReplayCommand, func(reqID string) any {
		return proto.ReplayCommand{ReqID: reqID, SessionID: idOrName}
	}, nil, proto.TypeReplayCommandOK)
	if err != nil {
		return nil, 0, 0, err
	}
	var ok proto.ReplayCommandOK
	if err := resp.DecodeHeader(&ok); err != nil {
		return nil, 0, 0, err
	}
	return resp.Payload, ok.StartSeq, ok.EndSeq, nil
}

// SubscribeEvents subscribes this connection to lifecycle events
// (sessionFilter "" = all). The returned channel closes when the connection
// dies. One subscription per client.
func (c *Client) SubscribeEvents(ctx context.Context, sessionFilter string) (<-chan Event, error) {
	c.mu.Lock()
	if c.events != nil {
		c.mu.Unlock()
		return nil, errcodes.New(errcodes.InvalidInput, "already subscribed to events")
	}
	events := make(chan Event, 64)
	c.events = events
	c.mu.Unlock()

	_, err := c.request(ctx, proto.TypeSubEvents, func(reqID string) any {
		return proto.SubscribeEvents{ReqID: reqID, SessionID: sessionFilter}
	}, nil, proto.TypeOK)
	if err != nil {
		c.mu.Lock()
		c.events = nil
		c.mu.Unlock()
		return nil, err
	}
	return events, nil
}

func (c *Client) routeEvent(ev proto.Event) {
	c.mu.Lock()
	events := c.events
	c.mu.Unlock()
	if events == nil {
		return
	}
	select {
	case events <- Event{Type: ev.Type, SessionID: ev.SessionID, At: time.UnixMilli(ev.AtMs).UTC(), Data: ev.Data}:
	default:
		// SDK-side drop mirrors the daemon's non-blocking contract.
	}
}

func (c *Client) routeOutput(h proto.OutputHeader, payload []byte) {
	c.mu.Lock()
	st := c.streams[h.SessionID]
	c.mu.Unlock()
	if st != nil {
		st.deliver(h.Seq, payload)
	}
}

func (c *Client) routeExit(ex proto.Exit) {
	c.mu.Lock()
	st := c.streams[ex.SessionID]
	c.mu.Unlock()
	if st != nil {
		st.exit(ex)
	}
}

func (c *Client) routePushError(em proto.ErrorMsg) {
	// Push errors are stream-scoped (RING_GONE, NO_WRITE_LOCK on input, …).
	// Without a session id on the wire error, deliver to every stream —
	// v1 has one attach per session per conn, so this is precise enough.
	c.mu.Lock()
	sts := make([]*Stream, 0, len(c.streams))
	for _, st := range c.streams {
		sts = append(sts, st)
	}
	c.mu.Unlock()
	for _, st := range sts {
		st.pushErr(errcodes.New(em.Code, em.Message))
	}
}

var _ io.Reader = (*Stream)(nil) // Stream is documented in stream.go
