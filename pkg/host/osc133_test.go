package host

import (
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// feedChunks pushes chunks through the monitor at 1ms intervals.
func feedChunks(h *monitorHarness, chunks ...[]byte) {
	at := h.t0
	for _, c := range chunks {
		h.mon.Feed(c, at)
		at = at.Add(time.Millisecond)
	}
}

func TestOSC133_CommandLifecycleEvents(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, time.Hour)

	// prompt(A) → typed(B) → exec(C) → output → done(D;0): one started,
	// one finished with exit_code 0 and a coherent seq window.
	feedChunks(h,
		[]byte("\x1b]133;A\x07$ "),
		[]byte("\x1b]133;B\x07"),
		[]byte("\x1b]133;C\x07"),
		[]byte("build output here\r\n"),
		[]byte("\x1b]133;D;0\x07"),
	)
	var started, finished *proto.Event
	for _, ev := range h.events() {
		ev := ev
		switch ev.Type {
		case proto.EventCommandStarted:
			started = &ev
		case proto.EventCommandFinished:
			finished = &ev
		}
	}
	if started == nil || finished == nil {
		t.Fatalf("started=%v finished=%v", started, finished)
	}
	if finished.Data["exit_code"] != "0" {
		t.Errorf("exit_code = %q", finished.Data["exit_code"])
	}
	if finished.Data["start_seq"] != started.Data["start_seq"] {
		t.Errorf("start_seq mismatch: %s vs %s", finished.Data["start_seq"], started.Data["start_seq"])
	}
	if finished.Data["end_seq"] <= finished.Data["start_seq"] {
		t.Errorf("window empty: %s → %s", finished.Data["start_seq"], finished.Data["end_seq"])
	}
	if s, e, ok := h.mon.LastCommand(); !ok || e <= s {
		t.Errorf("LastCommand = (%d, %d, %v)", s, e, ok)
	}
}

func TestOSC133_FailedCommandExitCode(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, time.Hour)
	feedChunks(h, []byte("\x1b]133;C\x07boom\r\n\x1b]133;D;127\x07"))
	for _, ev := range h.events() {
		if ev.Type == proto.EventCommandFinished {
			if ev.Data["exit_code"] != "127" {
				t.Errorf("exit_code = %q, want 127", ev.Data["exit_code"])
			}
			return
		}
	}
	t.Fatal("no command-finished event")
}

func TestOSC133_MarksSplitAcrossOneByteReads(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, time.Hour)
	seq := []byte("\x1b]133;C\x07x\x1b]133;D;3\x1b\\") // ST-terminated D
	chunks := make([][]byte, 0, len(seq))
	for _, b := range seq {
		chunks = append(chunks, []byte{b})
	}
	feedChunks(h, chunks...)
	sawStart, sawEnd := false, false
	for _, ev := range h.events() {
		switch ev.Type {
		case proto.EventCommandStarted:
			sawStart = true
		case proto.EventCommandFinished:
			sawEnd = true
			if ev.Data["exit_code"] != "3" {
				t.Errorf("exit_code = %q", ev.Data["exit_code"])
			}
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("split marks lost: started=%v finished=%v", sawStart, sawEnd)
	}
}

func TestOSC133_NoFalsePositives(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, time.Hour)
	feedChunks(h,
		[]byte("\x1b]0;window title with 133;C inside\x07"),      // title OSC, not 133
		[]byte("plain text mentioning 133;D;0 in prose"),         // no ESC ] at all
		[]byte("\x1b]8;;http://x/133;C\x1b\\link\x1b]8;;\x1b\\"), // hyperlink OSC
	)
	for _, ev := range h.events() {
		if ev.Type == proto.EventCommandStarted || ev.Type == proto.EventCommandFinished {
			t.Fatalf("false positive: %+v", ev)
		}
	}
}

func TestOSC133_AidAnnotationTolerated(t *testing.T) {
	t.Parallel()
	h := newMonitorHarness(t, time.Hour)
	// Kitty-style: D;<code>;aid=… — code parses, suffix ignored.
	feedChunks(h, []byte("\x1b]133;C\x07out\x1b]133;D;1;aid=42\x07"))
	for _, ev := range h.events() {
		if ev.Type == proto.EventCommandFinished {
			if ev.Data["exit_code"] != "1" {
				t.Errorf("exit_code = %q, want 1", ev.Data["exit_code"])
			}
			return
		}
	}
	t.Fatal("no finished event")
}

func TestOSC133_SeqAxisMatchesRing(t *testing.T) {
	// End-to-end through a real session: the seq window recorded by the
	// monitor must slice the RING to exactly the command's output.
	printf := `printf '\033]133;C\007COMMAND-BODY-OUTPUT\033]133;D;0\007'`
	s := spawnT(t, shCfg(printf))
	waitExit(t, s)
	start, end, ok := s.Monitor().LastCommand()
	if !ok {
		t.Fatal("no command recorded")
	}
	data, from, _ := s.ReplayFrom(start)
	if from != start {
		t.Fatalf("replay from %d, want %d", from, start)
	}
	window := string(data[:end-start])
	if !contains(window, "COMMAND-BODY-OUTPUT") {
		t.Errorf("command window %q missing body", window)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
