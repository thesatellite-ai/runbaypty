// Package constants centralizes every closed-set string and default the
// runbaypty surface shares: filesystem paths, env-var names, and daemon
// defaults. Rule: NO BARE STRINGS at call sites for anything listed here —
// a typo becomes a compile error and a rename is one place.
package constants

import (
	"os"
	"path/filepath"
	"time"
)

// BinaryName is the name of the CLI binary as it appears on the user's PATH
// and in every user-facing help/hint string. All code that prints command
// suggestions must build them from this constant, never a literal.
const BinaryName = "runbaypty"

// Env-var names. Every override the daemon or CLI honors is listed here;
// nothing else reads the environment.
const (
	// EnvHome overrides the runbaypty home directory (default ~/.runbaypty).
	EnvHome = "RUNBAYPTY_HOME"
	// EnvSock overrides the UDS socket path. Setting it gives you an isolated
	// daemon — parallel daemons for tests/workspaces come free.
	EnvSock = "RUNBAYPTY_SOCK"
)

// Network names for net.Listen/Dial and the WS surface. Closed set — the
// transports the daemon speaks; call sites never write the literals.
const (
	// NetworkUnix is the Go net package's name for Unix domain sockets.
	NetworkUnix = "unix"
	// NetworkTCP backs the loopback WebSocket listener.
	NetworkTCP = "tcp"
	// LoopbackHost is the ONLY host the WS listener may bind (MISSION:
	// never non-loopback TCP; remote access is SSH forwarding).
	LoopbackHost = "127.0.0.1"
	// WSPathV1 is the WebSocket endpoint path, versioned with the protocol.
	WSPathV1 = "/v1"
)

// Filenames inside the runbaypty home directory.
const (
	// SocketFilename is the UDS the daemon listens on, mode 0600.
	SocketFilename = "runbaypty.sock"
	// DiscoveryFilename is the JSON discovery file the daemon writes on boot
	// {socketPath, port?, tokenPath, pid, version, protocolVersion}.
	DiscoveryFilename = "daemon.json"
	// LockFilename is the flock that guarantees one daemon per home dir.
	LockFilename = "daemon.lock"
	// TokenFilename holds the WS control token, mode 0600.
	TokenFilename = "token"
	// LogDirname is where opt-in per-session durable logs live by default.
	LogDirname = "logs"
	// DaemonStdoutLog / DaemonStderrLog capture the supervised daemon's own
	// output (launchd StandardOutPath/StandardErrorPath; journald covers
	// systemd, but the constants keep the plist generator literal-free).
	DaemonStdoutLog = "daemon.out.log"
	DaemonStderrLog = "daemon.err.log"
	// StableBinDirname is where `daemon install` copies the binary inside
	// the home dir — supervisors must never point at a moving PATH binary.
	StableBinDirname = "bin"
)

// Daemon defaults. Each is a starting value, overridable by flag; the
// firehose benchmark (task-m3-bench) may retune the batching pair.
const (
	// DefaultRingBytes is the per-session ring-buffer cap: the reconnect
	// replay window, not the full history (durable logs are the history).
	DefaultRingBytes = 2 * 1024 * 1024
	// MaxRingBytes clamps a SPAWN's ring_bytes request (guardrail against
	// one client soaking the daemon's memory).
	MaxRingBytes = 64 * 1024 * 1024
	// DefaultRingTotalBytes caps the SUM of all sessions' ring reservations
	// — the daemon's worst-case ring memory. SPAWN beyond it is refused
	// with E_LIMIT_EXCEEDED rather than OOM-ing a forever-daemon.
	DefaultRingTotalBytes = 512 * 1024 * 1024
	// DefaultMaxSessions bounds concurrent sessions (guardrail, not quota).
	DefaultMaxSessions = 256
	// DefaultBatchFlushMs is the output coalescing timer in milliseconds.
	DefaultBatchFlushMs = 4
	// DefaultBatchMaxBytes flushes a batch early once it reaches this size.
	DefaultBatchMaxBytes = 32 * 1024
	// DefaultRetentionTTL is how long an exited session's record + ring
	// linger before the reaper removes them (late clients must see the
	// death, not a 404).
	DefaultRetentionTTL = 10 * time.Minute
	// TickInterval paces the daemon's housekeeping loop (silence monitors
	// + retention reaper).
	TickInterval = 500 * time.Millisecond
)

// Home returns the runbaypty home directory: $RUNBAYPTY_HOME if set,
// otherwise ~/.runbaypty. The daemon creates it 0700 on first boot.
func Home() (string, error) {
	if h := os.Getenv(EnvHome); h != "" {
		return h, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".runbaypty"), nil
}

// SocketPath returns the UDS path: $RUNBAYPTY_SOCK if set, otherwise
// <home>/runbaypty.sock.
func SocketPath() (string, error) {
	if s := os.Getenv(EnvSock); s != "" {
		return s, nil
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, SocketFilename), nil
}
