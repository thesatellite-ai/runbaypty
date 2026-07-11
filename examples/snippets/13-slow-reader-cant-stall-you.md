# 13. A slow reader cannot stall you

> A fast reader and a deliberately slow one share one firehose; the session and the fast reader are unaffected.

**What it shows:** backpressure is per-reader. Every subscriber has its own pump (pull, not a shared broadcast buffer), so one slow consumer never slows the session or the other readers.

## Try it

This one is clearest as the Go example, which measures throughput for both readers:

```sh
go run ./examples/slow-consumer
```

Expected output (numbers vary):

```
in 3s the session produced ~15.7 MiB of output
  fast reader consumed: 15.7 MiB
  slow reader consumed: 236.0 KiB
```

The fast reader kept pace with the firehose; the slow one (a 50ms sleep between reads) fell far behind. Crucially, the fast reader was not held back one byte, and the session ran at full speed for both.

## What just happened

Many streaming systems fan out through a shared buffer, so one slow consumer backs it up and everyone stalls (head-of-line blocking). runbaypty has no shared buffer: each subscriber gets its own pump that replays from the ring at its own pace. A browser terminal on hotel wifi does not slow your build; a log-shipping sidecar that pauses under load does not stall the session it is tailing. See the [slow-consumer](../slow-consumer/) example for the full explanation, including what happens when a reader falls out of the ring window entirely.
