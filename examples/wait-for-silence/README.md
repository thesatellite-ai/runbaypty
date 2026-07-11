# wait-for-silence

**Use case:** an agent runs a command and needs to know when it's done — including commands that *don't exit*, like a shell sitting at a prompt or an installer waiting for input.

## Run it

```sh
# Exits on its own → the "exited" verdict.
go run ./examples/wait-for-silence -- sh -c 'echo building; sleep 1; echo built'

# Never exits, but stops printing → the "silence" verdict.
go run ./examples/wait-for-silence -- sh -c 'echo working; sleep 2; echo done; sleep 300'

# Something real.
go run ./examples/wait-for-silence -- npm install
```

The silence case prints:

```
── verdict ──
no output for 5000ms — the command is probably done, or waiting for input
the session is STILL ALIVE: `runbaypty attach ses_…` to take over
```

## Why silence is the right signal

An agent driving a terminal has four ways to know a command finished, and three are bad:

| Approach | Why it fails |
|---|---|
| Poll the output | Ships every byte to a process that only cares about the last one; you still have to guess what "done" looks like |
| Parse the terminal | Now you're a terminal emulator, and a spinner redrawing looks like activity forever |
| Wait for exit | Useless for a shell that survives between commands, or anything interactive |
| **Wait for silence** | The daemon already tracks output timing. It tells you. Zero bytes, zero polling. |

"No output for N seconds" is an imperfect heuristic — a slow compile with a quiet phase can trip it — but it is *cheap*, *general*, and requires no knowledge of the command. Tune the threshold per session, and combine it with the exit event (this example races both) and with [`command-boundaries`](../command-boundaries/) when the shell has OSC 133 integration and can tell you exactly when a command ended.

## The shape

```go
// Subscribe BEFORE spawning — a fast command can go quiet before a later
// subscription would exist.
events, _ := c.SubscribeEvents(ctx, "")
sessionID, _, _ := c.Spawn(ctx, client.SpawnOpts{Cmd: "npm", Args: []string{"install"}})

for ev := range events {
    if ev.SessionID != sessionID { continue }
    switch ev.Type {
    case proto.EventSilence:  // ev.Data["quiet_ms"]
        return "probably done"
    case proto.EventExited:   // ev.Data["exit_code"]
        return "definitely done"
    }
}
```

Note the ordering: subscribe, *then* spawn. Events are pushed, not queued per-client — a subscription created after the fact never sees what already happened.

## The session outlives the verdict

When the verdict is `silence`, the command is still running. That's a feature: the agent decides what happens next, and a human can `runbaypty attach` and take over the very same session with the very same process — see [write-lock-handoff](../write-lock-handoff/).

The example kills the session on exit because it's a demo. A real supervisor would leave it and act on it.

## Tuning

The silence threshold defaults to 5 seconds. It's a per-session property on the engine (`SpawnConfig.SilenceAfter`); the wire `SPAWN` message doesn't expose it yet — if you need per-session thresholds today, watch the `activity` events (which carry `quiet_ms`) and apply your own threshold client-side.

## Next

- [command-boundaries](../command-boundaries/) — exact per-command exit codes, when the shell supports OSC 133
- [expect-watch](../expect-watch/) — when "quiet" actually means "waiting for input", answer the prompt
- [multi-agent-supervisor](../multi-agent-supervisor/) — this pattern, but for many sessions at once
