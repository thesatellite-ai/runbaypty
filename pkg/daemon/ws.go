// ws.go — the loopback WebSocket listener (M5).
//
// Transport equivalence: websocket.NetConn adapts the WS connection to a
// net.Conn where every Write becomes one binary message — and our Encoder
// writes exactly one frame per Write, so one WS message = one frame with
// zero extra code. The SAME conn handler serves both transports; the only
// WS-specific parts are the HTTP upgrade and token auth.
//
// Auth model: UDS trusts file permissions; WS cannot, so the daemon mints
// two tokens at boot, written 0600 into HomeDir:
//
//	token     — control scope: the full protocol
//	token.ro  — read-only scope: LIST / INFO / ATTACH(read-only) / DETACH /
//	            SUBSCRIBE_EVENTS — share-a-view-of-my-session
//
// The token rides in HELLO (never in the URL — URLs leak into logs).
// Comparison is constant-time. Loopback bind is enforced, not assumed.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

// connScope is what a connection's credentials allow.
type connScope int

const (
	// scopeControl: the full protocol (UDS always; WS with the control token).
	scopeControl connScope = iota
	// scopeReadOnly: observation only; ATTACH is forced read-only.
	scopeReadOnly
)

// tokenROFilename is the read-only token beside constants.TokenFilename.
const tokenROFilename = constants.TokenFilename + ".ro"

// tokens holds the two boot-minted WS credentials.
type tokens struct {
	control  string
	readOnly string
}

// mintTokens generates and persists the WS tokens (0600).
func mintTokens(homeDir string) (*tokens, error) {
	gen := func() (string, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("daemon: token entropy: %w", err)
		}
		return hex.EncodeToString(b), nil
	}
	control, err := gen()
	if err != nil {
		return nil, err
	}
	readOnly, err := gen()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(homeDir, constants.TokenFilename), []byte(control+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("daemon: write control token: %w", err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, tokenROFilename), []byte(readOnly+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("daemon: write read-only token: %w", err)
	}
	return &tokens{control: control, readOnly: readOnly}, nil
}

// scopeFor validates a presented token in constant time. ok=false → refuse.
func (t *tokens) scopeFor(presented string) (connScope, bool) {
	if subtle.ConstantTimeCompare([]byte(presented), []byte(t.control)) == 1 {
		return scopeControl, true
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(t.readOnly)) == 1 {
		return scopeReadOnly, true
	}
	return scopeReadOnly, false
}

// serveWS runs the WS listener until ctx cancels. Returns the bound port
// (for discovery) via the ready callback before accepting.
func (s *Server) serveWS(ctx context.Context, port int, ready func(boundPort int)) error {
	addr := net.JoinHostPort(constants.LoopbackHost, strconv.Itoa(port))
	ln, err := net.Listen(constants.NetworkTCP, addr)
	if err != nil {
		return fmt.Errorf("daemon: ws listen %s: %w", addr, err)
	}
	// Belt and suspenders: refuse to serve on anything but loopback even
	// if a future refactor changes the addr above.
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return fmt.Errorf("daemon: ws listener bound non-loopback %s — refusing", ln.Addr())
	}
	ready(tcp.Port)

	mux := http.NewServeMux()
	mux.HandleFunc(constants.WSPathV1, func(w http.ResponseWriter, r *http.Request) {
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Same-origin policy is meaningless on localhost tools (Origin
			// can be anything from file:// pages); the token IS the auth.
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}
		// NetConn: every conn-handler Write = one binary WS message.
		nc := websocket.NetConn(r.Context(), wsc, websocket.MessageBinary)
		c := newConn(s, nc)
		c.needsToken = true
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		c.handle() // block this HTTP handler for the connection's life
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-serveDone
		return nil
	case err := <-serveDone:
		return fmt.Errorf("daemon: ws serve: %w", err)
	}
}
