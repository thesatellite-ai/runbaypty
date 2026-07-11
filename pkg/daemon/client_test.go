package daemon

// client_test.go — the test-side protocol client: a demuxing reader plus
// typed request helpers. Deliberately test-local; the shipped SDK
// (pkg/client, M7) will grow from this shape.

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

const testTimeout = 10 * time.Second

// testClient speaks the wire protocol over one UDS connection.
type testClient struct {
	t        *testing.T
	nc       net.Conn
	enc      *proto.Encoder
	frames   chan proto.Frame // every frame the reader decodes, in order
	closed   chan struct{}
	reqSeq   int
	clientID string
	mu       sync.Mutex
}

// dialT connects, handshakes, and returns a ready client.
func dialT(t *testing.T, socketPath string) *testClient {
	t.Helper()
	c := dialRaw(t, socketPath)
	ack := c.hello(proto.ProtocolVersion)
	var m proto.HelloAck
	if err := ack.DecodeHeader(&m); err != nil {
		t.Fatalf("hello ack: %v", err)
	}
	c.clientID = m.ClientID
	return c
}

// dialRaw connects WITHOUT the handshake (for handshake-gate tests).
func dialRaw(t *testing.T, socketPath string) *testClient {
	t.Helper()
	nc, err := net.DialTimeout("unix", socketPath, testTimeout)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	c := &testClient{
		t:      t,
		nc:     nc,
		enc:    proto.NewEncoder(nc),
		frames: make(chan proto.Frame, 4096),
		closed: make(chan struct{}),
	}
	go func() {
		defer close(c.closed)
		defer close(c.frames)
		dec := proto.NewDecoder(nc)
		for {
			f, err := dec.Read()
			if err != nil {
				return
			}
			c.frames <- f
		}
	}()
	t.Cleanup(c.close)
	return c
}

func (c *testClient) close() { _ = c.nc.Close(); <-c.closed }

func (c *testClient) nextReqID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqSeq++
	return "req-" + strconv.Itoa(c.reqSeq)
}

// send writes one frame, failing the test on error.
func (c *testClient) send(t proto.FrameType, msg any, payload []byte) {
	c.t.Helper()
	if err := c.enc.WriteMsg(t, msg, payload); err != nil {
		c.t.Fatalf("send %s: %v", t, err)
	}
}

// next returns the next frame of any type.
func (c *testClient) next() (proto.Frame, bool) {
	select {
	case f, ok := <-c.frames:
		return f, ok
	case <-time.After(testTimeout):
		c.t.Fatalf("no frame within %v", testTimeout)
		return proto.Frame{}, false
	}
}

// expect returns the next frame, requiring the given type.
func (c *testClient) expect(want proto.FrameType) proto.Frame {
	c.t.Helper()
	f, ok := c.next()
	if !ok {
		c.t.Fatalf("connection closed while expecting %s", want)
	}
	if f.Type != want {
		c.t.Fatalf("got %s frame, want %s (header %s)", f.Type, want, f.Header)
	}
	return f
}

// expectSkipping returns the next frame of type want, skipping frames whose
// type is in skip (e.g. OUTPUT noise while waiting for OK).
func (c *testClient) expectSkipping(want proto.FrameType, skip ...proto.FrameType) proto.Frame {
	c.t.Helper()
	skippable := make(map[proto.FrameType]bool, len(skip))
	for _, s := range skip {
		skippable[s] = true
	}
	deadline := time.After(testTimeout)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				c.t.Fatalf("connection closed while expecting %s", want)
			}
			if f.Type == want {
				return f
			}
			if !skippable[f.Type] {
				c.t.Fatalf("got %s frame, want %s (header %s)", f.Type, want, f.Header)
			}
		case <-deadline:
			c.t.Fatalf("no %s frame within %v", want, testTimeout)
		}
	}
}

func (c *testClient) hello(protocolVersion int) proto.Frame {
	c.t.Helper()
	c.send(proto.TypeHello, proto.Hello{ReqID: c.nextReqID(), ProtocolVersion: protocolVersion, ClientName: "test"}, nil)
	f, ok := c.next()
	if !ok {
		c.t.Fatal("connection closed during handshake")
	}
	return f
}

