// agent-watch demonstrates the SDK's event stream — the agent-era killer
// primitive: know when a session goes quiet ("command probably finished"),
// wakes up, rings a bell, or finishes an OSC 133-marked command, WITHOUT
// polling and without shipping output bytes to an idle watcher.
//
// Run:
//
//	runbaypty serve &                      # or: runbaypty daemon start
//	go run ./examples/agent-watch          # then create sessions and watch
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-watch:", err)
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
	defer c.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	events, err := c.SubscribeEvents(ctx, "" /* all sessions */)
	if err != nil {
		return err
	}
	fmt.Println("watching daemon events — ctrl-c to stop")

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("event stream closed (daemon gone?)")
			}
			switch ev.Type {
			case proto.EventSilence:
				fmt.Printf("[%s] %s quiet for %sms — probably done\n", ev.At.Format("15:04:05"), ev.SessionID, ev.Data["quiet_ms"])
			case proto.EventActivity:
				fmt.Printf("[%s] %s woke up after %sms\n", ev.At.Format("15:04:05"), ev.SessionID, ev.Data["quiet_ms"])
			case proto.EventCommandFinished:
				fmt.Printf("[%s] %s command finished, exit %s\n", ev.At.Format("15:04:05"), ev.SessionID, ev.Data["exit_code"])
			case proto.EventBell:
				fmt.Printf("[%s] %s BELL\n", ev.At.Format("15:04:05"), ev.SessionID)
			case proto.EventExited:
				fmt.Printf("[%s] %s exited (%s)\n", ev.At.Format("15:04:05"), ev.SessionID, ev.Data["exit_code"])
			}
		}
	}
}
