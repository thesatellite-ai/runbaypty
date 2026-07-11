# 08. The agent/human handoff (steal the keyboard)

> Exactly one client can type at a time; a new attach takes the keyboard on demand.

**What it shows:** the single write lock. An agent drives a session; a human takes over mid-task by attaching (which steals the lock), does something, and hands it back. Two writers never interleave keystrokes.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand (two terminals)

### Step 1: spawn a shell

```sh
bin/runbaypty run --name box -- /bin/sh -i
```

### Step 2: an "agent" attaches and drives it

In terminal A:

```sh
bin/runbaypty attach box
echo agent-was-here
```

Terminal A holds the write lock, so its keystrokes reach the shell.

### Step 3: a human steals the keyboard

In terminal B, just attach:

```sh
bin/runbaypty attach box
echo human-took-over
```

The moment terminal B attaches, it takes the write lock. Now B can type and A cannot; if A tries, it gets `E_NO_WRITE_LOCK` with a hint to take the lock back. Attaching from A again steals it back.

## What just happened

Only one writer at a time, and any `attach` takes the lock. That is the human/agent handoff: a person can grab the wheel from an automated agent at any moment (and vice versa) without the two fighting over stdin. A `--read-only` attach never takes the lock, so watchers never interfere. See the [write-lock-handoff](../write-lock-handoff/) example for the SDK version with `TakeWrite`/`ReleaseWrite`.

## Run it (headless proof)

The script drives the session as the "agent", then a second attach steals the lock and types as the "human", and prints the transcript:

```sh
bash examples/snippets/08-agent-hands-you-the-keyboard.sh
```

Expected transcript:

```
sh-3.2$ echo agent-was-here
agent-was-here
sh-3.2$ echo human-took-over-$((6*7))
human-took-over-42
```
