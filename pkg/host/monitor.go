// monitor.go — per-session activity / silence / bell detection.
//
// Design: no timers. Feed() is called from the read pump with each output
// chunk; Tick() is called by one daemon-wide ticker goroutine. Both take an
// explicit `now`, so tests drive the clock synchronously — no fake-timer
// machinery, no sleeps in tests.
//
// Semantics:
//   - activity: emitted when output arrives after ≥ silenceAfter of quiet
//     (the "it woke up" edge, not every byte).
//   - silence:  emitted ONCE per quiet period when no output for
//     silenceAfter (the "command probably finished" edge). Resets on output.
//   - bell:     emitted per BEL byte in the stream, EXCEPT the BEL that
//     terminates an OSC escape sequence (ESC ] … BEL) — that's a string
//     terminator, not a ring of the bell.
package host

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// oscState tracks the tiny escape-scanner state across Feed chunks —
// sequences split across reads must not confuse bell detection.
type oscState int

const (
	oscGround   oscState = iota // normal stream
	oscEsc                      // saw ESC, next byte decides
	oscInOSC                    // inside ESC ] … ; consuming until terminator
	oscInOSCEsc                 // inside OSC and saw ESC (possible ST: ESC \)
)

// Monitor watches one session's output timing and bytes.
type Monitor struct {
	sessionID string
	bus       *EventBus
	// silenceAfter is the quiet threshold for both the silence event and
	// the activity edge.
	silenceAfter time.Duration

	// mu guards the mutable state below: Feed runs on the session's read
	// pump, Tick on the daemon-wide ticker goroutine.
	mu             sync.Mutex
	lastOutput     time.Time
	silenceEmitted bool
	started        bool
	osc            oscState
	// oscBuf captures the current OSC payload (bounded, 133-sized).
	oscBuf []byte
	// streamPos is the absolute seq of the next byte Feed will see —
	// command marks are recorded on the ring's sequence axis.
	streamPos uint64
	// OSC 133 command tracking: the open command's start, whether one is
	// running, and the last completed command's window.
	cmdStartSeq  uint64
	cmdRunning   bool
	lastCmdStart uint64
	lastCmdEnd   uint64
}

// NewMonitor returns a monitor emitting on bus. silenceAfter ≤ 0 uses
// DefaultSilenceAfter.
func NewMonitor(sessionID string, bus *EventBus, silenceAfter time.Duration) *Monitor {
	if silenceAfter <= 0 {
		silenceAfter = DefaultSilenceAfter
	}
	return &Monitor{sessionID: sessionID, bus: bus, silenceAfter: silenceAfter}
}

// DefaultSilenceAfter is the default quiet threshold. 5s ≈ "the command is
// probably done" for interactive judgment; agents override per taste.
const DefaultSilenceAfter = 5 * time.Second

// Feed processes one output chunk. Called from the read pump; the mutex
// makes it safe against the concurrent Tick goroutine.
func (m *Monitor) Feed(p []byte, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Activity edge: output after a full quiet period.
	if m.started && now.Sub(m.lastOutput) >= m.silenceAfter {
		m.bus.EmitSession(proto.EventActivity, m.sessionID, map[string]string{
			proto.DataKeyQuietMs: strconv.FormatInt(now.Sub(m.lastOutput).Milliseconds(), 10),
		})
	}
	m.started = true
	m.lastOutput = now
	m.silenceEmitted = false

	// Bell + OSC 133 scan. The scanner stays raw-passthrough (MISSION: the
	// one deliberate exception at the seam's edge — it READS the stream for
	// in-band markers, never alters it, never builds a grid). streamPos is
	// the absolute seq of p[0], so command marks land on the ring's axis.
	for i, b := range p {
		switch m.osc {
		case oscGround:
			switch b {
			case 0x1b: // ESC
				m.osc = oscEsc
			case 0x07: // BEL in the clear — a real bell
				m.bus.EmitSession(proto.EventBell, m.sessionID, nil)
			}
		case oscEsc:
			if b == ']' {
				m.osc = oscInOSC // OSC opened: ESC ]
				m.oscBuf = m.oscBuf[:0]
			} else {
				m.osc = oscGround // any other escape — not our business
			}
		case oscInOSC:
			switch b {
			case 0x07: // BEL as OSC terminator — NOT a bell
				m.osc = oscGround
				m.finishOSC(m.streamPos + uint64(i) + 1)
			case 0x1b:
				m.osc = oscInOSCEsc
			default:
				// Capture only what OSC 133 needs; anything longer (titles,
				// hyperlinks) is truncated — we never parse those anyway.
				if len(m.oscBuf) < oscCaptureMax {
					m.oscBuf = append(m.oscBuf, b)
				}
			}
		case oscInOSCEsc:
			// ESC \ is ST (string terminator); anything else stays in OSC
			// (ESC inside an OSC payload is unusual but must not wedge us).
			if b == '\\' {
				m.osc = oscGround
				m.finishOSC(m.streamPos + uint64(i) + 1)
			} else {
				m.osc = oscInOSC
			}
		}
	}
	m.streamPos += uint64(len(p))
}

