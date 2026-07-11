# hello-session

**Use case:** run a command in a terminal you don't own, read what it prints, and find out how it ended.

This is the smallest complete runbaypty program. It spawns a command in a PTY that the *daemon* owns, streams the output, and reports the exit code or the signal that killed it.

## Run it

```sh
bin/runbaypty serve &            # if the daemon isn't already running

go run ./examples/hello-session -- echo "hello from a PTY"
go run ./examples/hello-session -- sh -c 'for i in 1 2 3; do echo $i; sleep 0.3; done'
go run ./examples/hello-session -- sh -c 'exit 42'
```

## What to look for

**The process is not your child.** `os/exec` would make the command a child of this program — kill this program and the command dies with it. Here the daemon is the parent. Try it: start the sleepy variant, then hit `ctrl-c` on the example. Now run `bin/runbaypty ls` — the session is still there, still running, and `bin/runbaypty attach <id>` picks it up mid-stream.

**It's a real terminal.** The command sees a PTY, not a pipe, so `isatty()` is true: colors stay on, `ls` uses columns, progress bars redraw. Compare `go run ./examples/hello-session -- ls --color=auto` with `ls --color=auto | cat`.

**Exit is data, not an error.** A command that exits 42 is not a transport failure. The exit code arrives on the stream (`stream.Exit()`), after the output has fully drained — so you never miss the last line before the death.

## The three calls

```go
sessionID, pid, err := c.Spawn(ctx, client.SpawnOpts{Cmd: "sh", Args: []string{"-c", "…"}})
stream, err := c.Attach(ctx, sessionID, nil /* replay everything */, false /* readOnly: false → writable */)
io.Copy(os.Stdout, stream)   // Stream is an io.Reader; EOF when the session exits
code, signal, exited := stream.Exit()
```

`Attach`'s third argument is `sinceSeq`. Passing `nil` replays the session's whole ring buffer before the live bytes — which is why you see output the command printed *before* you attached. Passing a specific sequence number is how [reattach-zero-gap](../reattach-zero-gap/) resumes exactly where a previous reader stopped.

## Cleanup

The example kills the session when it's done. Without that, an exited session is *retained* for the daemon's retention TTL (default 10 minutes) so a late client can still replay its scrollback and its death. That's deliberate: a session that vanishes the instant it exits is useless to a supervisor that reconnects a second later.

## Next

- [reattach-zero-gap](../reattach-zero-gap/) — disconnect mid-stream and resume at the exact byte
- [wait-for-silence](../wait-for-silence/) — decide when a command is "done" without parsing its output
