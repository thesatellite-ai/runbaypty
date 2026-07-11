# session-dashboard

**Use case:** a live "mission control" view of every session the daemon owns — for a process manager, an app's running-services panel, or an ops dashboard. Sessions appear, change, and die in the table as they happen.

## Run it

```sh
go run ./examples/session-dashboard
```

Then, from another terminal, make things happen:

```sh
runbaypty run --name api    -- sh -c 'while :; do echo tick; sleep 1; done'
runbaypty run --name worker -- sh -c 'sleep 300'
runbaypty kill api
```

The table redraws on each change:

```
runbaypty dashboard — 2 session(s) — updated 21:22:44 (ctrl-c to quit)

ID                                     NAME    STATE    PID    CLIENTS  CMD
────────────────────────────────────────────────────────────────────────
ses_…                                  api     exited   63257  0        sh -c i=0; while :; do echo tic…
ses_…                                  worker  running  63271  0        sh -c sleep 300
```

## Seed, then subscribe — in that order

The pattern for any "current state + live updates" view has a subtle ordering requirement:

```go
events, _ := c.SubscribeEvents(ctx, "")   // 1. subscribe FIRST
sessions, _ := c.List(ctx)                 // 2. snapshot second
// … then apply events forever
```

If you listed first and subscribed second, a session created in the gap between the two calls would be missed by *both* — the list is already taken, the subscription doesn't exist yet. Subscribing first means every session is caught by the snapshot or by an event (and a duplicate is harmless, because the model is a map keyed by session id). This is the same "subscribe before you act" rule as [wait-for-silence](../wait-for-silence/), for the same reason.

## Events as a change feed

The dashboard keeps a `map[id]SessionInfo` and updates it from the event stream. On each relevant event it re-fetches the authoritative `Info` for the changed session (events tell you *that* something changed; `Info` tells you the current truth). Events that matter here:

| Event | Table effect |
|---|---|
| `created` | new row |
| `exited` | state → `exited` (kept, not removed — a supervisor wants to see what died) |
| `renamed` / `meta-changed` / `resized` | row updates |
| `attached` / `detached` | `CLIENTS` count changes |

No polling anywhere. The daemon pushes; the dashboard reacts. A process manager polling `ls` every second would be both wasteful and laggy; this is neither.

## Why exited sessions stay in the table

When you kill `api`, it doesn't vanish — it shows `exited`. That's deliberate at two levels: the daemon *retains* exited sessions for its retention TTL (so a late client can still see the death and replay the scrollback), and this dashboard keeps the row rather than deleting it, because "what just died" is exactly what an operator is watching for. It drops from the table only when the daemon reaps it (TTL) or you build a UI that hides exited rows.

## Making it a real UI

This prints to a terminal with ANSI clear-screen. The exact same model — seed from `List`, mutate from events — drives a web dashboard, a TUI (bubbletea), or an app's native panel. Swap `render()` for your rendering layer; the data flow doesn't change. Over the [WebSocket transport](../browser-terminal/) with a read-only token, you can even serve this dashboard to a browser that can watch but not touch.

## Next

- [multi-agent-supervisor](../multi-agent-supervisor/) — a dashboard that also *reacts* per-session, not just displays
- [dev-server](../dev-server/) — the sessions this dashboard is built to watch
