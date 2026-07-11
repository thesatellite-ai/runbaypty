// raw-protocol-node speaks the runbaypty wire protocol directly over the
// loopback WebSocket — no SDK, just the framing rules and a WebSocket. It
// exists to show the protocol is small enough to reimplement in any language
// in well under 100 lines: if you can open a WebSocket and pack a few integers,
// you can drive runbaypty.
//
// It performs a full round trip: HELLO (with the control token) → SPAWN a short
// command → ATTACH to it → read the OUTPUT frames (auditing the seq counter as
// it goes) → stop at EXIT.
//
// Wire frame layout (all integers big-endian), one frame per binary WS message:
//
//   [u32 length][u8 type][u16 headerLen][header JSON][payload bytes]
//
// `length` counts everything AFTER itself: type + headerLen + header + payload.
//
// Requires the daemon's WebSocket listener:  runbaypty serve --ws-port 8377
// Run:  RUNBAYPTY_HOME=<home> node examples/raw-protocol-node/main.js
'use strict';

const fs = require('node:fs');
const path = require('node:path');

// ── Frame types (the wire enum; see pkg/proto/frame.go) ──────────────────────
// A closed set, so it lives in one named table rather than as bare numbers at
// the call sites.
const Type = Object.freeze({
  HELLO: 1,
  HELLO_ACK: 2,
  ERROR: 3,
  SPAWN: 10,
  SPAWN_OK: 11,
  OUTPUT: 14,
  ATTACH: 21,
  ATTACH_OK: 22,
  EXIT: 31,
});
const typeName = Object.fromEntries(Object.entries(Type).map(([k, v]) => [v, k]));

const PROTOCOL_VERSION = 1; // must match the daemon's major (HELLO negotiation)

// ── Config: where the daemon is and how to authenticate ──────────────────────
// The token authenticates WS connections (UDS trusts file perms; WS can't).
// It rides in the HELLO header, never the URL — URLs leak into logs. We read
// the control token the daemon wrote 0600 into its home directory.
const home = process.env.RUNBAYPTY_HOME;
if (!home) {
  console.error('set RUNBAYPTY_HOME to the daemon home dir (holds the token file)');
  process.exit(2);
}
const token = fs.readFileSync(path.join(home, 'token'), 'utf8').trim();
const port = process.env.RUNBAYPTY_WS_PORT || '8377';
const url = `ws://127.0.0.1:${port}/v1`;

// ── Framing ──────────────────────────────────────────────────────────────────

// encodeFrame packs a type + JSON header (+ optional payload) into one wire
// frame. This is the entire "serialization layer" — there isn't more to it.
function encodeFrame(type, header, payload = Buffer.alloc(0)) {
  const headerBuf = Buffer.from(JSON.stringify(header), 'utf8');
  const total = 1 + 2 + headerBuf.length + payload.length; // type + headerLen + header + payload
  const buf = Buffer.alloc(4 + total);
  buf.writeUInt32BE(total, 0); // length prefix (excludes itself)
  buf.writeUInt8(type, 4);
  buf.writeUInt16BE(headerBuf.length, 5);
  headerBuf.copy(buf, 7);
  payload.copy(buf, 7 + headerBuf.length);
  return buf;
}

// decodeFrame unpacks one wire frame. Because the daemon writes exactly one
// frame per binary WS message, each message we receive IS one whole frame — we
// never have to buffer across messages or hunt for boundaries.
function decodeFrame(buf) {
  const total = buf.readUInt32BE(0);
  const type = buf.readUInt8(4);
  const headerLen = buf.readUInt16BE(5);
  const header = JSON.parse(buf.toString('utf8', 7, 7 + headerLen));
  const payload = buf.subarray(7 + headerLen, 4 + total);
  return { type, header, payload };
}

// A tiny request-id source; the daemon echoes req_id back on acks and errors.
let reqSeq = 0;
const nextReqID = () => `n-${++reqSeq}`;

// ── The exchange ─────────────────────────────────────────────────────────────

const ws = new WebSocket(url);
ws.binaryType = 'arraybuffer';

// State carried across the async message handler.
let sessionID = null;
let expectedSeq = null; // running audit of the OUTPUT sequence counter
let outputBytes = 0;

const send = (type, header, payload) => ws.send(encodeFrame(type, header, payload));

ws.addEventListener('open', () => {
  // Step 1: HELLO. Announce our protocol major and present the control token.
  send(Type.HELLO, {
    req_id: nextReqID(),
    protocol_version: PROTOCOL_VERSION,
    token,
    client_name: 'raw-protocol-node',
  });
});

ws.addEventListener('message', (ev) => {
  const { type, header, payload } = decodeFrame(Buffer.from(ev.data));

  switch (type) {
    case Type.HELLO_ACK:
      // Handshake done. Step 2: SPAWN a short command to observe.
      console.log(`connected to daemon ${header.daemon_version} (client ${header.client_id})`);
      send(Type.SPAWN, {
        req_id: nextReqID(),
        cmd: '/bin/sh',
        args: ['-c', 'for i in 1 2 3; do echo "line $i"; sleep 0.2; done'],
        cols: 80,
        rows: 24,
      });
      break;

    case Type.SPAWN_OK:
      // Step 3: ATTACH to the new session. No since_seq → replay the whole ring
      // then stream live. read_only: we only observe here.
      sessionID = header.session_id;
      console.log(`spawned session ${sessionID} (pid ${header.pid}) — attaching`);
      send(Type.ATTACH, { req_id: nextReqID(), session_id: sessionID, read_only: true });
      break;

    case Type.ATTACH_OK:
      // The daemon is current at last_seq; replay OUTPUT frames follow. Seed the
      // seq audit so we can prove the stream is gap-free.
      expectedSeq = 0;
      console.log(`attached; daemon last_seq=${header.last_seq}\n--- output ---`);
      break;

    case Type.OUTPUT: {
      // The header carries the seq of the FIRST payload byte. The next frame's
      // seq must equal this seq + this payload length — that invariant is the
      // whole basis of zero-gap reattach, and we check it here by hand.
      const seq = header.seq;
      if (expectedSeq !== null && seq !== expectedSeq) {
        console.error(`SEQ GAP: expected ${expectedSeq}, got ${seq}`);
        ws.close();
        process.exitCode = 1;
        return;
      }
      outputBytes += payload.length;
      expectedSeq = seq + payload.length;
      process.stdout.write(`seq ${String(seq).padStart(3)}: ${JSON.stringify(payload.toString('utf8'))}\n`);
      break;
    }

    case Type.EXIT:
      // The process ended. This is our cue to stop.
      console.log('--- exit ---');
      console.log(`session exited: code=${header.exit_code}${header.signal ? ` signal=${header.signal}` : ''}`);
      console.log(`read ${outputBytes} bytes; seq audit ended at ${expectedSeq} (contiguous)`);
      ws.close();
      break;

    case Type.ERROR:
      console.error(`daemon ERROR [${header.code}]: ${header.message}`);
      ws.close();
      process.exitCode = 1;
      break;

    default:
      // Additive forward-compat: unknown frame types are skipped, never fatal.
      console.log(`(ignoring ${typeName[type] || `type ${type}`})`);
  }
});

ws.addEventListener('error', (ev) => {
  console.error('websocket error:', ev.message || ev);
  process.exitCode = 1;
});
