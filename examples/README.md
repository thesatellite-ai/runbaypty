# runbaypty examples

Every example is a standalone, runnable program with its own README explaining the use case, the runbaypty concepts it exercises, and what to look for when you run it. Start the daemon once, then work through them:

```sh
go build -o bin/runbaypty ./cmd/runbaypty
bin/runbaypty serve &          # or: bin/runbaypty daemon install
```

Most examples take a session id or name as an argument and clean up after themselves. All of them use the Go SDK (`pkg/client`) except the ones that deliberately speak the raw wire protocol.

## Start here

| Example | Use case | Key concepts |
|---|---|---|
| [hello-session](hello-session/) | Spawn a command, read its output, get the exit code | `Spawn`, `Attach`, `Stream` as `io.Reader`, `Exit()` |
| [reattach-zero-gap](reattach-zero-gap/) | Disconnect mid-stream and resume at the exact byte — the product's core promise | `LastSeq()`, `Attach(since_seq)`, seq audit |
| [follow-resilient](follow-resilient/) | Read a session that survives daemon-connection loss, transparently | `client.Follow`, auto-reconnect + resume |
| [tail-history](tail-history/) | Print a session's complete history, then follow live, byte-exact at the seam | durable log + ring stitched on one seq axis |

## For AI agents

| Example | Use case | Key concepts |
|---|---|---|
| [agent-watch](agent-watch/) | Observe every session's lifecycle without polling or shipping bytes | `SubscribeEvents`, silence/activity/bell |
| [wait-for-silence](wait-for-silence/) | "Is the command done?" — run something, block until output stops, capture it | `silence` event, no polling, no PTY parsing |
| [command-boundaries](command-boundaries/) | Per-command exit codes and output windows from a shell session | OSC 133 events, `LastCommandOutput` |
| [expect-watch](expect-watch/) | Drive an interactive program: wait for a prompt, answer it, repeat | server-side regex `Watch`, `Input` |
| [write-lock-handoff](write-lock-handoff/) | A human takes over a session an agent was driving, then hands it back | `TakeWrite`, `ReleaseWrite`, `WriteRefusal` |
| [multi-agent-supervisor](multi-agent-supervisor/) | One process supervising many agent sessions, reacting as each goes quiet | events + `Watch` + per-session state |

## For apps and infrastructure

| Example | Use case | Key concepts |
|---|---|---|
| [dev-server](dev-server/) | A dev server that outlives every rebuild of the app that started it | named sessions, `linger`, reattach |
| [record-and-export](record-and-export/) | Record a build and publish a playable asciinema cast | `--log`, `(ts,bytes)` format, `export` |
| [session-dashboard](session-dashboard/) | A live table of every session, updated from the event stream | `List`, `Info`, `SubscribeEvents` |
| [embed-in-app](embed-in-app/) | Embed terminal sessions inside your own Go program | SDK as a library, `io.Reader` plumbing |
| [zero-downtime-upgrade](zero-downtime-upgrade/) | Replace the daemon binary without ending a single session | `serve --takeover`, fd handover |
| [slow-consumer](slow-consumer/) | What happens when a reader falls behind the ring buffer | backpressure, `E_RING_GONE`, durable log |

## Transports and other languages

| Example | Use case | Key concepts |
|---|---|---|
| [terminal-playground](terminal-playground/) | A full web control panel — the widest example; nearly the whole protocol from a browser UI | xterm.js, spawn/attach/write-lock, events, watch, OSC 133, `task play` |
| [browser-terminal](browser-terminal/) | A full terminal in the browser over the loopback WebSocket | WS transport, scoped tokens, xterm.js |
| [read-only-share](read-only-share/) | Let someone watch your session without being able to type | read-only token scope, forced RO attach |
| [remote-over-ssh](remote-over-ssh/) | Use a remote machine's daemon as if it were local | `ssh -L` UDS forwarding, no daemon changes |
| [raw-protocol-node](raw-protocol-node/) | Speak the wire protocol directly — no SDK, ~80 lines of JS | framing, HELLO, ATTACH, OUTPUT seq |
| [raw-protocol-python](raw-protocol-python/) | The same, in Python — for scripting and CI glue | framing, control/data plane split |

## Running them

Each directory's README lists its exact command. The Go examples run with `go run`:

```sh
go run ./examples/hello-session -- echo "hello from a PTY"
go run ./examples/wait-for-silence -- npm install
go run ./examples/agent-watch                       # watches everything
```

The WebSocket examples (`raw-protocol-node`, `browser-terminal`, `read-only-share`) need a daemon with the WebSocket listener enabled, and read the daemon's token from its home directory:

```sh
bin/runbaypty serve --ws-port 8377
RUNBAYPTY_HOME=<daemon-home> node examples/raw-protocol-node/main.js
```

`raw-protocol-python` is the exception among the non-Go examples: it speaks the wire protocol over the **Unix socket**, so it needs no WebSocket listener and no token — just `RUNBAYPTY_SOCK`. That contrast (same protocol, WS vs UDS transport) is the point of having both.

[terminal-playground](terminal-playground/) has a one-command runner that handles all of the above for you — it builds, starts an isolated daemon, and opens the UI:

```sh
task -d examples/terminal-playground play
```

## A note on what these demonstrate

runbaypty is deliberately policy-free: the daemon holds processes and bytes, and every decision — when to restart, what a prompt looks like, which agent owns which session — lives in the client. These examples are that client half. Read them as patterns to lift, not as a framework to adopt.
