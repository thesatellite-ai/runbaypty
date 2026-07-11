# zero-downtime-upgrade

**Use case:** you need to ship a new version of the daemon — the process that holds *all* your sessions — without killing a single one of them. A build in session A, an agent in session B, a dev server in session C, all mid-flight. The upgrade should be invisible to every client.

## Run it

```sh
go run ./examples/zero-downtime-upgrade
```

```
daemon A up (pid 33152) on /tmp/rpty-zdu-33103.sock
counter session ses_019f… spawned

read 10 ticks from daemon A (last: tick 9)

── upgrading: starting daemon B with --takeover ──
daemon A (pid 33152) handed off and exited; daemon B (pid 33290) now serving
read 19 ticks total (last: tick 18)

── verdict ──
daemon pid changed:     33152 → 33290   (true)
session id unchanged:   ses_019f…   (survived the swap)
tick stream contiguous: yes — 19 ticks, no gap, no duplicate across the upgrade
```

The example runs its **own** dedicated daemon on a private socket and home, so it never touches your real one. It spawns a session that prints an incrementing counter, follows it, swaps the daemon underneath, and proves the counter the follower read is perfectly contiguous — no gap, no duplicate — while the daemon process ID changed.

## How the handover works

A daemon started with `serve --takeover` doesn't cold-boot. It connects to the daemon already running on that socket over a private Unix socket and, using `SCM_RIGHTS` (the POSIX mechanism for passing open file descriptors between processes), receives:

1. **The PTY master file descriptors** — the actual kernel handles to the running child processes, one per live session (`ctrlSessionLive`). The processes never notice; their controlling terminal is the same open kernel file description throughout — only the fd *number* in a different process changes. This is why the sessions don't die: nobody re-`fork`s or re-`exec`s anything.
2. **The ring-buffer state** — including the sequence counter for each session, streamed alongside each fd. The new daemon continues the *same* seq axis, so seq N still means "N total bytes have flowed," continuously across the swap. The old daemon freezes each reader (`PauseReader`) before snapshotting, so the ring is captured at a clean seq boundary with no torn frame.
3. **The flock fd** — passed last (`ctrlFinale`). The single-daemon lock transfers *with* the descriptor, so the lock is never dropped even as the old daemon exits: there's no instant where a third daemon could grab ownership.

The listening socket is **not** handed over — the old daemon closes (and unlinks) it, and the new daemon binds the path fresh. So there is a sub-second window where a *new* connection would be refused. Existing clients ride through it because `client.Follow` reconnects with backoff and resumes at the exact seq — which is precisely why this example reads through `Follow` rather than a plain `Attach`. The sessions and their output are continuous; only the socket blips, and the reconnect hides it.

Once the new daemon has adopted everything, the old one exits (rolling back — `ResumeReader` + re-listen, sessions unharmed — if any step fails before the ack). From `serve.go`:

```go
adopted, err = daemon.RequestTakeover(sock, home)   // receive fds + ring state
// … if no daemon is there to take over, cold-boot instead
srv, _ := daemon.New(daemon.Options{ Adopted: adopted, … })
```

If there's nothing to take over, `--takeover` cold-boots — so it's safe to make it the default in your service unit.

## Why the client never notices: `client.Follow`

The client side of this example is three lines:

```go
f, _ := client.Follow(ctx, sock, sessionID, client.FollowOpts{ReadOnly: true})
sc := bufio.NewScanner(f)
for sc.Scan() { record(sc.Text()) }
```

`Follow` is a reader that reconnects on connection loss and resumes at the exact seq it last read (see [follow-resilient](../follow-resilient/)). When daemon A exits, the follower's connection drops; `Follow` re-dials the same socket — now served by daemon B — and reattaches with `ATTACH{since_seq: lastSeq}`. Because B continued A's seq axis, that resume lands exactly where the read left off.

The crucial point: **the follower does not know an upgrade happened.** It didn't get an "upgrade starting" signal, it didn't coordinate, it has no upgrade-aware code path. It saw a connection drop and did what it always does — reconnect and resume. The same mechanism that survives a network blip survives a daemon replacement. That's the design paying off: one resilience primitive covers both.

## The zero-gap proof

The counter increments by exactly 1 forever. The example records every `tick N` the follower reads and then checks the recorded sequence is perfectly consecutive:

```go
func firstGap(ticks []int) int {
    for i := 1; i < len(ticks); i++ {
        if ticks[i] != ticks[i-1]+1 {
            return ticks[i-1] // a skip OR a duplicate — either breaks contiguity
        }
    }
    return -1 // flawless
}
```

`tick 0 … tick 18` with nothing missing and nothing repeated means the daemon swap lost no bytes and replayed none twice. A skipped tick would mean output produced during the swap was dropped; a repeated tick would mean the resume overlapped. Neither happens, because the seq axis is handed over, not reconstructed.

## Doing this for real

In production the upgrade is a service-manager operation, not a Go program. The shape is the same:

```sh
# install the new binary, then:
RUNBAYPTY_SOCK=… RUNBAYPTY_HOME=… runbaypty-new serve --takeover
# the old daemon hands off and exits; the new one is now serving every session
```

Wire that into a launchd/systemd `ExecStart` that runs `serve --takeover`, and a restart *is* an upgrade — sessions ride through it. Because `--takeover` cold-boots when there's nothing to adopt, the first start and every subsequent restart use the identical command.

## Next

- [follow-resilient](../follow-resilient/) — the reconnect-and-resume primitive this upgrade rides on
- [reattach-zero-gap](../reattach-zero-gap/) — the seq-axis mechanics that make gap-free resume possible
- [dev-server](../dev-server/) — the long-lived sessions you most want to survive a daemon upgrade
