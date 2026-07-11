# command-boundaries

**Use case:** a long-lived shell session runs many commands. You want the exit code and output of *each* command separately — not the whole scrollback, and not by guessing where one command's output ends and the next begins.

## Run it

```sh
go run ./examples/command-boundaries
```

```
command 1: exit=0   ok      output bytes [8, 31)
command 2: exit=3   FAILED  output bytes [39, 65)
command 3: exit=0   ok      output bytes [73, 98)

last command's output (seq 73–98):
"final command\r\n…"
```

## How it works: OSC 133

Modern shells emit **OSC 133 shell-integration marks** in the byte stream around each command:

```
ESC ] 133 ; A BEL     prompt starts
ESC ] 133 ; B BEL     command has been typed
ESC ] 133 ; C BEL     command output starts here
ESC ] 133 ; D ; 3 BEL command finished with exit code 3
```

bash-preexec, zsh's precmd/preexec, fish, PowerShell, and terminals like WezTerm, kitty, VS Code, and Warp all speak this. The example emits the marks by hand with `printf` so it runs without configuring your shell, but the bytes on the wire are identical to what a real shell hook produces.

The daemon runs a tiny scanner over the output stream looking for these marks. Crucially, **it does not build a terminal grid** — it reads the bytes for markers and passes them through untouched. When it sees a `C` it records the start; when it sees a `D` it emits a `command-finished` event carrying the exit code and the **sequence numbers** bracketing that command's output.

## The two ways to consume it

**Events** — react as each command finishes:

```go
for ev := range events {
    if ev.Type == proto.EventCommandFinished {
        code  := ev.Data["exit_code"]
        start := ev.Data["start_seq"]   // byte range of this command's output
        end   := ev.Data["end_seq"]
    }
}
```

**Replay** — pull the last command's output slice on demand:

```go
body, start, end, _ := c.LastCommandOutput(ctx, sessionID)
// body is exactly the bytes between the last C and D marks
```

Because the boundaries are sequence numbers on the same axis as everything else, `LastCommandOutput` just slices the ring — no re-parsing, no ambiguity about where the command's output started.

## Why this is the agent feature

Warp built its entire "blocks" UX on OSC 133. An agent that can ask "what was the exit code of the last command, and show me only its output" — without screen-scraping, without shipping the whole scrollback — can supervise a shell the way a human reads a terminal, but programmatically. Combined with [wait-for-silence](../wait-for-silence/) (for shells *without* integration) it covers both worlds.

## What you see in the output

Notice `last command's output` still contains the trailing `\x1b]133;D;0\a` — the daemon passes the marks through verbatim (raw-passthrough is the whole design). A renderer strips them; an agent that cares only about the text ignores them. runbaypty's job is to tell you *where the boundaries are*, not to rewrite your bytes.

## Next

- [agent-watch](../agent-watch/) — the full event stream this builds on
- [multi-agent-supervisor](../multi-agent-supervisor/) — command results across many concurrent agent sessions
