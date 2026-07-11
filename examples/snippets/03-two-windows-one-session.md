# 03. Many watchers, one session (type here, everyone sees it)

> Type in one window and it appears in all of them, live.

**What it shows:** any number of clients can attach to the same session at once. Great for pairing, teaching, or watching a long job from a second screen.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand (two terminals)

### Step 1: spawn a shared shell

```sh
bin/runbaypty run --name shared -- /bin/sh -i
```

`/bin/sh -i` is an interactive shell, so you get a prompt to type at.

### Step 2: attach from TWO terminals

In terminal A **and** terminal B (both need the same `RUNBAYPTY_SOCK` exported):

```sh
bin/runbaypty attach shared
```

### Step 3: type in either one

Type a command in terminal A:

```sh
echo hello-from-A
```

It appears in **both** terminals, live, with its output. Type in terminal B and A sees it too. Detach either with `ctrl-\` (backslash); the shell keeps running for whoever is still attached.

## What just happened

Output fans out to every attached client from the daemon's per-subscriber pump, so all watchers see the same live stream. Only one client can *type* at a time (the write lock, see [snippet 08](08-agent-hands-you-the-keyboard.md)), which is why keystrokes never interleave into garbage.

## Run it (headless proof)

The script proves the mechanic without two terminals: it runs an independent read-only watcher that records the session while a driver types into it, then shows that the watcher saw the driver's commands and their output.

```sh
bash examples/snippets/03-two-windows-one-session.sh
```

Expected tail:

```
sh-3.2$ echo hello-from-the-driver
hello-from-the-driver
sh-3.2$ expr 6 \* 7
42
```
