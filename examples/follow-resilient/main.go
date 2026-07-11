// follow-resilient demonstrates client.Follow — the zero-gap resilient
// stream. Point it at a session (id or name) and it prints the full
// scrollback then follows live, surviving daemon-connection loss with
// automatic reconnect + resume at the exact byte it had seen.
//
// Run:
//
//	runbaypty run --name ticker -- sh -c 'i=0; while :; do echo tick-$i; i=$((i+1)); sleep 1; done'
//	go run ./examples/follow-resilient ticker
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: follow-resilient <session-id|name>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "follow-resilient:", err)
		os.Exit(1)
	}
}

func run(target string) error {
	sock, err := constants.SocketPath()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fl, err := client.Follow(ctx, sock, target, client.FollowOpts{ReadOnly: true})
	if err != nil {
		return err
	}
	defer fl.Close()

	if _, err := io.Copy(os.Stdout, fl); err != nil && ctx.Err() == nil {
		return err
	}
	if code, sig, exited := fl.Exit(); exited {
		if sig != "" {
			fmt.Fprintf(os.Stderr, "\nsession exited: signal %s\n", sig)
		} else {
			fmt.Fprintf(os.Stderr, "\nsession exited: code %d\n", code)
		}
	}
	return nil
}
