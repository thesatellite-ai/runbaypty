// client_cmds.go — the control-plane CLI verbs: run · ls · info · kill ·
// resize · rename · meta · events. Every verb is a thin client of the
// daemon socket via pkg/client; --json on anything that outputs data.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/client"
	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"

	"github.com/spf13/cobra"
)

// dial connects to the daemon honoring --sock (then env, then default).
func dial(cmd *cobra.Command) (*client.Client, error) {
	sock, _ := cmd.Flags().GetString(flagSock)
	if sock == "" {
		var err error
		if sock, err = constants.SocketPath(); err != nil {
			return nil, errcodes.New(errcodes.Internal, "resolve socket path").WithCause(err)
		}
	}
	return client.Dial(sock)
}

// printJSON writes v as indented JSON to the command's stdout.
func printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newRunCommand() *cobra.Command {
	var (
		name      string
		cwd       string
		env       []string
		meta      []string
		cols      uint16
		rows      uint16
		ringBytes int
		logPath   string
		noLinger  bool
		jsonOut   bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command> [args…]",
		Short: "Spawn a new PTY session in the daemon",
		Example: `  runbaypty run -- claude
  runbaypty run --name dev -- npm run dev
  runbaypty run --name build --log ~/.runbaypty/logs/build.log -- make -j8`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			metaMap, err := parseKVs(meta)
			if err != nil {
				return err
			}
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			id, pid, err := c.Spawn(cmd.Context(), client.SpawnOpts{
				Cmd: args[0], Args: args[1:], Cwd: cwd, Env: env,
				Cols: cols, Rows: rows, Name: name, Meta: metaMap,
				RingBytes: ringBytes, LogPath: logPath, NoLinger: noLinger,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd, map[string]any{"id": id, "pid": pid, "name": name})
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "unique human name (attach target)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory for the command")
	cmd.Flags().StringArrayVar(&env, "env", nil, "extra environment KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&meta, "meta", nil, "client-owned metadata k=v (repeatable)")
	cmd.Flags().Uint16Var(&cols, "cols", 80, "initial columns")
	cmd.Flags().Uint16Var(&rows, "rows", 24, "initial rows")
	cmd.Flags().IntVar(&ringBytes, "ring", 0, "ring buffer bytes (0 = daemon default)")
	cmd.Flags().StringVar(&logPath, "log", "", "durable output log path (empty = off)")
	cmd.Flags().BoolVar(&noLinger, "no-linger", false, "kill the session when the last client detaches")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	return cmd
}

func newLsCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			sessions, err := c.List(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd, sessions)
			}
			if len(sessions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no sessions — start one: "+constants.BinaryName+" run -- <command>")
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%-38s %-16s %-8s %-7s %-8s %s\n", "ID", "NAME", "STATE", "PID", "CLIENTS", "CMD")
			for _, s := range sessions {
				cmdline := s.Cmd
				if len(s.Args) > 0 {
					cmdline += " " + strings.Join(s.Args, " ")
				}
				if len(cmdline) > 40 {
					cmdline = cmdline[:37] + "…"
				}
				fmt.Fprintf(w, "%-38s %-16s %-8s %-7d %-8d %s\n", s.ID, s.Name, s.State, s.Pid, s.Subscribers, cmdline)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	return cmd
}

func newInfoCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "info <id|name>",
		Short: "Show one session's detail",
		Args:  cobra.ExactArgs(1),
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
			if jsonOut {
				return printJSON(cmd, info)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "id:          %s\n", info.ID)
			if info.Name != "" {
				fmt.Fprintf(w, "name:        %s\n", info.Name)
			}
			fmt.Fprintf(w, "state:       %s\n", info.State)
			fmt.Fprintf(w, "pid:         %d\n", info.Pid)
			fmt.Fprintf(w, "cmd:         %s %s\n", info.Cmd, strings.Join(info.Args, " "))
			if info.Cwd != "" {
				fmt.Fprintf(w, "cwd:         %s\n", info.Cwd)
			}
			fmt.Fprintf(w, "size:        %dx%d\n", info.Cols, info.Rows)
			fmt.Fprintf(w, "started:     %s\n", time.UnixMilli(info.StartedAtMs).UTC().Format(time.RFC3339))
			if info.ExitCode != nil {
				fmt.Fprintf(w, "exit code:   %d\n", *info.ExitCode)
			}
			fmt.Fprintf(w, "bytes:       out %d · in %d (last seq %d)\n", info.BytesOut, info.BytesIn, info.LastSeq)
			fmt.Fprintf(w, "clients:     %d\n", info.Subscribers)
			if info.WriteLockHolder != "" {
				fmt.Fprintf(w, "write lock:  %s\n", info.WriteLockHolder)
			}
			if len(info.Meta) > 0 {
				keys := make([]string, 0, len(info.Meta))
				for k := range info.Meta {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Fprintf(w, "meta.%s: %s\n", k, info.Meta[k])
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	return cmd
}

func newKillCommand() *cobra.Command {
	var signal string
	cmd := &cobra.Command{
		Use:   "kill <id|name>",
		Short: "Signal a session's whole process tree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			return c.Kill(cmd.Context(), args[0], signal)
		},
	}
	cmd.Flags().StringVar(&signal, "signal", proto.SignalTERM, "signal name: TERM | KILL | INT | HUP")
	return cmd
}

func newResizeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resize <id|name> <cols> <rows>",
		Short: "Resize a session's grid (last writer wins)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cols, err := strconv.ParseUint(args[1], 10, 16)
			if err != nil {
				return errcodes.Newf(errcodes.InvalidInput, "cols %q", args[1])
			}
			rows, err := strconv.ParseUint(args[2], 10, 16)
			if err != nil {
				return errcodes.Newf(errcodes.InvalidInput, "rows %q", args[2])
			}
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			return c.Resize(cmd.Context(), args[0], uint16(cols), uint16(rows))
		},
	}
	return cmd
}

func newRenameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <id|name> <new-name>",
		Short: "Rename a session (empty string clears the name)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			return c.Rename(cmd.Context(), args[0], args[1])
		},
	}
}

func newMetaCommand() *cobra.Command {
	meta := &cobra.Command{
		Use:   "meta",
		Short: "Manage a session's client-owned metadata",
	}
	set := &cobra.Command{
		Use:   "set <id|name> k=v [k=v…]",
		Short: "Replace the session's metadata wholesale",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kv, err := parseKVs(args[1:])
			if err != nil {
				return err
			}
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			return c.SetMeta(cmd.Context(), args[0], kv)
		},
	}
	get := &cobra.Command{
		Use:   "get <id|name>",
		Short: "Print the session's metadata as JSON",
		Args:  cobra.ExactArgs(1),
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
			return printJSON(cmd, info.Meta)
		},
	}
	meta.AddCommand(set, get)
	return meta
}

func newEventsCommand() *cobra.Command {
	var (
		session string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream daemon lifecycle events (created/exited/silence/bell/…)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			events, err := c.SubscribeEvents(cmd.Context(), session)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			for {
				select {
				case <-cmd.Context().Done():
					return nil
				case ev, ok := <-events:
					if !ok {
						return errcodes.New(errcodes.DaemonUnreachable, "event stream ended")
					}
					if jsonOut {
						_ = enc.Encode(ev)
						continue
					}
					line := fmt.Sprintf("%s  %-13s %s", ev.At.Format("15:04:05.000"), ev.Type, ev.SessionID)
					if len(ev.Data) > 0 {
						pairs := make([]string, 0, len(ev.Data))
						for k, v := range ev.Data {
							pairs = append(pairs, k+"="+v)
						}
						sort.Strings(pairs)
						line += "  " + strings.Join(pairs, " ")
					}
					fmt.Fprintln(cmd.OutOrStdout(), line)
				}
			}
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "filter to one session id")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "one JSON object per line")
	return cmd
}

func newLastCmdCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "lastcmd <id|name>",
		Short: "Print the last completed command's output (needs OSC 133 shell integration)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			data, _, _, err := c.LastCommandOutput(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
}

// parseKVs turns ["k=v", …] into a map, refusing malformed entries.
func parseKVs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, errcodes.Newf(errcodes.InvalidInput, "metadata %q: want k=v", p)
		}
		out[k] = v
	}
	return out, nil
}
