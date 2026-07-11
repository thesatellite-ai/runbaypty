# follow-resilient

**Use case:** read a session's output as a plain `io.Reader` and never think about reconnection again — even if the daemon connection drops repeatedly while you read.

`client.Follow` wraps dial + attach + read in a reconnect loop that resumes at the exact byte it last delivered. Your code sees an uninterrupted stream.

## Run it

```sh
bin/runbaypty run --name ticker -- sh -c 'i=0; while :; do echo tick-$i; i=$((i+1)); sleep 1; done'
go run ./examples/follow-resilient ticker
```

Now break things while it reads. Every one of these is survivable:

```sh
# In another terminal — the follower keeps printing, contiguously:
bin/runbaypty serve --takeover      # replace the daemon binary underneath it
```

Kill the session and the follower ends cleanly with the exit status, because a dead session is a *definitive* answer, not a transport failure:

```sh
bin/runbaypty kill ticker
# follower prints: session exited: signal TERM
```

## What Follow guarantees

```go
fl, err := client.Follow(ctx, socketPath, "ticker", client.FollowOpts{ReadOnly: true})
io.Copy(os.Stdout, fl)     // reconnects transparently; never a byte lost or repeated
code, sig, exited := fl.Exit()
```

- **Zero-gap resume.** On reconnect it re-attaches with the sequence number of the last byte you consumed. Continuity is arithmetic (see [reattach-zero-gap](../reattach-zero-gap/)).
- **Exponential backoff**, 100 ms → 3 s by default, unbounded retries. It gives up only when the context is canceled.
- **Definitive answers surface.** If the daemon comes back and says the session was reaped, or your token lacks the scope, `Read` returns that error rather than retrying into eternity. Only *transport* failures are retried.
- **Names resolve once.** `Follow("ticker")` pins the canonical `ses_…` id at first connect, so a rename mid-stream doesn't break the follow.

## How it's tested

The test for this (`TestFollow_ZeroGapAcrossRepeatedConnectionLoss`) puts a chaos proxy between the follower and the daemon, assassinates the connection three times mid-stream, and then audits that the consumer-visible bytes are a perfect contiguous sequence. That's the bar: not "it reconnects," but "you cannot tell it reconnected."

## When to use Follow vs Attach

| | `Attach` | `Follow` |
|---|---|---|
| Returns | `*Stream` (io.Reader) | `*Follower` (io.ReadCloser) |
| Connection dies | `Read` returns an error | reconnects and resumes |
| You want | control over the retry policy | to not think about it |
| Multiple sessions on one conn | yes | one session per Follower |

Use `Attach` when you're multiplexing several sessions over a single connection and handling reconnect at a higher level (a UI, a supervisor). Use `Follow` when you just want the bytes.

## Next

- [tail-history](../tail-history/) — get the *complete* history first, then follow live
- [dev-server](../dev-server/) — a long-lived session your app reconnects to across rebuilds
