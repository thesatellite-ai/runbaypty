# 05. "Is it done yet?" answered without polling

> Run a job, then let the daemon TELL you the moment its output goes quiet.

**What it shows:** no `while ps | grep`, no tailing and eyeballing. The daemon pushes a `silence` event when a session stops producing output, which for most commands means "finished".

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand

### Step 1: kick off a job and capture its id

```sh
ID=$(bin/runbaypty run --name job -- sh -c 'echo building; sleep 0.4; echo linking; sleep 0.4; echo done')
echo "$ID"
```

`run` prints the new session's id on stdout, so `$ID` now holds it (`ses_...`). The job does a little work then goes quiet.

### Step 2: block until the daemon reports silence

```sh
bin/runbaypty events --json --session "$ID" | grep -m1 '"silence"'
```

`events --json` streams lifecycle events as JSON, one per line; `--session "$ID"` filters to just this session. `grep -m1 '"silence"'` blocks until the first `silence` line arrives, then exits. You will see:

```
{"Type":"silence","SessionID":"ses_...","At":"...","Data":{"quiet_ms":"5058"}}
```

That line is your "done" signal. `quiet_ms` is how long it had been quiet (the threshold is 5 seconds of no output by default).

### Step 3: do the next thing

```sh
echo ">>> job finished, run the deploy now"
```

## What just happened

You waited for completion without polling anything. The daemon watches each session's output and emits `activity` when it resumes and `silence` when it stops for the threshold. An agent or a script blocks on that event instead of guessing with `sleep`. This is the CLI form of the [wait-for-silence](../wait-for-silence/) SDK example.

## Run it all at once

```sh
bash examples/snippets/05-ping-me-when-done.sh
```
