# 10. Reprint a session's complete history

> With a durable log, reprint everything a session ever output, not just the recent window.

**What it shows:** the difference between the ring buffer (a bounded window of the newest bytes) and the durable log (the complete record).

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand

### Step 1: spawn a short session with a log

```sh
bin/runbaypty run --name demo --log /tmp/demo.log -- sh -c 'for i in 1 2 3 4 5; do echo "line $i of history"; sleep 0.2; done; echo done'
```

### Step 2: reprint the full history

```sh
bin/runbaypty tail demo --no-follow
```

```
line 1 of history
line 2 of history
line 3 of history
line 4 of history
line 5 of history
done
```

`tail <name>` reads the session's durable log for the complete history, then would follow live; `--no-follow` prints the history and exits. `tail` takes a session id or name (not a file path), and reads through the daemon, so the session must still exist (live, or lingering within the 10-minute retention).

### Step 3: after the session is gone, read the file directly

Once the session has been reaped, `tail <name>` no longer finds it, but the log file is still there:

```sh
bin/runbaypty export /tmp/demo.log --out /tmp/demo.cast
```

## What just happened

Without `--log`, `tail` shows only the ring window (the newest ~2 MiB); older output has scrolled off. With `--log`, the history is complete, so `tail --no-follow` reprints the whole thing. The log is a plain file, so even after the daemon forgets the session you can still replay it with `export`. See [snippet 09](09-record-and-replay.md) for the export path in detail.

## Run it all at once

```sh
bash examples/snippets/10-reprint-full-history.sh
```
