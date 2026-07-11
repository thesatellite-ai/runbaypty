// Package daemon runs the runbaypty server: it owns the listeners, the
// housekeeping ticker, the discovery file, and graceful shutdown, and it
// drives the transport-free engine (pkg/host) on behalf of connected
// clients. UDS is the v1 transport; the WS listener (M5) reuses the same
// conn handler over a different net.Listener.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/host"
	"github.com/thesatellite-ai/runbaypty/pkg/processlock"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// Options configures a Server. Zero values take the constants defaults.
type Options struct {
	// SocketPath is the UDS path ("" = constants.SocketPath()).
	SocketPath string
	// HomeDir is the runbaypty home ("" = constants.Home()).
	HomeDir string
	// MaxSessions caps concurrent sessions (0 = default).
	MaxSessions int
	// RingTotalBytes caps the sum of all ring reservations (0 = default).
	RingTotalBytes int64
	// WSPort enables the loopback WebSocket listener on 127.0.0.1:<port>
	// (0 = WS disabled). Auth: scoped tokens minted at boot (see token
	// files in HomeDir); UDS needs no token — file perms are its auth.
	WSPort int
	// RetentionTTL is the exited-session linger (0 = default).
	RetentionTTL time.Duration
	// Version is the daemon binary version for HELLO_ACK + discovery.
	Version string
	// Logger receives structured operational logs (nil = slog.Default()).
	Logger *slog.Logger
	// Adopted carries a takeover's received state (serve --takeover).
	Adopted *Adopted
}

