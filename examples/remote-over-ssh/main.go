// remote-over-ssh uses a daemon running on another machine as if it were
// local — no remote agent, no daemon changes, no runbaypty-specific network
// code. The trick is entirely in the transport: ssh forwards the remote
// daemon's Unix socket to a local path, and because every runbaypty client
// dials a socket PATH (not a host:port it interprets), pointing it at the
// forwarded path is all it takes. The client is transport-agnostic by
// construction; ssh does the rest.
//
// The magic is one ssh flag (see the README):
//
//	ssh -N -L /tmp/rpty-remote.sock:/run/user/1000/runbaypty/runbaypty.sock you@host
//
// After that, `RUNBAYPTY_SOCK=/tmp/rpty-remote.sock` makes every client — this
// program, the CLI, anything — operate on the REMOTE daemon. Nothing in THIS
// file knows or cares that the socket is a forward; that's the whole point.
//
// Run (against a local or forwarded socket):
//
//	RUNBAYPTY_SOCK=/tmp/rpty-remote.sock go run ./examples/remote-over-ssh
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "remote-over-ssh:", err)
		os.Exit(1)
	}
}

func run() error {
	// SocketPath honors RUNBAYPTY_SOCK. In production that env var points at the
	// ssh-forwarded socket, so this exact line connects to a daemon on another
	// continent. Locally it's just the local daemon. The code is identical.
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	fmt.Printf("connecting to the daemon at %s\n", sock)
	fmt.Println("(in production this path is an ssh -L forward — see the README)")

	c, err := client.Dial(sock)
	if err != nil {
		return fmt.Errorf("dial %s: %w (is the ssh forward up and the remote daemon running?)", sock, err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// List sessions on the (possibly remote) daemon — proof we're talking to a
	// real daemon over this socket, whatever machine it lives on.
	sessions, err := c.List(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nthe daemon owns %d session(s)\n", len(sessions))

	// Spawn a command that reports WHERE it ran. When the socket is an ssh
	// forward, `hostname` prints the REMOTE host's name — vivid proof that the
	// process is executing on the other machine while we drive it from here.
	fmt.Println("\nspawning `hostname && uname -sm` on the daemon's host:")
	id, _, err := c.Spawn(ctx, client.SpawnOpts{
		Cmd:  "/bin/sh",
		Args: []string{"-c", "hostname; uname -sm"},
	})
	if err != nil {
		return err
	}

	// Read the output the same way as any local session — Attach returns an
	// io.Reader regardless of how many network hops are under the socket.
	stream, err := c.Attach(ctx, id, nil, true)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(stream)
	var lines []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	code, _, _ := stream.Exit()

	fmt.Printf("  host:   %s\n", firstOr(lines, 0, "?"))
	fmt.Printf("  system: %s\n", firstOr(lines, 1, "?"))
	fmt.Printf("  (exit %d)\n", code)

	fmt.Println("\nEverything above used the plain SDK against a socket path. Swap the")
	fmt.Println("path for an ssh -L forward and the identical code drives a daemon on")
	fmt.Println("another machine — no remoting logic here, because the transport is")
	fmt.Println("ssh's job and the client only ever speaks to a local socket.")
	return nil
}

// firstOr returns lines[i] or a fallback if the index is out of range — the
// remote command's output may be shorter than expected on a stripped-down host.
func firstOr(lines []string, i int, fallback string) string {
	if i < len(lines) {
		return lines[i]
	}
	return fallback
}
