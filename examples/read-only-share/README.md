# read-only-share

**Use case:** let someone watch your terminal session live — a teammate pairing, a support engineer, a status dashboard, a recording — **without** the ability to type into it, kill it, or spawn anything. Share the view, keep the keyboard.

## Run it

Start the daemon with the WebSocket listener, then run the script:

```sh
bin/runbaypty serve --ws-port 8377 &
RUNBAYPTY_HOME=<daemon-home> node examples/read-only-share/main.js
```

```
owner spawned session ses_019f… (control token)
viewer attached read-only (last_seq=0)
--- viewer sees ---
  viewer: "owner says 1\r\n"
  viewer tries to KILL the session…
  viewer tries to TAKE_WRITE…
  ✗ daemon refused [E_READ_ONLY_SCOPE]: KILL requires the control token
  ✗ daemon refused [E_READ_ONLY_SCOPE]: TAKE_WRITE requires the control token
  viewer: "owner says 2\r\n"
  viewer: "owner says 3\r\n"
--- session exited on its own (viewer never affected it) ---
```

The script plays two roles over two WebSocket connections: an **owner** (control token) that spawns a session, and a **viewer** (read-only token) that watches it and then discovers it can't touch it.

## Two tokens, two scopes

WebSocket connections can't be authenticated by file permissions the way the Unix socket can (any localhost process can open a TCP connection). So the daemon mints two tokens at boot, mode 0600, in its home directory:

| File | Scope | Can do |
|---|---|---|
| `token` | control | the full protocol — spawn, input, kill, take-write, everything |
| `token.ro` | read-only | `LIST`, `INFO`, `ATTACH` (forced read-only), `DETACH`, `SUBSCRIBE_EVENTS` — observe only |

Sharing is just handing over the right token. Give someone `token.ro` and they can watch every session on the daemon but change nothing. Sharing a *single* session read-only is a matter of giving them the token plus the session id — the [remote-over-ssh](../remote-over-ssh/) example shows how to get that token to a remote viewer.

## The boundary is enforced by the daemon, not by convention

This is the part that matters for security: read-only isn't a suggestion the client politely honors. The daemon checks the connection's scope on **every control frame** and refuses the whole class before it touches any session:

```go
// pkg/daemon/conn.go
if c.scopeReadOnly() && controlOnlyTypes[f.Type] {
    _ = c.sendErr(probe.ReqID,
        errcodes.Newf(errcodes.ReadOnlyScope, "%s requires the control token", f.Type))
    // …frame is dropped; never dispatched
}
```

So when the viewer sends `KILL` and `TAKE_WRITE`, both come back as `E_READ_ONLY_SCOPE` — the daemon rejected them at the door, echoing the request id so the client knows exactly which call failed. A malicious or buggy read-only client can hammer control verbs all day; none of them do anything.

## "Forced read-only attach" — you can't opt out of being a viewer

Notice the viewer attaches with `read_only: false` **on purpose**:

```js
ws.send2(Type.ATTACH, { session_id, read_only: false }); // asks for a writable attach
```

…and still ends up read-only. A read-only credential can never yield a writable attach — the daemon overrides the request:

```go
if c.scopeReadOnly() {
    m.ReadOnly = true // a read-only credential can never yield a writable attach
}
```

This closes the obvious loophole. If a read-only client could request a writable attach and then take the write lock, the scope would be meaningless. Instead, the scope wins over the attach flag every time. Even the write lock — which normally *any* writer can steal (see [write-lock-handoff](../write-lock-handoff/)) — is unreachable from a read-only connection, because `TAKE_WRITE` itself is a control verb.

## What the viewer can still do

Read-only is watch-only, but watching is powerful. A read-only client gets the full observation surface: attach and see live output (with zero-gap replay of the scrollback), list sessions, read `Info`, and subscribe to the lifecycle event stream. That's everything [session-dashboard](../session-dashboard/) needs — which means you can serve a live, auto-updating dashboard of every session to a browser holding only `token.ro`, with a hard guarantee it can't start, stop, or drive anything.

## Next

- [raw-protocol-node](../raw-protocol-node/) — the framing and handshake this builds on, explained in full
- [remote-over-ssh](../remote-over-ssh/) — get a read-only token to a viewer on another machine
- [write-lock-handoff](../write-lock-handoff/) — the write lock the read-only scope can never reach
