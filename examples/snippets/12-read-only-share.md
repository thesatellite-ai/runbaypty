# 12. Share a session read-only

> Let someone watch your session live without being able to type into it.

**What it shows:** two ways to grant watch-only access, one local and one across the WebSocket with a scoped token.

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
bin/runbaypty run --name demo -- sh -c 'i=0; while :; do echo "tick $i"; i=$((i+1)); sleep 0.5; done'
```

## Option A: local watch-only attach

```sh
bin/runbaypty attach demo --read-only
```

You see everything, but you cannot type and you never take the write lock away from whoever is driving. This is the CLI form of "watch over my shoulder". `ctrl-c` to stop watching.

## Option B: read-only over the WebSocket (remote / browser)

The daemon mints two tokens at boot, in its home directory:

```sh
ls "$RUNBAYPTY_HOME"
# token      -> control scope (the full protocol)
# token.ro   -> read-only scope (watch only)
```

Hand a viewer the `token.ro` value (and the WebSocket URL). With it they can attach and watch, list sessions, and subscribe to events, but every attempt to type, spawn, or kill is refused by the daemon with `E_READ_ONLY_SCOPE`. The scope is enforced by the daemon, not by politeness, and a read-only credential can never yield a writable attach.

## What just happened

Read-only is a real security boundary, checked on the daemon on every control frame. Locally it is a flag; over the network it is a separate token you can hand out safely. See the [read-only-share](../read-only-share/) example for the owner/viewer walkthrough and the exact refusals, and [snippet 11](11-terminal-in-your-browser.md) to serve a read-only terminal to a browser (`--read-only`).
