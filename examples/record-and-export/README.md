# record-and-export

**Use case:** record a build, a deploy, a debugging session — anything that runs in a terminal — and produce a shareable, playable [asciinema](https://asciinema.org) cast, with correct timing and no extra instrumentation.

## Run it

```sh
go run ./examples/record-and-export
```

```
recording session ses_… → /tmp/runbaypty-build.log
build finished (exit 0) — log is complete

exported → /tmp/runbaypty-build.cast
cast header: {"version":2,"width":80,"height":24,"timestamp":…,"title":"runbaypty build"}
cast events: 6 frames, each [elapsed-seconds, "o", bytes]

play it:  asciinema play /tmp/runbaypty-build.cast
```

If you have asciinema installed, `asciinema play /tmp/runbaypty-build.cast` replays the build with the original pauses between steps.

## Why the log is a recording, not a transcript

runbaypty's durable log doesn't store raw bytes — it stores `(timestamp-delta, length, bytes)` records:

```
header:  "RPTY" · version · start-unix-ms
record:  uvarint ms-since-previous · uvarint length · bytes
record:  …
```

That timestamp was a deliberate day-one decision. It costs about three bytes per record, and it turns the log from "what was printed" into "what was printed, *when*." Which means exporting to asciinema is a pure arithmetic transform — subtract the start time, emit `[elapsed, "o", data]` per record:

```sh
runbaypty run --log build.log -- make -j8
runbaypty export build.log --out build.cast
asciinema play build.cast
```

**The daemon isn't involved in the export.** `runbaypty export` reads a file and writes a file. It works on a log from a session that exited days ago, on a different machine, with no daemon running at all. Recording and replaying are decoupled.

## Two ways to record

1. **Spawn with `--log`** (this example) — the daemon writes the log as the session runs. Zero overhead in your code; the log is complete the moment the session exits.
2. **Read the ring and write your own** — if you only want to capture from a certain point, attach and copy the stream. But you'd be re-deriving what `--log` already gives you for free, including the timing.

## Crash safety

A daemon killed mid-write leaves a torn final record. The reader detects it (a length that runs past EOF) and stops at the last complete record. A crash costs you the last write in flight, never the whole file — so a recording of a build that crashed the daemon still plays up to the crash.

## The cast format

asciinema cast v2 is one JSON object per line: a header, then `[elapsed_seconds, "o", "output bytes"]` per frame. Because runbaypty passes bytes through raw (no terminal emulation), the escape codes in the log are the *real* ones the program emitted — colors, cursor moves, everything — so the playback looks exactly like the original session. Compare with a tool that re-renders a grid: it can only replay what its emulator understood.

## Next

- [tail-history](../tail-history/) — read that same log live, stitched to the running stream
- [dev-server](../dev-server/) — a long-lived session whose complete history you'd want logged
