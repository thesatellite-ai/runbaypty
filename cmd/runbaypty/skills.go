// skills.go — `runbaypty skills`: list the built-in agent skills, or print one
// with `skills get <name>`.
//
// Why it exists: the skills are markdown guides embedded in the binary so an AI
// agent (or a human) can discover what runbaypty does and how to drive it, then
// act, with no external docs and no network. `skills` lists them; `skills get
// <name>` prints one guide's full text. This mirrors the `agent-browser skills`
// pattern: a self-describing tool an agent can introspect.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// Each skill's body is embedded from cmd/runbaypty/skills/<name>.md so the
// guides live as plain markdown files (easy to edit) but ship inside the binary.
var (
	//go:embed skills/core.md
	skillCore string
	//go:embed skills/sessions.md
	skillSessions string
	//go:embed skills/history.md
	skillHistory string
	//go:embed skills/events.md
	skillEvents string
	//go:embed skills/remote.md
	skillRemote string
	//go:embed skills/sdk.md
	skillSDK string
)

// skill is one embedded agent-facing guide.
type skill struct {
	Name string `json:"name"`        // stable id; the argument to `skills get <name>`
	Desc string `json:"description"` // one-line summary shown in the list
	Body string `json:"-"`           // full markdown; omitted from the list JSON
}

// skillRegistry is the canonical, ordered set of built-in skills. This slice is
// the single source of truth: `skills` iterates it to list, and `skills get`
// looks up in it. Order is intended reading order (core first).
var skillRegistry = []skill{
	{Name: "core", Desc: "Read first: the daemon model, env vars, and the spawn to attach to read loop", Body: skillCore},
	{Name: "sessions", Desc: "Spawn, drive input, the single-writer lock, kill/rename/resize", Body: skillSessions},
	{Name: "history", Desc: "Durable --log, reprint with tail, replay with export (asciinema)", Body: skillHistory},
	{Name: "events", Desc: "Wait-for-silence, wait-for-ready, OSC 133 command blocks, lastcmd", Body: skillEvents},
	{Name: "remote", Desc: "WebSocket transport, scoped tokens, ssh -L to drive a remote daemon", Body: skillRemote},
	{Name: "sdk", Desc: "The Go SDK (pkg/client): Spawn, Attach, Follow, Watch", Body: skillSDK},
}

// findSkill returns the skill with the given name. ok=false for an unknown name
// (the caller turns that into E_INVALID_INPUT).
func findSkill(name string) (skill, bool) {
	for _, s := range skillRegistry {
		if s.Name == name {
			return s, true
		}
	}
	return skill{}, false
}

// newSkillsCommand builds `skills` (bare = list) plus the `get` subcommand,
// mirroring `agent-browser skills` / `agent-browser skills get <name>`.
func newSkillsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "skills [get <name>]",
		Short: "List the built-in agent skills (guides an agent reads, then acts on)",
		Long: `skills are markdown guides embedded in the binary that teach an agent (or a
human) how to use runbaypty, then get out of the way.

  runbaypty skills            list the skills
  runbaypty skills --json     list them as JSON
  runbaypty skills get core   print one skill's full guide

Start with 'core'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listSkills(cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "list as a JSON array of {name, description}")

	get := &cobra.Command{
		Use:   "get <name>",
		Short: "Print a skill's full guide (markdown) to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, ok := findSkill(args[0])
			if !ok {
				return errcodes.Newf(errcodes.InvalidInput,
					"unknown skill %q; run `runbaypty skills` to list them", args[0])
			}
			fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(s.Body, "\n"))
			return nil
		},
	}
	cmd.AddCommand(get)
	return cmd
}

// listSkills prints the registry as an aligned two-column table (name +
// description), or as a JSON array when asJSON is set.
func listSkills(w io.Writer, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(skillRegistry)
	}
	width := 0
	for _, s := range skillRegistry {
		if len(s.Name) > width {
			width = len(s.Name)
		}
	}
	for _, s := range skillRegistry {
		fmt.Fprintf(w, "  %-*s  %s\n", width, s.Name, s.Desc)
	}
	return nil
}
