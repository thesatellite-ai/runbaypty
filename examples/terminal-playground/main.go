// terminal-playground is a full-featured web control panel for a runbaypty
// daemon: a real xterm.js terminal plus UI for nearly every capability the
// daemon exposes over its WebSocket — spawn, attach, resize, the write lock,
// kill/rename/meta, the lifecycle event stream, server-side watches, OSC 133
// command boundaries, replay, and auto-reconnect with zero-gap resume.
//
// It's the "kitchen sink" companion to the minimal browser-terminal example:
// where that one shows the smallest path to a browser terminal, this one is a
// test bench for exercising the whole protocol surface from a browser.
//
// Architecture (same thin-helper pattern as browser-terminal): this Go process
// serves ONE page and injects the daemon's WS tokens into it. The browser then
// talks to the daemon's WebSocket directly — this helper is never in the data
// path. It hands out both the control token and the read-only token so the page
// can toggle scopes; keep it bound to loopback.
//
// Requires the daemon's WebSocket listener:  runbaypty serve --ws-port 8377
// Run:
//
//	RUNBAYPTY_HOME=<home> go run ./examples/terminal-playground
//	# then open the printed URL
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

//go:embed index.html
var pageHTML string

// configPlaceholder is the literal the page's JS declares; we replace it with a
// real JSON config at serve time so the secret tokens live server-side until
// the page is delivered to the (same-origin, loopback) browser.
const configPlaceholder = "/*__RPTY_CONFIG__*/ null"

// pageConfig is injected as window.RPTY. Field names are the JSON keys the page
// reads — this struct is the single source of that contract.
type pageConfig struct {
	WSURL         string `json:"wsUrl"`
	WSPath        string `json:"wsPath"`
	ControlToken  string `json:"controlToken"`
	ReadOnlyToken string `json:"readOnlyToken"`
	ProtocolVer   int    `json:"protocolVersion"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "terminal-playground:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:9099", "address for this web helper to listen on")
	wsPort := flag.Int("ws-port", 8377, "the daemon's WebSocket port")
	flag.Parse()

	home, err := constants.Home()
	if err != nil {
		return err
	}
	control, err := readToken(home, constants.TokenFilename)
	if err != nil {
		return fmt.Errorf("read control token (is the daemon running with --ws-port?): %w", err)
	}
	readOnly, err := readToken(home, constants.TokenFilename+".ro")
	if err != nil {
		return fmt.Errorf("read read-only token: %w", err)
	}

	cfg := pageConfig{
		WSURL:         fmt.Sprintf("ws://127.0.0.1:%d%s", *wsPort, constants.WSPathV1),
		WSPath:        constants.WSPathV1,
		ControlToken:  control,
		ReadOnlyToken: readOnly,
		ProtocolVer:   1,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
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

	fmt.Printf("runbaypty terminal playground\n")
	fmt.Printf("daemon WS: %s\n", cfg.WSURL)
	fmt.Printf("open:      http://%s/\n", *addr)
	fmt.Println("(ctrl-c to stop the web helper; daemon sessions keep running)")

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

// readToken reads and trims a token file from the daemon home directory.
func readToken(home, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
