// command-boundaries turns a long-lived shell session into a stream of
// structured command results: what ran, when it started and ended (on the
// byte axis), and its real exit code — no screen scraping.
//
// This works because modern shells emit OSC 133 "shell integration" marks
// in-band: ESC ] 133 ; C BEL before a command's output, ESC ] 133 ; D ; <code>
// BEL after it. The daemon scans for those marks while passing the bytes
// through untouched (it never builds a terminal grid) and republishes them as
// `command-started` / `command-finished` events carrying ring sequence
// numbers. Ask for the last command's output and you get exactly that slice.
//
// Run:
//
//	go run ./examples/command-boundaries
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// The "shell": three commands, each bracketed by OSC 133 marks with a real
// exit code. `printf` in sh turns the backslash escapes into the actual
// control bytes, so the session's byte stream carries the marks exactly as a
// shell-integration hook (bash-preexec, zsh precmd, fish, kitty, WezTerm,
// VS Code, Warp) would emit them. No shell configuration needed to run this.
//
//	ESC ] 133 ; C BEL          — output of a command starts here
//	ESC ] 133 ; D ; <code> BEL — the command finished with <code>
const script = `
printf '\033]133;C\007'; echo "hello world";    printf '\033]133;D;0\007'
printf '\033]133;C\007'; echo "this one fails"; printf '\033]133;D;3\007'
printf '\033]133;C\007'; echo "final command";  printf '\033]133;D;0\007'
sleep 300
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "command-boundaries:", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := c.SubscribeEvents(ctx, "")
	if err != nil {
		return err
	}
	sessionID, _, err := c.Spawn(ctx, client.SpawnOpts{Cmd: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return err
	}
	defer func() { _ = c.Kill(context.Background(), sessionID, "") }()
	fmt.Printf("session %s — watching for OSC 133 command marks\n\n", sessionID)

	// Every `command-finished` event carries the exit code AND the byte range
	// of that command's output on the session's sequence axis.
	seen := 0
	for seen < 3 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("only saw %d of 3 commands", seen)
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("event stream closed")
			}
			if ev.SessionID != sessionID || ev.Type != proto.EventCommandFinished {
				continue
			}
			seen++
			start, _ := strconv.ParseUint(ev.Data[proto.DataKeyStartSeq], 10, 64)
			end, _ := strconv.ParseUint(ev.Data[proto.DataKeyEndSeq], 10, 64)
			code := ev.Data[proto.DataKeyExitCode]

			status := "ok"
			if code != "0" {
				status = "FAILED"
			}
			fmt.Printf("command %d: exit=%-3s %-7s output bytes [%d, %d)\n", seen, code, status, start, end)
		}
	}

	// The last command's output, sliced out of the ring by its recorded
	// boundaries. No parsing, no guessing where it began.
	body, start, end, err := c.LastCommandOutput(ctx, sessionID)
	if err != nil {
		return err
	}
	fmt.Printf("\nlast command's output (seq %d–%d):\n%q\n", start, end, string(body))
	return nil
}
