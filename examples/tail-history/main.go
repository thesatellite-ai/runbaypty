// tail-history prints a session's COMPLETE output history and then follows
// it live, with a byte-exact seam between the two.
//
// The trick: the durable log and the in-memory ring share ONE sequence
// axis. The log holds bytes [0, N). So after replaying N bytes from the
// log, ATTACH{since_seq: N} resumes the live stream at exactly byte N —
// no overlap, no gap. Same arithmetic as a reconnect, applied across
// storage tiers.
//
// Without a log the history is whatever the ring still holds (the newest
// 2 MiB by default) — this program falls back to that and says so.
//
// Run:
//
//	bin/runbaypty run --name build --log /tmp/build.log -- make -j8
//	go run ./examples/tail-history build
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/host"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: tail-history <session-id-or-name>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "tail-history:", err)
		os.Exit(1)
	}
}

func run(target string) error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	info, err := c.Info(ctx, target)
	if err != nil {
		return err
	}

	// ── Tier 1: the durable log, if the session was spawned with one. It is
	// the complete history from byte 0, and it outlives the ring window.
	//
	// sinceSeq stays nil when there is no log: then "history" means "replay
	// the whole ring", which is what a nil sinceSeq asks the daemon for.
	var sinceSeq *uint64
	if info.LogPath != "" {
		records, err := host.ReadSessionLog(info.LogPath)
		if err != nil {
			return fmt.Errorf("read log %s: %w", info.LogPath, err)
		}
		var replayed uint64
		for _, rec := range records {
			if _, err := os.Stdout.Write(rec.Data); err != nil {
				return err
			}
			replayed += uint64(len(rec.Data))
		}
		fmt.Fprintf(os.Stderr, "\n[history: %d bytes from %s — live stream resumes at seq %d]\n",
			replayed, info.LogPath, replayed)
		sinceSeq = &replayed
	} else {
		fmt.Fprintln(os.Stderr, "[no durable log — history is the ring window; spawn with --log for complete history]")
	}

	// ── Tier 2: the live stream, resumed at exactly where the log ended.
	// Attach (not Follow) because only Attach takes a resume point — the
	// exact seam is the whole point of this example. A production tail
	// would wrap this in its own reconnect loop, re-reading the log's
	// length each time (the log keeps growing while you read).
	stream, err := c.Attach(ctx, info.ID, sinceSeq, true /* read-only */)
	if err != nil {
		return err
	}

	if _, err := io.Copy(os.Stdout, stream); err != nil {
		return err
	}
	if code, signal, exited := stream.Exit(); exited {
		if signal != "" {
			fmt.Fprintf(os.Stderr, "\n[session exited: signal %s]\n", signal)
		} else {
			fmt.Fprintf(os.Stderr, "\n[session exited: code %d]\n", code)
		}
	}
	return nil
}
