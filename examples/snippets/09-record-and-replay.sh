#!/usr/bin/env bash
# 09 - Record a session and replay it later with the original timing.
# Spawn with --log; the daemon writes a durable (timestamp, bytes) log.
# `export` turns it into an asciinema cast - no daemon needed to replay.
source "$(dirname "$0")/_common.sh"
snip_up
LOG=/tmp/rpty-snip-record-$$.log
CAST=/tmp/rpty-snip-record-$$.cast

say "Spawn a session WITH a durable log:"
rp run --name build --log "$LOG" -- sh -c 'for s in fetch compile link package; do echo ">> $s"; sleep 0.4; done; echo "done"'
sleep 3

hr; say "The log on disk (magic header 'RPTY' + timestamped records):"
ls -la "$LOG"; printf 'first bytes: '; head -c 24 "$LOG" | cat -v; echo

hr; say "Export it to a playable asciinema cast (a pure file transform):"
rp export "$LOG" --out "$CAST" --title "build"
say "cast header + first frames:"; head -3 "$CAST"
hr; say "Replay it (if you have asciinema):  asciinema play $CAST"
rm -f "$LOG" "$CAST"
