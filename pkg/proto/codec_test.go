package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// sampleFrames covers every implemented frame type with a realistic header
// (and payload where the type carries one).
func sampleFrames(t *testing.T) []Frame {
	t.Helper()
	sinceSeq := uint64(42)
	exitCode := 1
	linger := false
	msgs := []struct {
		typ     FrameType
		msg     any
		payload []byte
	}{
		{TypeHello, Hello{ReqID: "r1", ProtocolVersion: ProtocolVersion, Token: "tokvalue", ClientName: "test"}, nil},
		{TypeHelloAck, HelloAck{ReqID: "r1", ProtocolVersion: ProtocolVersion, DaemonVersion: "0.1.0", ClientID: "cli_018f3b2c9a8e7891b4f51234567890ab"}, nil},
		{TypeError, ErrorMsg{ReqID: "r2", Code: errcodes.SessionNotFound, Message: "nope"}, nil},
		{TypeSpawn, Spawn{ReqID: "r3", Cmd: "bash", Args: []string{"-l"}, Cwd: "/tmp", Env: []string{"A=b"}, Cols: 80, Rows: 24, Name: "dev", Meta: map[string]string{"k": "v"}, RingBytes: 1024, LogPath: "/tmp/x.log", Linger: &linger}, nil},
		{TypeSpawnOK, SpawnOK{ReqID: "r3", SessionID: "ses_018f3b2c9a8e7891b4f51234567890ab", Pid: 4242}, nil},
		{TypeInput, InputHeader{ReqID: "r4", SessionID: "ses_x"}, []byte("ls -la\r")},
		{TypeInputEOF, InputEOF{ReqID: "r5", SessionID: "ses_x"}, nil},
		{TypeOutput, OutputHeader{SessionID: "ses_x", Seq: 1234567}, []byte("total 42\r\n\x1b[32mok\x1b[0m")},
		{TypeResize, Resize{ReqID: "r6", SessionID: "ses_x", Cols: 120, Rows: 40}, nil},
		{TypeKill, Kill{ReqID: "r7", SessionID: "ses_x", Signal: SignalTERM}, nil},
		{TypeList, List{ReqID: "r8"}, nil},
		{TypeListOK, ListOK{ReqID: "r8", Sessions: []SessionInfo{{ID: "ses_x", State: StateRunning, Pid: 1, Cmd: "bash", Cols: 80, Rows: 24, StartedAtMs: 1700000000000, LastSeq: 10}}}, nil},
		{TypeInfo, Info{ReqID: "r9", SessionID: "dev"}, nil},
		{TypeInfoOK, InfoOK{ReqID: "r9", Session: SessionInfo{ID: "ses_x", State: StateExited, ExitCode: &exitCode}}, nil},
		{TypeAttach, Attach{ReqID: "ra", SessionID: "ses_x", SinceSeq: &sinceSeq, ReadOnly: true}, nil},
		{TypeAttachOK, AttachOK{ReqID: "ra", SessionID: "ses_x", LastSeq: 99, Truncated: true}, nil},
		{TypeDetach, Detach{ReqID: "rb", SessionID: "ses_x"}, nil},
		{TypeRename, Rename{ReqID: "rc", SessionID: "ses_x", Name: "newname"}, nil},
		{TypeSetMeta, SetMeta{ReqID: "rd", SessionID: "ses_x", Meta: map[string]string{"app": "runbay"}}, nil},
		{TypeTakeWrite, TakeWrite{ReqID: "re", SessionID: "ses_x"}, nil},
		{TypeReleaseWrite, ReleaseWrite{ReqID: "rf", SessionID: "ses_x"}, nil},
		{TypeSubEvents, SubscribeEvents{ReqID: "rg", SessionID: ""}, nil},
		{TypeOK, OK{ReqID: "rg"}, nil},
		{TypeEvent, Event{Type: EventSilence, SessionID: "ses_x", AtMs: 1700000000000, Data: map[string]string{"quiet_ms": "5000"}}, nil},
		{TypeExit, Exit{SessionID: "ses_x", ExitCode: -1, Signal: SignalKILL}, nil},
	}
	frames := make([]Frame, 0, len(msgs))
	for _, m := range msgs {
		f, err := NewFrame(m.typ, m.msg, m.payload)
		if err != nil {
			t.Fatalf("NewFrame(%s): %v", m.typ, err)
		}
		frames = append(frames, f)
	}
	return frames
}

