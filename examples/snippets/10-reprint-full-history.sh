#!/usr/bin/env bash
# 10 - Reprint a session's COMPLETE history, not just the recent window.
# Without a log you get the ring window (newest ~2 MiB). With --log the history
# is complete, and `tail --no-follow` prints all of it and exits.
source "$(dirname "$0")/_common.sh"
snip_up
LOG=/tmp/rpty-snip-hist-$$.log

say "Spawn a short session with a durable log:"
rp run --name demo --log "$LOG" -- sh -c 'for i in 1 2 3 4 5; do echo "line $i of history"; sleep 0.2; done; echo done'
sleep 2

hr; say "Reprint the FULL history from the log (by session name), then exit:"
rp tail demo --no-follow

hr; say "Every line is there, in order - the log captured the complete stream."
say "(After the session is reaped you can still read the file: rp export $LOG ...)"
rm -f "$LOG"
