# expect-watch

**Use case:** drive an interactive program — wait for a prompt, send an answer, repeat — like `expect(1)`, but with the pattern matching running inside the daemon so an idle waiter costs zero bytes.

## Run it

```sh
go run ./examples/expect-watch
```

```
saw prompt "name> " at seq 0 → answering "ada"
saw prompt "color> " at seq 11 → answering "green"
saw prompt "confirm (yes/no)> " at seq 25 → answering "yes"

program result: hello ada, your green answer is: yes
```

The "program" is a shell script with three `read` calls — it genuinely blocks for input, so this is a real interactive drive, not a scripted sleep.

## Server-side WATCH

A `Watch` registers a regular expression (RE2 — linear time, no catastrophic backtracking) on a session's **future** output. The daemon scans the byte stream and pushes a match the instant the pattern appears:

```go
matches, _ := c.Watch(ctx, sessionID, `password:\s*$`)
<-matches   // blocks until the prompt appears; we ship no bytes waiting
c.Input(sessionID, []byte(secret+"\n"))
```

Three things make this better than reading and matching client-side:

1. **Idle watchers cost nothing.** No output is shipped to a client that's only waiting for a pattern. Across many sessions, that's the difference between streaming everything and streaming nothing.
2. **Boundary-correct.** A prompt split across two output chunks (`pass` then `word: `) still matches — the daemon keeps a bounded overlap between scans. A naive client-side matcher on chunk boundaries would miss it.
3. **Future-only, race-free.** A watch matches output from its registration point onward. Register before the program prints and you cannot miss the prompt to a timing race — which is why this example registers each watch just before expecting its prompt.

## Why the write lock

The example calls `TakeWrite` before typing. Input from a client that isn't the lock holder is refused (`E_NO_WRITE_LOCK`). An *unheld* lock permits input, but claiming it makes the intent explicit and blocks anyone else from typing into your interactive flow mid-answer. When a human needs to jump in, that's the handoff in [write-lock-handoff](../write-lock-handoff/).

## Match limits

- Up to 16 watches per connection (a guardrail).
- Matched text is capped at 256 bytes in the event.
- The cross-chunk overlap window is bounded (1 KiB): a single match longer than that could straddle undetected — a documented limit, not a bug. Prompts are short; this never bites in practice.

## Compared to expect(1)

`expect` spawns the process itself and owns the PTY. Here the daemon owns the PTY, the matching is server-side, and *any* client can drive the session — including one on another machine over an SSH-forwarded socket. The session also survives your expect script crashing: reconnect and it's still at the prompt.

## Next

- [write-lock-handoff](../write-lock-handoff/) — when the interactive answer should come from a human, not the script
- [wait-for-silence](../wait-for-silence/) — for programs without predictable prompts
