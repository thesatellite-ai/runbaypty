// expect-watch drives an interactive program the way `expect(1)` does —
// wait for a prompt to appear, send an answer, repeat — but the pattern
// matching runs INSIDE the daemon.
//
// A server-side WATCH registers a regular expression on a session's future
// output. The daemon scans the stream and pushes a WATCH_EVENT the instant
// the pattern matches, carrying the matched text and its sequence position.
// An idle watcher receives zero output bytes — only the matches. That makes
// "wait for this prompt" cheap even across many sessions, and correct even
// when a prompt lands split across two output chunks (the daemon keeps a
// bounded overlap between scans).
//
// This program spawns a tiny interactive quiz and answers each question as
// its prompt appears.
//
// Run:
//
//	go run ./examples/expect-watch
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

// A stand-in for any interactive program: it prints prompts and reads
// answers from its PTY. read waits for real input, so this genuinely blocks
// until we type — there is no sleep-based faking here.
const interactiveScript = `
printf 'name> '; read name
printf 'color> '; read color
printf 'confirm (yes/no)> '; read ok
echo "hello $name, your $color answer is: $ok"
`

// exchange is one wait-for-prompt / send-answer step.
type exchange struct {
	prompt string // a regex matched against the session's output
	answer string // what to type when it appears
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "expect-watch:", err)
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

	sessionID, _, err := c.Spawn(ctx, client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", interactiveScript}})
	if err != nil {
		return err
	}
	defer func() { _ = c.Kill(context.Background(), sessionID, "") }()

	// We are going to type, so take the write lock. Without it INPUT is
	// refused (an unheld lock permits input, but claiming it makes intent
	// explicit and blocks anyone else from typing over us).
	if err := c.TakeWrite(ctx, sessionID); err != nil {
		return err
	}

	// Watch for ALL the prompts up front. Each registration returns its own
	// channel of matches. Registering before the program prints guarantees
	// we can't miss a prompt to a race (watches see future output; we
	// register while the shell is still starting).
	script := []exchange{
		{prompt: `name> `, answer: "ada"},
		{prompt: `color> `, answer: "green"},
		{prompt: `confirm \(yes/no\)> `, answer: "yes"},
	}

	fmt.Printf("session %s — driving the interactive quiz\n\n", sessionID)
	for _, step := range script {
		matches, err := c.Watch(ctx, sessionID, step.prompt)
		if err != nil {
			return fmt.Errorf("watch %q: %w", step.prompt, err)
		}
		// Block until the prompt appears — the daemon pushes the match; we
		// ship no output bytes waiting for it.
		select {
		case ev := <-matches:
			fmt.Printf("saw prompt %q at seq %d → answering %q\n", ev.Match, ev.Seq, step.answer)
		case <-ctx.Done():
			return fmt.Errorf("prompt %q never appeared", step.prompt)
		}
		// Type the answer, with the newline that a `read` needs to return.
		if err := c.Input(sessionID, []byte(step.answer+"\n")); err != nil {
			return err
		}
	}

	// Read the final line the program printed with our answers.
	stream, err := c.Attach(ctx, sessionID, nil, true)
	if err != nil {
		return err
	}
	var acc strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := stream.Read(buf)
		acc.Write(buf[:n])
		if idx := strings.Index(acc.String(), "hello ada"); idx >= 0 {
			fmt.Printf("\nprogram result: %s", strings.TrimSpace(acc.String()[idx:]))
			return nil
		}
		if rerr != nil {
			break
		}
	}
	fmt.Println("\n(the quiz completed; `runbaypty attach` for full output)")
	return nil
}
