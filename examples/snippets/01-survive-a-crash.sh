#!/usr/bin/env bash
# 01 - Your reader can die; the session doesn't care.
# Start a counter, watch it, KILL the watcher mid-stream, reattach: still
# running, full scrollback replays. This is the whole point of runbaypty.
source "$(dirname "$0")/_common.sh"
snip_up

say "Spawn a counter session that ticks forever:"
rp run --name counter -- sh -c 'i=0; while :; do echo "tick $i"; i=$((i+1)); sleep 0.2; done'
sleep 1

hr; say "Reader #1 watches for ~1.5s, then we KILL it (simulating your terminal dying):"
timeout 1.5 "$RPTY" attach counter --read-only || true

hr; say "The reader is gone. Is the session? Reattach and see:"
timeout 1.5 "$RPTY" attach counter --read-only || true

hr; say "Same session - the scrollback replayed and it kept ticking."
say "Your client died twice; the process never noticed."
rp ls
