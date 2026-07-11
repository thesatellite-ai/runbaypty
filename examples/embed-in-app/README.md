# embed-in-app

**Use case:** you're building a Go program — a CI tool, a build orchestrator, a task runner, an installer — and one part of it needs to run a command in a real PTY and consume its output. You don't want a terminal *client*; you want the terminal-session capability as a **library** inside your own code, hidden behind your own API.

## Run it

```sh
go run ./examples/embed-in-app
```

```
app: running an embedded terminal session and classifying its output

app result: 7 lines, exit code 2

  pass   3
  warn   1
  error  2
  other  1

app verdict: FAILED — 2 error line(s), exit 2
```

The program runs a stand-in "build," classifies every line it emits into the app's own categories, and renders an app-level verdict. Nothing in the output betrays that a PTY, a socket, or a wire protocol was involved — which is exactly the point.

## The whole idea: the SDK is a component, not the program

hello-session reads a session and prints it — the SDK *is* the program. This is the opposite: the SDK is one component behind an app-level type, and the rest of the program never touches runbaypty.

That type is `Runner`:

```go
type Runner struct {
    c *client.Client   // the SDK, contained
}

func NewRunner(socketPath string) (*Runner, error)
func (r *Runner) Run(ctx, cmd string, args ...string) (Tally, error)
func (r *Runner) Close() error
```

Everything runbaypty-specific — dial, spawn, attach, read-to-EOF, read the exit code — lives inside `Runner.Run`. The caller gets a `Tally` (the app's vocabulary) and never sees a `Stream`, a seq, or a frame. Lift `Runner` into any Go project and the capability travels with it, with zero protocol leakage into your call sites. That's the modularity the SDK is designed for.

## The crux: `Stream` is an `io.Reader`

The integration surface between runbaypty and your program is one interface you already know:

```go
stream, _ := r.c.Attach(ctx, id, nil, true) // Stream implements io.Reader
sc := bufio.NewScanner(stream)              // ← ordinary stdlib, no PTY awareness
for sc.Scan() {
    tally.Counts[classify(sc.Text())]++
}
```

`bufio.NewScanner` has no idea it's reading a pseudo-terminal over a Unix socket. It's reading a `Reader`. That's the reason embedding is clean: anywhere your program can take an `io.Reader` — `io.Copy`, `bufio.Scanner`, `json.NewDecoder`, a pipe into another stage — a runbaypty session drops straight in. When the process exits and the ring drains, `Read` returns `io.EOF` and the loop ends naturally.

The exit code comes off the same stream after EOF:

```go
code, _, _ := stream.Exit()
```

## Translate at the boundary, speak your own vocabulary everywhere else

The app's categories — `pass`, `warn`, `error`, `other` — are a typed closed set (`lineClass` constants with a canonical `lineClassValues` order for stable rendering). The daemon knows nothing about them.

`classify()` is the single boundary where free-form program text (`ERROR`, `WARN`, `PASS`, `FAIL`, `OK`) is translated *into* that vocabulary. Those substrings are the watched program's output — genuine external content, not our constants — so matching them literally is correct boundary behavior. Past `classify()`, the app deals only in `lineClass`; `appVerdict()` is plain business logic that couldn't care less that the data originated in a PTY.

This is the general pattern: **parse external shapes into your own types at the point of collection, and keep the rest of the program in your own terms.**

## Why a PTY at all, versus `os/exec`

You could run the command with `os/exec` and read its pipe. Embedding runbaypty instead buys you everything the daemon provides for free:

- the process **survives your program exiting or crashing** — reattach later and read from where you left off (see [reattach-zero-gap](../reattach-zero-gap/));
- it's a **real PTY**, so programs that behave differently when attached to a terminal (colors, progress bars, `isatty` checks) behave the way they would for a human;
- **other clients can attach to the same session** — your app consumes it while a dashboard watches it, with no contention (see [slow-consumer](../slow-consumer/));
- you get the **durable log, the event stream, and watches** on the same session whenever you want them.

`os/exec` gives you a subprocess bound to your process's lifetime. Embedding runbaypty gives you a *durable, shareable, observable* subprocess behind the same `io.Reader` you'd use either way.

## Making it real

Point `Run` at `make`, `npm test`, `terraform apply`, `cargo build` — anything that streams categorizable output and returns an exit code. Add an `Input`-driven method to `Runner` if your app also needs to *drive* the session (see [expect-watch](../expect-watch/)). The shape doesn't change: SDK behind your type, `io.Reader` into your logic, your vocabulary out.

## Next

- [hello-session](../hello-session/) — the SDK as the whole program, for contrast
- [reattach-zero-gap](../reattach-zero-gap/) — the durability that embedding a PTY buys you over `os/exec`
- [record-and-export](../record-and-export/) — capture that embedded session to a replayable cast
