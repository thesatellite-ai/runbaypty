// reattach-zero-gap proves the product's central promise: a client that
// disappears mid-stream can come back and resume at the exact byte it last
// saw — no gap, no duplicate, and it can PROVE it.
//
// The trick is that every output byte has an absolute sequence number.
// seq N means "the stream position after N total bytes." A reader that has
// consumed up to seq N reattaches with since_seq=N and the daemon sends
// exactly the suffix it missed. Continuity is arithmetic, not hope.
//
// This program spawns a counter, reads part of it, hard-drops the
// connection (no polite detach — we simulate a crash), waits while the
// session keeps printing to nobody, then reconnects and audits that the
// joined stream is a perfect 0,1,2,…N with no seam.
//
// Run:
//
//	go run ./examples/reattach-zero-gap
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

// The session prints "n-0", "n-1", … forever, fast enough that a 500ms
// disconnect definitely loses lines if replay is broken.
const counterScript = `i=0; while :; do echo n-$i; i=$((i+1)); sleep 0.01; done`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "reattach-zero-gap:", err)
		os.Exit(1)
	}
}

func run() error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	ctx := context.Background()

	// A control connection that outlives both readers, so the session has an
	// owner while we crash and restart the reader.
	control, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = control.Close() }()

	sessionID, _, err := control.Spawn(ctx, client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", counterScript}})
	if err != nil {
		return err
	}
	defer func() { _ = control.Kill(ctx, sessionID, "") }()
	fmt.Printf("session %s — counting…\n\n", sessionID)

	// ── Reader 1: read a while, remember where we stopped, then vanish.
	first, resumeAt, err := readUntil(sock, sessionID, nil, "n-20")
	if err != nil {
		return fmt.Errorf("reader 1: %w", err)
	}
	fmt.Printf("reader 1 saw %d lines, stopped at seq %d\n", countLines(first), resumeAt)
	fmt.Println("reader 1 crashed (connection dropped, no detach)")

	// The session keeps printing into the ring buffer with nobody attached.
	// Nothing is lost: the ring holds the newest 2 MiB by default.
	time.Sleep(500 * time.Millisecond)
	fmt.Println("…500ms of output nobody was listening to…")

	// ── Reader 2: resume at EXACTLY the sequence reader 1 stopped at.
	second, _, err := readUntil(sock, sessionID, &resumeAt, "n-80")
	if err != nil {
		return fmt.Errorf("reader 2: %w", err)
	}
	fmt.Printf("reader 2 resumed from seq %d and saw %d lines\n\n", resumeAt, countLines(second))

	// ── The audit: joined, the two reads must be a contiguous count.
	joined := strings.ReplaceAll(first+second, "\r\n", "\n")
	want := 0
	for _, line := range strings.Split(joined, "\n") {
		rest, ok := strings.CutPrefix(line, "n-")
		if !ok {
			continue // a partial line at the tail of the buffer
		}
		got, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			continue
		}
		if got != want {
			return fmt.Errorf("SEAM BROKEN: expected n-%d, got n-%d — a gap or a duplicate", want, got)
		}
		want++
	}
	if want < 80 {
		return fmt.Errorf("only audited %d lines", want)
	}

	fmt.Printf("✓ audited n-0 … n-%d across the crash: no gap, no duplicate.\n", want-1)
	fmt.Println("  The seam is invisible because seq numbers make it arithmetic.")
	return nil
}

// readUntil dials a FRESH connection, attaches (optionally resuming at
// sinceSeq), reads until `stop` appears, then hard-closes the connection
// without detaching — exactly what a crashed client looks like to the
// daemon. Returns what it read and the seq to resume from.
func readUntil(sock, sessionID string, sinceSeq *uint64, stop string) (string, uint64, error) {
	c, err := client.Dial(sock)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = c.Close() }() // no Detach: this IS the crash

	stream, err := c.Attach(context.Background(), sessionID, sinceSeq, true /* read-only */)
	if err != nil {
		return "", 0, err
	}

	var acc strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(acc.String(), stop) {
		if time.Now().After(deadline) {
			return "", 0, fmt.Errorf("timeout waiting for %q", stop)
		}
		n, err := stream.Read(buf)
		acc.Write(buf[:n])
		if err != nil {
			return "", 0, err
		}
	}
	// LastSeq is the absolute position after the last byte we consumed —
	// hand it to the next Attach and the daemon resumes precisely there.
	return acc.String(), stream.LastSeq(), nil
}

func countLines(s string) int { return strings.Count(s, "\n") }
