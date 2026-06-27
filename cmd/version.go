package cmd

import (
	"runtime"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/output"
)

// Build metadata — overridden at build time via -ldflags.
var (
	Version   = "0.4.0-dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// APIClient identifies the Hyperliquid client. Deliverator talks to the API
// directly via internal/hl — there is no third-party SDK in the signing path.
const APIClient = "native (internal/hl)"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print binary + schema version (compatibility assertion)",
	RunE: func(cmd *cobra.Command, args []string) error {
		output.Emit(output.Response{
			Cmd: "version",
			Data: map[string]any{
				"version":    Version,
				"schema":     output.SchemaVersion,
				"commit":     Commit,
				"build_date": BuildDate,
				"go":         runtime.Version(),
				"api_client": APIClient,
			},
			Meta: RootMeta(0),
		})
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
