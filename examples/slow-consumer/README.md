# slow-consumer

**Use case:** you're worried that a reader which can't keep up — a browser on a slow link, a laggy log shipper — will slow down the session or the other clients watching it. It won't, and this shows why.

## Run it

```sh
go run ./examples/slow-consumer
```

```
in 3s the session produced ~15.7 MiB of output
  fast reader consumed: 15.7 MiB
  slow reader consumed: 236.0 KiB

── the point ──
The fast reader kept pace with the firehose. The slow reader fell far
behind — but that lag is entirely its own…
```

Two readers on one firehose. The fast one consumed everything; the slow one (a 50 ms sleep between reads) fell ~68× behind. Crucially, the **fast reader was not held back one byte**, and the session ran at full speed for both.

## Why: pull, not push

Many streaming systems fan out through a shared broadcast buffer. One slow consumer backs up that buffer and everyone stalls — head-of-line blocking. runbaypty doesn't have a shared buffer.

Each subscriber gets its own pump on the daemon:

```
loop:
  wait for the ring to advance past MY last-seen position
  replay from MY position → send it
  advance MY position
```

There's nothing shared to congest. A fast reader's pump and a slow reader's pump run independently against the same ring buffer. The slow reader lagging just means *its* pump is further behind in *its* loop — invisible to everyone else. Backpressure is per-reader by construction, not a feature that had to be added.

## The ring window and E_RING_GONE

So what stops a slow reader from making the daemon buffer megabytes waiting for it? The ring buffer is fixed-size (2 MiB per session by default). It holds the *newest* bytes; older ones scroll off. A reader's pump replays from that ring.

If a reader falls so far behind that its position scrolls out of the ring window, the daemon doesn't buffer more and doesn't stall the session — it tells the reader with a push **`E_RING_GONE`** and resumes it at the oldest byte still held. The reader gets a documented "you missed bytes N through M" signal, never a silent gap.

**A subtlety this example is honest about:** the SDK's `Stream` always drains its socket (a background demux loop), so a slow *consumer of the SDK stream* accumulates on the client side rather than triggering `E_RING_GONE`. `E_RING_GONE` fires for a reader whose **socket itself** backs up — one that stops reading entirely, so the daemon's pump can't send and the ring scrolls past. That's a raw-protocol-level condition; the SDK trades it for client-side buffering (availability over backpressure). Both are valid; know which you're getting.

## The complete-history escape hatch

The ring is a *window*, not the archive. If you need every byte despite a small ring and a slow reader, spawn with `--log`: the durable log captures everything, and you read the log for history and the ring for live — stitched on one sequence axis. See [tail-history](../tail-history/).

## What this means in practice

- A browser terminal on hotel wifi doesn't slow your build.
- A log-shipping sidecar that pauses under load doesn't stall the session it's tailing.
- You can attach a dozen dashboards to one busy session; the session neither knows nor cares.

The session's job is to run and to fill the ring. Consuming the ring at your own pace is your job, and your pace is yours alone.

## Next

- [tail-history](../tail-history/) — complete history when the ring window isn't enough
- [browser-terminal](../browser-terminal/) — the classic slow consumer: a browser over WebSocket
