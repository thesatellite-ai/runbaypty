# events

Knowing when a command finished, waiting for readiness, and per-command exit codes. These are the primitives that let an agent supervise a session without polling.

## The event stream

```sh
runbaypty events --json
runbaypty events --json --session <id>     # filter to one session
```

`events` streams daemon lifecycle events, one JSON object per line: `{"Type": ..., "SessionID": ..., "At": ..., "Data": {...}}`. Types include:

- `created`, `exited` (`Data.exit_code`, `Data.signal`)
- `activity` (output resumed) and `silence` (output stopped for the threshold; `Data.quiet_ms`)
- `bell`
- `resized`, `renamed`, `attached`, `detached`, `meta-changed`
- `command-started`, `command-finished` (OSC 133; see below)

## "Is it done?" without polling

A `silence` event fires when a session has produced no output for the threshold (5 seconds by default). For most commands that means "finished".

```sh
ID=$(runbaypty run --name job -- ./long-build.sh)
runbaypty events --json --session "$ID" | grep -m1 '"silence"'
echo ">>> job went quiet, it is done"
```

`grep -m1` blocks until the first `silence` line, then exits. No `while ps | grep` loop.

## Wait for a server to be ready

There is no CLI regex-watch, but `tail` plus `grep` gates on any marker line:

```sh
runbaypty run --name api -- ./serve.sh
runbaypty tail api | grep -q "Listening on :8080"    # blocks until the line appears
echo ">>> server up"
```

(For a server-side regex watch that pushes matches, use the Go SDK's `Watch`; see the `sdk` skill.)

## Per-command exit codes and output (OSC 133)

If the session's shell emits OSC 133 shell-integration marks (real shells do with shell integration enabled), the daemon tracks each command's boundaries and exit code:

```sh
runbaypty events --json --session <id> | grep command-finished
# {"Type":"command-finished","Data":{"start_seq":"8","end_seq":"36","exit_code":"0"}}
```

Each `command-finished` carries `exit_code` and the `start_seq`/`end_seq` byte range of that command's output. To pull the last command's exact output:

```sh
runbaypty lastcmd <id|name>
```

`lastcmd` returns the output window of the most recently finished command, sliced by its recorded boundaries. No screen-scraping.

## Agent pattern

Spawn a session, then in parallel: subscribe to `events --json --session $ID` and react. Use `silence` to know work paused, `command-finished` for exit codes, `bell` for attention, `exited` for termination. Because these are pushed by the daemon, an agent blocks on the event instead of guessing with `sleep`.
