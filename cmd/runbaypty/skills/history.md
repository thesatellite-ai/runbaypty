# history

Recording a session's complete output, reprinting it, and replaying it.

## Two buffers: the ring (window) vs the log (complete)

- **Ring buffer** (always on, default 2 MiB per session): holds the newest bytes for reattach. Older output scrolls off, and the ring is discarded a retention window (default 10 min) after the session exits. This is a window, not an archive.
- **Durable log** (opt-in with `--log`): writes every output byte to a file on disk, permanently. This is the full history.

If you need the complete history, spawn with `--log`. Otherwise you only keep the recent window.

## Record a session

```sh
runbaypty run --name build --log /tmp/build.log -- make -j8
```

The daemon writes `/tmp/build.log` as the session runs. The format is a magic header (`RPTY`), then one record per output chunk as `(ms-since-previous, length, bytes)`. The per-record timestamp means timing is preserved, not just content.

## Reprint the full history

```sh
runbaypty tail build --no-follow
```

`tail <id|name>` reads the session's durable log for the complete history, then follows live; `--no-follow` prints the history and exits. Note `tail` takes a session id or name (not a file path) and reads through the daemon, so the session must still exist (live, or lingering within the retention window). Without a `--log`, `tail` shows only the ring window.

## Replay as asciinema (works after the session is gone)

```sh
runbaypty export /tmp/build.log --out /tmp/build.cast --title build
asciinema play /tmp/build.cast
```

`export` reads the log FILE and writes an asciinema cast v2. It needs no daemon and works days later on another machine, because the log is self-contained. Each cast frame is `[elapsed_seconds, "o", bytes]` with the original timing. Because runbaypty passes bytes through raw, the escape codes replay exactly as the program emitted them.

Flags: `export <log-file> --out <path> --title <title>`. With no `--out`, the cast goes to stdout.

## Which command for which situation

- Session still exists, want to see everything so far: `runbaypty tail <name> --no-follow`.
- Session still exists, want to watch it going forward: `runbaypty tail <name>` (or `attach --read-only`).
- Only have the log file (session reaped, or on another machine): `runbaypty export <file> ...`.
- Want just the newest output and no log was set: `runbaypty tail <name>` gives the ring window.
