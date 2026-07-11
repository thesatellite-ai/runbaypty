# 06. Block until a server is ready, then move on

> Wait for a server to print its readiness line, then proceed. No sleep-and-pray.

**What it shows:** deterministic startup gating. `tail` follows the live stream; `grep -q` unblocks you on the first match.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand

### Step 1: spawn a server that takes a moment to come up

```sh
bin/runbaypty run --name api -- sh -c 'echo booting; sleep 1.5; echo "Listening on :8080"; while :; do echo alive; sleep 0.5; done'
```

It prints `booting`, waits 1.5s, then prints `Listening on :8080` and starts serving.

### Step 2: block until it is ready

```sh
bin/runbaypty tail api | grep -q "Listening on :8080"
```

`tail api` follows the session's live output. `grep -q` (quiet) exits with success the instant it sees the readiness line, which unblocks this command. It returns only once the server is actually up.

### Step 3: run the next step safely

```sh
echo ">>> server is up, running smoke tests now"
```

## What just happened

You replaced `sleep 5 && hope-it-is-up` with a real wait on the server's own readiness signal. Because the session's output is a stream the daemon holds, any client can watch for a marker line and gate on it. This is exactly how a CI step would wait for a service before hitting it. (For a pattern match without reading bytes yourself, the SDK offers a server-side regex watch; see the [expect-watch](../expect-watch/) example.)

## Run it all at once

```sh
bash examples/snippets/06-wait-for-ready.sh
```
