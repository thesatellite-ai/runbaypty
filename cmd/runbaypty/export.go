// export.go — `runbaypty export`: turn a durable session log into an
// asciinema cast v2 file. The (ts, bytes) log format was designed for this
// (task-m3-tslog): export is a pure file transform, no daemon needed —
// works on logs from sessions long dead.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/host"

	"github.com/spf13/cobra"
)

// castHeader is the asciinema cast v2 header line
// (https://docs.asciinema.org/manual/asciicast/v2/).
type castHeader struct {
	Version   int    `json:"version"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Timestamp int64  `json:"timestamp"`
	Title     string `json:"title,omitempty"`
}

// writeCast renders records as cast v2: header line, then one
// [elapsedSeconds, "o", data] JSON array per record.
func writeCast(w *bufio.Writer, records []host.SessionLogRecord, width, height int, title string) error {
	if len(records) == 0 {
		return errcodes.New(errcodes.InvalidInput, "log contains no records").
			WithHint("was the session spawned with --log?")
	}
	start := records[0].At
	head, err := json.Marshal(castHeader{Version: 2, Width: width, Height: height, Timestamp: start.Unix(), Title: title})
	if err != nil {
		return fmt.Errorf("export: header: %w", err)
	}
	if _, err := w.Write(append(head, '\n')); err != nil {
		return fmt.Errorf("export: write header: %w", err)
	}
	for _, rec := range records {
		elapsed := rec.At.Sub(start).Seconds()
		line, err := json.Marshal([]any{elapsed, "o", string(rec.Data)})
		if err != nil {
			return fmt.Errorf("export: record: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("export: write record: %w", err)
		}
	}
	return nil
}

func newExportCommand() *cobra.Command {
	var (
		out    string
		width  int
		height int
		title  string
	)
	cmd := &cobra.Command{
		Use:   "export <session-log-file>",
		Short: "Convert a durable session log to an asciinema cast (v2)",
		Long: `export reads a (timestamp, bytes) session log — written when a session is
spawned with --log — and emits an asciinema cast v2 file playable with
` + "`asciinema play`" + ` or the asciinema web player. Pure file transform: the
daemon is not involved and the session may be long gone.`,
		Example: `  runbaypty run --log ~/.runbaypty/logs/build.log -- make -j8
  runbaypty export ~/.runbaypty/logs/build.log --out build.cast
  asciinema play build.cast`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			records, err := host.ReadSessionLog(args[0])
			if err != nil {
				return err
			}
			dst := cmd.OutOrStdout()
			if out != "" {
				f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) // #nosec G302 -- casts are shareable artifacts by intent
				if err != nil {
					return errcodes.Newf(errcodes.InvalidInput, "create %s: %v", out, err).WithCause(err)
				}
				defer func() {
					if cerr := f.Close(); cerr != nil && err == nil {
						err = cerr
					}
				}()
				dst = f
			}
			w := bufio.NewWriter(dst)
			if err := writeCast(w, records, width, height, title); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return fmt.Errorf("export: flush: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output .cast path (default: stdout)")
	cmd.Flags().IntVar(&width, "width", 80, "terminal width recorded in the cast header")
	cmd.Flags().IntVar(&height, "height", 24, "terminal height recorded in the cast header")
	cmd.Flags().StringVar(&title, "title", "", "cast title")
	return cmd
}
