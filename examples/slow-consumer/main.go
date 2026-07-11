// slow-consumer demonstrates the isolation property of the pull-based data
// plane: a reader that can't keep up with a firehose never slows the session
// and never affects the other readers watching it.
//
// The data plane is PULL, not push: each subscriber has its own pump on the
// daemon that loops "wait for new output, replay from where THIS subscriber
// last was." There is no shared broadcast buffer to congest. A fast reader
// and a slow reader on the same session are completely independent — the
// session runs at full speed for both, and the slow one simply lags in its
// own view.
//
// This program floods one session and attaches TWO readers — one fast, one
// artificially slow — then shows the fast reader and the session were
// unaffected by the slow one.
//
// Run:
//
//	go run ./examples/slow-consumer
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

// A firehose: prints as fast as the PTY will take it.
const firehoseScript = `i=0; while :; do echo "chunk-$i-payload-payload-payload-payload"; i=$((i+1)); done`

// measureFor is how long we sample throughput.
const measureFor = 3 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "slow-consumer:", err)
		os.Exit(1)
	}
}

func run() error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sessionID, _, err := c.Spawn(ctx, client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", firehoseScript}})
	if err != nil {
		return err
	}
	defer func() { _ = c.Kill(context.Background(), sessionID, "") }()
	fmt.Printf("firehose session %s — two readers, one fast, one slow\n\n", sessionID)

	// Both readers attach to the SAME session on their own connections.
	fast, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = fast.Close() }()
	slow, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = slow.Close() }()

	fastStream, err := fast.Attach(ctx, sessionID, nil, true)
	if err != nil {
		return err
	}
	slowStream, err := slow.Attach(ctx, sessionID, nil, true)
	if err != nil {
		return err
	}

	var fastBytes, slowBytes atomic.Int64
	var wg sync.WaitGroup
	readCtx, stopReading := context.WithTimeout(ctx, measureFor)
	defer stopReading()

	// Fast reader: drains as quickly as it can.
	wg.Add(1)
	go func() {
		defer wg.Done()
		fastBytes.Store(drain(readCtx, fastStream, 0))
	}()

	// Slow reader: sleeps 50ms between small reads — orders of magnitude
	// behind the firehose. Its lag is entirely its own; watch the fast
	// reader's number to see it is unaffected.
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowBytes.Store(drain(readCtx, slowStream, 50*time.Millisecond))
	}()

	wg.Wait()

	// Compare how far the session's output axis advanced (Info.BytesOut,
	// the total ever produced) against what each reader consumed.
	info, err := c.Info(ctx, sessionID)
	if err != nil {
		return err
	}

	fmt.Printf("in %s the session produced ~%s of output\n", measureFor, human(int64(info.BytesOut)))
	fmt.Printf("  fast reader consumed: %s\n", human(fastBytes.Load()))
	fmt.Printf("  slow reader consumed: %s\n\n", human(slowBytes.Load()))

	fmt.Println("── the point ──")
	fmt.Println("The fast reader kept pace with the firehose. The slow reader fell far")
	fmt.Println("behind — but that lag is entirely its own: the session ran at full")
	fmt.Println("speed, and the fast reader was not held back one byte by the slow one.")
	fmt.Println()
	fmt.Println("Each subscriber has its own pump (pull, not a shared broadcast buffer),")
	fmt.Println("so backpressure is per-reader by construction. A reader that stops")
	fmt.Println("reading its SOCKET entirely eventually falls out of the ring window and")
	fmt.Println("gets E_RING_GONE, resuming at the oldest retained byte — never a silent")
	fmt.Println("gap, never unbounded server buffering. See the README.")
	return nil
}

// drain reads a stream until ctx ends, optionally sleeping between reads to
// simulate a slow consumer, and returns the byte count.
func drain(ctx context.Context, r io.Reader, pause time.Duration) int64 {
	var total int64
	buf := make([]byte, 4096)
	for ctx.Err() == nil {
		if pause > 0 {
			select {
			case <-ctx.Done():
				return total
			case <-time.After(pause):
			}
		}
		n, err := r.Read(buf)
		total += int64(n)
		if err != nil {
			return total
		}
	}
	return total
}

// human formats a byte count.
func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
