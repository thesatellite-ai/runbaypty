// messages.go — the JSON header struct for every frame type.
//
// Contract rules:
//   - Every field carries an explicit json tag; additive-only evolution.
//   - ReqID correlates a control request with its OK/typed-response/ERROR.
//     Data-plane frames (OUTPUT, EVENT, EXIT) have no ReqID — they are pushes.
//   - INPUT carries ReqID but gets NO per-frame ack (too chatty for
//     keystrokes); the daemon replies only on failure (ERROR with that ReqID).
package proto

import (
	"encoding/json"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// ProtocolVersion is the wire-protocol major version this package speaks.
// HELLO negotiation: major must match; unknown minor additions are ignored.
const ProtocolVersion = 1

// SessionState is the closed lifecycle enum for a session.
type SessionState string

const (
	// StateStarting — SPAWN accepted, PTY not yet confirmed running. RESERVED:
	// today the daemon spawns synchronously and a session is born StateRunning,
	// so this value is never emitted on the wire. It stays in the enum
	// (additive-only contract) for a future async-spawn path.
	StateStarting SessionState = "starting"
	// StateRunning — PTY live, reader pumping.
	StateRunning SessionState = "running"
	// StateExited — process ended; record + ring retained until reap TTL.
	StateExited SessionState = "exited"
)

// SessionStateValues is the canonical iteration order.
var SessionStateValues = []SessionState{StateStarting, StateRunning, StateExited}

// EventType is the closed enum of lifecycle/activity events pushed on the
// EVENT frame. Additive-only.
type EventType string

const (
	EventCreated        EventType = "created"
	EventExited         EventType = "exited"
	EventResized        EventType = "resized"
	EventAttached       EventType = "attached"
	EventDetached       EventType = "detached"
	EventRenamed        EventType = "renamed"
	EventMetaChanged    EventType = "meta-changed"
	EventActivity       EventType = "activity"
	EventSilence        EventType = "silence"
	EventBell           EventType = "bell"
	EventDaemonStopping EventType = "daemon-stopping"
	// EventCommandStarted / EventCommandFinished are the OSC 133 shell-
	// integration marks (v1.5 semantic layer): data carries start_seq /
	// end_seq (ring axis) and exit_code on finished. The reserved frame
	// types 40/41 remain reserved; these ride the EVENT stream.
	EventCommandStarted  EventType = "command-started"
	EventCommandFinished EventType = "command-finished"
)

// Event data keys — the closed set of keys that appear in Event.Data /
// Exit-adjacent payloads. Wire contract like frame types: additive-only.
// PROTOCOL.md documents which keys each event type carries.
const (
	DataKeyExitCode = "exit_code"
	DataKeySignal   = "signal"
	DataKeyQuietMs  = "quiet_ms"
	DataKeyStartSeq = "start_seq"
	DataKeyEndSeq   = "end_seq"
	DataKeyClient   = "client"
	DataKeyName     = "name"
	DataKeyCmd      = "cmd"
	DataKeyCols     = "cols"
	DataKeyRows     = "rows"
	// DataKeyLogBroken flags a disabled durable log on meta-changed events.
	DataKeyLogBroken = "log_broken"
	// DataKeyMetaKeys / DataKeyMetaVersion ride meta-changed events: the
	// comma-joined top-level keys the write touched, and the new version. Let a
	// subscriber react (blackboard pattern) without a follow-up INFO round-trip.
	DataKeyMetaKeys    = "meta_keys"
	DataKeyMetaVersion = "meta_version"
)

// DataKeyValues is the canonical iteration order (doc-drift + tests).
var DataKeyValues = []string{
	DataKeyExitCode, DataKeySignal, DataKeyQuietMs, DataKeyStartSeq,
	DataKeyEndSeq, DataKeyClient, DataKeyName, DataKeyCmd, DataKeyCols,
	DataKeyRows, DataKeyLogBroken, DataKeyMetaKeys, DataKeyMetaVersion,
}

// EventTypeValues is the canonical iteration order.
var EventTypeValues = []EventType{
	EventCreated, EventExited, EventResized, EventAttached, EventDetached,
	EventRenamed, EventMetaChanged, EventActivity, EventSilence, EventBell,
	EventDaemonStopping, EventCommandStarted, EventCommandFinished,
}

// Signal names accepted by KILL. Closed set — names, not numbers, so the
// wire stays platform-independent; the daemon maps to the platform signal.
const (
	SignalTERM = "TERM"
	SignalKILL = "KILL"
	SignalINT  = "INT"
	SignalHUP  = "HUP"
)

// SignalValues is the canonical iteration order.
var SignalValues = []string{SignalTERM, SignalKILL, SignalINT, SignalHUP}

// ── Handshake ───────────────────────────────────────────────────────

// Hello opens every connection. Token is required on WS, ignored on UDS
// (file permissions are the auth there).
type Hello struct {
	ReqID           string `json:"req_id"`
	ProtocolVersion int    `json:"protocol_version"`
	Token           string `json:"token,omitempty"`
	ClientName      string `json:"client_name,omitempty"` // free-form, for logs/INFO attribution
}

// HelloAck accepts the handshake.
type HelloAck struct {
	ReqID           string `json:"req_id"`
	ProtocolVersion int    `json:"protocol_version"`
	DaemonVersion   string `json:"daemon_version"`
	ClientID        string `json:"client_id"` // daemon-minted cli_… id for this connection
}

// ErrorMsg is the wire error envelope. ReqID is empty for push-side errors
// (e.g. INPUT refused, subscriber disconnect notice).
type ErrorMsg struct {
	ReqID   string        `json:"req_id,omitempty"`
	Code    errcodes.Code `json:"code"`
	Message string        `json:"message"`
}

// ── Session control ─────────────────────────────────────────────────

// Spawn starts a PTY session.
type Spawn struct {
	ReqID string   `json:"req_id"`
	Cmd   string   `json:"cmd"`
	Args  []string `json:"args,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	Env   []string `json:"env,omitempty"` // KEY=VALUE pairs appended to the daemon's base env
	Cols  uint16   `json:"cols"`
	Rows  uint16   `json:"rows"`
	// Name is the optional unique human name (tmux-style attach target).
	Name string `json:"name,omitempty"`
	// Meta is the legacy flat client-owned KV. It still works: the daemon folds
	// each pair into the JSON meta document as a top-level string field. Prefer
	// Annotations for anything structured. See docsi/META_SPEC.md.
	Meta map[string]string `json:"meta,omitempty"`
	// Annotations seeds the session's JSON meta document at spawn (arbitrary
	// JSON object). "annotations" is the wire name for what the CLI/SDK call
	// `meta`. Merge/replace after spawn via SET_META_JSON.
	Annotations json.RawMessage `json:"annotations,omitempty"`
	// RingBytes overrides the per-session ring cap (0 = daemon default).
	RingBytes int `json:"ring_bytes,omitempty"`
	// LogPath enables the durable (ts,bytes) log at the given path ("" = off).
	LogPath string `json:"log_path,omitempty"`
	// Linger: nil/true = session outlives all clients (the default and the
	// whole point); false = kill when the last subscriber detaches.
	Linger *bool `json:"linger,omitempty"`
}

// SpawnOK confirms the spawn.
type SpawnOK struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	Pid       int    `json:"pid"`
}

// InputHeader rides TypeInput; the keystroke bytes are the frame payload.
// No per-frame ack — ERROR (echoing ReqID) only on refusal.
type InputHeader struct {
	ReqID     string `json:"req_id,omitempty"`
	SessionID string `json:"session_id"`
}

// InputEOF half-closes the session's stdin.
type InputEOF struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
}

// OutputHeader rides TypeOutput; the PTY bytes are the frame payload.
// Seq is the ring sequence of the FIRST byte in the payload; the next
// frame's Seq is this Seq + len(payload). Zero-gap reattach audits this.
type OutputHeader struct {
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
}

// Resize sets the PTY grid. Last-writer-wins; min-of-viewers is client policy.
type Resize struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

// Kill signals the session's process tree.
type Kill struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	Signal    string `json:"signal,omitempty"` // SignalTERM default
}

// List enumerates sessions.
type List struct {
	ReqID string `json:"req_id"`
}

// ListOK carries the summaries.
type ListOK struct {
	ReqID    string        `json:"req_id"`
	Sessions []SessionInfo `json:"sessions"`
}

// Info requests one session's detail. SessionID accepts an id or a name.
type Info struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
}

// InfoOK carries the detail.
type InfoOK struct {
	ReqID   string      `json:"req_id"`
	Session SessionInfo `json:"session"`
}

// SessionInfo is the shared session snapshot for LIST_OK / INFO_OK.
type SessionInfo struct {
	ID    string       `json:"id"`
	Name  string       `json:"name,omitempty"`
	State SessionState `json:"state"`
	Pid   int          `json:"pid"`
	Cmd   string       `json:"cmd"`
	Args  []string     `json:"args,omitempty"`
	// Cwd is the live working directory of the process when the platform
	// supports the lookup (task-m2-fgproc), else the spawn cwd.
	Cwd  string `json:"cwd,omitempty"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
	// StartedAtMs / ExitedAtMs are unix milliseconds, UTC.
	StartedAtMs int64  `json:"started_at_ms"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	ExitedAtMs  *int64 `json:"exited_at_ms,omitempty"`
	// LastSeq is the ring sequence after the newest byte (next OUTPUT starts here).
	LastSeq  uint64 `json:"last_seq"`
	BytesOut uint64 `json:"bytes_out"`
	BytesIn  uint64 `json:"bytes_in"`
	// Subscribers counts active attaches; WriteLockHolder is the cli_… id
	// holding the write lock ("" = unheld).
	Subscribers     int    `json:"subscribers"`
	WriteLockHolder string `json:"write_lock_holder,omitempty"`
	// Meta is the legacy flat projection: the top-level string fields of the
	// JSON meta document, so pre-upgrade clients still see their tags.
	Meta map[string]string `json:"meta,omitempty"`
	// Annotations is the full JSON meta document (the CLI/SDK `meta`).
	Annotations json.RawMessage `json:"annotations,omitempty"`
	// MetaVersion increments on every meta write; used for optimistic
	// concurrency (SET_META_JSON if_version). 0 means never written.
	MetaVersion uint64 `json:"meta_version,omitempty"`
	// LogPath is the durable log location ("" = logging off). Lets clients
	// stitch full history (log) with the live stream (ring seq axis): read
	// the log's N bytes, then ATTACH {since_seq: N} — same byte axis.
	LogPath string `json:"log_path,omitempty"`
	// FgPid / FgComm are the FOREGROUND process group leader on the PTY
	// (tcgetpgrp) and its command name — "is vim in the way?" for agents.
	// Present only while running and when the platform lookup succeeds.
	FgPid  int    `json:"fg_pid,omitempty"`
	FgComm string `json:"fg_comm,omitempty"`
}

// Attach subscribes to a session's byte stream.
type Attach struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	// SinceSeq: nil = replay the whole ring; N = replay from sequence N
	// (zero-gap resume for a client that saw up to N already).
	SinceSeq *uint64 `json:"since_seq,omitempty"`
	// ReadOnly attaches can never take the write lock (E_READ_ONLY_ATTACH).
	ReadOnly bool `json:"read_only,omitempty"`
}

// AttachOK confirms; ring replay follows immediately as OUTPUT frames.
type AttachOK struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	// LastSeq at attach time — after replay the client is current at this seq.
	LastSeq uint64 `json:"last_seq"`
	// Truncated: SinceSeq predated the ring start; replay begins at the
	// oldest retained byte instead (pair with the durable log for history).
	Truncated bool `json:"truncated,omitempty"`
}

// Detach unsubscribes one session, keeping the connection.
type Detach struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
}

// Rename changes the session's unique human name.
type Rename struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

// SetMeta replaces the legacy flat KV metadata wholesale. Retained for
// back-compat: the daemon converts the map into a wholesale replace of the
// JSON meta document. New clients use SET_META_JSON instead.
type SetMeta struct {
	ReqID     string            `json:"req_id"`
	SessionID string            `json:"session_id"`
	Meta      map[string]string `json:"meta"`
}

// MetaMode is the closed set of write modes for the JSON meta document.
type MetaMode string

const (
	// MetaModeMerge applies an RFC 7386 JSON Merge Patch (the default): only
	// the fields present in the patch change; null deletes a key.
	MetaModeMerge MetaMode = "merge"
	// MetaModeReplace swaps the whole document for the patch.
	MetaModeReplace MetaMode = "replace"
	// MetaModeIncr adds the patch's numeric leaves to the matching fields
	// (atomic counters that a merge cannot express). Missing fields count as 0.
	MetaModeIncr MetaMode = "incr"
)

// MetaModeValues is the canonical iteration order.
var MetaModeValues = []MetaMode{MetaModeMerge, MetaModeReplace, MetaModeIncr}

// SetMetaJSON merges or replaces a session's JSON meta document. The daemon
// applies it atomically under the session lock, so concurrent patches to
// different fields never clobber each other (see docsi/META_SPEC.md).
type SetMetaJSON struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	// Mode defaults to MetaModeMerge when empty.
	Mode MetaMode `json:"mode,omitempty"`
	// IfVersion, when set, makes the write conditional: the daemon rejects it
	// with E_META_CONFLICT unless the current MetaVersion equals *IfVersion.
	IfVersion *uint64 `json:"if_version,omitempty"`
	// Patch is the JSON merge patch (merge mode) or the full document (replace).
	Patch json.RawMessage `json:"patch"`
}

// TakeWrite claims the single write lock (steals from the current holder —
// explicit takeover is the agent↔human handoff).
type TakeWrite struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
}

// ReleaseWrite releases the write lock if held by this client.
type ReleaseWrite struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
}

// SubscribeEvents subscribes this connection to the event stream.
// SessionID filters to one session; empty = all sessions + daemon events.
type SubscribeEvents struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id,omitempty"`
}

// OK is the generic success ack, echoing the request's ReqID.
type OK struct {
	ReqID string `json:"req_id"`
}

// ReplayCommand asks for the LAST completed OSC 133 command's output
// (requires the session's shell to emit 133 marks — see PROTOCOL.md Events).
type ReplayCommand struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
}

