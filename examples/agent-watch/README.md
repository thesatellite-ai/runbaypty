# agent-watch

**Use case:** know what every session is doing — started, went quiet, rang the bell, finished a command, died — without polling anything and without shipping a single output byte to a watcher that isn't reading them.

This is the primitive AI-agent supervisors keep hand-rolling badly. runbaypty pushes it as structured events.

## Run it

```sh
go run ./examples/agent-watch      # leave it running
```

Then, in another terminal, give it something to watch:

```sh
bin/runbaypty run -- sh -c 'echo starting; sleep 8; echo done'
bin/runbaypty run -- sh -c 'printf "\a"'      # rings the bell
bin/runbaypty run -- sh -c 'exit 3'
```

You'll see:

```
watching daemon events — ctrl-c to stop
[14:22:01] ses_… woke up after 0ms
[14:22:06] ses_… quiet for 5000ms — probably done
[14:22:09] ses_… exited (0)
[14:22:14] ses_… BELL
[14:22:20] ses_… exited (3)
```

## Why events, not polling

An agent that wants to know "did the build finish?" has three bad options and one good one:

1. **Poll the output** — you must ship every byte to a process that only cares about the *last* one, and you still have to guess what "finished" looks like.
2. **Parse the terminal** — now you're a terminal emulator, and a progress bar redrawing looks like activity forever.
3. **Wait for process exit** — useless for a shell session that stays alive between commands.
4. **Subscribe to events** — the daemon already knows when output stopped. It tells you.

The `silence` event is the workhorse: emitted once per quiet period when a session produces no output for the threshold (5 s by default). "Output has been silent for 5 seconds" is, empirically, an excellent proxy for "the command probably finished" — and it costs zero bytes on the wire.

## The event types

| Event | Fires when | Data |
|---|---|---|
| `created` / `exited` | session lifecycle | `cmd`, `name` / `exit_code`, `signal?` |
| `activity` | output resumes after a quiet period | `quiet_ms` |
| `silence` | no output for the silence threshold | `quiet_ms` |
| `bell` | a real BEL byte (OSC terminators excluded) | — |
| `command-started` | OSC 133 command began | `start_seq` |
| `command-finished` | OSC 133 command ended | `start_seq`, `end_seq`, `exit_code` (code only if the `D` mark carried one) |
| `attached` / `detached` | a client subscribed or left | `client` |
| `resized` / `renamed` / `meta-changed` | metadata changed | varies |
| `daemon-stopping` | the daemon is shutting down | — |

Delivery is best-effort by design: a subscriber that stops reading drops events rather than stalling the data plane. Events are *advisory signals*; the byte stream is the source of truth.

## The code

```go
events, err := c.SubscribeEvents(ctx, "" /* "" = all sessions, or a session id */)
for ev := range events {
    switch ev.Type {
    case proto.EventSilence:
        // ev.Data["quiet_ms"] — the command is probably done
    case proto.EventCommandFinished:
        // ev.Data["exit_code"] — real per-command status, from OSC 133
    }
}
```

## Next

- [wait-for-silence](../wait-for-silence/) — turn the `silence` event into a blocking "run and wait" primitive
- [command-boundaries](../command-boundaries/) — per-command exit codes and output windows via OSC 133
- [multi-agent-supervisor](../multi-agent-supervisor/) — one watcher, many agent sessions, reacting per-session
