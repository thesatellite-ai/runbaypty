// runbaypty is the single binary for the runbaypty PTY daemon and its CLI.
//
// Build:    go build -trimpath -ldflags='-s -w' -o bin/runbaypty ./cmd/runbaypty
// Daemon:   ./bin/runbaypty serve
// Client:   ./bin/runbaypty run -- <cmd> · ls · attach <id|name> · kill · …
//
// One binary, two roles (RPTY-001 open question resolved toward "single"):
// `serve` runs the daemon; every other verb is a client of the daemon socket.
// Single-binary keeps distribution to one artifact and lets the launchd /
// systemd unit point at the same file the user already has on PATH.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/style"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags '-X main.version=<value>'.
var version = "0.1.0-dev"

// Persistent flag names — looked up by name across command files, so they
// are constants (a typo'd lookup silently returns "" otherwise).
const (
	flagSock  = "sock"
	flagColor = "color"
)

// exit codes — the CLI contract (task-m4-skeleton): 0 ok · 1 error ·
// 2 usage (cobra's own) · 3 daemon unreachable.
const (
	exitOK                = 0
	exitError             = 1
	exitDaemonUnreachable = 3
)

// newRootCommand builds the full command tree. Fresh per invocation so
// tests can run commands in-process without shared cobra state.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   constants.BinaryName,
		Short: "runbaypty: persistent, programmable PTY daemon",
		Long: `runbaypty owns PTY sessions so no app has to.

A tiny OS-managed daemon holds your terminal sessions (dev servers, agents,
shells) so they survive any client rebuild, crash, or quit. Clients connect
over a Unix socket or WebSocket, stream bytes with zero-gap replay, detach,
and reattach. Policy-free by design: no DB, no recipes, no restarts.`,
		Version:       version,
		SilenceErrors: true, // errors render in main via style + errcodes envelopes
		SilenceUsage:  true,
	}
	root.PersistentFlags().String(flagColor, "auto", "color output: auto | always | never")
	root.PersistentFlags().String(flagSock, "", "daemon socket path (default: $"+constants.EnvSock+" or ~/.runbaypty/runbaypty.sock)")

	root.AddCommand(newServeCommand())
	root.AddCommand(newDaemonCommand())
	root.AddCommand(newVersionCommand())
	root.AddCommand(newErrorsCommand())
	root.AddCommand(newRunCommand())
	root.AddCommand(newLsCommand())
	root.AddCommand(newInfoCommand())
	root.AddCommand(newKillCommand())
	root.AddCommand(newResizeCommand())
	root.AddCommand(newRenameCommand())
	root.AddCommand(newMetaCommand())
	root.AddCommand(newAttachCommand())
	root.AddCommand(newEventsCommand())
	root.AddCommand(newExportCommand())
	root.AddCommand(newTailCommand())
	root.AddCommand(newLastCmdCommand())
	root.AddCommand(newSkillsCommand())
	return root
}

// main is a three-line shell around run() so the whole CLI is testable:
// os.Exit is unreachable from tests, everything below it is not.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI with the given args and streams, returning the
// process exit code (see the exit* constants). It never calls os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	colorFlag, _ := root.PersistentFlags().GetString(flagColor)
	style.Init(style.ParseMode(colorFlag))

	if err := root.Execute(); err != nil {
		fmt.Fprintln(stderr, style.Error("ERROR: ")+renderError(err))
		return exitCodeFor(err)
	}
	return exitOK
}

// renderError prefers the errcodes plain rendering when the error carries a
// registered code, falling back to the bare message.
func renderError(err error) string {
	var cli *errcodes.CLIError
	if errors.As(err, &cli) {
		return cli.Plain()
	}
	return err.Error()
}

// exitCodeFor maps error classes to the CLI exit-code contract.
func exitCodeFor(err error) int {
	if errcodes.IsCode(err, errcodes.DaemonUnreachable) {
		return exitDaemonUnreachable
	}
	return exitError
}
