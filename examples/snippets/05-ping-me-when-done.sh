#!/usr/bin/env bash
# 05 - "Is it done yet?" answered without polling.
# Run a job, then let the daemon TELL you the moment its output goes quiet.
# No loops, no grepping ps, no tailing and eyeballing.
source "$(dirname "$0")/_common.sh"
snip_up

say "Kick off a job (a couple of bursts of work, then it goes quiet):"
ID=$(rp run --name job -- sh -c 'echo building...; sleep 0.4; echo linking...; sleep 0.4; echo "build finished"')
say "session: $ID"

hr; say "Now block on the daemon's event stream until it reports SILENCE for this session."
say "(The silence threshold is 5s of no output: 'the command is probably done'.)"
# grep -m1 exits on the first match and SIGPIPEs the events stream; run it in a
# subshell with pipefail off so that expected signal does not trip 'set -e'.
if ( set +o pipefail; timeout 9 "$RPTY" events --json --session "$ID" 2>/dev/null | grep -m1 '"silence"' ); then
  say ">>> DING! The job went quiet. That is your 'done' signal, push-delivered, no polling."
else
  say "(no silence within the window)"
fi
