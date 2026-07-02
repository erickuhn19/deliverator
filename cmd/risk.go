package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
)

var riskCmd = &cobra.Command{
	Use:   "risk",
	Short: "Show the risk envelope + live utilization (operator-owned)",
	Long: `Report the configured risk caps and how much of each is currently in use
(net exposure, account leverage, per-coin concentration, open positions, drawdown,
daily loss), against live equity, plus the trading posture (what the agent may trade:
outcome markets, limit-only, allowed coins, sub-dexes). Read-only — it never moves the
drawdown/daily-loss anchors the agent's gates depend on.

The risk envelope is the operator's domain: the agent trades within it and may widen
a cap only loudly (` + "`config set risk.*`" + ` warns), never silently. This command is
also the data source for ` + "`deliverator console`" + `.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReadMeta("risk", func(ctx context.Context, c core.ClientAPI) (any, core.ReadMeta, error) {
			rv, err := c.RiskStatus(ctx)
			if err != nil {
				return nil, core.ReadMeta{}, err
			}
			// Surface the source portfolio's partial-read markers: utilization
			// computed from an incomplete book must be loud, never silent.
			return rv, core.ReadMeta{Degraded: rv.Degraded, DegradedDexs: rv.DegradedDexs}, nil
		})
	},
}

func init() {
	rootCmd.AddCommand(riskCmd)
}