func TestCodec_RoundTripEveryType(t *testing.T) {
	t.Parallel()
	frames := sampleFrames(t)
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, f := range frames {
		if err := enc.Write(f); err != nil {
			t.Fatalf("encode %s: %v", f.Type, err)
		}
	}
	dec := NewDecoder(&buf)
	for i, want := range frames {
		got, err := dec.Read()
		if err != nil {
			t.Fatalf("decode frame %d (%s): %v", i, want.Type, err)
		}
		if got.Type != want.Type {
			t.Errorf("frame %d type = %s, want %s", i, got.Type, want.Type)
		}
		if !bytes.Equal(got.Header, want.Header) {
			t.Errorf("frame %d header mismatch:\n got %s\nwant %s", i, got.Header, want.Header)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("frame %d payload mismatch: got %d bytes, want %d", i, len(got.Payload), len(want.Payload))
		}
	}
	if _, err := dec.Read(); err != io.EOF {
		t.Errorf("expected clean io.EOF after last frame, got %v", err)
	}
}

// oneByteReader fragments every read to a single byte — simulates the worst
// TCP segmentation so io.ReadFull usage is proven, not hoped.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

func TestDecoder_PartialReads(t *testing.T) {
	t.Parallel()
	frames := sampleFrames(t)
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, f := range frames {
		if err := enc.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	dec := NewDecoder(oneByteReader{&buf})
	for i, want := range frames {
		got, err := dec.Read()
		if err != nil {
			t.Fatalf("decode frame %d over 1-byte reads: %v", i, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Header, want.Header) || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d corrupted over 1-byte reads", i)
		}
	}
}

func TestDecoder_OversizedFrameRefusedBeforeAllocation(t *testing.T) {
	t.Parallel()
	// A length prefix claiming 512 MiB. The decoder must refuse from the
	// 4-byte prefix alone — the body bytes are never provided, so any
	// attempt to allocate/read them would error differently or hang.
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], 512<<20)
	dec := NewDecoder(bytes.NewReader(head[:]))
	_, err := dec.Read()
	if !errcodes.IsCode(err, errcodes.FrameTooLarge) {
		t.Fatalf("expected E_FRAME_TOO_LARGE, got %v", err)
	}
}