// oscCaptureMax bounds the captured OSC payload. "133;D;<code>" needs ~16
// bytes; 64 leaves margin for aid= annotations without buffering titles.
const oscCaptureMax = 64

// finishOSC inspects a completed OSC payload for shell-integration marks
// (OSC 133, emitted by shells integrated with WezTerm/kitty/VS Code/Warp):
//
//	133;A  prompt start · 133;B  command typed · 133;C  output starts
//	133;D[;exitCode]  command finished
//
// COMMAND events ride the EVENT stream as additive event types; the
// reserved frame types 40/41 stay reserved for a future dedicated push.
// endSeq is the stream position just after the mark's terminator — the
// boundary for last-command replay (task-m8-lastcmd).
func (m *Monitor) finishOSC(endSeq uint64) {
	payload := string(m.oscBuf)
	m.oscBuf = m.oscBuf[:0]
	rest, ok := strings.CutPrefix(payload, "133;")
	if !ok || rest == "" {
		return
	}
	kind := rest[0]
	switch kind {
	case 'C':
		m.cmdStartSeq = endSeq
		m.cmdRunning = true
		m.bus.EmitSession(proto.EventCommandStarted, m.sessionID, map[string]string{
			proto.DataKeyStartSeq: strconv.FormatUint(endSeq, 10),
		})
	case 'D':
		exitCode := ""
		if parts := strings.SplitN(rest, ";", 2); len(parts) == 2 {
			// "D;0" — trim any aid= suffix fields after another semicolon.
			exitCode = strings.SplitN(parts[1], ";", 2)[0]
		}
		data := map[string]string{
			proto.DataKeyStartSeq: strconv.FormatUint(m.cmdStartSeq, 10),
			proto.DataKeyEndSeq:   strconv.FormatUint(endSeq, 10),
		}
		if exitCode != "" {
			data[proto.DataKeyExitCode] = exitCode
		}
		m.cmdRunning = false
		m.lastCmdStart, m.lastCmdEnd = m.cmdStartSeq, endSeq
		m.bus.EmitSession(proto.EventCommandFinished, m.sessionID, data)
	}
}

// LastCommand returns the ring-seq boundaries of the most recently finished
// command (OSC 133 C→D window). ok=false until the first D mark.
func (m *Monitor) LastCommand() (startSeq, endSeq uint64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCmdStart, m.lastCmdEnd, m.lastCmdEnd != 0
}

// Tick evaluates the silence threshold. Called by the daemon's ticker (or
// tests) with the current time; emits at most once per quiet period.
func (m *Monitor) Tick(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started || m.silenceEmitted {
		return
	}
	if quiet := now.Sub(m.lastOutput); quiet >= m.silenceAfter {
		m.silenceEmitted = true
		m.bus.EmitSession(proto.EventSilence, m.sessionID, map[string]string{
			proto.DataKeyQuietMs: strconv.FormatInt(quiet.Milliseconds(), 10),
		})
	}
}
