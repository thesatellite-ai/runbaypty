package daemon

// ws_test.go — transport conformance for the WebSocket listener: the same
// protocol flows that pass over UDS must pass over WS, plus the WS-only
// concerns (token auth, scopes, loopback bind).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// startServerWS runs a daemon with the WS listener on an OS-assigned port,
// returning the UDS socket path, the WS port, and the two token values.
func startServerWS(t *testing.T) (sock string, wsPort int, control, readOnly string) {
	t.Helper()
	// Port 0 → the daemon binds an ephemeral loopback port and publishes
	// it in the discovery file… except WSPort>0 is the enable switch, so
	// pick a free port explicitly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	sock, srv := startServer(t, Options{WSPort: port})
	_ = srv

	// Tokens + port land in the discovery file next to the home dir.
	home := filepath.Dir(sock) // NOT the home; discovery lives in HomeDir
	_ = home
	// startServer put HomeDir at a t.TempDir(); find discovery via srv.
	raw, err := os.ReadFile(filepath.Join(srv.opts.HomeDir, constants.DiscoveryFilename))
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	var d Discovery
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.WSPort != port {
		t.Fatalf("discovery ws_port = %d, want %d", d.WSPort, port)
	}
	readTok := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("token file %s: %v", p, err)
		}
		return strings.TrimSpace(string(b))
	}
	return sock, port, readTok(d.TokenPath), readTok(d.TokenROPath)
}

// dialWSRaw opens a WS connection WITHOUT the handshake.
func dialWSRaw(t *testing.T, port int) *testClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	wsc, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d/v1", port), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	nc := websocket.NetConn(context.Background(), wsc, websocket.MessageBinary)
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

// helloWS handshakes with a token and returns the response frame.
func (c *testClient) helloWS(token string) proto.Frame {
	c.t.Helper()
	c.send(proto.TypeHello, proto.Hello{ReqID: c.nextReqID(), ProtocolVersion: proto.ProtocolVersion, Token: token, ClientName: "ws-test"}, nil)
	f, ok := c.next()
	if !ok {
		c.t.Fatal("connection closed during ws handshake")
	}
	return f
}

// dialWST returns a handshaken WS client with the given token.
func dialWST(t *testing.T, port int, token string) *testClient {
	t.Helper()
	c := dialWSRaw(t, port)
	f := c.helloWS(token)
	if f.Type != proto.TypeHelloAck {
		t.Fatalf("ws hello: got %s (%s)", f.Type, f.Header)
	}
	var ack proto.HelloAck
	if err := f.DecodeHeader(&ack); err != nil {
		t.Fatal(err)
	}
	c.clientID = ack.ClientID
	return c
}

func TestWS_BadTokenRefused(t *testing.T) {
	_, port, _, _ := startServerWS(t)
	c := dialWSRaw(t, port)
	f := c.helloWS("wrong-token")
	if f.Type != proto.TypeError {
		t.Fatalf("got %s, want ERROR", f.Type)
	}
	var em proto.ErrorMsg
	if err := f.DecodeHeader(&em); err != nil {
		t.Fatal(err)
	}
	if em.Code != errcodes.BadToken {
		t.Errorf("code = %s, want E_BAD_TOKEN", em.Code)
	}
	// Missing token entirely: same refusal.
	c2 := dialWSRaw(t, port)
	if f := c2.helloWS(""); f.Type != proto.TypeError {
		t.Errorf("empty token accepted: %s", f.Type)
	}
}

func TestWS_ControlTokenFullLifecycle(t *testing.T) {
	_, port, control, _ := startServerWS(t)
	c := dialWST(t, port, control)

	ok := spawnShell(c, "cat")
	c.attach(ok.SessionID, nil, false)
	c.input(ok.SessionID, []byte("over-websocket\r"))
	data, _ := c.collectOutput(ok.SessionID, "over-websocket")
	if len(data) == 0 {
		t.Fatal("no output over WS")
	}
	c.send(proto.TypeKill, proto.Kill{ReqID: c.nextReqID(), SessionID: ok.SessionID, Signal: proto.SignalKILL}, nil)
	c.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)
	c.expectSkipping(proto.TypeExit, proto.TypeOutput, proto.TypeEvent)
}

func TestWS_ReadOnlyScopeEnforced(t *testing.T) {
	sock, port, _, readOnly := startServerWS(t)

	// A UDS client creates the session a viewer will observe.
	owner := dialT(t, sock)
	ok := spawnShell(owner, "echo scoped-view; sleep 300")

	viewer := dialWST(t, port, readOnly)

	// Every control verb refused with E_READ_ONLY_SCOPE.
	viewer.send(proto.TypeSpawn, proto.Spawn{ReqID: viewer.nextReqID(), Cmd: "/bin/sh", Cols: 80, Rows: 24}, nil)
	viewer.expectErrCode(string(errcodes.ReadOnlyScope))
	viewer.send(proto.TypeKill, proto.Kill{ReqID: viewer.nextReqID(), SessionID: ok.SessionID}, nil)
	viewer.expectErrCode(string(errcodes.ReadOnlyScope))
	viewer.send(proto.TypeTakeWrite, proto.TakeWrite{ReqID: viewer.nextReqID(), SessionID: ok.SessionID}, nil)
	viewer.expectErrCode(string(errcodes.ReadOnlyScope))
	viewer.input(ok.SessionID, []byte("nope"))
	viewer.expectErrCode(string(errcodes.ReadOnlyScope))

	// LIST / INFO / ATTACH still work — and the attach is FORCED read-only
	// even though the client asked for writable.
	viewer.send(proto.TypeList, proto.List{ReqID: viewer.nextReqID()}, nil)
	viewer.expect(proto.TypeListOK)
	att := viewer.attach(ok.SessionID, nil, false /* asks writable; daemon forces RO */)
	if att.SessionID != ok.SessionID {
		t.Fatalf("attach = %+v", att)
	}
	data, _ := viewer.collectOutput(ok.SessionID, "scoped-view")
	if len(data) == 0 {
		t.Fatal("read-only viewer got no output")
	}

	owner.send(proto.TypeKill, proto.Kill{ReqID: owner.nextReqID(), SessionID: ok.SessionID, Signal: proto.SignalKILL}, nil)
	owner.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)
}

func TestWS_MixedTransportsOneSession(t *testing.T) {
	sock, port, control, _ := startServerWS(t)

	uds := dialT(t, sock)
	ws := dialWST(t, port, control)

	ok := spawnShell(uds, "sleep 0.2; echo both-transports; sleep 300")
	uds.attach(ok.SessionID, nil, false)
	ws.attach(ok.SessionID, nil, true)

	d1, _ := uds.collectOutput(ok.SessionID, "both-transports")
	d2, _ := ws.collectOutput(ok.SessionID, "both-transports")
	if string(d1) != string(d2) {
		t.Errorf("transports diverge: uds %d bytes, ws %d bytes", len(d1), len(d2))
	}
	uds.send(proto.TypeKill, proto.Kill{ReqID: uds.nextReqID(), SessionID: ok.SessionID, Signal: proto.SignalKILL}, nil)
	uds.expectSkipping(proto.TypeOK, proto.TypeOutput, proto.TypeEvent)
}

func TestWS_TokenFilesPermissions(t *testing.T) {
	_, srv := startServer(t, Options{WSPort: freePort(t)})
	for _, name := range []string{constants.TokenFilename, tokenROFilename} {
		fi, err := os.Stat(filepath.Join(srv.opts.HomeDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perms = %o, want 600", name, perm)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

var _ = time.Second // keep time import for future timing assertions
