// version.go — `runbaypty version` and `runbaypty errors list`.
//
// Stable JSON contracts: fields are additive-only; consumers key off names.
package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"

	"github.com/spf13/cobra"
)

// versionInfo is the JSON envelope for `runbaypty version --json`.
type versionInfo struct {
	BinaryVersion   string `json:"binary_version"`
	ProtocolVersion int    `json:"protocol_version"`
	GoVersion       string `json:"go_version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
}

// protocolVersion is the wire-protocol major version this binary speaks.
// Bumped only on breaking frame/message changes (additive-only policy —
// see PROTOCOL.md once task-m7-protodoc lands).
const protocolVersion = 1

func newVersionCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := versionInfo{
				BinaryVersion:   version,
				ProtocolVersion: protocolVersion,
				GoVersion:       runtime.Version(),
				OS:              runtime.GOOS,
				Arch:            runtime.GOARCH,
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s (protocol v%d, %s %s/%s)\n",
				constants.BinaryName, info.BinaryVersion, info.ProtocolVersion, info.GoVersion, info.OS, info.Arch)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	return cmd
}

// errorsListEntry is one row of `runbaypty errors list --json`.
type errorsListEntry struct {
	Code        errcodes.Code `json:"code"`
	Description string        `json:"description"`
}

func newErrorsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "errors",
		Short: "Inspect the stable error-code registry",
	}
	var jsonOut bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List every registered error code with its description",
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonOut {
				rows := make([]errorsListEntry, 0, len(errcodes.All()))
				for _, c := range errcodes.All() {
					rows = append(rows, errorsListEntry{Code: c, Description: errcodes.Description(c)})
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, c := range errcodes.All() {
				fmt.Fprintf(cmd.OutOrStdout(), "%-26s %s\n", c, errcodes.Description(c))
			}
			return nil
		},
	}
	list.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	cmd.AddCommand(list)
	return cmd
}
