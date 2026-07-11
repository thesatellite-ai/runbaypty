// write-lock-handoff demonstrates the agent↔human handoff: an agent drives a
// session, a human takes over the SAME live session (same process, same
// scrollback), does something by hand, and hands control back.
//
// The mechanism is the single write lock. One client holds the right to type
// at a time. TAKE_WRITE steals the lock (an explicit takeover — that IS the
// handoff), and the previous holder's next keystroke is refused with a push
// error it can observe. Nobody ever types over anybody.
//
// This program plays both roles in sequence against one cat session, so you
// can watch the lock move.
//
// Run:
//
//	go run ./examples/write-lock-handoff
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "write-lock-handoff:", err)
		os.Exit(1)
	}
}

func run() error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	ctx := context.Background()

	// Two independent connections to the same daemon: the "agent" and the
	// "human". In a real system these are different processes on different
	// machines; here they're two clients in one program.
	agent, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = agent.Close() }()
	human, err := client.Dial(sock)
	if err != nil {
		return err
	}
	defer func() { _ = human.Close() }()

	// The agent spawns a session and starts driving it.
	sessionID, _, err := agent.Spawn(ctx, client.SpawnOpts{Cmd: "/bin/cat", Name: "shared"})
	if err != nil {
		return err
	}
	defer func() { _ = agent.Kill(context.Background(), sessionID, "") }()

	agentStream, err := agent.Attach(ctx, sessionID, nil, false)
	if err != nil {
		return err
	}
	humanStream, err := human.Attach(ctx, sessionID, nil, false)
	if err != nil {
		return err
	}

	// ── Act 1: the agent holds the lock and types.
	if err := agent.TakeWrite(ctx, sessionID); err != nil {
		return err
	}
	fmt.Println("agent has the write lock")
	if err := agent.Input(sessionID, []byte("line-from-agent\r")); err != nil {
		return err
	}
	waitFor(agentStream, "line-from-agent")
	fmt.Println("  agent typed: line-from-agent")

	// ── Act 2: the human tries to type WITHOUT the lock. It's refused, and
	// the refusal is observable on the human's stream (input is fire-and-
	// forget on the wire; refusals come back as a push error).
	if err := human.Input(sessionID, []byte("sneaky-human\r")); err != nil {
		return err
	}
	if refusal := waitRefusal(humanStream); errcodes.IsCode(refusal, errcodes.NoWriteLock) {
		fmt.Println("human tried to type without the lock → refused (E_NO_WRITE_LOCK) ✓")
	} else {
		return fmt.Errorf("expected a write-lock refusal, got %v", refusal)
	}

	// ── Act 3: the handoff. The human TAKES the lock — this steals it from
	// the agent. Now the human types and the agent is the one refused.
	if err := human.TakeWrite(ctx, sessionID); err != nil {
		return err
	}
	fmt.Println("\n→ handoff: human takes the write lock")
	if err := human.Input(sessionID, []byte("line-from-human\r")); err != nil {
		return err
	}
	waitFor(humanStream, "line-from-human")
	fmt.Println("  human typed: line-from-human")

	// The agent's next keystroke is now refused — it lost the lock.
	if err := agent.Input(sessionID, []byte("agent-again\r")); err != nil {
		return err
	}
	if refusal := waitRefusal(agentStream); errcodes.IsCode(refusal, errcodes.NoWriteLock) {
		fmt.Println("agent tried to type after the handoff → refused ✓")
	}

	// ── Act 4: hand control back. The human releases; the agent re-takes.
	if err := human.ReleaseWrite(ctx, sessionID); err != nil {
		return err
	}
	if err := agent.TakeWrite(ctx, sessionID); err != nil {
		return err
	}
	fmt.Println("\n← handoff back: human released, agent re-took the lock")
	if err := agent.Input(sessionID, []byte("agent-resumes\r")); err != nil {
		return err
	}
	waitFor(agentStream, "agent-resumes")
	fmt.Println("  agent typed: agent-resumes")

	fmt.Println("\n✓ one live session, one process, control moved agent → human → agent")
	fmt.Println("  Nobody ever typed over anybody: the single write lock is the referee.")
	return nil
}

// waitFor drains a stream until it contains want (a demo helper).
func waitFor(st *client.Stream, want string) {
	buf := make([]byte, 4096)
	var acc string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := st.Read(buf)
		acc += string(buf[:n])
		if strings.Contains(acc, want) {
			return
		}
		if err != nil {
			return
		}
	}
}

// waitRefusal polls a stream's one-shot write-refusal slot (input refusals
// arrive as push errors, not inline in the byte stream).
func waitRefusal(st *client.Stream) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if refusal := st.WriteRefusal(); refusal != nil {
			return refusal
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("no refusal surfaced")
}
