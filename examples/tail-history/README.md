# tail-history

**Use case:** show me *everything* this session ever printed — not just what's still in memory — and then keep following it live, without a duplicated or missing byte at the join.

## Run it

```sh
# A session with a durable log. The log is the complete history.
bin/runbaypty run --name build --log /tmp/build.log -- \
  sh -c 'for i in $(seq 1 200); do echo "step $i"; sleep 0.05; done'

go run ./examples/tail-history build
```

You'll see every line from `step 1`, a note about where the seam is, then the live stream continuing. Compare with no log:

```sh
bin/runbaypty run --name nolog -- sh -c 'for i in $(seq 1 200); do echo "step $i"; sleep 0.05; done'
go run ./examples/tail-history nolog
# [no durable log — history is the ring window; …]
```

## The two tiers, one axis

runbaypty stores a session's output in two places, and they agree:

| Tier | Holds | Lives in |
|---|---|---|
| **Ring buffer** | the newest bytes (2 MiB default, `--ring` to change) | daemon memory |
| **Durable log** | every byte, from seq 0, with timestamps | a file you named with `--log` |

Because both are indexed by the **same absolute sequence number**, stitching them is arithmetic:

```go
// Read the log. It contains bytes [0, N).
records, _ := host.ReadSessionLog(info.LogPath)
var n uint64
for _, rec := range records { os.Stdout.Write(rec.Data); n += uint64(len(rec.Data)) }

// Resume the live stream at exactly byte N.
stream, _ := c.Attach(ctx, info.ID, &n, true)
io.Copy(os.Stdout, stream)
```

No overlap. No gap. The same trick [reattach-zero-gap](../reattach-zero-gap/) uses across a crash, applied across storage tiers.

## Why the log has timestamps

Each record is `(timestamp-delta, length, bytes)`. That was a deliberate choice on day one: it costs about three bytes per record and it makes the log a **recording**, not just a transcript. `runbaypty export` turns it into an asciinema cast with correct timing, no re-instrumentation — see [record-and-export](../record-and-export/).

A daemon killed mid-write leaves a torn final record. The reader detects it and stops at the last complete one, so a crash costs you the last write, never the file.

## `Attach` vs `Follow` here

This example uses `Attach` because only `Attach` takes a resume point, and the exact seam is the entire lesson. A production tail would wrap it in a reconnect loop — and would re-read the log's length on each retry, since the daemon keeps appending to it while you read.

If you don't need the pre-ring history, [`Follow`](../follow-resilient/) is simpler and handles reconnection for you.

## The CLI does this too

```sh
bin/runbaypty tail build              # history, then follow
bin/runbaypty tail --no-follow build  # history, then exit
```

## Next

- [record-and-export](../record-and-export/) — turn that same log into a playable asciinema cast
- [slow-consumer](../slow-consumer/) — what the daemon does when a live reader falls behind the ring
