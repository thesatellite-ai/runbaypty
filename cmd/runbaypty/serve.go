// serve.go — `runbaypty serve`: run the daemon in this process.
//
// This is the entrypoint the launchd/systemd unit points at. Supervisors
// want a non-forking child, so serve always stays in the foreground; the
// --foreground flag exists for symmetry with other daemons' CLIs and to
// make intent explicit in unit files.
package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/daemon"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"

	"github.com/spf13/cobra"
)

func newServeCommand() *cobra.Command {
	var (
		foreground   bool
		jsonLogs     bool
		maxSessions  int
		retentionTTL time.Duration
		ringTotal    int64
		wsPort       int
		takeover     bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the runbaypty daemon",
		Long: `serve runs the PTY-owning daemon: UDS listener, session registry,
ring buffers, and the byte-stream fanout. Normally started by launchd/systemd
(runbaypty daemon install); run it directly for development.

The socket path honors ` + constants.EnvSock + ` and the home directory honors
` + constants.EnvHome + ` — point them at a scratch dir for an isolated daemon.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = foreground // serve never forks; the flag documents intent
			var logger *slog.Logger
			if jsonLogs {
				logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
			} else {
				logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
			}

			var adopted *daemon.Adopted
			var err error
			if takeover {
				sock, serr := constants.SocketPath()
				if serr != nil {
					return serr
				}
				home, herr := constants.Home()
				if herr != nil {
					return herr
				}
				adopted, err = daemon.RequestTakeover(sock, home)
				if err != nil {
					if !errcodes.IsCode(err, errcodes.DaemonUnreachable) {
						return err
					}
					// No daemon to take over — normal cold boot.
					adopted = nil
				} else {
					logger.Info("takeover: adopted state received", "sessions", len(adopted.Sessions))
				}
			}
			srv, nerr := daemon.New(daemon.Options{
				Adopted:        adopted,
				MaxSessions:    maxSessions,
				RetentionTTL:   retentionTTL,
				RingTotalBytes: ringTotal,
				WSPort:         wsPort,
				Version:        version,
				Logger:         logger,
			})
			if nerr != nil {
				return nerr
			}
			// SIGTERM (supervisor stop) and SIGINT (ctrl-c in dev) both
			// trigger the graceful path.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return srv.Serve(ctx)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", true, "stay in the foreground (always true; serve never forks)")
	cmd.Flags().BoolVar(&jsonLogs, "log-json", false, "emit structured JSON logs")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", 0, "cap concurrent sessions (0 = default)")
	cmd.Flags().DurationVar(&retentionTTL, "retention-ttl", 0, "how long exited sessions linger before reap (0 = default)")
	cmd.Flags().Int64Var(&ringTotal, "ring-total", 0, "global cap on summed ring-buffer bytes (0 = default)")
	cmd.Flags().IntVar(&wsPort, "ws-port", 0, "enable the loopback WebSocket listener on this port (0 = disabled)")
	cmd.Flags().BoolVar(&takeover, "takeover", false, "adopt a running daemon's sessions (zero-downtime upgrade), else cold boot")
	return cmd
}
