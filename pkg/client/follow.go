// follow.go — the resilient stream: zero-gap resume as a LIBRARY guarantee.
//
// Follow wraps dial + attach + read in a reconnect loop: when the
// connection to the daemon drops (client crash-adjacent conditions, daemon
// restart of the LISTENER, network-of-one hiccups), it re-dials with
// backoff and re-attaches at the exact sequence it had seen — the consumer
// just sees an uninterrupted io.Reader. If the SESSION meanwhile exited,
// the replay + EXIT still arrive (retention), then Read returns io.EOF.
package client

import (
	"context"
	"io"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// FollowOpts tunes the resilient stream.
type FollowOpts struct {
	// ReadOnly attaches without write intent.
	ReadOnly bool
	// InitialBackoff is the first reconnect delay (default 100ms), doubling
	// to MaxBackoff (default 3s). No retry cap — Follow gives up only when
	// ctx cancels or the daemon says the session is gone.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Follower is the resilient reader. Close releases the current connection.
type Follower struct {
	ctx      context.Context
	sockPath string
	opts     FollowOpts

	sessionID string
	lastSeq   uint64

	c  *Client
	st *Stream

	exitCode   int
	exitSignal string
	exited     bool
}

// Follow attaches to idOrName with automatic reconnect + zero-gap resume.
func Follow(ctx context.Context, socketPath, idOrName string, opts FollowOpts) (*Follower, error) {
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = 100 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 3 * time.Second
	}
	f := &Follower{ctx: ctx, sockPath: socketPath, opts: opts}

	// First connect resolves the name to the canonical id; reconnects use
	// the id — a rename mid-stream must not break the follow.
	c, err := Dial(socketPath)
	if err != nil {
		return nil, err
	}
	st, err := c.Attach(ctx, idOrName, nil, opts.ReadOnly)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	f.c, f.st = c, st
	f.sessionID = st.SessionID()
	return f, nil
}

// SessionID returns the canonical id being followed.
func (f *Follower) SessionID() string { return f.sessionID }

// LastSeq returns the seq after the last byte the consumer has seen.
func (f *Follower) LastSeq() uint64 { return f.lastSeq }

// Exit reports the session outcome once known.
func (f *Follower) Exit() (code int, signal string, exited bool) {
	return f.exitCode, f.exitSignal, f.exited
}

// Read implements io.Reader with transparent reconnect.
func (f *Follower) Read(p []byte) (int, error) {
	for {
		n, err := f.st.Read(p)
		if n > 0 {
			f.lastSeq = f.st.LastSeq()
			return n, nil
		}
		if err == nil {
			continue
		}
		// Clean end: the session exited and the stream drained.
		if err == io.EOF {
			if code, sig, exited := f.st.Exit(); exited {
				f.exitCode, f.exitSignal, f.exited = code, sig, true
			}
			return 0, io.EOF
		}
		// Connection-shaped failure: reconnect and resume at lastSeq.
		if !errcodes.IsCode(err, errcodes.DaemonUnreachable) {
			return 0, err // protocol-level error — not retriable
		}
		if rerr := f.reconnect(); rerr != nil {
			return 0, rerr
		}
	}
}

// reconnect re-dials with exponential backoff until ctx cancels or the
// daemon answers definitively (session gone → that error surfaces).
func (f *Follower) reconnect() error {
	_ = f.c.Close()
	backoff := f.opts.InitialBackoff
	for {
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-time.After(backoff):
		}
		c, err := Dial(f.sockPath)
		if err == nil {
			since := f.lastSeq
			st, aerr := c.Attach(f.ctx, f.sessionID, &since, f.opts.ReadOnly)
			if aerr == nil {
				f.c, f.st = c, st
				return nil
			}
			_ = c.Close()
			// The daemon answered: a definitive no (session reaped, bad
			// scope) must surface, not retry forever.
			if !errcodes.IsCode(aerr, errcodes.DaemonUnreachable) {
				return aerr
			}
		}
		if backoff *= 2; backoff > f.opts.MaxBackoff {
			backoff = f.opts.MaxBackoff
		}
	}
}

// Close releases the current connection (the daemon-side subscription dies
// with it). The Follower is not reusable after Close.
func (f *Follower) Close() error {
	return f.c.Close()
}

var _ io.ReadCloser = (*Follower)(nil)
