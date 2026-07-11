package host

import (
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// monitorHarness collects events synchronously — Feed/Tick take explicit
// times, so these tests involve zero sleeps and zero real clocks.
type monitorHarness struct {
	bus *EventBus
	ch  <-chan proto.Event
	mon *Monitor
	t0  time.Time
}

func newMonitorHarness(t *testing.T, silenceAfter time.Duration) *monitorHarness {
	t.Helper()
	bus := NewEventBus()
	id, ch := bus.Subscribe("")
	t.Cleanup(func() { bus.Unsubscribe(id) })
	return &monitorHarness{
		bus: bus,
		ch:  ch,
		mon: NewMonitor("ses_m", bus, silenceAfter),
		t0:  time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
}

// events drains everything currently buffered.
func (h *monitorHarness) events() []proto.Event {
	var out []proto.Event
	for {
		select {
		case ev := <-h.ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func typesOf(evs []proto.Event) []proto.EventType {
	out := make([]proto.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func TestMonitor_SilenceFiresOncePerQuietPeriod(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, 5*time.Second)

	h.mon.Feed([]byte("output"), h.t0)
	h.mon.Tick(h.t0.Add(1 * time.Second)) // not quiet long enough
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("premature events: %v", typesOf(evs))
	}

	h.mon.Tick(h.t0.Add(5 * time.Second)) // threshold crossed
	evs := h.events()
	if len(evs) != 1 || evs[0].Type != proto.EventSilence || evs[0].Data["quiet_ms"] != "5000" {
		t.Fatalf("expected one silence{5000}, got %+v", evs)
	}

	// Repeated ticks in the same quiet period: no re-fire.
	h.mon.Tick(h.t0.Add(20 * time.Second))
	h.mon.Tick(h.t0.Add(200 * time.Second))
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("silence re-fired: %v", typesOf(evs))
	}
}

func TestMonitor_ActivityEdgeAfterQuietAndSilenceResets(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, 5*time.Second)

	h.mon.Feed([]byte("a"), h.t0)
	h.mon.Feed([]byte("b"), h.t0.Add(time.Second)) // continuous output — no edge
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("activity fired during continuous output: %v", typesOf(evs))
	}

	h.mon.Tick(h.t0.Add(7 * time.Second)) // silence at 6s quiet
	h.mon.Feed([]byte("woke"), h.t0.Add(11*time.Second))
	evs := h.events()
	if len(evs) != 2 || evs[0].Type != proto.EventSilence || evs[1].Type != proto.EventActivity {
		t.Fatalf("expected [silence, activity], got %v", typesOf(evs))
	}
	if evs[1].Data["quiet_ms"] != "10000" {
		t.Errorf("activity quiet_ms = %q, want 10000", evs[1].Data["quiet_ms"])
	}

	// New quiet period after the wake: silence can fire again.
	h.mon.Tick(h.t0.Add(16 * time.Second))
	if evs := h.events(); len(evs) != 1 || evs[0].Type != proto.EventSilence {
		t.Fatalf("silence did not re-arm after activity: %v", typesOf(evs))
	}
}

func TestMonitor_NeverFiresBeforeFirstOutput(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, time.Second)
	h.mon.Tick(h.t0.Add(time.Hour)) // command that never printed anything
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("silence fired before any output: %v", typesOf(evs))
	}
}

func TestMonitor_BellDetection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		feeds [][]byte
		bells int
	}{
		{"plain bell", [][]byte{{'h', 'i', 0x07}}, 1},
		{"two bells", [][]byte{{0x07, 'x', 0x07}}, 2},
		{"OSC title BEL-terminated is not a bell", [][]byte{[]byte("\x1b]0;my title\x07")}, 0},
		{"OSC ST-terminated then real bell", [][]byte{[]byte("\x1b]8;;http://x\x1b\\ding\x07")}, 1},
		{"OSC split across feeds", [][]byte{[]byte("\x1b]0;par"), []byte("tial\x07after\x07")}, 1},
		{"ESC alone does not eat next bell", [][]byte{{0x1b, 'A', 0x07}}, 1},
		{"ESC inside OSC payload stays in OSC", [][]byte{[]byte("\x1b]0;a\x1bb\x07")}, 0},
		{"bell split at chunk boundary", [][]byte{[]byte("abc"), {0x07}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newMonitorHarness(t, time.Hour)
			at := h.t0
			for _, chunk := range tc.feeds {
				h.mon.Feed(chunk, at)
				at = at.Add(time.Millisecond)
			}
			bells := 0
			for _, ev := range h.events() {
				if ev.Type == proto.EventBell {
					bells++
				}
			}
			if bells != tc.bells {
				t.Errorf("bells = %d, want %d", bells, tc.bells)
			}
		})
	}
}

func TestRegistry_ReapExpired(t *testing.T) {
	r := NewRegistry(0)
	t.Cleanup(func() { killAll(t, r) })

	dead, err := r.Spawn(shCfg("true"))
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, dead)
	alive, err := r.Spawn(shCfg("sleep 300"))
	if err != nil {
		t.Fatal(err)
	}

	exitedAt, _ := dead.ExitedAt()

	// Before the TTL: nothing reaped.
	if got := r.ReapExpired(exitedAt.Add(time.Minute), 5*time.Minute); len(got) != 0 {
		t.Errorf("reaped early: %v", got)
	}
	// After the TTL: only the exited session goes.
	got := r.ReapExpired(exitedAt.Add(6*time.Minute), 5*time.Minute)
	if len(got) != 1 || got[0] != dead.ID() {
		t.Errorf("reaped = %v, want [%s]", got, dead.ID())
	}
	if _, err := r.Lookup(dead.ID()); err == nil {
		t.Error("reaped session still resolvable")
	}
	if _, err := r.Lookup(alive.ID()); err != nil {
		t.Errorf("living session was reaped: %v", err)
	}
}
