// multi-agent-supervisor is one process supervising many agent sessions at
// once, reacting to each as it changes state — the pattern behind an
// "agent runner" or an orchestration layer that fans work across a pool of
// long-lived terminal sessions.
//
// It is the synthesis of the earlier AI-agent examples: it combines the
// lifecycle EVENT stream (agent-watch), the silence signal (wait-for-silence),
// and server-side WATCH → Input (expect-watch) into a single control loop that
// holds PER-SESSION state and drives a policy over it.
//
// The thesis it demonstrates: runbaypty's daemon is policy-FREE. It holds
// processes and bytes; it never decides when an agent is "stuck", what a
// prompt looks like, or when to reap. Every one of those decisions lives HERE,
// in the client. Swap the policy and the daemon doesn't change.
//
// The scenario — three simulated agents, three outcomes:
//
//	alpha  does its work and exits cleanly.               → we observe the exit.
//	beta   reaches an approval gate and blocks on input.  → we auto-approve it.
//	gamma  goes quiet without exiting (a stalled agent).  → we reap it on silence.
//
// Run:
//
//	go run ./examples/multi-agent-supervisor
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// agentStatus is the closed set of states the supervisor tracks per agent.
// Kept as named constants (never bare strings at a use site) so the render
// path, the transition logic, and the terminal-state test all read from one
// vocabulary.
type agentStatus string

const (
	statusRunning agentStatus = "running"           // producing output or working
	statusBlocked agentStatus = "blocked: approval" // hit the approval gate, awaiting our input
	statusStalled agentStatus = "stalled: silent"   // quiet past the threshold, not exited
	statusExited  agentStatus = "exited"            // finished on its own — terminal
	statusReaped  agentStatus = "reaped (killed)"   // we killed it after it stalled — terminal
)

// isTerminal reports whether a status is an end state the supervisor no longer
// acts on. The control loop finishes when every agent is terminal.
func (s agentStatus) isTerminal() bool { return s == statusExited || s == statusReaped }

// The approval protocol between supervisor and agent. An agent that needs a
// human/automation decision prints approvalSentinel; the supervisor answers
// with approvalGrant. This is a CLIENT-side convention — the daemon knows
// nothing about it; it just watches the byte stream for the pattern we asked
// it to (server-side RE2, so we ship no bytes to scan ourselves).
const (
	approvalSentinel = "[[NEEDS-APPROVAL]]"
	approvalGrant    = "yes\n"
)

// agent is one supervised session plus the supervisor's view of it.
type agent struct {
	name   string
	id     string
	status agentStatus
	note   string // last thing that happened, for the board
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "multi-agent-supervisor:", err)
		os.Exit(1)
	}
}

