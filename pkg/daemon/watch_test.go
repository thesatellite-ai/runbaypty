package daemon

// watch_test.go — task-m9-watch: server-side regex watches over the wire.

import (
	"strings"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func TestWatch_MatchesIncludingChunkBoundary(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// cat echoes what we type — WE control the chunking via separate
	// INPUT frames, so the boundary case is deterministic.
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TakeWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}

	matches, err := c.Watch(ctxT(t), id, `MARKER-[0-9]+`)
	if err != nil {
		t.Fatal(err)
	}

	// Whole-chunk match…
	if err := c.Input(id, []byte("xx MARKER-111 yy\r")); err != nil {
		t.Fatal(err)
	}
	// …then a match split across two writes: the PTY echoes each write as
	// it arrives, so "MARK" and "ER-222" reach the scanner as separate
	// chunks and only the overlap tail can join them.
	if err := c.Input(id, []byte("MARK")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // force separate PTY reads
	if err := c.Input(id, []byte("ER-222\r")); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	deadline := time.After(testTimeout)
	for len(got) < 2 {
		select {
		case ev, ok := <-matches:
			if !ok {
				t.Fatal("watch channel closed early")
			}
			if ev.SessionID != id || !strings.HasPrefix(ev.WatchID, "wch_") {
				t.Fatalf("event = %+v", ev)
			}
			got[ev.Match] = true
		case <-deadline:
			t.Fatalf("matches seen: %v (boundary straddle lost?)", got)
		}
	}
	if !got["MARKER-111"] || !got["MARKER-222"] {
		t.Errorf("matches = %v", got)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestWatch_BadPatternAndLimits(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Watch(ctxT(t), id, `([`); !errcodes.IsCode(err, errcodes.InvalidInput) {
		t.Errorf("bad pattern: %v", err)
	}
	for range maxWatchesPerConn {
		if _, err := c.Watch(ctxT(t), id, `x`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Watch(ctxT(t), id, `x`); !errcodes.IsCode(err, errcodes.LimitExceeded) {
		t.Errorf("watch cap: %v", err)
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}

func TestWatch_OnlyFutureOutput(t *testing.T) {
	sock, _ := startServer(t, Options{})
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// History contains the pattern BEFORE the watch registers.
	id, _, err := c.Spawn(ctxT(t), client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", "echo OLD-MATCH; sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the history is definitely in the ring.
	deadline := time.Now().Add(testTimeout)
	for {
		info, ierr := c.Info(ctxT(t), id)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if info.LastSeq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no output")
		}
		time.Sleep(10 * time.Millisecond)
	}

	matches, err := c.Watch(ctxT(t), id, `(OLD|NEW)-MATCH`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TakeWrite(ctxT(t), id); err != nil {
		t.Fatal(err)
	}
	if err := c.Input(id, []byte("echo NEW-MATCH\n")); err != nil {
		t.Fatal(err)
	}

	// First (and only first) match must be NEW — history never fires.
	select {
	case ev := <-matches:
		if !strings.Contains(ev.Match, "NEW") && !strings.Contains(ev.Match, "MATCH") {
			t.Fatalf("unexpected match %+v", ev)
		}
		if strings.Contains(ev.Match, "OLD") {
			t.Fatalf("watch fired on HISTORY: %+v", ev)
		}
	case <-time.After(testTimeout):
		t.Fatal("no match on new output")
	}
	_ = c.Kill(ctxT(t), id, proto.SignalKILL)
}
