// attach.go — `runbaypty attach`: the tmux-style raw attach.
//
// TTY mode (stdin is a terminal): put the local terminal in raw mode, take
// the write lock (unless --read-only), mirror SIGWINCH into RESIZE, forward
// keystrokes, and stream output — detach with ctrl-\ without touching the
// session. Terminal state restores on EVERY exit path (defer + signal).
//
// Pipe mode (stdin is not a terminal): stream output to stdout and copy
// stdin to the session without raw-mode games — `runbaypty attach x | grep`
// and e2e tests both ride this path.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// detachKey is ctrl-\ (FS, 0x1c) — chosen over tmux's prefix model: a single
// dedicated byte keeps the daemon out of key-parsing entirely.
const detachKey = 0x1c

// errDetached signals a clean local detach (session keeps running).
var errDetached = errors.New("detached")

func newAttachCommand() *cobra.Command {
	var (
		readOnly bool
		sinceSeq uint64
		fromSeq  bool
	)
	cmd := &cobra.Command{
		Use:   "attach <id|name>",
		Short: "Attach your terminal to a session (detach: ctrl-\\)",
		Long: `attach connects your terminal to a running session: full replay of the
scrollback ring, then the live byte stream. Keystrokes forward to the session
(taking the write lock) unless --read-only. Detach with ctrl-\ — the session
keeps running. Attaching to an exited session prints its scrollback and exit
status.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }() // CLI exit path; conn error carries nothing

			var since *uint64
			if fromSeq {
				since = &sinceSeq
			}
			st, err := c.Attach(cmd.Context(), args[0], since, readOnly)
			if err != nil {
				return err
			}

			// Interactive only when the command's stdin is a real terminal
			// (tests and pipes inject readers — those take the piped path).
			in := cmd.InOrStdin()
			out := cmd.OutOrStdout()
			inFile, isFile := in.(*os.File)
			interactive := isFile && term.IsTerminal(int(inFile.Fd()))

			if interactive && !readOnly {
				if err := c.TakeWrite(cmd.Context(), st.SessionID()); err != nil {
					return err
				}
			}
			if interactive {
				err = attachInteractive(cmd.Context(), c, st, readOnly, inFile, out)
			} else {
				err = attachPiped(cmd.Context(), c, st, readOnly, in, out)
			}
			if errors.Is(err, errDetached) {
				fmt.Fprintf(cmd.ErrOrStderr(), "\ndetached — session %s keeps running\n", st.SessionID())
				return nil
			}
			if err != nil {
				return err
			}
			// Stream ended because the session exited: report the outcome.
			if code, sig, exited := st.Exit(); exited {
				if sig != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "\nsession exited: signal %s\n", sig)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "\nsession exited: code %d\n", code)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "watch without the ability to type")
	cmd.Flags().Uint64Var(&sinceSeq, "since-seq", 0, "resume the stream from this sequence")
	cmd.Flags().BoolVar(&fromSeq, "from-seq", false, "honor --since-seq instead of full ring replay")
	return cmd
}

// attachInteractive is the raw-mode TTY path. stdin is the confirmed-TTY
// file; out is the command's stdout writer.
func attachInteractive(ctx context.Context, c *client.Client, st *client.Stream, readOnly bool, stdin *os.File, out io.Writer) error {
	stdinFd := int(stdin.Fd())
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return errcodes.New(errcodes.Internal, "raw mode").WithCause(err)
	}
	// Restore on every exit path — a mangled terminal is the one bug users
	// never forgive. The signal handler covers SIGTERM-while-attached.
	restore := func() { _ = term.Restore(stdinFd, oldState) }
	defer restore()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	// Size the session to THIS terminal now and on every SIGWINCH.
	// Last-writer-wins by protocol; min-of-viewers is an app-layer policy.
	resize := func() {
		if w, h, err := term.GetSize(stdinFd); err == nil {
			_ = c.Resize(ctx, st.SessionID(), uint16(w), uint16(h))
		}
	}
	if !readOnly {
		resize()
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Stdin pump: keystrokes → INPUT, watching for the detach key.
	inputErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				for i, b := range chunk {
					if b == detachKey {
						if i > 0 && !readOnly {
							_ = c.Input(st.SessionID(), chunk[:i])
						}
						inputErr <- errDetached
						return
					}
				}
				if !readOnly {
					if err := c.Input(st.SessionID(), chunk); err != nil {
						inputErr <- err
						return
					}
				}
			}
			if err != nil {
				inputErr <- err
				return
			}
		}
	}()

	// Output pump: stream → stdout.
	outputErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, st)
		outputErr <- err
	}()

	for {
		select {
		case <-winch:
			if !readOnly {
				resize()
			}
		case sig := <-sigCh:
			restore()
			signal.Stop(sigCh)
			// Re-raise so the exit status is honest (137-style semantics).
			p, _ := os.FindProcess(os.Getpid())
			_ = p.Signal(sig)
			return errDetached
		case err := <-inputErr:
			return err
		case err := <-outputErr:
			return err // nil = session exited and stream drained
		}
	}
}

// attachPiped is the non-TTY path: no raw mode, no detach key, no resize.
func attachPiped(_ context.Context, c *client.Client, st *client.Stream, readOnly bool, stdin io.Reader, out io.Writer) error {
	// No context plumbing needed here: the stdin pump ends on EOF/refusal
	// and the output copy ends when the stream closes (exit/detach).
	if !readOnly {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := stdin.Read(buf)
				if n > 0 {
					if werr := c.Input(st.SessionID(), buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return // stdin closed; keep streaming output
				}
			}
		}()
	}
	_, err := io.Copy(out, st)
	return err
}
