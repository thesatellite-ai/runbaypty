// wait-for-silence answers the question every agent asks: "is it done yet?"
//
// The naive answers are all bad. Polling the output means shipping every
// byte to a process that only cares about the last one. Parsing the terminal
// means becoming a terminal emulator. Waiting for process exit is useless for
// a shell that stays alive between commands.
//
// The daemon already knows when a session stopped producing output, and it
// says so: a `silence` event fires once per quiet period. This program runs a
// command, blocks on the event stream until the session goes quiet or dies,
// and reports what it printed plus why the wait ended.
//
// Run:
//
//	go run ./examples/wait-for-silence -- sh -c 'echo working; sleep 2; echo done; sleep 60'
//	go run ./examples/wait-for-silence -- npm install
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// hardTimeout bounds the whole wait: a command that neither goes quiet nor
// exits (a log tail, a REPL at a prompt that keeps redrawing) must not hang
// the supervisor forever.
const hardTimeout = 5 * time.Minute

func main() {
	argv := os.Args[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: wait-for-silence [--] <command> [args…]")
		os.Exit(2)
	}
	if err := run(argv); err != nil {
		fmt.Fprintln(os.Stderr, "wait-for-silence:", err)
		os.Exit(1)
	}
}

// waitResult is why the wait ended and what the daemon told us about it.
type waitResult struct {
	exited   bool
	exitCode int
	quietMs  string
}

func run(argv []string) error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), hardTimeout)
	defer cancel()

	// Subscribe BEFORE spawning: a fast command can go quiet before a
	// subscription placed afterwards would exist.
	events, err := c.SubscribeEvents(ctx, "")
	if err != nil {
		return err
	}

	sessionID, _, err := c.Spawn(ctx, client.SpawnOpts{Cmd: argv[0], Args: argv[1:], Cols: 100, Rows: 30})
	if err != nil {
		return err
	}
	defer func() { _ = c.Kill(context.Background(), sessionID, "") }()
	fmt.Fprintf(os.Stderr, "[running %s in %s]\n", strings.Join(argv, " "), sessionID)

	// Capture the output in the background. Read-only: this program observes,
	// it never types. The buffer is snapshot-able at any moment, so we can
	// print what was said even while the session is still alive.
	stream, err := c.Attach(ctx, sessionID, nil, true)
	if err != nil {
		return err
	}
	captured := &syncBuffer{}
	go func() { _, _ = io.Copy(captured, stream) }()

	// The actual wait: no polling, no output parsing.
	result, err := waitForQuietOrExit(ctx, events, sessionID)
	if err != nil {
		return err
	}
	// The reader is a separate goroutine; give the last bytes a moment to
	// land before we snapshot (silence means output stopped, not that our
	// socket has drained).
	time.Sleep(50 * time.Millisecond)

	output := captured.String()
	fmt.Println("── output ──")
	fmt.Print(output)
	if !strings.HasSuffix(output, "\n") {
		fmt.Println()
	}

	fmt.Println("── verdict ──")
	if result.exited {
		fmt.Printf("command exited with code %d\n", result.exitCode)
		return nil
	}
	fmt.Printf("no output for %sms — the command is probably done, or waiting for input\n", result.quietMs)
	fmt.Printf("the session is STILL ALIVE: `runbaypty attach %s` to take over\n", sessionID)
	return nil
}

// waitForQuietOrExit consumes the event stream until this session either
// goes silent or exits. This is the whole point of the example: the decision
// costs zero output bytes and zero polling.
func waitForQuietOrExit(ctx context.Context, events <-chan client.Event, sessionID string) (waitResult, error) {
	for {
		select {
		case <-ctx.Done():
			return waitResult{}, fmt.Errorf("gave up after %v: the command neither went quiet nor exited", hardTimeout)

		case ev, ok := <-events:
			if !ok {
				return waitResult{}, errors.New("daemon closed the event stream")
			}
			if ev.SessionID != sessionID {
				continue
			}
			switch ev.Type {
			case proto.EventSilence:
				return waitResult{quietMs: ev.Data[proto.DataKeyQuietMs]}, nil
			case proto.EventExited:
				code, _ := strconv.Atoi(ev.Data[proto.DataKeyExitCode])
				return waitResult{exited: true, exitCode: code}, nil
			}
		}
	}
}

// syncBuffer is an io.Writer whose contents can be read at any time from
// another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
