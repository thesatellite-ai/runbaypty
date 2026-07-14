// Package errcodes is the canonical registry of stable error codes for
// runbaypty. Every error the daemon sends over the wire (ERROR frame) and
// every CLI error path carries a code from this registry. Codes are the
// stable contract: additive across minor versions; removal/rename is a
// breaking change requiring a major protocol bump.
//
// Usage:
//
//	return errcodes.Newf(errcodes.SessionNotFound, "session %s not found", id).
//	    WithHint("run `runbaypty ls` to see live sessions")
//
// Output formats:
//
//	plain → "[E_SESSION_NOT_FOUND] session ses_… not found
//	         Hint: run `runbaypty ls` to see live sessions"
//	json  → {"error":{"code":"E_SESSION_NOT_FOUND","message":"…","hint":"…"}}
//	wire  → ERROR frame {reqId, code, message}
package errcodes

// Code is a stable identifier for an error class. Format: E_<UPPER_SNAKE>.
// Codes are added freely (additive minor); removal = breaking major.
type Code string

// Registered error codes. Add new codes at the end of their group; never
// reorder; never remove (deprecate via doc + retain).
const (
	// ── Protocol / handshake ────────────────────────────────────────
	ProtocolMismatch Code = "E_PROTOCOL_MISMATCH"
	BadFrame         Code = "E_BAD_FRAME"
	FrameTooLarge    Code = "E_FRAME_TOO_LARGE"
	HandshakeFirst   Code = "E_HANDSHAKE_FIRST"
	BadToken         Code = "E_BAD_TOKEN"
	ReadOnlyScope    Code = "E_READ_ONLY_SCOPE"

	// ── Session lifecycle ───────────────────────────────────────────
	SessionNotFound Code = "E_SESSION_NOT_FOUND"
	SessionExited   Code = "E_SESSION_EXITED"
	NameTaken       Code = "E_NAME_TAKEN"
	SpawnFailed     Code = "E_SPAWN_FAILED"
	InvalidName     Code = "E_INVALID_NAME"

	// ── Data plane ──────────────────────────────────────────────────
	RingGone       Code = "E_RING_GONE"
	NotAttached    Code = "E_NOT_ATTACHED"
	NoWriteLock    Code = "E_NO_WRITE_LOCK"
	ReadOnlyAttach Code = "E_READ_ONLY_ATTACH"
	LogDisabled    Code = "E_LOG_DISABLED"

	// ── Limits / guardrails ─────────────────────────────────────────
	LimitExceeded  Code = "E_LIMIT_EXCEEDED"
	SubscriberSlow Code = "E_SUBSCRIBER_SLOW"

	// ── Daemon / environment ────────────────────────────────────────
	DaemonUnreachable Code = "E_DAEMON_UNREACHABLE"
	DaemonStopping    Code = "E_DAEMON_STOPPING"
	LockHeld          Code = "E_LOCK_HELD"
	VersionMismatch   Code = "E_VERSION_MISMATCH"

	// ── Input / validation ──────────────────────────────────────────
	InvalidID    Code = "E_INVALID_ID"
	InvalidInput Code = "E_INVALID_INPUT"

	// ── Session meta (JSON document) ────────────────────────────────
	MetaTooLarge    Code = "E_META_TOO_LARGE"    // merged meta doc exceeds the size cap
	MetaConflict    Code = "E_META_CONFLICT"     // if-version CAS check failed (concurrent write)
	ReservedMetaKey Code = "E_RESERVED_META_KEY" // client wrote a daemon-reserved rpty.* key

	// ── Generic ─────────────────────────────────────────────────────
	NotFound       Code = "E_NOT_FOUND"
	Internal       Code = "E_INTERNAL"
	NotImplemented Code = "E_NOT_IMPLEMENTED"
	Unsupported    Code = "E_UNSUPPORTED"
)

// All returns every registered code. Used by `runbaypty errors list`, the
// PROTOCOL.md doc-drift test, and the coverage test.
func All() []Code {
	return []Code{
		ProtocolMismatch, BadFrame, FrameTooLarge, HandshakeFirst, BadToken, ReadOnlyScope,
		SessionNotFound, SessionExited, NameTaken, SpawnFailed, InvalidName,
		RingGone, NotAttached, NoWriteLock, ReadOnlyAttach, LogDisabled,
		LimitExceeded, SubscriberSlow,
		DaemonUnreachable, DaemonStopping, LockHeld, VersionMismatch,
		InvalidID, InvalidInput,
		MetaTooLarge, MetaConflict, ReservedMetaKey,
		NotFound, Internal, NotImplemented, Unsupported,
	}
}

// Description returns a one-line human description for documentation and
// `runbaypty errors list` output.
func Description(c Code) string {
	descs := map[Code]string{
		ProtocolMismatch: "client and daemon protocol major versions differ; upgrade one side",
		BadFrame:         "frame failed to decode (malformed header or truncated payload)",
		FrameTooLarge:    "frame exceeds the maximum allowed size",
		HandshakeFirst:   "a control message arrived before HELLO completed",
		BadToken:         "WS auth token missing or invalid",
		ReadOnlyScope:    "token scope is read-only; control operations are refused",

		SessionNotFound: "no live or retained session with that id or name",
		SessionExited:   "session process has exited; only replay and INFO are available",
		NameTaken:       "another session already uses that name",
		SpawnFailed:     "PTY spawn failed (command not found, permission, or resource error)",
		InvalidName:     "session name contains disallowed characters or is too long",

		RingGone:       "requested sinceSeq predates the ring buffer; replay is truncated",
		NotAttached:    "operation requires an active ATTACH on this session",
		NoWriteLock:    "INPUT requires the write lock; TAKE-WRITE first",
		ReadOnlyAttach: "this attach is read-only and can never take the write lock",
		LogDisabled:    "session was spawned without a durable log",

		LimitExceeded:  "daemon limit reached (max sessions or ring memory cap)",
		SubscriberSlow: "subscriber stalled beyond the deadline and was disconnected",

		DaemonUnreachable: "cannot connect to the daemon socket; is runbaypty daemon running?",
		DaemonStopping:    "daemon is shutting down and refuses new work",
		LockHeld:          "another runbaypty daemon already owns this socket path",
		VersionMismatch:   "client and daemon binary versions differ; restart the daemon to update",

		InvalidID:    "id failed format validation (expected <prefix>_<32-hex-uuidv7>)",
		InvalidInput: "input value failed validation",

		MetaTooLarge:    "merged meta document exceeds the maximum allowed size",
		MetaConflict:    "meta if-version check failed; another writer updated it first",
		ReservedMetaKey: "meta key uses the daemon-reserved rpty. namespace",

		NotFound:       "requested entity does not exist",
		Internal:       "internal error; please file a bug report",
		NotImplemented: "feature not yet implemented",
		Unsupported:    "operation not supported by this transport or platform",
	}
	if d, ok := descs[c]; ok {
		return d
	}
	return string(c)
}
