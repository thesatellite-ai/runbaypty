// Package host is the runbaypty engine: PTY sessions, ring buffers,
// subscriber fanout, write locks, and lifecycle events. It is transport-free
// — the UDS/WS listeners (M3/M5) drive it through plain method calls, which
// keeps every piece unit-testable without a socket.
package host

import (
	"sync"
)

// Ring is a byte ring buffer with an absolute sequence axis.
//
// Seq semantics: seq N is "the stream position after N bytes have ever been
// written". Every OUTPUT frame carries the seq of its first byte; a client
// that saw up to seq N reattaches with sinceSeq=N and replays exactly the
// missed suffix — the zero-gap guarantee is arithmetic, not bookkeeping.
//
// The ring retains the newest `cap` bytes; older bytes fall off (they live
// on only in the optional durable log). Safe for one writer + many readers.
type Ring struct {
	mu  sync.RWMutex
	buf []byte
	// end is the seq after the newest byte (== total bytes ever written).
	end uint64
	// filled is how many valid bytes the ring currently holds (≤ len(buf)).
	filled int
	// next is the buf index the next byte lands at (circular).
	next int
}

// NewRing returns a ring retaining the newest capBytes bytes. capBytes must
// be > 0 — the daemon substitutes its default before construction.
func NewRing(capBytes int) *Ring {
	if capBytes <= 0 {
		panic("host: NewRing capBytes must be > 0") // programmer error, not operational
	}
	return &Ring{buf: make([]byte, capBytes)}
}

// Write appends p, advancing the sequence by len(p). If p alone exceeds the
// capacity only its newest cap bytes are retained (seq still advances by the
// full len(p) — the sequence axis tracks the stream, not the retention).
func (r *Ring) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.end += uint64(len(p))
	if len(p) >= len(r.buf) {
		// The write alone overwrites everything: keep the newest cap bytes.
		copy(r.buf, p[len(p)-len(r.buf):])
		r.next = 0
		r.filled = len(r.buf)
		return
	}
	n := copy(r.buf[r.next:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.next = (r.next + len(p)) % len(r.buf)
	if r.filled += len(p); r.filled > len(r.buf) {
		r.filled = len(r.buf)
	}
}

// LastSeq returns the seq after the newest byte (0 for an empty ring).
func (r *Ring) LastSeq() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.end
}

// Len returns how many bytes the ring currently retains.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.filled
}

// Snapshot returns a copy of the retained bytes and the seq after the
// newest byte — the ring's transferable state (HANDOVER.md).
func (r *Ring) Snapshot() (data []byte, endSeq uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	data, _, _ = r.replayLocked(r.end - uint64(r.filled))
	return data, r.end
}

// NewRingFromSnapshot rebuilds a ring whose seq axis continues from a
// snapshot — replay arithmetic stays valid across a daemon handover.
func NewRingFromSnapshot(capBytes int, data []byte, endSeq uint64) *Ring {
	r := NewRing(capBytes)
	r.Write(data) // fills content; end == len(data)
	r.mu.Lock()
	r.end = endSeq // restore the absolute axis
	r.mu.Unlock()
	return r
}

// ReplayFrom returns a copy of the bytes from seq `since` to the end, the
// seq of the first returned byte, and whether the request was truncated
// (since predates the oldest retained byte → replay starts at the oldest
// byte instead, and the caller should signal E_RING_GONE-style truncation).
//
// since ≥ LastSeq returns (nil, since, false): the caller is already current.
func (r *Ring) ReplayFrom(since uint64) (data []byte, fromSeq uint64, truncated bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.replayLocked(since)
}

// replayLocked is ReplayFrom's body; caller holds at least RLock.
func (r *Ring) replayLocked(since uint64) (data []byte, fromSeq uint64, truncated bool) {
	if since >= r.end {
		return nil, since, false
	}
	oldest := r.end - uint64(r.filled)
	fromSeq = since
	if since < oldest {
		fromSeq = oldest
		truncated = true
	}
	count := int(r.end - fromSeq)
	data = make([]byte, count)

	// The newest `filled` bytes end at index r.next (exclusive, circular).
	// The byte at seq (end-1) sits at index (next-1+len)%len, and so on back.
	start := (r.next - count + len(r.buf)*2) % len(r.buf) // buf index of fromSeq's byte
	n := copy(data, r.buf[start:min(start+count, len(r.buf))])
	if n < count {
		copy(data[n:], r.buf[:count-n])
	}
	return data, fromSeq, truncated
}
