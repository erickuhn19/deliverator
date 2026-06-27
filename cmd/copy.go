package cmd

import (
	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

var (
	cpScaleMode   string
	cpScale       float64
	cpMirrored    string
	cpCoins       string
	cpMinDiff     float64
	cpMinLiqDist  float64
	cpMaxLeverage int
	cpNoNewOpens  bool
	cpMaxPerCycle int
	cpExecute     bool
	cpYes         bool
)

// copyCmd mirrors a leader's perp book onto your account (#27). Diff-first:
// `copy <leader>` shows the trades that would bring your book to the leader's
// (read-only, signs nothing); `--execute --yes` places the surviving legs through
// the guarded Place/Close path (all risk gates apply). Stateless — pass the coins
// you're mirroring via --mirrored and persist data.mirrored_now for the next tick.
var copyCmd = &cobra.Command{
	Use:   "copy <leader_address>",
	Short: "Mirror a leader's perp book: diff (default) or --execute the surviving legs",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("copy", err)
		}
		p := core.CopyParams{
			Leader:            args[0],
			ScaleMode:         orDefault(cpScaleMode, Cfg.Copy.DefaultScaleMode),
			Scale:             orDefaultF(cpScale, Cfg.Copy.DefaultScale),
			Mirrored:          splitCoins(cpMirrored),
			Coins:             splitCoins(cpCoins),
			MinDiffUSD:        orDefaultF(cpMinDiff, Cfg.Copy.MinDiffUSD),
			MinLiqDistancePct: orDefaultF(cpMinLiqDist, Cfg.Copy.MinLiqDistancePct),
			MaxLeverage:       orDefaultI(cpMaxLeverage, Cfg.Copy.MaxLeverage),
			NoNewOpens:        cpNoNewOpens,
			MaxOrdersPerCycle: orDefaultI(cpMaxPerCycle, Cfg.Copy.MaxOrdersPerCycle),
		}

		diff, err := c.Copy(ctx, p)
		if err != nil {
			return fail("copy", err)
		}

		// Default = read-only diff. --execute (with --yes, unless --dry-run) places.
		if !cpExecute || flagDryRun {
			emit("copy.diff", diff)
			return nil
		}
		if !cpYes {
			return fail("copy", output.Validation("confirm", "refusing to --execute without --yes").
				WithHint("re-run with --yes (or use --dry-run / omit --execute to preview the diff)"))
		}
		res, err := c.CopyExecute(ctx, diff, p)
		if err != nil {
			return fail("copy", err)
		}
		var warnings []string
		if len(res.UnknownCloids) > 0 {
			warnings = append(warnings, "outcome-unknown legs (exit 42) — feed these cloids to `reconcile` next cycle, do NOT blind-resubmit")
		}
		emit("copy.execute", res, warnings...)
		switch {
		case len(res.UnknownCloids) > 0:
			return output.ExitWith(output.ExitTimeout) // 42: outcome unknown, reconcile next
		case !res.Complete:
			return output.ExitWith(output.ExitPartial) // 60: some legs rejected/deferred — inspect
		}
		return nil
	},
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func orDefaultF(v, d float64) float64 {
	if v != 0 {
		return v
	}
	return d
}

func orDefaultI(v, d int) int {
	if v != 0 {
		return v
	}
	return d
}

func init() {
	f := copyCmd.Flags()
	f.StringVar(&cpScaleMode, "scale-mode", "", "equity | fixed (default: config copy.default_scale_mode)")
	f.Float64Var(&cpScale, "scale", 0, "size multiplier (default: config copy.default_scale)")
	f.StringVar(&cpMirrored, "mirrored", "", "comma coins you're currently mirroring (for exit detection; from your loop state)")
	f.StringVar(&cpCoins, "coins", "", "restrict the diff to these coins (comma)")
	f.Float64Var(&cpMinDiff, "min-diff", 0, "skip diff legs below this USD delta")
	f.Float64Var(&cpMinLiqDist, "min-liq-distance", 0, "skip open/increase legs whose est liq is closer than this %")
	f.IntVar(&cpMaxLeverage, "max-leverage", 0, "per-leg size clip hint (not a backstop)")
	f.BoolVar(&cpNoNewOpens, "no-new-opens", false, "drop 'open' legs; keep adjustments/exits")
	f.IntVar(&cpMaxPerCycle, "max-orders-per-cycle", 0, "cap legs executed this run")
	f.BoolVar(&cpExecute, "execute", false, "place the surviving legs (requires --yes; routes through the guarded order path)")
	f.BoolVar(&cpYes, "yes", false, "confirm --execute")
	rootCmd.AddCommand(copyCmd)
}
