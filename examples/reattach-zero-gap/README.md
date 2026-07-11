# reattach-zero-gap

**Use case:** your client crashed, was rebuilt, or lost its connection mid-stream. You want to come back and continue reading exactly where you stopped — and you want to *prove* nothing was lost or duplicated.

This is the promise runbaypty exists for, and this example demonstrates it against a session that never stops printing.

## Run it

```sh
go run ./examples/reattach-zero-gap
```

Expected output:

```
session ses_… — counting…

reader 1 saw 21 lines, stopped at seq 147
reader 1 crashed (connection dropped, no detach)
…500ms of output nobody was listening to…
reader 2 resumed from seq 147 and saw 61 lines

✓ audited n-0 … n-80 across the crash: no gap, no duplicate.
  The seam is invisible because seq numbers make it arithmetic.
```

## How it works

Every output byte a session produces has an absolute **sequence number**. Seq `N` means "the stream position after `N` total bytes have ever been written." Each `OUTPUT` frame carries the seq of its first byte, so consecutive frames satisfy `seq₂ = seq₁ + len(payload₁)`.

That gives you a resume protocol with no bookkeeping:

```go
// Reader 1 stops. Its last consumed position:
resumeAt := stream.LastSeq()

// Reader 2, on a brand new connection:
stream, _ := c.Attach(ctx, sessionID, &resumeAt, true)
// The daemon sends exactly the bytes from resumeAt onward. Nothing else.
```

The example makes reader 1 **hard-drop** its connection — no `Detach`, just `Close`. To the daemon that is indistinguishable from a crashed process, which is the case that matters. Meanwhile the session keeps printing for 500 ms into the ring buffer with nobody listening. Reader 2 then reattaches with `since_seq` and the audit walks the joined output asserting a perfect `n-0, n-1, … n-80`.

## Why this beats "redraw the screen"

tmux and shpool re-emulate a terminal grid and repaint it on reattach. You get a picture of the *current screen*, and whatever scrolled past while you were gone is gone or lives in a separate scrollback format. runbaypty never emulates: it hands you the raw byte stream, and the sequence axis lets you ask for precisely the bytes you missed.

That's what makes the guarantee *auditable*. This example doesn't trust the daemon — it counts.

## The ring buffer boundary

The ring holds the newest bytes (2 MiB per session by default, `--ring` at spawn to change it). If you're gone long enough that your `since_seq` falls out of the window, the daemon tells you: `ATTACH_OK` comes back with `truncated: true`, replay starts at the oldest byte it still has, and a live subscriber that falls behind gets a push `E_RING_GONE`. Nothing is silently dropped.

For complete history beyond the ring, spawn with `--log` and read the durable log first — see [tail-history](../tail-history/), which stitches log and ring on the *same* seq axis so the seam is exact.

## Where else this shows up

Same arithmetic, three places:

- **[follow-resilient](../follow-resilient/)** — the SDK's `Follow` does this reconnect loop for you
- **[tail-history](../tail-history/)** — the log → live stream handoff
- **[zero-downtime-upgrade](../zero-downtime-upgrade/)** — the daemon itself is replaced mid-stream and the seq axis survives
