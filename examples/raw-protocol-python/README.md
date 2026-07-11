# raw-protocol-python

**Use case:** scripting and CI glue on the same host as the daemon — a Python job that spawns a command, watches its output, and reacts, with no SDK and no `pip install`. This is the wire protocol over the Unix socket in one file of pure standard library.

## Run it

```sh
RUNBAYPTY_SOCK=/tmp/rpty-ex.sock python3 examples/raw-protocol-python/main.py
```

```
connected to daemon 0.1.0-dev (client cli_019f…)
spawned session ses_019f… (pid 51076) — attaching
attached; daemon last_seq=0
--- output ---
seq   0: 'line 1\r\n'
seq   8: 'line 2\r\n'
seq  16: 'line 3\r\n'
--- exit ---
session exited: code=0
read 24 bytes; seq audit ended at 24 (contiguous)
```

Only `json`, `os`, `socket`, `struct`, `sys` — all stdlib. Nothing to install, which is exactly what you want for a CI runner or a bootstrap script.

## Same protocol, different transport

The [Node example](../raw-protocol-node/) speaks this identical protocol over the loopback **WebSocket** — the transport a browser needs. This one speaks it over the **Unix domain socket** — the transport a same-host script wants. The daemon uses one codec for both (see `pkg/proto`); the only differences a client sees are:

| | UDS (this example) | WebSocket ([node](../raw-protocol-node/)) |
|---|---|---|
| Auth | file permissions on the socket — **no token** | a token in the `HELLO` header |
| Framing | a **byte stream** — you frame it yourself | one frame per WS message — boundaries for free |
| Reach | same host only | any localhost process, including browsers |

## Why UDS makes you frame the stream

A WebSocket delivers discrete messages, so each message is conveniently one whole frame. A Unix socket delivers a **byte stream**: `recv()` can hand you half a frame, or two frames at once. So the client has to reassemble frames itself — read the 4-byte length, then read exactly that many bytes:

```python
def _recv_exactly(self, n):
    buf = bytearray()
    while len(buf) < n:
        chunk = self.sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("daemon closed the connection")
        buf.extend(chunk)
    return bytes(buf)

def recv(self):
    total = struct.unpack(">I", self._recv_exactly(4))[0]
    body = self._recv_exactly(total)
    # body = [u8 type][u16 headerLen][header JSON][payload]
    ...
```

That `_recv_exactly` loop is the one thing UDS demands that WebSocket gives you for free. It's ~10 lines, and it's the whole reason this example wraps the socket in a small `Conn` class: past it, the program thinks in whole frames.

## The control plane / data plane split, made visible

The example is structured in two distinct phases, and the protocol is too:

**Control plane** — a handful of request/reply frames that set things up. Each is a send-then-receive:

```python
conn.send(HELLO,  {...});  ftype, header, _ = conn.recv()  # → HELLO_ACK
conn.send(SPAWN,  {...});  ftype, header, _ = conn.recv()  # → SPAWN_OK
conn.send(ATTACH, {...});  ftype, header, _ = conn.recv()  # → ATTACH_OK
```

**Data plane** — after `ATTACH_OK` the shape flips. No more request/reply; just a one-way push of seq-numbered `OUTPUT` frames flowing until `EXIT`:

```python
while True:
    ftype, header, payload = conn.recv()
    if ftype == OUTPUT: ...   # seq-numbered bytes
    elif ftype == EXIT: break
```

This split is fundamental to runbaypty: control is transactional and low-volume; data is a firehose the daemon pushes at you. Separating them is why one slow reader can't stall the control plane, and why backpressure is per-subscriber (see [slow-consumer](../slow-consumer/)).

## Authentication is the filesystem's job here

Notice the `HELLO` sends no token. Over the Unix socket, the daemon trusts the socket's file permissions — if you can `connect()` to it, the OS already decided you're allowed. That's the same trust model as `ssh-agent` or a database socket. Put the socket in a directory only you can enter and access control is handled, no tokens to manage. (Over the network-facing WebSocket that reasoning doesn't hold, which is why *that* transport requires a token — see [read-only-share](../read-only-share/).)

## The seq audit

Just like the Node example, each `OUTPUT` header carries the seq of its first byte, and the next frame's seq must equal this one's seq plus its payload length. `seq 0, 8, 16` for three 8-byte lines proves the stream is contiguous. Hand those seq values to an `ATTACH` with a `since_seq` field and you've built zero-gap reattach from scratch.

## Next

- [raw-protocol-node](../raw-protocol-node/) — the same protocol over WebSocket, with token auth and message framing for free
- [slow-consumer](../slow-consumer/) — why the control/data split means a slow reader can't stall anything
- [wait-for-silence](../wait-for-silence/) — the SDK version of "spawn, watch output, react" this example does by hand
