#!/usr/bin/env bash
# 06 - Block until a server says it is ready, then move on. No sleep-and-pray.
# tail follows the live stream; grep exits on the first match and unblocks you.
source "$(dirname "$0")/_common.sh"
snip_up

say "Spawn a 'server' that takes a moment to come up, then serves:"
rp run --name api -- sh -c 'echo booting...; sleep 1.5; echo "Listening on :8080"; while :; do echo "[api] alive"; sleep 0.5; done'

hr; say "Wait for the readiness line (tail | grep -q blocks until it appears):"
# grep -q exits on first match and SIGPIPEs tail; turn off pipefail so that
# expected 141 from tail does not trip 'set -e'.
if ( set +o pipefail; timeout 8 "$RPTY" tail api 2>/dev/null | grep -q "Listening on :8080" ); then
  say ">>> Server is ready. Now safe to run the next step (smoke tests, migrations, etc.)."
else
  say "(readiness line not seen within the window)"
fi

hr; say "Proof it really was up: a peek at the live server:"
timeout 1.2 "$RPTY" attach api --read-only || true
