package host

import (
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// drainOne receives one event or fails after the timeout.
func drainOne(t *testing.T, ch <-chan proto.Event) proto.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(testTimeout):
		t.Fatal("no event within timeout")
		return proto.Event{}
	}
}

func TestEventBus_DeliverAndFilter(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	allID, all := bus.Subscribe("")
	oneID, one := bus.Subscribe("ses_a")
	defer bus.Unsubscribe(allID)
	defer bus.Unsubscribe(oneID)

	bus.EmitSession(proto.EventCreated, "ses_a", nil)
	bus.EmitSession(proto.EventCreated, "ses_b", nil)
	bus.EmitSession(proto.EventDaemonStopping, "", nil)

	// Unfiltered subscriber sees all three, in order.
	for _, want := range []string{"ses_a", "ses_b", ""} {
		if ev := drainOne(t, all); ev.SessionID != want {
			t.Errorf("all-sub got %q, want %q", ev.SessionID, want)
		}
	}
	// Filtered subscriber sees only its session.
	if ev := drainOne(t, one); ev.SessionID != "ses_a" {
		t.Errorf("filtered sub got %q", ev.SessionID)
	}
	select {
	case ev := <-one:
		t.Errorf("filtered sub leaked event %+v", ev)
	default:
	}
}

func TestEventBus_SlowSubscriberDropsNeverBlocks(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	id, ch := bus.Subscribe("")
	defer bus.Unsubscribe(id)

	// Nobody drains ch. Emitting far past the buffer must return promptly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range eventBufSize * 3 {
			bus.EmitSession(proto.EventActivity, "ses_x", nil)
		}
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Emit blocked on a slow subscriber")
	}
	if dropped := bus.Dropped(id); dropped != eventBufSize*2 {
		t.Errorf("Dropped = %d, want %d", dropped, eventBufSize*2)
	}
	// The buffered prefix is still delivered intact.
	if ev := drainOne(t, ch); ev.Type != proto.EventActivity {
		t.Errorf("first buffered event = %+v", ev)
	}
}

func TestEventBus_UnsubscribeClosesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	id, ch := bus.Subscribe("")
	bus.Unsubscribe(id)
	bus.Unsubscribe(id) // second call: no panic
	if _, open := <-ch; open {
		t.Error("channel not closed on Unsubscribe")
	}
	bus.Emit(proto.Event{Type: proto.EventCreated}) // no panic on empty bus
}

func TestEventBus_ExitedDataShape(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	id, ch := bus.Subscribe("")
	defer bus.Unsubscribe(id)

	bus.EmitExited("ses_x", -1, proto.SignalKILL)
	ev := drainOne(t, ch)
	if ev.Type != proto.EventExited || ev.Data["exit_code"] != "-1" || ev.Data["signal"] != proto.SignalKILL {
		t.Errorf("exited event = %+v", ev)
	}
	bus.EmitExited("ses_y", 0, "")
	if ev := drainOne(t, ch); ev.Data["exit_code"] != "0" || ev.Data["signal"] != "" {
		t.Errorf("clean exit event = %+v", ev)
	}
}

func TestSessionLifecycle_EmitsCreatedAndExited(t *testing.T) {
	r := NewRegistry(0)
	id, ch := r.Events().Subscribe("")
	defer r.Events().Unsubscribe(id)

	s, err := r.Spawn(shCfg("exit 7"))
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, s)

	if ev := drainOne(t, ch); ev.Type != proto.EventCreated || ev.SessionID != s.ID() {
		t.Errorf("first event = %+v, want created", ev)
	}
	// Skip any activity events; find exited.
	for {
		ev := drainOne(t, ch)
		if ev.Type == proto.EventExited {
			if ev.Data["exit_code"] != "7" {
				t.Errorf("exited data = %v", ev.Data)
			}
			return
		}
	}
}
