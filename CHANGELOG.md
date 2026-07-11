# Changelog

All notable changes to runbaypty are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [SemVer](https://semver.org/) with the wire protocol versioned independently (additive-only within a major).

## [Unreleased]

### Added
- Wire protocol v1: length-prefixed binary frames over UDS and loopback WebSocket (one codec, two transports), HELLO negotiation, additive-only policy, doc-drift-gated PROTOCOL.md.
- Session engine: seq-numbered ring buffers with zero-gap `ATTACH {since_seq}` replay, pull-model subscribers, single write lock with explicit takeover, exit retention with TTL reaper.
- Events: created/exited/resized/attached/detached/renamed/meta-changed, activity/silence/bell, daemon-stopping, OSC 133 `command-started` / `command-finished {exit_code}` with ring-axis boundaries.
- `REPLAY_COMMAND` (last command's output window), server-side regex `WATCH` (RE2, per-connection, boundary-straddle-safe).
- Zero-downtime daemon upgrade: `serve --takeover` transfers PTY fds + ring state + the flock fd via `SCM_RIGHTS`; rollback on failure; mid-stream seq audit in CI.
- Scoped WebSocket tokens (control / read-only) with forced read-only attach downgrade.
- Durable `(timestamp, bytes)` session logs; `export` to asciinema cast v2; `tail` with exact log→live stitch.
- CLI: run · ls · info · attach (raw TTY, ctrl-\ detach) · kill · resize · rename · meta · events · tail · lastcmd · export · serve · daemon install/uninstall/start/stop/status (launchd + systemd).
- Go SDK (`pkg/client`): Spawn/Attach/Watch/LastCommandOutput/SubscribeEvents and `Follow` — resilient reader with automatic reconnect + zero-gap resume.
- Live foreground-process reporting in INFO (`fg_pid`/`fg_comm`), live cwd on Linux.
- Browser smoke client (`web/smoke.html`, xterm.js, protocol implemented in-page).

[Unreleased]: https://github.com/thesatellite-ai/runbaypty/commits/main
