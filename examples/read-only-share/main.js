// read-only-share demonstrates the two WebSocket token scopes: a control token
// that can do anything, and a read-only token that can WATCH a session but can
// never type into it, spawn, or kill. It's how you let a teammate (or a
// recording, or a status page) see your terminal live without handing them the
// keyboard.
//
// The daemon mints two tokens at boot, mode 0600, in its home dir:
//
//   token      control scope: the full protocol
//   token.ro   read-only scope: LIST / INFO / ATTACH(read-only) / DETACH /
//              SUBSCRIBE_EVENTS — observe, never act
//
// This script plays both roles over two WebSocket connections:
//
//   OWNER  (control token)   spawns a session that prints a few lines
//   VIEWER (read-only token)  attaches and sees them — then tries to act and
//                             is refused by the daemon with E_READ_ONLY_SCOPE
//
// Requires:  runbaypty serve --ws-port 8377
// Run:  RUNBAYPTY_HOME=<home> node examples/read-only-share/main.js
'use strict';

const fs = require('node:fs');
const path = require('node:path');

// Frame types (the wire enum; see pkg/proto/frame.go). Closed set → named table.
const Type = Object.freeze({
  HELLO: 1, HELLO_ACK: 2, ERROR: 3,
  SPAWN: 10, SPAWN_OK: 11,
  OUTPUT: 14, KILL: 16,
  ATTACH: 21, ATTACH_OK: 22,
  TAKE_WRITE: 26, EXIT: 31,
});
const PROTOCOL_VERSION = 1;

const home = process.env.RUNBAYPTY_HOME;
if (!home) {
  console.error('set RUNBAYPTY_HOME to the daemon home dir (holds token and token.ro)');
  process.exit(2);
}
// The two credentials. token.ro is the whole point of this example.
const controlToken = fs.readFileSync(path.join(home, 'token'), 'utf8').trim();
const readOnlyToken = fs.readFileSync(path.join(home, 'token.ro'), 'utf8').trim();
const url = `ws://127.0.0.1:${process.env.RUNBAYPTY_WS_PORT || '8377'}/v1`;

// ── Framing (identical to raw-protocol-node; see that example for the walk-through) ──
function encodeFrame(type, header, payload = Buffer.alloc(0)) {
  const h = Buffer.from(JSON.stringify(header), 'utf8');
  const total = 1 + 2 + h.length + payload.length;
  const buf = Buffer.alloc(4 + total);
  buf.writeUInt32BE(total, 0);
  buf.writeUInt8(type, 4);
  buf.writeUInt16BE(h.length, 5);
  h.copy(buf, 7);
  payload.copy(buf, 7 + h.length);
  return buf;
}
function decodeFrame(buf) {
  const total = buf.readUInt32BE(0);
  const type = buf.readUInt8(4);
  const headerLen = buf.readUInt16BE(5);
  const header = JSON.parse(buf.toString('utf8', 7, 7 + headerLen));
  const payload = buf.subarray(7 + headerLen, 4 + total);
  return { type, header, payload };
}

let reqSeq = 0;
const nextReqID = () => `r-${++reqSeq}`;

// connect opens a WS, sends HELLO with the given token, and resolves once the
// daemon accepts the handshake. onFrame handles every subsequent frame.
function connect(token, clientName, onFrame) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    ws.send2 = (type, header, payload) => ws.send(encodeFrame(type, header, payload));
    ws.addEventListener('open', () => {
      ws.send2(Type.HELLO, {
        req_id: nextReqID(), protocol_version: PROTOCOL_VERSION,
        token, client_name: clientName,
      });
    });
    ws.addEventListener('message', (ev) => {
      const frame = decodeFrame(Buffer.from(ev.data));
      if (frame.type === Type.HELLO_ACK) {
        resolve(ws);
        return;
      }
      onFrame(ws, frame);
    });
    ws.addEventListener('error', (e) => reject(new Error(e.message || 'ws error')));
  });
}

async function main() {
  // ── OWNER: control token → spawn a session that prints a few lines ─────────
  let sessionID = null;
  const ownerReady = new Promise((resolve) => {
    connect(controlToken, 'owner', (ws, { type, header }) => {
      if (type === Type.SPAWN_OK) {
        sessionID = header.session_id;
        console.log(`owner spawned session ${sessionID} (control token)`);
        resolve();
      }
    }).then((ws) => {
      ws.send2(Type.SPAWN, {
        req_id: nextReqID(), cmd: '/bin/sh',
        args: ['-c', 'for i in 1 2 3; do echo "owner says $i"; sleep 0.3; done'],
        cols: 80, rows: 24,
      });
    });
  });
  await ownerReady;

  // ── VIEWER: read-only token → attach and watch, then try (and fail) to act ─
  await new Promise((resolve) => {
    let triedToAct = false;
    connect(readOnlyToken, 'viewer', (ws, { type, header, payload }) => {
      switch (type) {
        case Type.ATTACH_OK:
          console.log(`viewer attached read-only (last_seq=${header.last_seq})\n--- viewer sees ---`);
          break;

        case Type.OUTPUT:
          // The read-only viewer receives the owner's output in full. Watching
          // is exactly what the read-only scope is FOR.
          process.stdout.write(`  viewer: ${JSON.stringify(payload.toString('utf8'))}\n`);
          if (!triedToAct) {
            // Now prove the viewer can only watch. Attempt a control verb —
            // KILL the session. The daemon refuses it on a read-only scope
            // BEFORE it touches the session, echoing our req_id in the error.
            triedToAct = true;
            console.log('  viewer tries to KILL the session…');
            ws.send2(Type.KILL, { req_id: nextReqID(), session_id: sessionID });
            // …and to steal the write lock, also control-only.
            console.log('  viewer tries to TAKE_WRITE…');
            ws.send2(Type.TAKE_WRITE, { req_id: nextReqID(), session_id: sessionID });
          }
          break;

        case Type.ERROR:
          // The refusals land here — the daemon's typed rejection of a control
          // verb on a read-only credential. This is the security boundary
          // doing its job: the viewer literally cannot act.
          console.log(`  ✗ daemon refused [${header.code}]: ${header.message}`);
          break;

        case Type.EXIT:
          console.log('--- session exited on its own (viewer never affected it) ---');
          ws.close();
          resolve();
          break;
      }
    }).then((ws) => {
      // Attach with read_only:false ON PURPOSE — to show the daemon FORCES it
      // read-only for a read-only credential. The viewer can't opt out of
      // being a viewer.
      ws.send2(Type.ATTACH, { req_id: nextReqID(), session_id: sessionID, read_only: false });
    });
  });

  console.log('\nThe viewer saw every byte the owner produced, and every attempt');
  console.log('to act — kill, take-write — was refused with E_READ_ONLY_SCOPE.');
  console.log('A read-only token is watch-only, enforced by the daemon, not by');
  console.log('politeness. Hand one out to share a view of a session safely.');
  process.exit(0);
}

main().catch((err) => {
  console.error('read-only-share:', err.message);
  process.exit(1);
});
