<div align="center">

# runbaypty

**The persistent, programmable PTY daemon.**

[![Go Reference](https://pkg.go.dev/badge/github.com/thesatellite-ai/runbaypty.svg)](https://pkg.go.dev/github.com/thesatellite-ai/runbaypty)
[![Go Report Card](https://goreportcard.com/badge/github.com/thesatellite-ai/runbaypty)](https://goreportcard.com/report/github.com/thesatellite-ai/runbaypty)
[![test](https://github.com/thesatellite-ai/runbaypty/actions/workflows/test.yml/badge.svg)](https://github.com/thesatellite-ai/runbaypty/actions/workflows/test.yml)
[![lint](https://github.com/thesatellite-ai/runbaypty/actions/workflows/lint.yml/badge.svg)](https://github.com/thesatellite-ai/runbaypty/actions/workflows/lint.yml)
[![coverage](https://codecov.io/gh/thesatellite-ai/runbaypty/graph/badge.svg)](https://codecov.io/gh/thesatellite-ai/runbaypty)
[![tag](https://img.shields.io/github/v/tag/thesatellite-ai/runbaypty?sort=semver&label=tag&color=blue)](https://github.com/thesatellite-ai/runbaypty/tags)
[![License](https://img.shields.io/github/license/thesatellite-ai/runbaypty?color=blue)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/thesatellite-ai/runbaypty)](go.mod)

*Terminal sessions that survive client rebuilds, crashes, quits — and even daemon upgrades.*

</div>

A tiny OS-managed daemon owns your PTY sessions — dev servers, AI agents, builds, shells — so no app ever has to. Clients connect over a Unix socket or WebSocket, stream bytes with provable zero-gap replay, detach, and reattach. Policy-free by design: no database, no recipes, no restarts — it holds processes and bytes, bulletproof, and everything smart lives in clients.

> Status: **pre-alpha**. The v1 protocol and feature surface are implemented and heavily tested; the wire protocol is additive-only from here.

## Contents

- [Why runbaypty?](#why-runbaypty)
- [Quick start](#quick-start)
- [Try it in the browser](#try-it-in-the-browser)
- [Commands](#commands)
- [Highlights](#highlights)
- [How it compares](#how-it-compares)
- [FAQ](#faq)
- [Docs](#docs)
- [Testing philosophy](#testing-philosophy)
- [Platform](#platform)
- [License](#license)

## Why runbaypty?

Every tool that needs a long-lived terminal process rebuilds the same stack: spawn a PTY, buffer scrollback, multiplex clients, survive disconnects. tmux solved it for humans at a keyboard; nothing solved it for **programs**. runbaypty is that missing primitive — dtach's raw byte passthrough + Eternal Terminal's sequence-numbered replay + shpool's OS-managed lifecycle + ttyd's browser reach + a documented binary protocol and a first-class Go SDK, in one policy-free daemon. (Backed by field research across 37 PTY tools.)

Deliberately **not** in the daemon: no database, no session recipes, no auto-restart, no panes/layouts, no screen-grid emulation. That seam is why it never needs to churn — and rebuild-stability is the entire point.

## Quick start

```sh
go build -o bin/runbaypty ./cmd/runbaypty

bin/runbaypty serve &                        # dev; `daemon install` for launchd/systemd
bin/runbaypty run --name dev -- npm run dev  # spawn a session
bin/runbaypty ls
bin/runbaypty attach dev                     # full terminal; detach with ctrl-\
# … rebuild your app, quit your terminal, come back:
bin/runbaypty attach dev                     # scrollback replays, session never blinked
```

## Try it in the browser

Prefer to *see* it? [`examples/terminal-playground`](examples/terminal-playground/) is a full web control panel — a real xterm.js terminal plus clickable UI for nearly the entire protocol: spawn sessions, take the write lock and type, watch the live event stream, arm server-side regex watches, inspect OSC 133 command blocks with their exit codes, toggle read-only, and watch it auto-reconnect with zero-gap resume. It's the fastest way to understand what the daemon does — as a UI you can poke at.

[![runbaypty terminal playground — session list and spawn form on the left, an xterm.js terminal in the center, and live Info / Events / Watch / Cmds panels on the right](examples/terminal-playground/screenshot.png)](examples/terminal-playground/)

One command spins up an isolated daemon, serves the UI, and opens it in your browser:

```sh
task -d examples/terminal-playground play
```

For the minimal "just a terminal in a tab" version, see [`examples/browser-terminal`](examples/browser-terminal/); for the protocol by hand, [`examples/raw-protocol-node`](examples/raw-protocol-node/).

## Commands

`runbaypty serve` is the daemon; every other verb is a client that talks to it over the socket. Every command takes `--help` (with examples), and `--json` where machine-readable output makes sense. Run `runbaypty` with no arguments to see the whole surface:

```console
$ runbaypty
runbaypty owns PTY sessions so no app has to.

A tiny OS-managed daemon holds your terminal sessions (dev servers, agents,
shells) so they survive any client rebuild, crash, or quit. Clients connect
over a Unix socket or WebSocket, stream bytes with zero-gap replay, detach,
and reattach. Policy-free by design: no DB, no recipes, no restarts.

Usage:
  runbaypty [command]

Available Commands:
  attach      Attach your terminal to a session (detach: ctrl-\)
  completion  Generate the autocompletion script for the specified shell
  daemon      Install and control the OS-managed daemon (launchd / systemd)
  errors      Inspect the stable error-code registry
  events      Stream daemon lifecycle events (created/exited/silence/bell/…)
  export      Convert a durable session log to an asciinema cast (v2)
  help        Help about any command
  info        Show one session's detail
  kill        Signal a session's whole process tree
  lastcmd     Print the last completed command's output (needs OSC 133 shell integration)
  ls          List sessions
  meta        Manage a session's JSON metadata document
  rename      Rename a session (empty string clears the name)
  resize      Resize a session's grid (last writer wins)
  run         Spawn a new PTY session in the daemon
  serve       Run the runbaypty daemon
  skills      List the built-in agent skills (guides an agent reads, then acts on)
  tail        Print a session's full history (durable log + ring), then follow live
  version     Print version information

Flags:
      --color string   color output: auto | always | never (default "auto")
  -h, --help           help for runbaypty
      --sock string    daemon socket path (default: $RUNBAYPTY_SOCK or ~/.runbaypty/runbaypty.sock)
  -v, --version        version for runbaypty

Use "runbaypty [command] --help" for more information about a command.
```

**The daemon**

| Command | What it does |
|---|---|
| `serve` | Run the daemon in the foreground (what a launchd/systemd unit points at). `--ws-port N` also enables the loopback WebSocket. |
| `daemon install` | Copy the binary to a stable path and register the OS login service (launchd / systemd). |
| `daemon start` / `daemon stop` | Start (or restart) / stop the installed service. |
| `daemon status` | Report daemon liveness from the discovery file. |
| `daemon uninstall` | Stop the service and remove the registration (sessions die). |

**Spawn and interact with sessions**

| Command | What it does |
|---|---|
| `run -- <cmd>` | Spawn a new PTY session. Flags: `--name`, `--log <path>` (durable history), `--ring <bytes>`, `--cwd`, `--json`. |
| `attach <id\|name>` | Attach your terminal to a session. Detach with `ctrl-\`; `--read-only` to watch without typing. |
| `kill <id\|name>` | Signal the session's whole process tree (`--signal TERM\|KILL\|INT\|HUP`). |
| `resize <id\|name>` | Set the session's grid (cols/rows); last writer wins. |
| `rename <id\|name>` | Change a session's name (empty string clears it). |
| `meta <id\|name>` | Manage a session's JSON metadata: `get` / `merge` / `replace` / `unset` / `incr` (`k=v` string, `k:=v` JSON, dotted keys, `--json`, `--if-version`). |

**Inspect**

| Command | What it does |
|---|---|
| `ls` | List sessions (`--filter key=val` keeps those whose meta matches; repeatable = AND). |
| `info <id\|name>` | Show one session's full detail (`--json`). |
| `events` | Stream lifecycle events (created/exited/silence/activity/bell/command-\*). `--json`, `--session <id>`. |
| `errors` | Inspect the stable error-code registry. |
| `skills` | List the built-in agent guides; `skills get <name>` prints one, `--json` for machine-readable. |
| `version` | Print version information. |

**History and recording**

| Command | What it does |
|---|---|
| `tail <id\|name>` | Print a session's full history (durable log + ring), then follow live. `--no-follow` prints and exits. |
| `export <log-file>` | Convert a durable session log to an asciinema cast v2 (`--out`, `--title`). No daemon needed. |
| `lastcmd <id\|name>` | Print the last completed command's output (needs OSC 133 shell integration). |

Clients find the daemon via `RUNBAYPTY_SOCK` (socket path) and `RUNBAYPTY_HOME` (home dir, where the discovery file and WS tokens live); `--sock` overrides the socket per command. For copy-paste tours of these commands, see the [snippets playground](examples/snippets/).

## Highlights

- **Zero-gap replay, provable** — every output byte has a sequence number; `ATTACH {since_seq}` resumes exactly where you left off. Continuity is arithmetic the client can audit.
- **Zero-downtime daemon upgrades** — `serve --takeover` hands PTY fds + state to the new binary over `SCM_RIGHTS`; sessions keep their pid. Not even tmux can do this.
- **Agent-era primitives** — silence/activity/bell events (know when a command probably finished without polling), OSC 133 command tracking (`command-finished {exit_code}` + replay exactly one command's output), server-side regex `WATCH`, single write lock with explicit takeover (human ⇄ agent handoff).
- **Two transports, one codec** — 0600 Unix socket (file perms are the auth) + loopback WebSocket with scoped tokens (control / read-only) for browsers. [`examples/terminal-playground`](examples/terminal-playground/) is a full xterm.js control panel over that WebSocket.
- **Durable logs → asciinema** — opt-in `(timestamp, bytes)` session logs; `runbaypty export` emits a playable `.cast`. `tail` stitches log history with the live stream, exact to the byte.
- **Go SDK** — `pkg/client`: `Spawn/Attach/Watch/Follow` — `Follow` is an `io.Reader` with automatic reconnect + zero-gap resume built in. See [`examples/`](examples/).
- **Operationally boring on purpose** — launchd/systemd install, crash recovery over stale state, retention TTLs, session + ring-memory caps, structured logs, discovery file.

## How it compares

| | runbaypty | tmux / screen | dtach / abduco | shpool | ttyd / gotty |
|---|---|---|---|---|---|
| Sessions survive client death | ✅ | ✅ | ✅ | ✅ | ❌ |
| Survives **daemon upgrade** | ✅ fd handover | ❌ | ❌ | ❌ | ❌ |
| Zero-gap reattach (seq-numbered) | ✅ | ❌ redraw | ❌ | ❌ redraw | ❌ |
| Raw byte stream (no grid re-emulation) | ✅ | ❌ grid | ✅ | ❌ vt100 | ✅ relay |
| Documented programmable protocol + SDK | ✅ binary + Go SDK | ⚠️ text control mode | ❌ | ❌ | ❌ |
| Browser / WebSocket access | ✅ scoped tokens | ❌ | ❌ | ❌ | ✅ no persistence |
| OS-managed lifecycle (launchd/systemd) | ✅ | ❌ self-spawned | ❌ | ✅ | ❌ |
| Agent events (silence, OSC 133, watch) | ✅ | ⚠️ activity hooks | ❌ | ❌ | ❌ |

tmux remains the right answer for a human at an SSH prompt who wants panes. runbaypty is the layer *underneath* products — apps, agents, IDEs. You can even run tmux inside a runbaypty session.

## FAQ

**Is this a tmux replacement?**
No — a different layer. tmux is a terminal *product* for humans (panes, status bar, keybindings). runbaypty is PTY *infrastructure* for programs: a documented protocol, an SDK, and byte-exact streams. Think containerd, not Docker Desktop.

**What happens to my sessions when the daemon itself updates?**
They survive. `serve --takeover` transfers each session's PTY file descriptor and state to the new daemon over `SCM_RIGHTS`; processes keep running with the same pid and the stream stays contiguous — verified by a mid-upgrade sequence audit in CI.

**How is "zero-gap replay" different from tmux reattach?**
tmux redraws its screen grid. runbaypty replays the exact byte stream from the sequence number you last saw — no repainting, no lost output, and the client can verify continuity arithmetically.

**Can AI agents use it without parsing terminal escape codes?**
Yes — that's the point of the event stream: `silence` ("probably done"), OSC 133 `command-finished {exit_code}`, server-side regex `WATCH`, and last-command replay give agents structured signals while the raw bytes stay untouched for rendering.

**Does it work in the browser?**
Yes. The daemon serves a loopback WebSocket with scoped tokens (control vs read-only). [`examples/terminal-playground`](examples/terminal-playground/) is a full xterm.js control panel (`task -d examples/terminal-playground play`); [`examples/browser-terminal`](examples/browser-terminal/) is the minimal one-file version.

**What about Windows?**
Planned (ConPTY, build-tagged backend) — deliberately after v1. macOS and Linux are the current targets.

**How do I keep sessions across a reboot?**
You can't — no tool can; processes don't survive reboots. That's the one honest boundary: restart policies belong to clients (which is why the daemon has none).

## Docs

Design docs (protocol reference, upgrade-handover design, competitive landscape, mission/non-goals, benchmarks) are maintained internally and will be published as they stabilize. Until then: the protocol is fully expressed by `pkg/proto` (a doc-drift test keeps the internal reference honest), and `runbaypty errors list` + `--help` on every verb are self-documenting. Benchmarks: ~65 MB/s firehose end-to-end, ~41 µs keystroke round trip (Apple M1 Max).

## Testing philosophy

`go test -race ./...` — everything: goleak on every daemon/engine suite, fuzzers on the frame decoder and escape scanner, a chaos proxy that assassinates connections mid-stream, PTY-driven CLI tests, a mid-upgrade seq audit, and a soak that churns hundreds of sessions asserting flat goroutine/heap curves. macOS and Linux CI; nightly deep fuzz + 10-minute soak.

## Platform

macOS and Linux (v1). Windows ConPTY is a planned differentiator — the field research says almost nobody has it; the codebase is structured for a build-tagged backend.

## License

[Apache-2.0](LICENSE) © 2026 [khanakia](https://github.com/khanakia) · thesatellite-ai
