// tail.go — `runbaypty tail`: full history then live follow.
//
// The stitch is exact because the durable log and the ring share ONE byte
// axis: the log holds bytes [0, N); ATTACH {since_seq: N} resumes the live
// stream at exactly byte N. No overlap, no gap — the same arithmetic as
// reconnect, applied across storage tiers.
package main

import (
	"fmt"
	"io"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/host"

	"github.com/spf13/cobra"
)

func newTailCommand() *cobra.Command {
	var noFollow bool
	cmd := &cobra.Command{
		Use:   "tail <id|name>",
		Short: "Print a session's full history (durable log + ring), then follow live",
		Long: `tail prints everything the session ever output and keeps following.

With a durable log (session spawned with --log) the history is complete:
the log's bytes print first, then the live stream resumes at exactly the
next byte. Without a log, history is the ring buffer window (the newest
bytes, default 2 MiB). --no-follow exits after the history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()

			info, err := c.Info(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			// Tier 1: durable log — the complete history from byte 0.
			var since uint64
			if info.LogPath != "" {
				records, err := host.ReadSessionLog(info.LogPath)
				if err != nil {
					return errcodes.Newf(errcodes.LogDisabled, "session log %s unreadable: %v", info.LogPath, err).
						WithCause(err).WithHint("is the log on this machine? tail falls back to the ring without --log sessions")
				}
				for _, rec := range records {
					if _, err := out.Write(rec.Data); err != nil {
						return fmt.Errorf("tail: write history: %w", err)
					}
					since += uint64(len(rec.Data))
				}
			}
			if noFollow && info.LogPath != "" {
				return nil
			}

			// Tier 2: the live stream from exactly where the log ends
			// (or full ring replay when there is no log).
			var sincePtr *uint64
			if info.LogPath != "" {
				sincePtr = &since
			}
			st, err := c.Attach(cmd.Context(), info.ID, sincePtr, true)
			if err != nil {
				return err
			}
			if noFollow {
				// Ring-only history: drain what is buffered… a stream has no
				// "end of history" marker, so no-follow without a log prints
				// the replay then stops at the first live pause. Honest cut:
				// detach immediately after the replay drains.
				go func() { _ = st.Detach(cmd.Context()) }()
			}
			_, err = io.Copy(out, st)
			if err != nil {
				return err
			}
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
	cmd.Flags().BoolVar(&noFollow, "no-follow", false, "print history and exit instead of following")
	return cmd
}
