# 09. Record a session and replay it with real timing

> Spawn with a durable log, then export it to a playable asciinema cast.

**What it shows:** every byte of a session written to disk with timestamps, so you can replay it later (with the original pauses) on any machine, no daemon required.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand

### Step 1: spawn a session WITH a durable log

```sh
bin/runbaypty run --name build --log /tmp/build.log -- sh -c 'for s in fetch compile link; do echo ">> $s"; sleep 0.4; done'
```

`--log /tmp/build.log` tells the daemon to write every output byte to that file, with a timestamp per chunk. Without `--log`, output only lives in the ring buffer window.

### Step 2: look at the raw log

```sh
ls -la /tmp/build.log
head -c 24 /tmp/build.log | cat -v
```

```
RPTY......>> fetch
```

The file starts with the magic `RPTY`, a version, and a start timestamp, then one record per output chunk as `(ms-since-previous, length, bytes)`.

### Step 3: export it to an asciinema cast

```sh
bin/runbaypty export /tmp/build.log --out /tmp/build.cast --title build
head -3 /tmp/build.cast
```

```
{"version":2,"width":80,"height":24,"timestamp":...,"title":"build"}
[0,"o",">> fetch\r\n"]
[0.405,"o",">> compile\r\n"]
```

Each frame is `[elapsed_seconds, "o", bytes]`. The elapsed times come straight from the log's timestamps, so playback has the original pacing. `export` reads a file and writes a file: no daemon involved.

### Step 4: replay it

```sh
asciinema play /tmp/build.cast     # if you have asciinema installed
```

## What just happened

Recording and replay are decoupled. The daemon writes the log as the session runs (zero extra work from you); `export` turns it into a standard cast days later, on a different machine, with no daemon running. Because runbaypty passes bytes through raw, the escape codes in the log are the real ones, so playback looks exactly like the original session. See the [record-and-export](../record-and-export/) example.

## Run it all at once

```sh
bash examples/snippets/09-record-and-replay.sh
```