func TestDecoder_TruncatedMidFrame(t *testing.T) {
	t.Parallel()
	f, err := NewFrame(TypeList, List{ReqID: "r"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := f.AppendWire(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the frame in half — peer died mid-frame.
	dec := NewDecoder(bytes.NewReader(wire[:len(wire)/2]))
	if _, err := dec.Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestDecoder_HeaderLenExceedsBody(t *testing.T) {
	t.Parallel()
	// total=frameOverhead (no header bytes) but headerLen claims 10.
	var buf bytes.Buffer
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], frameOverhead)
	buf.Write(head[:])
	buf.WriteByte(byte(TypeList))
	var hl [2]byte
	binary.BigEndian.PutUint16(hl[:], 10)
	buf.Write(hl[:])
	dec := NewDecoder(&buf)
	if _, err := dec.Read(); !errcodes.IsCode(err, errcodes.BadFrame) {
		t.Fatalf("expected E_BAD_FRAME, got %v", err)
	}
}

func TestDecoder_UnknownTypeDecodesFine(t *testing.T) {
	t.Parallel()
	// A future frame type must decode (dispatcher skips it) — never a kill.
	unknown := Frame{Type: FrameType(250), Header: []byte(`{"future":"field"}`)}
	wire, err := unknown.AppendWire(nil)
	if err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(bytes.NewReader(wire))
	got, err := dec.Read()
	if err != nil {
		t.Fatalf("unknown type should decode, got %v", err)
	}
	if got.Type.IsKnown() {
		t.Errorf("type 250 reported as known")
	}
	if got.Type.String() != "UNKNOWN(250)" {
		t.Errorf("String() = %q, want UNKNOWN(250)", got.Type.String())
	}
}

func TestNewFrame_OversizedHeaderRefused(t *testing.T) {
	t.Parallel()
	big := SetMeta{ReqID: "r", SessionID: "ses_x", Meta: map[string]string{"blob": string(bytes.Repeat([]byte("a"), MaxHeaderLen))}}
	if _, err := NewFrame(TypeSetMeta, big, nil); !errcodes.IsCode(err, errcodes.FrameTooLarge) {
		t.Fatalf("expected E_FRAME_TOO_LARGE, got %v", err)
	}
}

// TestEncoder_ConcurrentWritersNeverInterleave hammers one Encoder from many
// goroutines and byte-audits the stream: every frame must decode and match
// one of the writers' payload patterns exactly.
func TestEncoder_ConcurrentWritersNeverInterleave(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// bytes.Buffer is not goroutine-safe; the Encoder's mutex is the only
	// thing serializing access — which is exactly what we're testing.
	enc := NewEncoder(&buf)

	const writers, perWriter = 8, 200
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pattern := bytes.Repeat([]byte{byte('A' + w)}, 100+w)
			for range perWriter {
				if err := enc.WriteMsg(TypeOutput, OutputHeader{SessionID: "ses_x", Seq: uint64(w)}, pattern); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	dec := NewDecoder(&buf)
	count := 0
	for {
		f, err := dec.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode after %d frames: %v", count, err)
		}
		var h OutputHeader
		if err := f.DecodeHeader(&h); err != nil {
			t.Fatalf("frame %d header: %v", count, err)
		}
		w := int(h.Seq)
		want := bytes.Repeat([]byte{byte('A' + w)}, 100+w)
		if !bytes.Equal(f.Payload, want) {
			t.Fatalf("frame %d (writer %d): payload interleaved/corrupted", count, w)
		}
		count++
	}
	if count != writers*perWriter {
		t.Errorf("decoded %d frames, want %d", count, writers*perWriter)
	}
}

func FuzzDecoder_NeverPanics(f *testing.F) {
	// Seed with every real frame plus adversarial shapes.
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	linger := true
	seedMsgs := []struct {
		typ FrameType
		msg any
		pl  []byte
	}{
		{TypeHello, Hello{ReqID: "r", ProtocolVersion: 1}, nil},
		{TypeOutput, OutputHeader{SessionID: "s", Seq: 9}, []byte("data")},
		{TypeSpawn, Spawn{ReqID: "r", Cmd: "sh", Linger: &linger}, nil},
	}
	for _, s := range seedMsgs {
		buf.Reset()
		if err := enc.WriteMsg(s.typ, s.msg, s.pl); err == nil {
			f.Add(bytes.Clone(buf.Bytes()))
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255, 1})
	f.Add([]byte{0, 0, 0, 3, 12, 255, 255})

	f.Fuzz(func(t *testing.T, data []byte) {
		dec := NewDecoder(bytes.NewReader(data))
		for {
			frame, err := dec.Read()
			if err != nil {
				return // any error is fine; panics are the failure
			}
			// Exercise the header path too.
			var m map[string]any
			_ = frame.DecodeHeader(&m)
		}
	})
}

func TestFrame_DecodeHeaderRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	f := Frame{Type: TypeList, Header: []byte(`{not json`)}
	var m List
	err := f.DecodeHeader(&m)
	if !errcodes.IsCode(err, errcodes.BadFrame) {
		t.Fatalf("DecodeHeader(bad json) = %v, want E_BAD_FRAME", err)
	}
	// Unknown JSON fields are IGNORED — additive forward-compat with a newer peer.
	f2 := Frame{Type: TypeList, Header: []byte(`{"req_id":"r","future_field":42}`)}
	if err := f2.DecodeHeader(&m); err != nil {
		t.Fatalf("unknown field broke decode: %v", err)
	}
	if m.ReqID != "r" {
		t.Errorf("ReqID = %q", m.ReqID)
	}
}

func TestEncoder_WriteMsgSurfacesOversizeHeader(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	big := SetMeta{ReqID: "r", SessionID: "s", Meta: map[string]string{"k": string(bytes.Repeat([]byte("x"), MaxHeaderLen))}}
	if err := enc.WriteMsg(TypeSetMeta, big, nil); !errcodes.IsCode(err, errcodes.FrameTooLarge) {
		t.Errorf("WriteMsg(oversize) = %v, want E_FRAME_TOO_LARGE", err)
	}
	if buf.Len() != 0 {
		t.Error("a refused frame still wrote bytes")
	}
}

// failWriter fails every Write — the transport-death path.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEncoder_SurfacesWriteErrors(t *testing.T) {
	t.Parallel()
	enc := NewEncoder(failWriter{})
	if err := enc.WriteMsg(TypeList, List{ReqID: "r"}, nil); err == nil {
		t.Error("a dead transport must surface a write error")
	}
}

func TestFrame_AppendWireRefusesOversizePayload(t *testing.T) {
	t.Parallel()
	f := Frame{Type: TypeOutput, Header: []byte(`{}`), Payload: make([]byte, MaxFrameLen)}
	if _, err := f.AppendWire(nil); !errcodes.IsCode(err, errcodes.FrameTooLarge) {
		t.Errorf("AppendWire(oversize payload) = %v", err)
	}
}
