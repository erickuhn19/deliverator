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

var riskResetAnchorYes bool

var riskResetAnchorCmd = &cobra.Command{
	Use:   "reset-anchor",
	Short: "Re-base the drawdown high-water mark to current equity (operator-only)",
	Long: `Re-base risk.max_drawdown_pct's peak-equity anchor to your CURRENT equity.

WHY THIS EXISTS. The drawdown gate measures from an equity high-water mark. When
that mark is the ALL-TIME peak, a realized and accepted loss is re-litigated
forever: the gate stops asking "is this period going wrong" and its only steady
states are strangling the account or being switched off. A live account reached
98.7% utilization with $1.52 of losable equity, and the only escape was setting
the cap to 100 — turning the ruin backstop OFF, which is strictly worse than a
well-anchored one.

Resetting is an OPERATOR ACKNOWLEDGMENT of a realized loss. It REDUCES protection,
so it is deliberately not reachable from the agent's order path — a trading loop
must never be able to unblock itself by moving its own floor. It requires --yes,
it is written to the audit log, and the superseded peak is preserved and reported
by ` + "`deliverator risk`" + ` afterwards.

CONSIDER THE ALTERNATIVE FIRST. risk.drawdown_window_days makes the peak trailing
(e.g. 30 = the highest equity in the last 30 days), which forgives history
automatically and needs no manual step. A reset is the right tool for a single
acknowledged step-change; a window is the right tool for an ongoing policy.

The daily-loss anchor is deliberately NOT reset — it measures a different horizon,
and clearing it here would quietly hand back the day's loss budget too.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("risk-reset-anchor", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.ResetDrawdownAnchor(ctx, riskResetAnchorYes)
		})
	},
}

func init() {
	riskResetAnchorCmd.Flags().BoolVar(&riskResetAnchorYes, "yes", false,
		"confirm re-basing the drawdown anchor — this REDUCES protection and is required")
	riskCmd.AddCommand(riskResetAnchorCmd)
	rootCmd.AddCommand(riskCmd)
}
