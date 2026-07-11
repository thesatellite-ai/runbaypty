# multi-agent-supervisor

**Use case:** one process driving a pool of long-lived agent sessions — the "agent runner" or orchestration layer that fans work across many terminals and reacts to each as it changes. This is where the earlier AI-agent examples come together: the lifecycle event stream, the silence signal, and server-side watch → input, combined into a single control loop that holds per-session state and runs a *policy* over it.

## Run it

```sh
go run ./examples/multi-agent-supervisor
```

```
supervising 3 agents — alpha (clean exit), beta (approval gate), gamma (stalls)

← alpha exited
? beta asked for approval (seq 24) — granting
← beta exited
! gamma stalled (silent 5418ms) — reaping
← gamma reaped (killed)

── all agents settled ──
  alpha  exited
  beta   exited
  gamma  reaped (killed)
```

Three simulated agents, three different endings — each driven by a different daemon signal:

| Agent | What it does | Signal the daemon reports | What the supervisor decides |
|---|---|---|---|
| `alpha` | works, exits cleanly | `exited` | nothing — just record it |
| `beta` | reaches an approval gate, blocks on stdin | a `watch` match on the approval sentinel | grant approval → `Input` |
| `gamma` | emits, then goes silent forever | `silence` (5s threshold) | it's stuck → `Kill` and reap |

## The one idea: the daemon reports facts, the client decides

runbaypty's daemon is deliberately **policy-free**. It holds processes and bytes. It will tell you *that* a session exited, *that* a pattern matched, *that* a session has been quiet for N milliseconds — but it never decides what those facts *mean*. It has no notion of "stuck", no notion of "approval", no notion of "reap".

Every one of those decisions lives in this example:

- The daemon reports `silence`. **The supervisor decides** that silence on a non-exited agent means "stalled" and kills it. A different supervisor might decide silence means "waiting for me" and send a nudge instead. Same fact, different policy — and the daemon doesn't change.
- The daemon matches the byte pattern we registered. **The supervisor decides** that the match means "this agent wants approval" and answers it.
- The daemon reports an exit. **The supervisor decides** whether that exit was natural (`alpha`, `beta`) or a reap it initiated (`gamma`) — a distinction the daemon can't make because it doesn't know *why* the kill happened.

That separation is the whole design. It's why runbaypty is a *substrate* for agent orchestration rather than a framework you adopt.

## The control loop

One `select` over three inputs:

```go
for remaining(agents) > 0 {
    select {
    case ev := <-events:      // lifecycle: exited, silence
    case w  := <-watchIn:     // approval matches (all agents' watches, fanned in)
    case <-ctx.Done():        // deadline backstop
    }
}
```

Each turn mutates a `map[id]*agent` of per-session state and repaints the board. The loop ends when every agent reaches a terminal state (`exited` or `reaped`). No polling anywhere — the daemon pushes every signal.

### Per-session state is a typed closed set

The agent statuses (`running`, `blocked: approval`, `stalled: silent`, `exited`, `reaped`) are named constants of an `agentStatus` type, not bare strings sprinkled through the code. `isTerminal()` is defined on that type, so "is this agent done?" reads from the same vocabulary the render path uses — the read and write sides can't drift.

## Three mechanisms, one policy

### Silence → reap (from wait-for-silence)

The daemon fires `silence` after a session is quiet past its threshold (5s by default). `alpha` and `beta` finish before then, so only `gamma` — which sleeps forever — trips it. The supervisor's rule: *silence on an agent that hasn't exited means it's stalled; kill it.* The resulting `exited` event comes back around, and because the id is in `killedByUs`, the supervisor records it as **reaped**, not a natural exit.

### Watch → input (from expect-watch)

Each agent gets a server-side watch for the approval sentinel. The pattern is RE2, so the literal `[[NEEDS-APPROVAL]]` is passed through `regexp.QuoteMeta` — otherwise `[` opens a character class and the pattern won't compile. When `beta` prints the sentinel, the daemon reports the match (with its seq) and the supervisor answers with `Input("yes\n")`. The supervisor never scans a byte of output itself — the match happens server-side.

### Events → state (from agent-watch)

The lifecycle stream is the spine. Subscribed **before** any agent is spawned (same subscribe-then-act rule as [wait-for-silence](../wait-for-silence/) and [session-dashboard](../session-dashboard/)), so no exit or silence that happens during startup can slip through the gap.

## An honest note on the approval race

`beta` sleeps 1 second before printing its approval sentinel. That pause is a demo guard, not part of the protocol: a watch only matches *future* output, and the supervisor registers the watch just after spawn — so the agent is given a beat to reach the gate after the watch is armed. In production you'd close this race one of two ways, both of which keep the daemon policy-free:

- the agent **re-emits** its prompt periodically until it gets input (so a late watcher still catches it), or
- the supervisor **scans scrollback on attach** (or via `LastCommandOutput`) to catch a prompt that was already printed before it started watching.

The fixed sleep is the simplest thing that makes the example deterministic; the README calls it out rather than pretending the race isn't there.

## Scaling this up

This is three agents with hardcoded scripts, but nothing about the loop is limited to three or to shell scripts. Point the specs at real agent processes, grow the pool, and the same loop supervises all of them: one goroutine per watch fanning into `watchIn`, one events subscription, one state map. The policy — when to approve, when to reap, when to restart — is yours to write, and changing it never touches the daemon.

## Next

- [session-dashboard](../session-dashboard/) — the read-only cousin: observe the pool without reacting to it
- [write-lock-handoff](../write-lock-handoff/) — hand one of these agent sessions to a human and take it back
- [dev-server](../dev-server/) — the durability primitive (named, lingering sessions) these agents are built on
