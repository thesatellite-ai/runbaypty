// browser-terminal serves a full, interactive terminal in a web browser,
// backed by a runbaypty session over the loopback WebSocket. It's ttyd/gotty in
// ~120 lines — except the session it shows is persistent, reattachable, and
// shareable, because it's a real runbaypty session, not a fresh child of the
// web server.
//
// The helper does three things:
//
//  1. ensures a session to show (spawns an interactive shell if you don't name one),
//  2. reads the daemon's WS token so the browser never has to,
//  3. serves a self-contained xterm.js page with that token injected, which
//     connects straight to the daemon's WebSocket and speaks the wire protocol.
//
// The token is the crux of the design: a browser can't read the daemon's 0600
// token file, so this local helper reads it and injects it into the page it
// serves (same-origin, localhost only). The browser talks to the DAEMON
// directly for the terminal stream; this helper only hands out the page.
//
// Requires the daemon's WebSocket listener:  runbaypty serve --ws-port 8377
// Run:
//
//	RUNBAYPTY_HOME=<home> go run ./examples/browser-terminal
//	# then open the printed URL
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

//go:embed index.html
var pageHTML string

// configPlaceholder is the token the page's JS declares; we replace it with a
// real JSON config at serve time so the secret token lives server-side until
// the page is delivered.
const configPlaceholder = "/*__RPTY_CONFIG__*/ null"

// pageConfig is injected into the page as window.RPTY. Field names are the JSON
// keys the page reads — kept here as the single source of that contract.
type pageConfig struct {
	WSURL     string `json:"wsUrl"`
	Token     string `json:"token"`
	SessionID string `json:"sessionId"`
	ReadOnly  bool   `json:"readOnly"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "browser-terminal:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:9090", "address for this web helper to listen on")
	sessionArg := flag.String("session", "", "existing session id/name to show (default: spawn a demo shell)")
	wsPort := flag.Int("ws-port", 8377, "the daemon's WebSocket port")
	readOnly := flag.Bool("read-only", false, "serve a view-only terminal (uses the read-only token)")
	flag.Parse()

	home, err := constants.Home()
	if err != nil {
		return err
	}
	// Read the token whose scope matches the mode. token.ro yields a page that
	// can watch but never type (the daemon enforces it); token yields a full
	// interactive terminal.
	tokenFile := constants.TokenFilename
	if *readOnly {
		tokenFile += ".ro"
	}
	tokenBytes, err := os.ReadFile(filepath.Join(home, tokenFile))
	if err != nil {
		return fmt.Errorf("read %s (is the daemon running with --ws-port?): %w", tokenFile, err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	// Ensure there is a session to show. If the user named one, use it; else
	// spawn an interactive shell so the page has a live terminal on first open.
	sessionID := *sessionArg
	if sessionID == "" {
		sessionID, err = spawnDemoShell()
		if err != nil {
			return err
		}
		fmt.Printf("spawned a demo shell session: %s\n", sessionID)
	}

	cfg := pageConfig{
		WSURL:     fmt.Sprintf("ws://127.0.0.1:%d%s", *wsPort, constants.WSPathV1),
		Token:     token,
		SessionID: sessionID,
		ReadOnly:  *readOnly,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// Replace the placeholder with `window.RPTY = {…}` — valid JS the page reads.
	page := strings.Replace(pageHTML, configPlaceholder, string(cfgJSON), 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})

	scope := "read-write"
	if *readOnly {
		scope = "read-only"
	}
	fmt.Printf("\nbrowser terminal (%s) for session %s\n", scope, sessionID)
	fmt.Printf("open:  http://%s/\n", *addr)
	fmt.Println("(ctrl-c to stop the web helper; the session keeps running)")

	// The web helper is intentionally dumb: it serves ONE page and holds no
	// terminal state. The browser streams from the daemon directly.
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

// spawnDemoShell starts an interactive shell over the UDS transport (the SDK)
// so there's something to attach to. It returns the session id. The session
// lingers after this helper exits — that's the persistence point: closing the
// browser or the helper doesn't kill the shell.
func spawnDemoShell() (string, error) {
	sock, err := constants.SocketPath()
	if err != nil {
		return "", err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A login-ish interactive shell. TERM is set so full-screen apps behave.
	id, _, err := c.Spawn(ctx, client.SpawnOpts{
		Cmd:  "/bin/sh",
		Args: []string{"-i"},
		Env:  []string{"TERM=xterm-256color", "PS1=runbaypty$ "},
		Cols: 100, Rows: 30,
		Name: fmt.Sprintf("browser-term-%d", os.Getpid()),
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
