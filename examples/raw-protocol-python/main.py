#!/usr/bin/env python3
"""raw-protocol-python speaks the runbaypty wire protocol directly over the Unix
domain socket — no SDK, no third-party packages, just the standard library.

Where raw-protocol-node uses the WebSocket transport (for browsers), this uses
the UDS transport (for scripting and CI glue on the same host). It is the same
one codec over a different pipe: over UDS there are no message boundaries, so
the client must frame the byte stream itself — read the u32 length, then read
exactly that many bytes. That framing loop is the heart of this example.

It performs a full round trip and makes the control-plane / data-plane split
explicit: a handful of request/reply CONTROL frames (HELLO, SPAWN, ATTACH) set
things up, then a stream of DATA-plane frames (OUTPUT, seq-numbered) flows until
EXIT.

Wire frame layout (all integers big-endian):

    [u32 length][u8 type][u16 headerLen][header JSON][payload bytes]

`length` counts everything AFTER itself: type + headerLen + header + payload.

Run:  RUNBAYPTY_SOCK=/tmp/rpty-ex.sock python3 examples/raw-protocol-python/main.py
"""

import json
import os
import socket
import struct
import sys

# ── Frame types (the wire enum; see pkg/proto/frame.go) ──────────────────────
# A closed set, so it lives in one named table, not as bare numbers at call sites.
HELLO, HELLO_ACK, ERROR = 1, 2, 3
SPAWN, SPAWN_OK = 10, 11
OUTPUT = 14
ATTACH, ATTACH_OK = 21, 22
EXIT = 31

TYPE_NAME = {
    HELLO: "HELLO", HELLO_ACK: "HELLO_ACK", ERROR: "ERROR",
    SPAWN: "SPAWN", SPAWN_OK: "SPAWN_OK", OUTPUT: "OUTPUT",
    ATTACH: "ATTACH", ATTACH_OK: "ATTACH_OK", EXIT: "EXIT",
}

PROTOCOL_VERSION = 1  # must match the daemon's major (HELLO negotiation)


class Conn:
    """A framed connection over the daemon's Unix socket.

    Wraps a stream socket with encode/recv-one-frame so the rest of the program
    thinks in whole frames, not bytes. This is the piece a language without an
    SDK has to supply; it is about 20 lines.
    """

    def __init__(self, sock_path: str):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(sock_path)
        self._req = 0

    def next_req_id(self) -> str:
        self._req += 1
        return f"py-{self._req}"

    def send(self, ftype: int, header: dict, payload: bytes = b"") -> None:
        """Pack a control/data frame and write it. One send == one frame."""
        header_bytes = json.dumps(header).encode("utf-8")
        total = 1 + 2 + len(header_bytes) + len(payload)  # type + headerLen + header + payload
        frame = (
            struct.pack(">I", total)          # u32 length prefix (excludes itself)
            + struct.pack(">B", ftype)        # u8 type
            + struct.pack(">H", len(header_bytes))  # u16 header length
            + header_bytes
            + payload
        )
        self.sock.sendall(frame)

    def _recv_exactly(self, n: int) -> bytes:
        """Read exactly n bytes or raise on a short/closed stream. This is why
        UDS needs explicit framing: recv() may return fewer bytes than asked, so
        we loop until we have the whole frame."""
        buf = bytearray()
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise ConnectionError("daemon closed the connection")
            buf.extend(chunk)
        return bytes(buf)

    def recv(self):
        """Read one whole frame → (type, header dict, payload bytes)."""
        total = struct.unpack(">I", self._recv_exactly(4))[0]
        body = self._recv_exactly(total)
        ftype = body[0]
        header_len = struct.unpack(">H", body[1:3])[0]
        header = json.loads(body[3 : 3 + header_len].decode("utf-8"))
        payload = body[3 + header_len :]
        return ftype, header, payload

    def close(self) -> None:
        self.sock.close()


def main() -> int:
    sock_path = os.environ.get("RUNBAYPTY_SOCK")
    if not sock_path:
        print("set RUNBAYPTY_SOCK to the daemon's socket path", file=sys.stderr)
        return 2

    conn = Conn(sock_path)

    # ── Control plane: three request/reply frames set the session up ─────────
    # Step 1: HELLO. Over UDS no token is needed — the daemon trusts the socket's
    # file permissions, so authentication is the filesystem's job, not ours.
    conn.send(HELLO, {
        "req_id": conn.next_req_id(),
        "protocol_version": PROTOCOL_VERSION,
        "client_name": "raw-protocol-python",
    })
    ftype, header, _ = conn.recv()
    if ftype == ERROR:
        print(f"daemon ERROR [{header['code']}]: {header['message']}", file=sys.stderr)
        return 1
    assert ftype == HELLO_ACK, f"expected HELLO_ACK, got {TYPE_NAME.get(ftype, ftype)}"
    print(f"connected to daemon {header['daemon_version']} (client {header['client_id']})")

    # Step 2: SPAWN a short command to observe.
    conn.send(SPAWN, {
        "req_id": conn.next_req_id(),
        "cmd": "/bin/sh",
        "args": ["-c", 'for i in 1 2 3; do echo "line $i"; sleep 0.2; done'],
        "cols": 80,
        "rows": 24,
    })
    ftype, header, _ = conn.recv()
    assert ftype == SPAWN_OK, f"expected SPAWN_OK, got {TYPE_NAME.get(ftype, ftype)}"
    session_id = header["session_id"]
    print(f"spawned session {session_id} (pid {header['pid']}) — attaching")

    # Step 3: ATTACH. No since_seq → replay the whole ring, then stream live.
    conn.send(ATTACH, {"req_id": conn.next_req_id(), "session_id": session_id, "read_only": True})
    ftype, header, _ = conn.recv()
    assert ftype == ATTACH_OK, f"expected ATTACH_OK, got {TYPE_NAME.get(ftype, ftype)}"
    print(f"attached; daemon last_seq={header['last_seq']}\n--- output ---")

    # ── Data plane: OUTPUT frames stream until EXIT ──────────────────────────
    # Now the shape flips: no more request/reply, just a one-way push of
    # seq-numbered OUTPUT frames. We audit the seq counter as it advances — the
    # seq of each frame must equal the previous seq plus the previous payload
    # length, the invariant that makes zero-gap reattach possible.
    expected_seq = 0
    output_bytes = 0
    while True:
        ftype, header, payload = conn.recv()
        if ftype == OUTPUT:
            seq = header["seq"]
            if seq != expected_seq:
                print(f"SEQ GAP: expected {expected_seq}, got {seq}", file=sys.stderr)
                conn.close()
                return 1
            output_bytes += len(payload)
            expected_seq = seq + len(payload)
            print(f"seq {seq:>3}: {payload.decode('utf-8', 'replace')!r}")
        elif ftype == EXIT:
            print("--- exit ---")
            sig = f" signal={header['signal']}" if header.get("signal") else ""
            print(f"session exited: code={header['exit_code']}{sig}")
            print(f"read {output_bytes} bytes; seq audit ended at {expected_seq} (contiguous)")
            break
        elif ftype == ERROR:
            print(f"daemon ERROR [{header['code']}]: {header['message']}", file=sys.stderr)
            conn.close()
            return 1
        # Any other (unknown) frame type is skipped — additive forward-compat.

    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
