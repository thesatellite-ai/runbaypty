// watch.go — server-side regex watches (frame types 42/43/47).
//
// A watch is an expect-style primitive done right for the daemon era: the
// client registers an RE2 pattern on a session and receives WATCH_EVENT
// pushes on match — no output bytes ship to an idle watcher, and the
// matcher runs the pull model like any subscriber (self-paced, isolated).
//
// Boundary correctness: a match split across two OUTPUT chunks must still
// fire, so the scanner keeps a bounded overlap tail from the previous scan
// and prepends it to the next chunk. The tail is capped — a pattern cannot
// hold unbounded history hostage (task-m9-watch-t1).
package daemon

import (
	"context"
	"regexp"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/host"
	"github.com/thesatellite-ai/runbaypty/pkg/ids"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

const (
	// maxWatchesPerConn bounds a connection's registered watches.
	maxWatchesPerConn = 16
	// watchOverlapBytes is the cross-chunk tail kept between scans. A match
	// longer than this can straddle undetected — documented bound, not a bug.
	watchOverlapBytes = 1024
	// watchMatchCap truncates reported match text in WATCH_EVENT.
	watchMatchCap = 256
)

// watchReg is one registered watch on a connection.
type watchReg struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// onWatch registers a watch and starts its scanner goroutine.
func (c *conn) onWatch(f proto.Frame) {
	var m proto.Watch
	if err := f.DecodeHeader(&m); err != nil {
		_ = c.sendErr("", err)
		return
	}
	re, err := regexp.Compile(m.Pattern)
	if err != nil {
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.InvalidInput, "pattern %q: %v", m.Pattern, err))
		return
	}
	sess, err := c.srv.reg.Lookup(m.SessionID)
	if err != nil {
		_ = c.sendErr(m.ReqID, err)
		return
	}

	watchID := ids.MustNew(ids.PrefixWatch)
	ctx, cancel := context.WithCancel(context.Background())
	reg := &watchReg{cancel: cancel, done: make(chan struct{})}

	c.mu.Lock()
	if len(c.watches) >= maxWatchesPerConn {
		c.mu.Unlock()
		cancel()
		_ = c.sendErr(m.ReqID, errcodes.Newf(errcodes.LimitExceeded, "max %d watches per connection", maxWatchesPerConn))
		return
	}
	c.watches[watchID] = reg
	c.mu.Unlock()

	_ = c.enc.WriteMsg(proto.TypeWatchOK, proto.WatchOK{ReqID: m.ReqID, WatchID: watchID}, nil)
	go c.runWatch(ctx, reg, watchID, sess, re)
}

// runWatch is the scanner: pull-model subscriber scanning FUTURE output.
func (c *conn) runWatch(ctx context.Context, reg *watchReg, watchID string, sess *host.Session, re *regexp.Regexp) {
	defer close(reg.done)
	lastSeen := sess.LastSeq() // future output only, by contract
	var tail []byte            // overlap from the previous scan
	tailStart := lastSeen      // absolute seq of tail[0]

	for {
		data, fromSeq, _ := sess.ReplayFrom(lastSeen)
		if len(data) > 0 {
			// Scan tail+chunk so boundary-straddling matches fire. Matches
			// entirely inside the tail already fired last round — skip any
			// match that ends within the tail region.
			buf := append(append([]byte{}, tail...), data...)
			bufStart := tailStart
			for _, loc := range re.FindAllIndex(buf, -1) {
				if uint64(loc[1])+bufStart <= lastSeen {
					continue // fired in a previous scan
				}
				match := buf[loc[0]:loc[1]]
				if len(match) > watchMatchCap {
					match = match[:watchMatchCap]
				}
				if err := c.enc.WriteMsg(proto.TypeWatchEvent, proto.WatchEvent{
					WatchID:   watchID,
					SessionID: sess.ID(),
					Seq:       bufStart + uint64(loc[0]),
					Match:     string(match),
				}, nil); err != nil {
					return // connection gone
				}
			}
			lastSeen = fromSeq + uint64(len(data))
			// New tail: the last watchOverlapBytes of what we've seen.
			keep := min(len(buf), watchOverlapBytes)
			tail = append(tail[:0], buf[len(buf)-keep:]...)
			tailStart = lastSeen - uint64(keep)
		}

		last, exited := sess.WaitOutput(ctx, lastSeen)
		if ctx.Err() != nil {
			return
		}
		if exited && last == lastSeen {
			return // stream over; nothing left to match
		}
	}
}

// cancelWatches tears down every watch on this connection (cleanup path).
func (c *conn) cancelWatches() {
	c.mu.Lock()
	regs := c.watches
	c.watches = make(map[string]*watchReg)
	c.mu.Unlock()
	for _, reg := range regs {
		reg.cancel()
		<-reg.done
	}
}
