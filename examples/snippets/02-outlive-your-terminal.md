# 02. The dev server that outlives every rebuild

> Spawn a long-lived process, walk away, come back later: same pid, still up.

**What it shows:** start your dev server (or any long-running process) inside a session once, and it survives every hot-reload, every closed terminal, every SSH drop.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand

### Step 1: start a "dev server" as a named session

```sh
bin/runbaypty run --name devserver -- sh -c 'i=0; while :; do echo "[server] req $i"; i=$((i+1)); sleep 0.5; done'
```

Stands in for `npm run dev`, `flask run`, anything long-lived. It keeps handling "requests".

### Step 2: note its pid

```sh
bin/runbaypty info devserver --json | grep pid
```

```
  "pid": 12435,
```

`info` prints the full session detail; `--json` makes it machine-readable. Remember that pid.

### Step 3: walk away, then reattach

Close your terminal, rebuild your app, go get coffee. Then, in any shell with the same `RUNBAYPTY_SOCK`:

```sh
bin/runbaypty attach devserver --read-only
```

The server is still logging requests. `ctrl-c` to detach.

### Step 4: prove it is the same process

```sh
bin/runbaypty info devserver --json | grep pid
```

```
  "pid": 12435,       <- identical to step 2
```

## What just happened

The `devserver` pid never changed. Nothing restarted it; it simply kept running in the daemon while your terminal came and went. Compare with `npm run dev` in a plain terminal, where closing the terminal kills the server. Here the terminal is just a window onto a process the daemon owns.

## Run it all at once

```sh
bash examples/snippets/02-outlive-your-terminal.sh
```
