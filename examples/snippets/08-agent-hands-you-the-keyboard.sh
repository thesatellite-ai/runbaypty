#!/usr/bin/env bash
# 08 - The agent/human handoff: one writer at a time, stealable on demand.
# An "agent" is driving a session; a human takes the keyboard mid-flight
# (attach steals the single write lock), does something, and the session goes on.
source "$(dirname "$0")/_common.sh"
snip_up

say "Spawn a shell an 'agent' is driving:"
rp run --name box -- /bin/sh -i
sleep 0.5

say "The AGENT types a command (holding the write lock):"
printf 'echo agent-was-here\n' | timeout 2 "$RPTY" attach box || true
sleep 0.5

hr; say "A HUMAN takes over - a fresh attach STEALS the write lock and types:"
printf 'echo human-took-over-$((6*7))\n' | timeout 2 "$RPTY" attach box || true
sleep 0.5

hr; say "Both writers drove the same live session; the lock moved cleanly between them."
say "The full transcript (agent line, then human line):"
timeout 1.5 "$RPTY" attach box --read-only 2>/dev/null | sed 's/\x1b\[[0-9;]*m//g' | grep -E 'agent-was-here|human-took-over' | head
hr; say "Live version: while a 'runbaypty attach box' is open, run another - it takes the keyboard."
