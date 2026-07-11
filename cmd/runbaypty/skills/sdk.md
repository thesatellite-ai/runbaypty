# sdk

Driving runbaypty from Go with the `pkg/client` SDK, for when you are building a program rather than shelling out to the CLI.

## Import and connect

```go
import "github.com/thesatellite-ai/runbaypty/pkg/client"

c, err := client.Dial(socketPath)   // socketPath = the daemon's Unix socket
if err != nil { return err }
defer c.Close()
```

`Dial` connects over the Unix socket and completes the handshake. Get the socket path from `constants.SocketPath()` (which honors `RUNBAYPTY_SOCK`).

## Spawn

```go
id, pid, err := c.Spawn(ctx, client.SpawnOpts{
    Cmd:  "/bin/sh",
    Args: []string{"-c", "make -j8"},
    Name: "build",
    LogPath: "/tmp/build.log",   // optional durable log
})
```

`SpawnOpts` also has `Cwd`, `Env`, `Cols`, `Rows`, `RingBytes`, `Meta`, `NoLinger`.

## Read output: Attach and Follow

`Attach` returns a `*Stream` that implements `io.Reader`, so you can plumb it into `bufio.Scanner`, `io.Copy`, anything:

```go
stream, err := c.Attach(ctx, id, nil /* sinceSeq: nil = whole ring */, true /* readOnly */)
sc := bufio.NewScanner(stream)
for sc.Scan() { handle(sc.Text()) }
code, _, _ := stream.Exit()   // exit code after EOF
```

`Follow` is the resilient reader: it reconnects automatically on connection loss and resumes at the exact seq (zero-gap), so it survives the daemon restarting, a network blip, or a takeover:

```go
f, err := client.Follow(ctx, socketPath, id, client.FollowOpts{ReadOnly: true})
// f is an io.Reader too; read from it forever, reconnects are transparent
```

Zero-gap resume works because each output frame carries a sequence number; `Attach{sinceSeq: N}` replays from byte N. `Follow` tracks that for you.

## Send input, take the write lock

```go
c.TakeWrite(ctx, id)              // claim the single write lock (steals it)
c.Input(id, []byte("ls\n"))       // send stdin bytes
c.InputEOF(ctx, id)               // send EOF (ctrl-d)
c.ReleaseWrite(ctx, id)
```

## Watch (server-side regex) and events

```go
matches, _ := c.Watch(ctx, id, `Listening on :\d+`)   // server-side RE2 on future output
ev := <-matches                                       // {Seq, Match}

events, _ := c.SubscribeEvents(ctx, "")               // "" = all sessions
for e := range events { /* e.Type: created/exited/silence/command-finished/... */ }
```

`Watch` runs the regex on the daemon (you ship no bytes to scan). `SubscribeEvents` is the lifecycle stream (silence, OSC 133 command boundaries, etc.).

## Other calls

`List`, `Info`, `Kill`, `Resize`, `Rename`, `SetMeta`, `LastCommandOutput` (the last OSC 133 command's window). All are context-first and return typed structs.

## Where to look

The `examples/` directory has runnable programs for every one of these: `hello-session`, `reattach-zero-gap`, `follow-resilient`, `wait-for-silence`, `expect-watch`, `write-lock-handoff`, `multi-agent-supervisor`, and more. Each is heavily commented.
