# raw-protocol-node

**Use case:** you want to drive runbaypty from a language or runtime that has no SDK — or you just want to see how little there is to the wire protocol. This is the whole thing in one file of dependency-free JavaScript: open a WebSocket, pack a few integers, and you're spawning PTYs.

## Run it

Start the daemon with the WebSocket listener, then run the script:

```sh
bin/runbaypty serve --ws-port 8377 &
RUNBAYPTY_HOME=<daemon-home> node examples/raw-protocol-node/main.js
```

```
connected to daemon 0.1.0-dev (client cli_019f…)
spawned session ses_019f… (pid 48103) — attaching
attached; daemon last_seq=0
--- output ---
seq   0: "line 1\r\n"
seq   8: "line 2\r\n"
seq  16: "line 3\r\n"
--- exit ---
session exited: code=0
read 24 bytes; seq audit ended at 24 (contiguous)
```

No `npm install` — it uses Node's built-in global `WebSocket` (Node 22+). No runbaypty package. Just the framing rules and a socket.

## The frame is four fields

Every message in either direction is one frame:

```
[u32 length][u8 type][u16 headerLen][header JSON][payload bytes]
```

- **length** (big-endian u32) counts everything after itself: `type + headerLen + header + payload`.
- **type** (u8) is the frame type — a small closed enum (`HELLO=1`, `SPAWN=10`, `OUTPUT=14`, `ATTACH=21`, `EXIT=31`, …).
- **headerLen** (u16) is the byte length of the JSON header.
- **header** is a JSON object — the control fields for this frame.
- **payload** is raw bytes (PTY output on `OUTPUT`, keystrokes on `INPUT`) — empty for most control frames.

Encoding it is the entire "serialization layer":

```js
function encodeFrame(type, header, payload = Buffer.alloc(0)) {
  const h = Buffer.from(JSON.stringify(header));
  const total = 1 + 2 + h.length + payload.length;
  const buf = Buffer.alloc(4 + total);
  buf.writeUInt32BE(total, 0);
  buf.writeUInt8(type, 4);
  buf.writeUInt16BE(h.length, 5);
  h.copy(buf, 7);
  payload.copy(buf, 7 + h.length);
  return buf;
}
```

## One frame per WebSocket message

Decoding is even simpler than it looks, because of a deliberate design choice: the daemon writes **exactly one frame per binary WebSocket message**. So every `message` event carries one whole frame — you never buffer across messages or scan for boundaries. Set `ws.binaryType = 'arraybuffer'` and slice the fields straight out.

(This is the WS convenience. The [Python example](../raw-protocol-python/) speaks the same protocol over the raw Unix socket, where there are *no* message boundaries and the client must frame the byte stream itself — worth reading the two side by side to see the "one codec, two transports" design.)

## The exchange, start to finish

The whole program is a state machine over four incoming frame types:

1. **On open → send `HELLO`** with `protocol_version` and the control token. The token authenticates the WS connection (localhost WebSockets can't rely on file permissions the way the Unix socket does) and rides in the header, never the URL — URLs leak into logs.
2. **`HELLO_ACK` → send `SPAWN`** with `cmd`, `args`, `cols`, `rows`.
3. **`SPAWN_OK` → send `ATTACH`** with the returned `session_id` and `read_only: true`.
4. **`ATTACH_OK`** tells us the daemon's current `last_seq`; ring replay as `OUTPUT` frames follows.
5. **`OUTPUT`** frames carry the seq of their first payload byte. We print them and audit the counter.
6. **`EXIT`** tells us the process ended — our cue to close.

## The seq audit — proving zero-gap by hand

Each `OUTPUT` header has a `seq`: the ring position of the first byte in its payload. The invariant is that the *next* frame's seq equals this seq plus this payload's length. The example checks it explicitly:

```js
if (seq !== expectedSeq) { /* GAP */ }
expectedSeq = seq + payload.length;
```

`seq 0, 8, 16` for three 8-byte lines (`"line N\r\n"` — the PTY turns `\n` into `\r\n`) means the stream is perfectly contiguous. This is the same seq axis the SDK uses for zero-gap reattach; here you can watch it advance a frame at a time. Feed those seq numbers into an `ATTACH{since_seq}` and you have reattach — the [reattach-zero-gap](../reattach-zero-gap/) example, built from these primitives.

## Getting the token

The daemon writes two tokens, mode 0600, into its home directory at boot: `token` (control scope — the full protocol) and `token.ro` (read-only scope). This example reads `token` because it spawns. To only watch a session, read `token.ro` instead and you physically cannot take the write lock or spawn — see [read-only-share](../read-only-share/).

## Next

- [raw-protocol-python](../raw-protocol-python/) — the same protocol over the Unix socket, where you frame the byte stream yourself
- [browser-terminal](../browser-terminal/) — this same WS transport driving a real xterm.js terminal
- [reattach-zero-gap](../reattach-zero-gap/) — what the seq audit here is the foundation of
