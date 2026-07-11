# write-lock-handoff

**Use case:** an AI agent is driving a terminal session. A human needs to step in — take over the *same live session*, run a command by hand, then hand control back to the agent — without ever typing over each other.

This is the interaction that makes agent-operated terminals safe to supervise.

## Run it

```sh
go run ./examples/write-lock-handoff
```

```
agent has the write lock
  agent typed: line-from-agent
human tried to type without the lock → refused (E_NO_WRITE_LOCK) ✓

→ handoff: human takes the write lock
  human typed: line-from-human
agent tried to type after the handoff → refused ✓

← handoff back: human released, agent re-took the lock
  agent typed: agent-resumes

✓ one live session, one process, control moved agent → human → agent
```

The example plays both roles as two separate connections to one `cat` session. In production they're different processes on different machines — an agent runtime and a human's `runbaypty attach`.

## The single write lock

Any number of clients can *watch* a session. Exactly one can *type* at a time. That's the write lock:

- **`TakeWrite`** claims it. If someone else holds it, this **steals** it — deliberate, because the steal *is* the handoff.
- The previous holder's next keystroke is refused with `E_NO_WRITE_LOCK`, surfaced on their stream (input is fire-and-forget on the wire, so refusals come back as a push error you poll with `WriteRefusal()`).
- **`ReleaseWrite`** gives it up. It also auto-releases when the holder's connection drops — a crashed agent never wedges the session.
- An *unheld* lock permits input from anyone. Claiming it is how you assert "I'm driving now."

```go
agent.TakeWrite(ctx, id)                    // agent drives
agent.Input(id, []byte("build\n"))

human.TakeWrite(ctx, id)                    // human steals the lock — the handoff
human.Input(id, []byte("fix-it-by-hand\n"))
// agent.Input now returns E_NO_WRITE_LOCK on the agent's stream

human.ReleaseWrite(ctx, id)                 // hand it back
agent.TakeWrite(ctx, id)                    // agent resumes — same process, same scrollback
```

## Why steal-by-default is the right design

The alternative — "request the lock, wait for the holder to grant it" — deadlocks the exact case you need this for: the agent is stuck, unresponsive, or looping, and a human needs in *now*. A cooperative handoff assumes the party you're taking over from is healthy enough to cooperate. It usually isn't. So `TakeWrite` steals, and the loser finds out on its next write. The human is always able to seize control.

## What the human takes over

Not a copy, not a fresh shell — the **same process**. The agent ran `claude` or a dev server or a REPL; the human takes the lock and is typing into that exact running process, with all its in-memory state, at the point the agent left it. When the human is done, the agent resumes the same session. That continuity is only possible because the daemon — not the agent, not the terminal — owns the PTY.

## Read-only observers

A client can attach read-only (or with a read-only WS token) to *watch* the handoff without ever being able to take the lock — see [read-only-share](../read-only-share/). Useful for a dashboard, a reviewer, or a "watch what the agent is doing" pane.

## Next

- [read-only-share](../read-only-share/) — let people watch without touching
- [multi-agent-supervisor](../multi-agent-supervisor/) — many agent sessions, a human dropping into whichever one needs it