// ReplayCommandOK carries the window boundaries; the output bytes are the
// frame payload. Truncated: the ring evicted part of the window (payload
// begins at the oldest retained byte).
type ReplayCommandOK struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	StartSeq  uint64 `json:"start_seq"`
	EndSeq    uint64 `json:"end_seq"`
	Truncated bool   `json:"truncated,omitempty"`
}

// HandoverReq begins a daemon takeover (HANDOVER.md): the NEW daemon asks
// the OLD one to stream every session + fd to the private unix listener at
// ReplyPath. UDS-only; the old daemon replies OK then proceeds.
type HandoverReq struct {
	ReqID     string `json:"req_id"`
	ReplyPath string `json:"reply_path"`
}

// Watch registers a server-side regex watch on a session's FUTURE output
// (from the current seq onward). RE2 syntax — linear time, no catastrophic
// backtracking. Watches are per-connection: they die with it.
type Watch struct {
	ReqID     string `json:"req_id"`
	SessionID string `json:"session_id"`
	Pattern   string `json:"pattern"`
}

// WatchOK confirms registration.
type WatchOK struct {
	ReqID   string `json:"req_id"`
	WatchID string `json:"watch_id"`
}

// WatchEvent is pushed on every match. Seq is the absolute stream position
// of the match start; Match is capped at 256 bytes.
type WatchEvent struct {
	WatchID   string `json:"watch_id"`
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
	Match     string `json:"match"`
}

// ── Server pushes ───────────────────────────────────────────────────

// Event is a lifecycle/activity push.
type Event struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"session_id,omitempty"` // empty for daemon-scoped events
	AtMs      int64     `json:"at_ms"`                // unix milliseconds, UTC
	// Data carries per-type detail: renamed {"name"}, resized {"cols","rows"},
	// silence {"quiet_ms"}, exited {"exit_code"}. Closed per type, documented
	// in PROTOCOL.md; unknown keys are ignored (additive).
	Data map[string]string `json:"data,omitempty"`
}

// Exit is pushed to every subscriber when the session's process ends.
type Exit struct {
	SessionID string `json:"session_id"`
	ExitCode  int    `json:"exit_code"`
	// Signal is set when the process died by signal (ExitCode is then -1).
	Signal string `json:"signal,omitempty"`
}
