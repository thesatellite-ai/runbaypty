#!/usr/bin/env bash
# 03 - Many watchers, one session. Type in one place, everyone sees it.
# Best felt in two real terminals (see the note at the end). Here we prove the
# mechanic headlessly: a background watcher records the session while a driver
# types into it - the watcher sees the driver's keystrokes and their output.
source "$(dirname "$0")/_common.sh"
snip_up
WATCH=/tmp/rpty-snip-watch-$$.log

say "Spawn a shared shell session:"
rp run --name shared -- /bin/sh -i
sleep 0.5

say "Start a read-only WATCHER in the background (recording to a file):"
( timeout 4 "$RPTY" attach shared --read-only > "$WATCH" 2>&1 ) &
sleep 0.5

say "A DRIVER types two commands into the same session:"
printf 'echo hello-from-the-driver\nexpr 6 \* 7\n' | timeout 2 "$RPTY" attach shared || true
sleep 1

hr; say "What the independent watcher saw (it never typed a thing):"
sed 's/\x1b\[[0-9;]*m//g' "$WATCH" | grep -vE '^\s*$' | tail -6
rm -f "$WATCH"
hr; say "Try it live: run 'runbaypty attach shared' in TWO terminals and type in either."
