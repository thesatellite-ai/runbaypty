// stream.go — the attach stream: OUTPUT bytes as an io.Reader with seq
// tracking, exit delivery, and push-error surfacing.
package client

import (
	"context"
	"io"
	"sync"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// Stream is one attached session's byte stream. Read() delivers the PTY
// bytes in order; after the session exits and the stream drains, Read
// returns io.EOF and Exit() reports the outcome.
type Stream struct {
	c         *Client
	sessionID string

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	seq    uint64 // seq after the last buffered byte (for LastSeq/resume)
	closed bool
	err    error       // terminal error (nil on clean EXIT)
	exitAt *proto.Exit // set when EXIT arrived
	// lastWriteRefusal holds the most recent input refusal pushed by the
	// daemon (NO_WRITE_LOCK / READ_ONLY_ATTACH); read via WriteRefusal().
	lastWriteRefusal error
}

// Attach subscribes to a session's stream. sinceSeq nil replays the whole
// ring; a previous Stream's LastSeq() resumes with zero gap. readOnly
// streams can never take the write lock.
func (c *Client) Attach(ctx context.Context, idOrName string, sinceSeq *uint64, readOnly bool) (*Stream, error) {
	// Resolve names to the canonical id first — OUTPUT routing keys on id.
	info, err := c.Info(ctx, idOrName)
	if err != nil {
		return nil, err
	}
	st := &Stream{c: c, sessionID: info.ID}
	st.cond = sync.NewCond(&st.mu)
	if sinceSeq != nil {
		st.seq = *sinceSeq
	}

	c.mu.Lock()
	if _, dup := c.streams[info.ID]; dup {
		c.mu.Unlock()
		return nil, errcodes.Newf(errcodes.InvalidInput, "already attached to %s", info.ID)
	}
	c.streams[info.ID] = st
	c.mu.Unlock()

	_, err = c.request(ctx, proto.TypeAttach, func(reqID string) any {
		return proto.Attach{ReqID: reqID, SessionID: info.ID, SinceSeq: sinceSeq, ReadOnly: readOnly}
	}, nil, proto.TypeAttachOK)
	if err != nil {
		c.mu.Lock()
		delete(c.streams, info.ID)
		c.mu.Unlock()
		return nil, err
	}
	return st, nil
}

// Detach unsubscribes the stream; Read drains what already arrived, then
// returns io.EOF.
func (st *Stream) Detach(ctx context.Context) error {
	_, err := st.c.request(ctx, proto.TypeDetach, func(reqID string) any {
		return proto.Detach{ReqID: reqID, SessionID: st.sessionID}
	}, nil, proto.TypeOK)
	st.c.mu.Lock()
	delete(st.c.streams, st.sessionID)
	st.c.mu.Unlock()
	st.finish(nil)
	return err
}

// SessionID returns the canonical ses_… id this stream follows.
func (st *Stream) SessionID() string { return st.sessionID }

// LastSeq returns the seq after the last byte delivered to this stream —
// hand it to Attach(sinceSeq) on a new connection for zero-gap resume.
func (st *Stream) LastSeq() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.seq
}

// Exit returns the session's exit outcome once the EXIT frame arrived.
func (st *Stream) Exit() (code int, signal string, exited bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.exitAt == nil {
		return 0, "", false
	}
	return st.exitAt.ExitCode, st.exitAt.Signal, true
}

// Read implements io.Reader over the session's output. Blocks until bytes
// arrive, the session exits (drain then io.EOF), or the stream breaks
// (connection loss / push error).
func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for len(st.buf) == 0 && !st.closed {
		st.cond.Wait()
	}
	if len(st.buf) > 0 {
		n := copy(p, st.buf)
		st.buf = st.buf[n:]
		return n, nil
	}
	if st.err != nil {
		return 0, st.err
	}
	return 0, io.EOF
}

// deliver appends bytes from an OUTPUT frame (reader goroutine only).
func (st *Stream) deliver(seq uint64, payload []byte) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	// Seq audit: frames must be contiguous. A mismatch means the ring
	// dropped bytes for us (RING_GONE push also arrives) — jump forward
	// honestly rather than pretending continuity.
	st.buf = append(st.buf, payload...)
	st.seq = seq + uint64(len(payload))
	st.cond.Broadcast()
}

// exit records the EXIT frame and closes the stream (drain-then-EOF).
func (st *Stream) exit(ex proto.Exit) {
	st.mu.Lock()
	defer st.mu.Unlock()
	exCopy := ex
	st.exitAt = &exCopy
	st.closed = true
	st.cond.Broadcast()
}

// finish closes the stream with an optional terminal error.
func (st *Stream) finish(err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	st.closed = true
	st.err = err
	st.cond.Broadcast()
}

// pushErr surfaces a push-side ERROR. The contract: RING_GONE is
// informational (the following OUTPUT resumes from the oldest retained
// byte, stream keeps flowing); write refusals never end the stream either —
// they park in lastWriteRefusal for WriteRefusal() polls.
func (st *Stream) pushErr(err error) {
	if errcodes.IsCode(err, errcodes.RingGone) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastWriteRefusal = err
}

// WriteRefusal returns and clears the most recent input-refusal error
// (NO_WRITE_LOCK / READ_ONLY_ATTACH pushed by the daemon after Input).
func (st *Stream) WriteRefusal() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	err := st.lastWriteRefusal
	st.lastWriteRefusal = nil
	return err
}
