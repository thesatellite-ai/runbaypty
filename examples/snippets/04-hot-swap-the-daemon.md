# 04. Replace the daemon binary without dropping a session

> A live counter keeps counting while the daemon process underneath it is swapped for a new one.

**What it shows:** zero-downtime daemon upgrades. `serve --takeover` hands the PTY file descriptors, ring buffer, and lock to a new daemon over `SCM_RIGHTS`. The session keeps its pid and its byte stream. Not even tmux can do this.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &                # this is "daemon A"
echo "daemon A pid: $!"
```

## Try it by hand

### Step 1: spawn a counter on daemon A

```sh
bin/runbaypty run --name counter -- sh -c 'i=0; while :; do echo "tick $i"; i=$((i+1)); sleep 0.2; done'
```

### Step 2: watch a few ticks

```sh
bin/runbaypty attach counter --read-only    # ctrl-c after a moment
```

### Step 3: upgrade the daemon underneath it

Start a second daemon with `--takeover`, pointed at the same home and socket:

```sh
bin/runbaypty serve --takeover &
echo "daemon B pid: $!"
```

Daemon B connects to daemon A, receives the live session's file descriptors and ring state, and daemon A hands off and exits. The pid you print for B is different from A.

### Step 4: the session never noticed

```sh
bin/runbaypty attach counter --read-only    # still ticking; ctrl-c
```

The counter kept counting straight through the swap. The daemon pid changed; the session pid did not.

## What just happened

The binary running all your sessions was replaced mid-stream. In production this is how you ship a daemon upgrade: your service manager runs `serve --takeover`, the old daemon hands everything to the new one, and no session blinks. The socket blips for a fraction of a second during the handoff, which reconnecting clients ride through transparently.

## Run it all at once

```sh
bash examples/snippets/04-hot-swap-the-daemon.sh
```