// Discovery is the JSON shape of the daemon.json discovery file.
type Discovery struct {
	SocketPath      string `json:"socket_path"`
	Pid             int    `json:"pid"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
	// WSPort is the loopback WebSocket port (0 = WS disabled).
	WSPort int `json:"ws_port,omitempty"`
	// TokenPath / TokenROPath locate the WS auth tokens (control /
	// read-only scope). The token VALUES never enter this file.
	TokenPath   string `json:"token_path,omitempty"`
	TokenROPath string `json:"token_ro_path,omitempty"`
}

// Server is one running daemon instance.
type Server struct {
	opts Options
	log  *slog.Logger
	reg  *host.Registry

	mu     sync.Mutex
	conns  map[*conn]struct{}
	closed bool
	ln     net.Listener
	lock   *processlock.Lock
	// handedOver closes when a takeover completed — Serve exits WITHOUT
	// touching sessions (they belong to the new daemon now).
	handedOver chan struct{}

	// wsTokens holds the boot-minted WS credentials (nil = WS disabled).
	wsTokens *tokens
	// wsBoundPort is the actual WS port after bind (0 = disabled).
	wsBoundPort int

	// ready closes once the listener is accepting (tests wait on it).
	ready chan struct{}
}

// New builds a Server (no I/O yet; Serve does the work).
func New(opts Options) (*Server, error) {
	if opts.SocketPath == "" {
		p, err := constants.SocketPath()
		if err != nil {
			return nil, fmt.Errorf("daemon: resolve socket path: %w", err)
		}
		opts.SocketPath = p
	}
	if opts.HomeDir == "" {
		h, err := constants.Home()
		if err != nil {
			return nil, fmt.Errorf("daemon: resolve home: %w", err)
		}
		opts.HomeDir = h
	}
	if opts.RetentionTTL <= 0 {
		opts.RetentionTTL = constants.DefaultRetentionTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	srv := &Server{
		opts:       opts,
		log:        opts.Logger,
		reg:        host.NewRegistry(opts.MaxSessions),
		conns:      make(map[*conn]struct{}),
		ready:      make(chan struct{}),
		handedOver: make(chan struct{}),
	}
	srv.reg.SetRingTotalCap(opts.RingTotalBytes)
	return srv, nil
}

// Ready closes once the listener is accepting connections.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Registry exposes the engine (tests + future in-process embedding).
func (s *Server) Registry() *host.Registry { return s.reg }

// Serve runs the daemon until ctx is canceled, then shuts down gracefully:
// stop accepting, notify clients, SIGTERM every session's process group,
// escalate to SIGKILL at the deadline, clean up socket + discovery + lock.
func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(s.opts.HomeDir, 0o700); err != nil {
		return fmt.Errorf("daemon: create home %s: %w", s.opts.HomeDir, err)
	}

	// One daemon per home dir: the flock is authoritative (the socket file
	// check below is just cleanup for stale files after a crash). A takeover
	// ADOPTS the previous daemon's lock fd — the flock moved with the fd.
	var lock *processlock.Lock
	if s.opts.Adopted != nil && s.opts.Adopted.LockFile != nil {
		lock = processlock.Adopt(s.opts.Adopted.LockPath, s.opts.Adopted.LockFile)
	} else {
		var err error
		lock, err = processlock.Acquire(filepath.Join(s.opts.HomeDir, constants.LockFilename))
		if err != nil {
			if errors.Is(err, processlock.ErrLockHeld) {
				return errcodes.Newf(errcodes.LockHeld, "another daemon owns %s", s.opts.HomeDir).
					WithHint("stop it first, or point RUNBAYPTY_HOME/RUNBAYPTY_SOCK elsewhere")
			}
			return fmt.Errorf("daemon: acquire lock: %w", err)
		}
	}
	s.lock = lock
	defer func() { _ = lock.Release() }() // best-effort; process exit releases anyway

	// Rebuild adopted sessions BEFORE accepting clients: the first LIST
	// after a takeover must already see every surviving session.
	if s.opts.Adopted != nil {
		for _, as := range s.opts.Adopted.Sessions {
			sess := host.AdoptSession(as.State, as.Ring, as.Ptmx, s.reg.Events())
			if err := s.reg.Adopt(sess); err != nil {
				s.log.Error("adopt failed", "session", as.State.ID, "err", err)
				continue
			}
			s.log.Info("adopted session", "session", as.State.ID, "name", as.State.Name, "pid", as.State.Pid)
		}
	}

	// A previous crash can leave the socket file behind. The flock proves
	// no live daemon owns it, so removal is safe.
	if _, statErr := os.Stat(s.opts.SocketPath); statErr == nil {
		s.log.Warn("removing stale socket", "path", s.opts.SocketPath)
		if err := os.Remove(s.opts.SocketPath); err != nil {
			return fmt.Errorf("daemon: remove stale socket: %w", err)
		}
	}

	ln, err := net.Listen(constants.NetworkUnix, s.opts.SocketPath)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", s.opts.SocketPath, err)
	}
	if err := os.Chmod(s.opts.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("daemon: chmod socket: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	// runCtx ends on EITHER caller cancellation or takeover retirement —
	// everything internal (WS listener, housekeeping) hangs off it.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Optional WS listener: tokens minted first, port bound before the
	// discovery file is published so clients read a complete picture.
	wsDone := make(chan error, 1)
	wsCtx, wsCancel := context.WithCancel(context.Background())
	defer wsCancel()
	if s.opts.WSPort > 0 {
		if s.wsTokens, err = mintTokens(s.opts.HomeDir); err != nil {
			_ = ln.Close()
			return err
		}
		bound := make(chan int, 1)
		go func() { wsDone <- s.serveWS(wsCtx, s.opts.WSPort, func(p int) { bound <- p }) }()
		select {
		case s.wsBoundPort = <-bound:
			s.log.Info("ws listening", "addr", constants.LoopbackHost+":"+itoaInt(s.wsBoundPort))
		case err := <-wsDone:
			_ = ln.Close()
			return err
		}
	} else {
		close(wsDone) // nothing to wait for at shutdown
	}

	if err := s.writeDiscovery(); err != nil {
		_ = ln.Close()
		return err
	}
	s.log.Info("daemon listening", "socket", s.opts.SocketPath, "pid", os.Getpid(), "version", s.opts.Version)

	// Housekeeping ticker: silence monitors + retention reaper.
	tickerDone := make(chan struct{})
	go s.housekeeping(runCtx, tickerDone)

	go s.acceptLoop(ln)
	close(s.ready)

	select {
	case <-ctx.Done():
	case <-s.handedOver:
		// Takeover: the NEW daemon owns every session and the lock fd now.
		// Exit without killing anything and without touching the socket or
		// discovery files (the new daemon rewrote them).
		runCancel()
		wsCancel()
		<-tickerDone
		if err, open := <-wsDone; open && err != nil {
			s.log.Warn("ws exit during retirement", "err", err)
		}
		s.log.Info("daemon retired after takeover")
		return nil
	}
	s.log.Info("daemon stopping")

	// 1. Tell every event subscriber, then stop accepting + close conns.
	s.reg.Events().Emit(proto.Event{Type: proto.EventDaemonStopping, AtMs: time.Now().UTC().UnixMilli()})
	s.mu.Lock()
	s.closed = true
	openConns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		openConns = append(openConns, c)
	}
	// Close the CURRENT listener — a rollback's relisten() may have swapped
	// s.ln since boot; closing the stale local would leak its accept loop.
	currentLn := s.ln
	s.mu.Unlock()
	if currentLn != nil {
		_ = currentLn.Close()
	}
	wsCancel()
	for _, c := range openConns {
		c.close()
	}
	<-tickerDone
	if err, open := <-wsDone; open && err != nil {
		s.log.Warn("ws listener exit", "err", err)
	}

	// 2. SIGTERM every live session's tree; escalate to SIGKILL at deadline.
	s.stopSessions()

	// 3. Remove the runtime files. Best-effort — the stale-file paths above
	// recover from a missed cleanup after a crash anyway.
	_ = os.Remove(s.opts.SocketPath)
	_ = os.Remove(filepath.Join(s.opts.HomeDir, constants.DiscoveryFilename))
	s.log.Info("daemon stopped")
	return nil
}

// acceptLoop runs until the listener closes (shutdown or takeover).
func (s *Server) acceptLoop(ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		c := newConn(s, nc)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go c.handle()
	}
}

// shutdownDeadline bounds how long graceful SIGTERM gets before SIGKILL.
// A var, not a const, so tests can shorten the escalation wait — the value
// is never mutated in production.
var shutdownDeadline = 5 * time.Second

// stopSessions terminates every live session: TERM first, KILL stragglers.
func (s *Server) stopSessions() {
	live := s.reg.List()
	for _, sess := range live {
		if err := sess.Kill(proto.SignalTERM); err != nil {
			s.log.Warn("shutdown TERM failed", "session", sess.ID(), "err", err)
		}
	}
	// Absolute deadline shared across all sessions. Using a single
	// time.After(...) channel here would be a bug: it fires exactly once, so
	// only the FIRST straggler could take the escalation branch — a second
	// TERM-ignoring session would then block on <-sess.Done() forever. Recompute
	// the wait against the absolute deadline each iteration so EVERY straggler
	// escalates to SIGKILL once the window passes (time.Until goes negative →
	// the timer fires immediately).
	deadline := time.Now().Add(shutdownDeadline)
	for _, sess := range live {
		select {
		case <-sess.Done():
		case <-time.After(time.Until(deadline)):
			s.log.Warn("shutdown escalating to SIGKILL", "session", sess.ID())
			_ = sess.Kill(proto.SignalKILL)
			<-sess.Done()
		}
	}
}

// housekeeping runs the periodic tick: silence monitors + retention reaper.
func (s *Server) housekeeping(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(constants.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, sess := range s.reg.List() {
				sess.Monitor().Tick(now)
			}
			for _, id := range s.reg.ReapExpired(now, s.opts.RetentionTTL) {
				s.log.Info("reaped expired session", "session", id)
			}
		}
	}
}

// writeDiscovery writes daemon.json atomically (tmp + rename) so readers
// never see a torn file.
func (s *Server) writeDiscovery() error {
	d := Discovery{
		SocketPath:      s.opts.SocketPath,
		Pid:             os.Getpid(),
		Version:         s.opts.Version,
		ProtocolVersion: proto.ProtocolVersion,
		WSPort:          s.wsBoundPort,
	}
	if s.wsBoundPort > 0 {
		d.TokenPath = filepath.Join(s.opts.HomeDir, constants.TokenFilename)
		d.TokenROPath = filepath.Join(s.opts.HomeDir, tokenROFilename)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: marshal discovery: %w", err)
	}
	path := filepath.Join(s.opts.HomeDir, constants.DiscoveryFilename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("daemon: write discovery: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("daemon: publish discovery: %w", err)
	}
	return nil
}

// itoaInt is strconv.Itoa without importing strconv twice at call sites.
func itoaInt(v int) string { return fmt.Sprintf("%d", v) }

// dropConn unregisters a finished connection.
func (s *Server) dropConn(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}
