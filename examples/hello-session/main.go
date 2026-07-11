// hello-session is the 30-second tour: spawn a command in a PTY the daemon
// owns, stream its output, and report how it ended.
//
// The point: your process is not the parent. If this program dies right
// now, the session keeps running inside the daemon and any other client can
// attach to it. That inversion is the whole product.
//
// Run:
//
//	go run ./examples/hello-session -- echo "hello from a PTY"
//	go run ./examples/hello-session -- sh -c 'for i in 1 2 3; do echo $i; sleep 0.3; done'
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hello-session:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	// `go run ./examples/hello-session -- echo hi` hands us the "--" too;
	// drop it so the same invocation works for `go run` and a built binary.
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return fmt.Errorf("usage: hello-session [--] <command> [args…]")
	}

	// The daemon's socket: $RUNBAYPTY_SOCK, else ~/.runbaypty/runbaypty.sock.
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err // E_DAEMON_UNREACHABLE carries a "start the daemon" hint
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Spawn: the command runs in a PTY (so it thinks it has a terminal —
	// colors, line editing, progress bars all behave), parented by the
	// daemon rather than by us.
	sessionID, pid, err := c.Spawn(ctx, client.SpawnOpts{
		Cmd:  argv[0],
		Args: argv[1:],
		Cols: 100, Rows: 30,
	})
	if err != nil {
		return err
	}
	fmt.Printf("session %s · pid %d\n\n", sessionID, pid)

	// Attach: a Stream is an io.Reader over the session's output. Passing
	// nil for sinceSeq replays the whole ring buffer first, so we see
	// everything the command printed even if it started before we attached.
	stream, err := c.Attach(ctx, sessionID, nil, false)
	if err != nil {
		return err
	}

	// Copy until the session exits and its stream drains (then io.EOF).
	if _, err := io.Copy(os.Stdout, stream); err != nil {
		return err
	}

	// The outcome is on the stream, not on an error: a command that fails is
	// not a transport failure.
	code, signal, exited := stream.Exit()
	switch {
	case !exited:
		fmt.Println("\n[stream ended without an exit — did someone detach us?]")
	case signal != "":
		fmt.Printf("\n[killed by signal %s]\n", signal)
	default:
		fmt.Printf("\n[exited with code %d]\n", code)
	}

	// The session lingers (exited sessions are retained ~10 min) so a late
	// client can still replay the death. Reap it now since we are done.
	return c.Kill(ctx, sessionID, "")
}
