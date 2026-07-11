# core

Read this first. It is the minimum an agent needs to drive runbaypty.

## What runbaypty is

A daemon that owns terminal (PTY) sessions. You start processes inside it; they keep running even after the client that started them exits, crashes, or disconnects. You attach to read their output and send input, detach, and reattach later exactly where you left off. The daemon is policy-free: it holds processes and bytes, nothing more.

`runbaypty serve` is the daemon. Every other verb is a client that talks to it over a Unix socket (or a loopback WebSocket).

## Finding the daemon

Clients connect to the daemon's socket. Point them at it with an env var (set once per shell):

- `RUNBAYPTY_SOCK`: the Unix socket path (default `~/.runbaypty/runbaypty.sock`).
- `RUNBAYPTY_HOME`: the daemon's home dir, where the discovery file and WebSocket tokens live.

If a command prints `E_DAEMON_UNREACHABLE`, the daemon is not running (or you are pointed at the wrong socket). Start it with `runbaypty serve &`, or check `runbaypty daemon status`.

## The essential loop: spawn, drive, read

```sh
# 1. spawn a session (returns its id; --name gives it a friendly handle)
runbaypty run --name work -- /bin/sh -i

# 2. list what is running
runbaypty ls

# 3. attach: a live terminal into the session (detach with ctrl-\)
runbaypty attach work

# 4. reattach any time: scrollback replays, the process never stopped
runbaypty attach work
```

`run -- <cmd>` spawns a PTY running `<cmd>`. Everything after `--` is the command. Use an interactive shell (`/bin/sh -i`, `bash`) when you want to keep sending commands; use a bare command (`-- make -j8`) for a one-shot job.

## Sending input without an interactive attach

Pipe stdin into `attach` to send commands non-interactively:

```sh
printf 'echo hello\nuname -s\nexit\n' | runbaypty attach work
```

Only one client can type at a time (the write lock). `attach` takes it; a second `attach` steals it. Use `attach --read-only` to watch without taking the keyboard.

## Reading output programmatically

- `runbaypty attach <id|name> --read-only` streams live output (pipe it, apply a timeout, grep it).
- `runbaypty tail <id|name>` prints history then follows; `--no-follow` prints and exits.
- With `--log <path>` at spawn time, the full history is written to disk; read it back with `tail` or `export`.

## Common failure signals

- `E_DAEMON_UNREACHABLE`: daemon not running / wrong socket.
- `E_SESSION_NOT_FOUND`: no session with that id or name (it may have exited and been reaped).
- `E_NO_WRITE_LOCK`: you tried to send input without the write lock; another client holds it (attach to take it).
- `E_READ_ONLY_SCOPE`: a control verb was refused on a read-only WebSocket token.

Run `runbaypty errors` to see the full stable error-code registry.

## Where to go next

Read the other skills for depth: `sessions` (driving a session, the write lock), `history` (logs, replay), `events` (know when a command finished, per-command exit codes), `remote` (WebSocket, ssh forwarding), `sdk` (the Go client library). List them with `runbaypty skills`.
