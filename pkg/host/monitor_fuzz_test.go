package host

import (
	"testing"
	"time"
)

// FuzzMonitorFeed — the scanner must never panic and never corrupt its
// state machine on arbitrary bytes (task-m8-osc133-t3). Passthrough
// integrity is structural (Feed never writes to p), so the fuzz asserts
// liveness: no panic, and a well-formed mark still parses afterward.
func FuzzMonitorFeed(f *testing.F) {
	f.Add([]byte("\x1b]133;C\x07hello\x1b]133;D;0\x07"))
	f.Add([]byte("\x1b]0;title\x07"))
	f.Add([]byte{0x1b, ']', 0x1b, '\\'})
	f.Add([]byte{0x1b})
	f.Add([]byte{0x07, 0x07, 0x07})
	f.Fuzz(func(t *testing.T, data []byte) {
		bus := NewEventBus()
		m := NewMonitor("ses_fuzz", bus, time.Hour)
		at := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
		// Feed the junk in two arbitrary chunks, then a clean mark: the
		// state machine must recover to parse it.
		half := len(data) / 2
		m.Feed(data[:half], at)
		m.Feed(data[half:], at.Add(time.Millisecond))
		// Force back to ground with an ST, then a clean command cycle.
		m.Feed([]byte("\x1b\\\x1b]133;C\x07x\x1b]133;D;9\x07"), at.Add(2*time.Millisecond))
		if _, _, ok := m.LastCommand(); !ok {
			t.Fatal("scanner wedged: clean D mark after fuzz input was not parsed")
		}
	})
}
