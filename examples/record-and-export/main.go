// record-and-export runs a command with a durable log, then turns that log
// into a playable asciinema cast — no extra instrumentation.
//
// This works because runbaypty's durable log stores (timestamp-delta, bytes)
// records, not just raw bytes. The timestamps were a deliberate day-one
// choice: they cost ~3 bytes per record and they make the log a RECORDING.
// Exporting to asciinema is then a pure file transform — the daemon isn't
// even involved — and any session you spawned with --log can be replayed
// later, long after it exited.
//
// This program does the whole loop: spawn-with-log, wait for the command to
// finish, then hand the log to `runbaypty export` and tell you how to play it.
//
// Run:
//
//	go run ./examples/record-and-export
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// A command with visible timing and color, so the recording is worth
// watching: the asciinema player will reproduce the pauses.
const recordedScript = `
echo "\033[1;32m▶ build starting\033[0m"
for step in fetch compile link package; do
  printf "  %-10s" "$step"
  sleep 0.4
  echo "\033[32mok\033[0m"
done
echo "\033[1;36m✔ build complete\033[0m"
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "record-and-export:", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The log lives wherever we say. The daemon opens it, writes every
	// output chunk as a timestamped record, and closes it when the session
	// exits — so by the time we read it below, it's complete.
	logPath := filepath.Join(os.TempDir(), "runbaypty-build.log")
	_ = os.Remove(logPath)

	events, err := c.SubscribeEvents(ctx, "")
	if err != nil {
		return err
	}
	sessionID, _, err := c.Spawn(ctx, client.SpawnOpts{
		Cmd:  "/bin/sh",
		Args: []string{"-c", recordedScript},
		// LogPath turns on the durable (ts, bytes) log. Without it, output
		// lives only in the ring buffer and is lost when the ring wraps.
		LogPath: logPath,
		Cols:    100, Rows: 30,
	})
	if err != nil {
		return err
	}
	fmt.Printf("recording session %s → %s\n", sessionID, logPath)

	// Wait for the command to finish so the log is fully flushed.
	code, err := waitExit(ctx, events, sessionID)
	if err != nil {
		return err
	}
	fmt.Printf("build finished (exit %d) — log is complete\n\n", code)

	// Export the log to an asciinema cast. This is `runbaypty export`, a
	// PURE FILE TRANSFORM: the daemon plays no part, and it works on any log
	// from any session, even one that exited days ago.
	castPath := filepath.Join(os.TempDir(), "runbaypty-build.cast")
	if err := exportCast(logPath, castPath); err != nil {
		return err
	}
	fmt.Printf("exported → %s\n\n", castPath)

	// Show the cast's shape so the transform is visible.
	if err := describeCast(castPath); err != nil {
		return err
	}
	fmt.Printf("\nplay it:  asciinema play %s\n", castPath)
	fmt.Printf("or share: asciinema upload %s\n", castPath)
	return nil
}

// waitExit blocks on the event stream until the session exits, returning its
// code (no polling; the daemon pushes the exit event).
func waitExit(ctx context.Context, events <-chan client.Event, sessionID string) (int, error) {
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return 0, fmt.Errorf("event stream closed")
			}
			if ev.SessionID == sessionID && ev.Type == proto.EventExited {
				code, _ := strconv.Atoi(ev.Data[proto.DataKeyExitCode])
				return code, nil
			}
		}
	}
}

// exportCast shells out to `runbaypty export`. Shown as a subprocess to make
// the point that this is a plain CLI step you can run by hand — but the same
// logic is importable from pkg/host + the export code if you want it in-proc.
func exportCast(logPath, castPath string) error {
	bin, err := exec.LookPath(constants.BinaryName)
	if err != nil {
		// Fall back to the freshly built binary in ./bin for the demo.
		bin = filepath.Join("bin", constants.BinaryName)
	}
	out, err := exec.Command(bin, "export", logPath, "--out", castPath, "--title", "runbaypty build").CombinedOutput()
	if err != nil {
		return fmt.Errorf("export: %v — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// describeCast prints the cast's header and record count so the (ts, bytes)
// → asciinema mapping is legible.
func describeCast(castPath string) error {
	data, err := os.ReadFile(castPath)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	fmt.Printf("cast header: %s\n", lines[0])
	fmt.Printf("cast events: %d frames, each [elapsed-seconds, \"o\", bytes]\n", len(lines)-1)
	if len(lines) > 1 {
		fmt.Printf("first frame: %s\n", lines[1])
	}
	return nil
}