func run() error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	c, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// One deadline for the whole run. gamma stalls forever on its own; the
	// silence policy reaps it well before this fires. The timeout is only a
	// backstop so a broken daemon can't hang the demo.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Subscribe to lifecycle events BEFORE spawning anything. Same rule as
	// wait-for-silence and session-dashboard: if we spawned first and
	// subscribed second, an agent that exited in the gap would be missed by
	// both the (not-yet-existing) subscription and any snapshot. Subscribe
	// first, and every state change is caught.
	events, err := c.SubscribeEvents(ctx, "")
	if err != nil {
		return err
	}

	// The three agents and the shell scripts that give them their behavior.
	// Scripts are data here, not policy — the supervisor reacts to what the
	// sessions DO, not to which script we happened to launch.
	specs := []struct {
		name   string
		script string
	}{
		// alpha: pure work, then a clean exit. The supervisor just watches it finish.
		{"alpha", `for i in 1 2 3; do echo "alpha: step $i/3"; done; echo "alpha: done"`},
		// beta: reaches an approval gate and blocks on stdin. The 1s pause
		// before the sentinel is a demo guard, not the protocol: a WATCH only
		// matches FUTURE output, and we register it just after spawn, so we
		// give the agent a beat to reach the gate after the watch is armed. A
		// production agent would re-emit its prompt periodically (or the
		// supervisor would scan scrollback on attach) to close that race
		// without a fixed sleep.
		{"beta", `echo "beta: preparing deploy"; sleep 1; echo "` + approvalSentinel + ` deploy to prod?"; read ans; echo "beta: approval='$ans' — deploying"; echo "beta: done"`},
		// gamma: emits a little, then goes silent forever (a stuck agent). It
		// never exits on its own; the supervisor's silence policy reaps it.
		{"gamma", `echo "gamma: starting long job"; echo "gamma: working..."; sleep 600`},
	}

	agents := map[string]*agent{}               // session id → supervisor's view
	killedByUs := map[string]bool{}             // ids we reaped, to tell reap from natural exit
	watchIn := make(chan client.WatchEvent, 32) // fan-in of every agent's approval watch

	for _, s := range specs {
		id, _, err := c.Spawn(ctx, client.SpawnOpts{
			Cmd:  "/bin/sh",
			Args: []string{"-c", s.script},
			// Suffix the session name with this process's pid so reruns don't
			// collide with the still-lingering sessions of a previous run (the
			// daemon retains exited sessions, and their names, for its TTL). The
			// board still shows the short role name; only the daemon-visible
			// session name carries the suffix.
			Name: fmt.Sprintf("%s-%d", s.name, os.Getpid()),
		})
		if err != nil {
			return fmt.Errorf("spawn %s: %w", s.name, err)
		}
		a := &agent{name: s.name, id: id, status: statusRunning, note: "spawned"}
		agents[id] = a

		// Arm a server-side watch for the approval sentinel on THIS agent.
		// Every agent gets the same watch: the supervisor applies one uniform
		// protocol across the pool and lets each agent opt in by printing the
		// sentinel (only beta does). Watches are per-connection and live until
		// the connection closes.
		//
		// The pattern is RE2, so we QuoteMeta the sentinel: "[[NEEDS-APPROVAL]]"
		// contains regex metacharacters ('[' opens a char class) and would
		// otherwise fail to compile. We match it as the literal text it is.
		w, err := c.Watch(ctx, id, regexp.QuoteMeta(approvalSentinel))
		if err != nil {
			return fmt.Errorf("watch %s: %w", s.name, err)
		}
		// Fan every per-agent watch channel into one, so the control loop can
		// select over a single approvals stream alongside events.
		go func() {
			for ev := range w {
				watchIn <- ev
			}
		}()
	}

	fmt.Printf("supervising %d agents — alpha (clean exit), beta (approval gate), gamma (stalls)\n\n", len(agents))
	render(agents)

	// The control loop. Three inputs drive it: lifecycle events (exit,
	// silence), approval matches (from the watches), and the deadline. Each
	// turn mutates per-agent state and repaints. It ends when every agent is
	// terminal.
	for remaining(agents) > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("deadline before all agents settled: %w", ctx.Err())

		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("daemon closed the event stream")
			}
			a := agents[ev.SessionID]
			if a == nil {
				continue // an event for a session we don't own
			}
			switch ev.Type {
			case proto.EventExited:
				// Distinguish a natural finish from a reap we initiated.
				if killedByUs[a.id] {
					a.status = statusReaped
				} else {
					a.status = statusExited
					a.note = "exit " + ev.Data[proto.DataKeyExitCode]
				}
				fmt.Printf("← %s %s\n", a.name, a.status)

			case proto.EventSilence:
				// POLICY: an agent that has gone quiet past the threshold and
				// has NOT exited is treated as stalled and reaped. This is the
				// entire point of the example — the daemon reports silence as a
				// fact; deciding that silence means "stuck, kill it" is ours.
				// alpha/beta exit before the threshold, so only gamma lands here.
				if a.status.isTerminal() {
					continue
				}
				quiet := ev.Data[proto.DataKeyQuietMs]
				a.status = statusStalled
				a.note = "silent " + quiet + "ms"
				fmt.Printf("! %s stalled (silent %sms) — reaping\n", a.name, quiet)
				killedByUs[a.id] = true
				// Empty signal → the daemon's default terminate. The resulting
				// EventExited comes back around and flips us to statusReaped.
				if err := c.Kill(ctx, a.id, ""); err != nil {
					return fmt.Errorf("reap %s: %w", a.name, err)
				}
			}
			render(agents)

		case w := <-watchIn:
			a := agents[w.SessionID]
			if a == nil || a.status.isTerminal() {
				continue
			}
			// The agent hit its approval gate. Grant it and let it proceed —
			// the daemon matched the pattern server-side and told us the seq;
			// we never scanned a byte. This is expect-watch's mechanism applied
			// as a supervision policy.
			a.status = statusBlocked
			a.note = fmt.Sprintf("approval @seq %d → granting", w.Seq)
			fmt.Printf("? %s asked for approval (seq %d) — granting\n", a.name, w.Seq)
			render(agents)
			if err := c.Input(a.id, []byte(approvalGrant)); err != nil {
				return fmt.Errorf("approve %s: %w", a.name, err)
			}
			// Its status returns to running until it exits; the render above
			// already showed the transient "blocked" so the causality is visible.
			a.status = statusRunning
			a.note = "approved — resuming"
		}
	}

	fmt.Println()
	fmt.Println("── all agents settled ──")
	for _, a := range sortedAgents(agents) {
		fmt.Printf("  %-6s %s\n", a.name, a.status)
	}
	fmt.Println()
	fmt.Println("alpha finished on its own; beta was unblocked by our approval; gamma")
	fmt.Println("was reaped because WE decided its silence meant it was stuck. The")
	fmt.Println("daemon supplied the facts (exit, match, silence); every decision was")
	fmt.Println("the supervisor's. That separation is the whole design.")
	return nil
}

// remaining counts agents not yet in a terminal state.
func remaining(agents map[string]*agent) int {
	n := 0
	for _, a := range agents {
		if !a.status.isTerminal() {
			n++
		}
	}
	return n
}

// sortedAgents returns the agents in stable name order for rendering.
func sortedAgents(agents map[string]*agent) []*agent {
	out := make([]*agent, 0, len(agents))
	for _, a := range agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// render draws the current supervision board. Plain reprint (no screen clear)
// so the run reads as a scrollable log of how state evolved.
func render(agents map[string]*agent) {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-6s %-22s %s\n", "AGENT", "STATUS", "NOTE")
	for _, a := range sortedAgents(agents) {
		fmt.Fprintf(&b, "  %-6s %-22s %s\n", a.name, a.status, a.note)
	}
	fmt.Print(b.String())
}
