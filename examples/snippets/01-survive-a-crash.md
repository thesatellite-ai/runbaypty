# 01. Your reader can die; the session does not care

> Start a counter, watch it, kill the watcher mid-stream, reattach: still running, full scrollback replays.

**What it shows:** the session lives in the daemon, not in your terminal. Your client is disposable; the process is not. This is the core promise of runbaypty.

## Setup (run once per shell)

A private, throwaway daemon so nothing you do here touches a real one:

```sh
go build -o bin/runbaypty ./cmd/runbaypty        # if not built yet
export RUNBAYPTY_HOME=$(mktemp -d)
export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &                            # background daemon; note the &
```

Tear it down when done: `kill %1; rm -rf "$RUNBAYPTY_HOME" "$RUNBAYPTY_SOCK"`.

## Try it by hand

### Step 1: spawn a session that counts forever

```sh
bin/runbaypty run --name counter -- sh -c 'i=0; while :; do echo "tick $i"; i=$((i+1)); sleep 0.2; done'
```

`run` spawns a PTY session in the daemon and returns its id (`ses_...`). `--name counter` gives it a friendly name so you can refer to it later. Everything after `--` is the command the session runs. It prints `tick 0`, `tick 1`, ... every 200ms, forever.

### Step 2: attach and watch, then kill your reader

```sh
bin/runbaypty attach counter --read-only
```

You will see the ticks streaming live (`--read-only` means you watch without taking the keyboard). Watch a few, then press `ctrl-c`. That kills your local client, simulating your terminal crashing or your SSH dropping. You will see something like:

```
tick 0
tick 1
tick 2
...
tick 12
```

### Step 3: reattach

```sh
bin/runbaypty attach counter --read-only
```

The scrollback replays from the start and the counter is still going, now at higher numbers:

```
tick 0
tick 1
...
tick 18       <- kept counting the whole time your reader was dead
```

Press `ctrl-c` again.

### Step 4: confirm it is still there

```sh
bin/runbaypty ls
```

```
ID          NAME     STATE    PID    CLIENTS  CMD
ses_...     counter  running  81712  0        sh -c i=0; while :; do echo "tick $i"...
```

## What just happened

You killed the client twice. The `counter` process never noticed, because it is a child of the daemon, not of your shell. The daemon buffered every byte in its ring, so each reattach replayed the history and picked up live. That is the whole idea: attach and detach as many times as you like, from as many terminals as you like, and the session keeps running underneath.

## Run it all at once

```sh
bash examples/snippets/01-survive-a-crash.sh
```
