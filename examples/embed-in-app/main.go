// embed-in-app shows runbaypty's SDK used as a LIBRARY inside a larger Go
// program, not as a standalone client. The terminal session is a component of
// the app: its output stream is a plain io.Reader that you plumb into your own
// abstractions — here a bufio.Scanner feeding a typed line classifier.
//
// The point that separates this from hello-session: hello-session reads a
// session and prints it. This wraps the SDK behind an app-level type (Runner)
// with its own API (Run → Tally), so the rest of the program never touches
// runbaypty directly. That's the shape you want when a terminal session is one
// part of a bigger system — a CI tool, a build orchestrator, a task runner —
// rather than the whole program.
//
// Run:
//
//	go run ./examples/embed-in-app
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

// lineClass is the closed set of categories the app sorts output lines into.
// This is the APP's vocabulary — runbaypty knows nothing about it. Named
// constants (never bare strings) so the classifier, the tally, and the render
// all agree on the category names.
type lineClass string

const (
	classPass  lineClass = "pass"
	classWarn  lineClass = "warn"
	classError lineClass = "error"
	classOther lineClass = "other"
)

// lineClassValues is the canonical iteration order for rendering the tally, so
// the summary lists categories in a stable, meaningful order (not map order).
var lineClassValues = []lineClass{classPass, classWarn, classError, classOther}

// classify buckets one line of program output. The substrings it looks for are
// the CONTENT of the watched program (free-form upstream text), not a closed
// set of our own — so matching on them literally is the correct boundary
// behavior: we translate external text INTO our lineClass vocabulary here, at
// the point of collection, and the rest of the app speaks only lineClass.
func classify(line string) lineClass {
	switch {
	case strings.Contains(line, "ERROR"), strings.Contains(line, "FAIL"):
		return classError
	case strings.Contains(line, "WARN"):
		return classWarn
	case strings.Contains(line, "PASS"), strings.Contains(line, "OK"):
		return classPass
	default:
		return classOther
	}
}

// Tally is the app-level result of running a command: how its output broke down
// by category, plus the process exit code. This is what the Runner hands back —
// the caller never sees a Stream, a seq, or a frame.
type Tally struct {
	Counts   map[lineClass]int
	Lines    int
	ExitCode int
}

// Runner embeds a runbaypty client and exposes a higher-level API over it. It
// is the seam: everything runbaypty-specific lives behind this type, so the
// rest of a program depends on Runner, not on the SDK. Lift this type into any
// Go project and the terminal-session capability comes with it, with no leakage
// of protocol details into the caller.
type Runner struct {
	c *client.Client
}

// NewRunner dials the daemon and returns a Runner ready to Run commands.
func NewRunner(socketPath string) (*Runner, error) {
	c, err := client.Dial(socketPath)
	if err != nil {
		return nil, err
	}
	return &Runner{c: c}, nil
}

// Close releases the underlying connection.
func (r *Runner) Close() error { return r.c.Close() }

// Run spawns cmd in a PTY, consumes its output line by line through the app's
// classifier, and returns a Tally once the process exits. The whole SDK
// interaction — spawn, attach, read to EOF, read the exit code — is contained
// here; the io.Reader plumbing (bufio.Scanner over the Stream) is the crux.
func (r *Runner) Run(ctx context.Context, cmd string, args ...string) (Tally, error) {
	id, _, err := r.c.Spawn(ctx, client.SpawnOpts{Cmd: cmd, Args: args})
	if err != nil {
		return Tally{}, fmt.Errorf("spawn: %w", err)
	}
	// Attach read-only: this app only consumes output, it never types. The
	// returned Stream IS an io.Reader — that's the entire integration surface.
	stream, err := r.c.Attach(ctx, id, nil, true)
	if err != nil {
		return Tally{}, fmt.Errorf("attach: %w", err)
	}

	tally := Tally{Counts: map[lineClass]int{}}
	// A bufio.Scanner is the idiomatic way to read an io.Reader line by line.
	// Nothing here knows it's reading a PTY over a socket — it's just a Reader,
	// which is the whole reason embedding is clean. When the process exits and
	// the ring drains, Read returns io.EOF and the scan loop ends.
	sc := bufio.NewScanner(stream)
	for sc.Scan() {
		line := sc.Text()
		tally.Lines++
		tally.Counts[classify(line)]++
	}
	if err := sc.Err(); err != nil {
		return Tally{}, fmt.Errorf("read: %w", err)
	}

	// The exit code is read from the Stream after EOF — it carries the terminal
	// result of the process the app just consumed.
	code, _, _ := stream.Exit()
	tally.ExitCode = code
	return tally, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embed-in-app:", err)
		os.Exit(1)
	}
}

func run() error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	runner, err := NewRunner(sock)
	if err != nil {
		return err
	}
	defer func() { _ = runner.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A stand-in "build" that emits a realistic mix of lines the app will
	// classify. In a real program this would be `make`, `npm test`, a linter —
	// anything that streams categorizable output and returns an exit code.
	script := `
echo "PASS  auth/login"
echo "PASS  auth/logout"
echo "WARN  deprecated flag --old"
echo "PASS  billing/charge"
echo "ERROR db/migrate: column exists"
echo "compiling module orders..."
echo "FAIL  orders/checkout"
exit 2
`
	fmt.Println("app: running an embedded terminal session and classifying its output")
	fmt.Println()

	tally, err := runner.Run(ctx, "/bin/sh", "-c", script)
	if err != nil {
		return err
	}

	// From here the app works purely in its own terms — Tally, lineClass — with
	// no trace of the SDK. This is the payoff of embedding behind Runner.
	fmt.Printf("app result: %d lines, exit code %d\n\n", tally.Lines, tally.ExitCode)
	for _, cls := range lineClassValues {
		fmt.Printf("  %-6s %d\n", cls, tally.Counts[cls])
	}
	fmt.Println()

	// The app's own verdict, derived from its own state — the reason it embedded
	// a terminal in the first place.
	verdict := appVerdict(tally)
	fmt.Println("app verdict:", verdict)
	return nil
}

// appVerdict turns the tally into the app's decision. Demonstrates that once
// output is in the app's vocabulary, the terminal session's origin is
// irrelevant — this is ordinary business logic.
func appVerdict(t Tally) string {
	switch {
	case t.Counts[classError] > 0 || t.ExitCode != 0:
		return fmt.Sprintf("FAILED — %d error line(s), exit %d", t.Counts[classError], t.ExitCode)
	case t.Counts[classWarn] > 0:
		return fmt.Sprintf("PASSED WITH WARNINGS — %d warning(s)", t.Counts[classWarn])
	default:
		return "CLEAN"
	}
}
