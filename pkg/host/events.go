// events.go — the lifecycle/activity event bus.
//
// Emit is NON-BLOCKING by contract: a slow event subscriber drops events
// (counted, surfaced in logs) rather than ever stalling the data plane.
// Events are advisory signals; the byte stream is the source of truth.
package host

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// eventBufSize is each subscriber's channel depth. Deep enough to absorb
// bursts (a mass-kill emits one exited event per session); a subscriber
// that falls further behind loses oldest-first and sees the drop counter.
const eventBufSize = 256

// EventBus fans lifecycle events out to subscribers.
type EventBus struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]*eventSub
}

type eventSub struct {
	ch chan proto.Event
	// sessionFilter narrows delivery to one session ("" = everything,
	// including daemon-scoped events with an empty SessionID).
	sessionFilter string
	dropped       atomic.Uint64
}

// NewEventBus returns an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[int]*eventSub)}
}

// Subscribe registers a subscriber. sessionFilter "" receives every event.
// The returned channel closes on Unsubscribe.
func (b *EventBus) Subscribe(sessionFilter string) (id int, ch <-chan proto.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	sub := &eventSub{ch: make(chan proto.Event, eventBufSize), sessionFilter: sessionFilter}
	b.subs[b.nextID] = sub
	return b.nextID, sub.ch
}

// Unsubscribe removes the subscriber and closes its channel. Idempotent.
func (b *EventBus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(sub.ch)
	}
}

// Dropped returns how many events the subscriber has lost to backpressure
// (0 for unknown ids — the subscriber is gone, the count no longer matters).
func (b *EventBus) Dropped(id int) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.subs[id]; ok {
		return sub.dropped.Load()
	}
	return 0
}

// Emit delivers ev to every matching subscriber without ever blocking:
// a full channel counts a drop instead.
func (b *EventBus) Emit(ev proto.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		if sub.sessionFilter != "" && sub.sessionFilter != ev.SessionID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			sub.dropped.Add(1)
		}
	}
}

// EmitSession is the convenience for session-scoped events.
func (b *EventBus) EmitSession(t proto.EventType, sessionID string, data map[string]string) {
	b.Emit(proto.Event{
		Type:      t,
		SessionID: sessionID,
		AtMs:      time.Now().UTC().UnixMilli(),
		Data:      data,
	})
}

// EmitExited emits the exited event with its documented data shape.
func (b *EventBus) EmitExited(sessionID string, exitCode int, signal string) {
	data := map[string]string{proto.DataKeyExitCode: strconv.Itoa(exitCode)}
	if signal != "" {
		data[proto.DataKeySignal] = signal
	}
	b.EmitSession(proto.EventExited, sessionID, data)
}
