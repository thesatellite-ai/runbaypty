// Package proto implements the runbaypty wire protocol: length-prefixed
// binary frames carrying a JSON control header and an optional raw payload.
//
// Frame layout (all integers big-endian):
//
//	[u32 length][u8 type][u16 headerLen][header JSON][payload bytes]
//
// length covers everything after itself (type + headerLen + header + payload).
// The same framing rides both transports: raw bytes over UDS, and one binary
// WebSocket message per frame over WS. One codec, two transports.
//
// Versioning policy: ADDITIVE ONLY. New frame types and new JSON header
// fields may be added in minor versions; existing type values and field
// meanings never change. Removal or re-numbering is a protocol-major bump
// (see HELLO negotiation in messages.go).
package proto

// FrameType is the closed enum of wire frame types. Rule: NO BARE numbers at
// call sites — every switch/dispatch references these constants. Values are
// wire contract: append new types, never renumber, never reuse.
type FrameType uint8

const (
	// ── Handshake / errors (1–9) ────────────────────────────────────
	TypeHello    FrameType = 1 // client → daemon: protocol version + optional token
	TypeHelloAck FrameType = 2 // daemon → client: accepted; daemon + protocol version
	TypeError    FrameType = 3 // daemon → client: typed error, echoes reqId when present

	// ── Session control (10–29) ─────────────────────────────────────
	TypeSpawn        FrameType = 10 // client → daemon: start a PTY session
	TypeSpawnOK      FrameType = 11 // daemon → client: session id + pid
	TypeInput        FrameType = 12 // client → daemon: stdin bytes (payload); needs write lock
	TypeInputEOF     FrameType = 13 // client → daemon: half-close stdin without killing
	TypeOutput       FrameType = 14 // daemon → client: PTY output bytes (payload), seq-numbered
	TypeResize       FrameType = 15 // client → daemon: set cols/rows (last-writer-wins)
	TypeKill         FrameType = 16 // client → daemon: signal the process tree
	TypeList         FrameType = 17 // client → daemon: enumerate sessions
	TypeListOK       FrameType = 18 // daemon → client: session summaries
	TypeInfo         FrameType = 19 // client → daemon: one session's detail
	TypeInfoOK       FrameType = 20 // daemon → client: full SessionInfo
	TypeAttach       FrameType = 21 // client → daemon: subscribe to a session's stream
	TypeAttachOK     FrameType = 22 // daemon → client: attach accepted; replay follows as OUTPUT
	TypeDetach       FrameType = 23 // client → daemon: unsubscribe, keep connection
	TypeRename       FrameType = 24 // client → daemon: change session name
	TypeSetMeta      FrameType = 25 // client → daemon: replace client-owned KV metadata
	TypeTakeWrite    FrameType = 26 // client → daemon: claim the single write lock
	TypeReleaseWrite FrameType = 27 // client → daemon: release the write lock
	TypeSubEvents    FrameType = 28 // client → daemon: subscribe to lifecycle events
	TypeOK           FrameType = 29 // daemon → client: generic success ack (echoes reqId)

	// ── Server pushes (30–39) ───────────────────────────────────────
	TypeEvent FrameType = 30 // daemon → client: lifecycle/activity event
	TypeExit  FrameType = 31 // daemon → client: session process exited

	// ── Reserved (40–49): v1.5/v2 semantic + watch layer ────────────
	// Reserved NOW so old daemons treat them as unknown-but-skippable
	// rather than colliding with a future reassignment (task-m1-reserve).
	TypeCommandStarted  FrameType = 40 // reserved: OSC 133 command began (ships as EVENT types today)
	TypeCommandFinished FrameType = 41 // reserved: OSC 133 command ended (ships as EVENT types today)
	TypeWatch           FrameType = 42 // client → daemon: register a server-side regex watch
	TypeWatchEvent      FrameType = 43 // daemon → client: watch matched (pushed)
	TypeReplayCommand   FrameType = 44 // client → daemon: replay the last OSC 133 command's output
	TypeReplayCommandOK FrameType = 45 // daemon → client: the window slice (payload) + boundaries
	TypeHandoverReq     FrameType = 46 // new daemon → old daemon: begin takeover (UDS only)
	TypeWatchOK         FrameType = 47 // daemon → client: watch registered, carries watch_id
	TypeSetMetaJSON     FrameType = 48 // client → daemon: merge/replace the JSON meta document
)

// knownTypes maps every assigned frame type to its wire name (used by logs,
// PROTOCOL.md drift checks, and the no-duplicates test). Reserved types are
// listed: they are assigned, just not implemented yet.
var knownTypes = map[FrameType]string{
	TypeHello:    "HELLO",
	TypeHelloAck: "HELLO_ACK",
	TypeError:    "ERROR",

	TypeSpawn:        "SPAWN",
	TypeSpawnOK:      "SPAWN_OK",
	TypeInput:        "INPUT",
	TypeInputEOF:     "INPUT_EOF",
	TypeOutput:       "OUTPUT",
	TypeResize:       "RESIZE",
	TypeKill:         "KILL",
	TypeList:         "LIST",
	TypeListOK:       "LIST_OK",
	TypeInfo:         "INFO",
	TypeInfoOK:       "INFO_OK",
	TypeAttach:       "ATTACH",
	TypeAttachOK:     "ATTACH_OK",
	TypeDetach:       "DETACH",
	TypeRename:       "RENAME",
	TypeSetMeta:      "SET_META",
	TypeTakeWrite:    "TAKE_WRITE",
	TypeReleaseWrite: "RELEASE_WRITE",
	TypeSubEvents:    "SUBSCRIBE_EVENTS",
	TypeOK:           "OK",

	TypeEvent: "EVENT",
	TypeExit:  "EXIT",

	TypeCommandStarted:  "COMMAND_STARTED",
	TypeCommandFinished: "COMMAND_FINISHED",
	TypeWatch:           "WATCH",
	TypeWatchEvent:      "WATCH_EVENT",
	TypeReplayCommand:   "REPLAY_COMMAND",
	TypeReplayCommandOK: "REPLAY_COMMAND_OK",
	TypeHandoverReq:     "HANDOVER_REQ",
	TypeWatchOK:         "WATCH_OK",
	TypeSetMetaJSON:     "SET_META_JSON",
}

// String returns the wire name of the type, or "UNKNOWN(<n>)" for
// unassigned values (which the dispatcher skips with a warning — additive
// forward-compat, never a connection kill).
func (t FrameType) String() string {
	if n, ok := knownTypes[t]; ok {
		return n
	}
	return "UNKNOWN(" + itoa(uint64(t)) + ")"
}

// IsKnown reports whether the type is assigned in this binary's registry.
func (t FrameType) IsKnown() bool {
	_, ok := knownTypes[t]
	return ok
}

// Wire size limits. MaxHeaderLen is bounded by the u16 length field; frames
// larger than MaxFrameLen are refused at decode before any allocation
// (E_FRAME_TOO_LARGE) so a corrupt length prefix cannot become an
// allocation bomb.
const (
	// MaxFrameLen caps length (type + headerLen + header + payload).
	// 1 MiB is ~32× the default output batch — generous for control JSON
	// and any realistic OUTPUT batch, tiny enough to bound per-conn memory.
	MaxFrameLen = 1 << 20
	// MaxHeaderLen caps the JSON header portion.
	MaxHeaderLen = 1<<16 - 1
	// frameOverhead is type(1) + headerLen(2) — bytes of `length` that are
	// not header or payload.
	frameOverhead = 3
)

// itoa is a minimal unsigned int formatter so String() has no fmt dependency
// in the hot path.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
