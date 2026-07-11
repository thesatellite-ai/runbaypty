#!/usr/bin/env bash
# 02 - The dev server that outlives every rebuild of the app that started it.
# Spawn a long-lived process, walk away, come back later: same pid, still up.
source "$(dirname "$0")/_common.sh"
snip_up
pid_of() { rp info "$1" --json | grep '"pid"' | grep -oE '[0-9]+' | head -1; }

say "Start a 'dev server' as a named session:"
rp run --name devserver -- sh -c 'i=0; while :; do echo "[server] request $i handled"; i=$((i+1)); sleep 0.5; done'
sleep 1
PID1=$(pid_of devserver)
say "It is running as pid $PID1. Peek at it:"
timeout 1.2 "$RPTY" attach devserver --read-only || true

hr; say "Now 'rebuild your app' / close your terminal (time passes)..."; sleep 2

say "Reattach by name - same pid, never restarted, full history intact:"
timeout 1.2 "$RPTY" attach devserver --read-only || true
PID2=$(pid_of devserver)
hr; say "pid before: $PID1   pid after: $PID2   (same process, it never blinked)"
