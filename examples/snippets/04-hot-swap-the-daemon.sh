#!/usr/bin/env bash
# 04 - Replace the daemon binary WITHOUT dropping a single session.
# A live counter keeps counting while the daemon process underneath it is
# swapped for a new one (serve --takeover). Not even tmux can do this.
source "$(dirname "$0")/_common.sh"
snip_up
say "Daemon A is up (pid $SNIP_DPID). Spawn a counter on it:"
rp run --name counter -- sh -c 'i=0; while :; do echo "tick $i"; i=$((i+1)); sleep 0.2; done'
sleep 1
say "A few ticks from daemon A:"
timeout 1.2 "$RPTY" attach counter --read-only || true

hr; say "Now UPGRADE: start daemon B with --takeover (adopts A's sessions over SCM_RIGHTS)."
"$RPTY" serve --takeover >/dev/null 2>&1 &
DAEMON_B=$!
sleep 1.5   # B adopts the PTY fds + ring + flock; A hands off and exits
say "Daemon A pid was $SNIP_DPID; daemon B pid is $DAEMON_B (different process)."

hr; say "The counter never noticed the swap - reattach and it is still ticking:"
timeout 1.2 "$RPTY" attach counter --read-only || true
SNIP_DPID=$DAEMON_B   # so cleanup kills the daemon that is now serving
hr; say "The binary running your sessions was replaced mid-stream. Zero downtime."