func (c *testClient) spawn(cfg proto.Spawn) proto.SpawnOK {
	c.t.Helper()
	cfg.ReqID = c.nextReqID()
	if cfg.Cols == 0 {
		cfg.Cols, cfg.Rows = 80, 24
	}
	c.send(proto.TypeSpawn, cfg, nil)
	f := c.expect(proto.TypeSpawnOK)
	var ok proto.SpawnOK
	if err := f.DecodeHeader(&ok); err != nil {
		c.t.Fatal(err)
	}
	return ok
}

func (c *testClient) attach(sessionID string, sinceSeq *uint64, readOnly bool) proto.AttachOK {
	c.t.Helper()
	c.send(proto.TypeAttach, proto.Attach{ReqID: c.nextReqID(), SessionID: sessionID, SinceSeq: sinceSeq, ReadOnly: readOnly}, nil)
	f := c.expect(proto.TypeAttachOK)
	var ok proto.AttachOK
	if err := f.DecodeHeader(&ok); err != nil {
		c.t.Fatal(err)
	}
	return ok
}

func (c *testClient) input(sessionID string, data []byte) {
	c.t.Helper()
	c.send(proto.TypeInput, proto.InputHeader{ReqID: c.nextReqID(), SessionID: sessionID}, data)
}

// expectErrCode waits for an ERROR frame (skipping data-plane noise) and
// asserts its code.
func (c *testClient) expectErrCode(code string) proto.ErrorMsg {
	c.t.Helper()
	f := c.expectSkipping(proto.TypeError, proto.TypeOutput, proto.TypeEvent, proto.TypeExit)
	var em proto.ErrorMsg
	if err := f.DecodeHeader(&em); err != nil {
		c.t.Fatal(err)
	}
	if string(em.Code) != code {
		c.t.Fatalf("error code = %s (%s), want %s", em.Code, em.Message, code)
	}
	return em
}

// collectOutput drains OUTPUT frames for sessionID until the accumulated
// bytes contain want (or times out). Returns all bytes and the next seq.
func (c *testClient) collectOutput(sessionID, want string) (data []byte, nextSeq uint64) {
	c.t.Helper()
	deadline := time.After(testTimeout)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				c.t.Fatalf("connection closed while collecting output (have %q)", data)
			}
			switch f.Type {
			case proto.TypeOutput:
				var h proto.OutputHeader
				if err := f.DecodeHeader(&h); err != nil {
					c.t.Fatal(err)
				}
				if h.SessionID != sessionID {
					continue
				}
				if nextSeq != 0 && h.Seq != nextSeq {
					c.t.Fatalf("OUTPUT seq gap: got %d, want %d", h.Seq, nextSeq)
				}
				data = append(data, f.Payload...)
				nextSeq = h.Seq + uint64(len(f.Payload))
				if want != "" && containsStr(data, want) {
					return data, nextSeq
				}
			case proto.TypeEvent, proto.TypeExit, proto.TypeOK:
				// fine to see; keep collecting
			default:
				c.t.Fatalf("unexpected %s while collecting output: %s", f.Type, f.Header)
			}
		case <-deadline:
			c.t.Fatalf("timeout collecting output; have %d bytes: %q", len(data), truncateForLog(data))
		}
	}
}

func containsStr(b []byte, s string) bool {
	return len(s) == 0 || (len(b) >= len(s) && stringContains(string(b), s))
}

func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func truncateForLog(b []byte) string {
	if len(b) > 200 {
		return string(b[len(b)-200:])
	}
	return string(b)
}

// startServer runs a daemon on a per-test socket and returns the socket path.
func startServer(t *testing.T, opts Options) (string, *Server) {
	t.Helper()
	dir := t.TempDir()
	opts.HomeDir = dir
	// Socket paths have a ~104-byte limit on macOS; TempDir can be long.
	// Use a short /tmp-side dir for the socket itself.
	sockDir, err := shortTempDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeAllQuiet(sockDir) })
	opts.SocketPath = sockDir + "/d.sock"
	if opts.Version == "" {
		opts.Version = "test"
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("Serve returned: %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("daemon did not shut down within timeout")
		}
	})
	select {
	case <-srv.Ready():
	case err := <-serveDone:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("daemon never became ready")
	}
	return opts.SocketPath, srv
}

func shortTempDir() (string, error) {
	return mkdirTempShort()
}

// spawnShell is the standard interactive-ish session for tests.
func spawnShell(c *testClient, script string) proto.SpawnOK {
	return c.spawn(proto.Spawn{Cmd: "/bin/sh", Args: []string{"-c", script}})
}
