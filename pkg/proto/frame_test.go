package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFrameTypes_NoDuplicateValues(t *testing.T) {
	t.Parallel()
	seenName := make(map[string]FrameType, len(knownTypes))
	for typ, name := range knownTypes {
		if prev, dup := seenName[name]; dup {
			t.Errorf("wire name %q assigned to both %d and %d", name, prev, typ)
		}
		seenName[name] = typ
	}
}

func TestFrameTypes_ReservedBlockAssignedButUnimplemented(t *testing.T) {
	t.Parallel()
	// The 40–49 block is reserved for the v1.5/v2 semantic layer. Assigned
	// names must exist (so nothing else claims the values)…
	for _, typ := range []FrameType{TypeCommandStarted, TypeCommandFinished} {
		if !typ.IsKnown() {
			t.Errorf("reserved type %d must be registered", typ)
		}
		if typ < 40 || typ > 49 {
			t.Errorf("reserved type %s = %d outside the 40–49 block", typ, typ)
		}
	}
	// …and unassigned values in the block must stay unknown.
	for v := FrameType(48); v <= 49; v++ {
		if v.IsKnown() {
			t.Errorf("value %d in the reserved block is assigned prematurely", v)
		}
	}
}

// messageSamples pairs every message struct with a fully-populated instance
// so the reflection round-trip catches any field whose json tag is missing,
// duplicated, or lossy.
func messageSamples() []any {
	seq := uint64(7)
	code := 3
	exitedAt := int64(1700000000123)
	linger := false
	return []any{
		&Hello{ReqID: "r", ProtocolVersion: 1, Token: "tokvalue", ClientName: "c"},
		&HelloAck{ReqID: "r", ProtocolVersion: 1, DaemonVersion: "v", ClientID: "cli_x"},
		&ErrorMsg{ReqID: "r", Code: "E_NOT_FOUND", Message: "m"},
		&Spawn{ReqID: "r", Cmd: "c", Args: []string{"a"}, Cwd: "/", Env: []string{"K=V"}, Cols: 1, Rows: 2, Name: "n", Meta: map[string]string{"k": "v"}, RingBytes: 9, LogPath: "/l", Linger: &linger},
		&SpawnOK{ReqID: "r", SessionID: "s", Pid: 1},
		&InputHeader{ReqID: "r", SessionID: "s"},
		&InputEOF{ReqID: "r", SessionID: "s"},
		&OutputHeader{SessionID: "s", Seq: 1},
		&Resize{ReqID: "r", SessionID: "s", Cols: 1, Rows: 2},
		&Kill{ReqID: "r", SessionID: "s", Signal: SignalINT},
		&List{ReqID: "r"},
		&ListOK{ReqID: "r", Sessions: []SessionInfo{{ID: "s", Name: "n", State: StateRunning, Pid: 1, Cmd: "c", Args: []string{"a"}, Cwd: "/", Cols: 1, Rows: 2, StartedAtMs: 5, ExitCode: &code, ExitedAtMs: &exitedAt, LastSeq: 6, BytesOut: 7, BytesIn: 8, Subscribers: 2, WriteLockHolder: "cli_x", Meta: map[string]string{"k": "v"}, LogPath: "/l", FgPid: 9, FgComm: "vim"}}},
		&Info{ReqID: "r", SessionID: "s"},
		&InfoOK{ReqID: "r", Session: SessionInfo{ID: "s", State: StateExited}},
		&Attach{ReqID: "r", SessionID: "s", SinceSeq: &seq, ReadOnly: true},
		&AttachOK{ReqID: "r", SessionID: "s", LastSeq: 1, Truncated: true},
		&Detach{ReqID: "r", SessionID: "s"},
		&Rename{ReqID: "r", SessionID: "s", Name: "n"},
		&SetMeta{ReqID: "r", SessionID: "s", Meta: map[string]string{"k": "v"}},
		&TakeWrite{ReqID: "r", SessionID: "s"},
		&ReleaseWrite{ReqID: "r", SessionID: "s"},
		&SubscribeEvents{ReqID: "r", SessionID: "s"},
		&OK{ReqID: "r"},
		&ReplayCommand{ReqID: "r", SessionID: "s"},
		&ReplayCommandOK{ReqID: "r", SessionID: "s", StartSeq: 1, EndSeq: 9, Truncated: true},
		&HandoverReq{ReqID: "r", ReplyPath: "/p"},
		&Watch{ReqID: "r", SessionID: "s", Pattern: "x+"},
		&WatchOK{ReqID: "r", WatchID: "wch_x"},
		&WatchEvent{WatchID: "wch_x", SessionID: "s", Seq: 4, Match: "xx"},
		&Event{Type: EventBell, SessionID: "s", AtMs: 1, Data: map[string]string{"k": "v"}},
		&Exit{SessionID: "s", ExitCode: -1, Signal: SignalKILL},
	}
}

// TestMessages_JSONRoundTripLossless marshals every populated message and
// unmarshals into a fresh instance — reflect.DeepEqual catches new-field
// drift the moment someone adds a field without a working tag.
func TestMessages_JSONRoundTripLossless(t *testing.T) {
	t.Parallel()
	for _, msg := range messageSamples() {
		name := reflect.TypeOf(msg).Elem().Name()
		data, err := json.Marshal(msg)
		if err != nil {
			t.Errorf("%s: marshal: %v", name, err)
			continue
		}
		fresh := reflect.New(reflect.TypeOf(msg).Elem()).Interface()
		if err := json.Unmarshal(data, fresh); err != nil {
			t.Errorf("%s: unmarshal: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(msg, fresh) {
			t.Errorf("%s: round-trip lost data:\n before %+v\n after  %+v\n wire   %s", name, msg, fresh, data)
		}
	}
}

// TestMessages_EveryExportedFieldTagged walks every message struct and
// asserts explicit snake_case json tags on all exported fields.
func TestMessages_EveryExportedFieldTagged(t *testing.T) {
	t.Parallel()
	for _, msg := range messageSamples() {
		typ := reflect.TypeOf(msg).Elem()
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			tag := field.Tag.Get("json")
			if tag == "" {
				t.Errorf("%s.%s has no json tag", typ.Name(), field.Name)
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name != strings.ToLower(name) {
				t.Errorf("%s.%s json tag %q is not lowercase snake_case", typ.Name(), field.Name, name)
			}
		}
	}
}

func TestEnums_NoDuplicates(t *testing.T) {
	t.Parallel()
	states := make(map[SessionState]bool)
	for _, s := range SessionStateValues {
		if states[s] {
			t.Errorf("duplicate SessionState %q", s)
		}
		states[s] = true
	}
	events := make(map[EventType]bool)
	for _, e := range EventTypeValues {
		if events[e] {
			t.Errorf("duplicate EventType %q", e)
		}
		events[e] = true
	}
	signals := make(map[string]bool)
	for _, s := range SignalValues {
		if signals[s] {
			t.Errorf("duplicate Signal %q", s)
		}
		signals[s] = true
	}
}
