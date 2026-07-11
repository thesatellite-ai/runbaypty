#!/usr/bin/env bash
# 07 - Per-command exit codes and output windows, the Warp "blocks" trick.
# With OSC 133 shell-integration marks, the daemon knows where each command
# started and ended and what it exited with - structured, not screen-scraped.
source "$(dirname "$0")/_common.sh"
snip_up

# The session brackets two commands with OSC 133 marks (C before output,
# D;<code> after). The leading `sleep 1` lets us subscribe to events BEFORE the
# marks fire (otherwise the command-finished events race ahead of us).
say "Run a session that emits OSC 133 marks around two commands (one ok, one failing):"
ID=$(rp run --name work -- sh -c '
  sleep 1
  printf "\033]133;C\007"; echo "deploy step 1 ok";     printf "\033]133;D;0\007"
  sleep 0.3
  printf "\033]133;C\007"; echo "deploy step 2 FAILED"; printf "\033]133;D;3\007"
  sleep 3')
say "session: $ID"

hr; say "Subscribe first, then watch the command-finished events (each carries exit_code + byte range):"
timeout 4 "$RPTY" events --json --session "$ID" 2>/dev/null | grep -m2 'command-finished' || true

hr; say "And pull the LAST command's exact output, no scraping:"
rp lastcmd work 2>/dev/null | head -1
