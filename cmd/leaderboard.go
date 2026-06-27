package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
)

// leaderboard flags
var (
	lbWindow       string
	lbSort         string
	lbOrder        string
	lbLimit        int
	lbOffset       int
	lbAddresses    string
	lbNamed        bool
	lbProfitable   bool
	lbProfitableIn string
	lbMinAV        float64
	lbMaxAV        float64
	lbMinPnl       float64
	lbMaxPnl       float64
	lbMinRoi       float64
	lbMaxRoi       float64
	lbMinVlm       float64
	lbMaxVlm       float64
	lbMinPrize     float64
	lbLive         bool
	lbLiveScan     int
	lbInMarket     bool
	lbFlat         bool
	lbMinLiveEq    float64
	lbMaxLiveEq    float64
	lbMaxLiveLev   float64
)

// leaderboardCmd fetches the official Hyperliquid trader leaderboard and exposes
// rich filtering/sorting/drill-down so an agent can mine it for a profitable,
// active address to `copy`. Read-only; no signing, no third-party data source.
var leaderboardCmd = &cobra.Command{
	Use:     "leaderboard",
	Aliases: []string{"lb"},
	Short:   "Hyperliquid trader leaderboard — filter/sort/drill-down to find an address to copy",
	Long: `Fetch the official Hyperliquid trader leaderboard (stats-data.hyperliquid.xyz)
and filter, sort, and page through it to find an address worth copy-trading.

Every row carries pnl/roi/vlm for all four windows (day/week/month/allTime) plus
account_value; --window selects which window the sort and the metric filters use.

Examples:
  # Top 25 by today's PnL (default)
  deliverator leaderboard --json

  # Most profitable-by-ROI this week among sizeable, active accounts
  deliverator leaderboard --window week --sort roi --min-account-value 50000 --min-vlm 1000000 --json

  # Consistently profitable across day, week AND month (good copy candidates)
  deliverator leaderboard --profitable-in day,week,month --sort pnl --window month --limit 10 --json

  # Drill down on specific addresses
  deliverator leaderboard --address 0xabc...,0xdef... --json

ROI bounds are fractions (0.1 = 10%). Output: data.rows[] (ranked), with matched/
total/returned counts. Feed a chosen data.rows[].address to 'deliverator copy <addr>'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		p := core.LeaderboardParams{
			Window:          lbWindow,
			SortBy:          lbSort,
			Order:           lbOrder,
			Limit:           lbLimit,
			Offset:          lbOffset,
			Addresses:       splitCoins(lbAddresses),
			Named:           lbNamed,
			Profitable:      lbProfitable,
			ProfitableIn:    splitCoins(lbProfitableIn),
			MinAccountValue: floatIfSet(f, "min-account-value", lbMinAV),
			MaxAccountValue: floatIfSet(f, "max-account-value", lbMaxAV),
			MinPnl:          floatIfSet(f, "min-pnl", lbMinPnl),
			MaxPnl:          floatIfSet(f, "max-pnl", lbMaxPnl),
			MinRoi:          floatIfSet(f, "min-roi", lbMinRoi),
			MaxRoi:          floatIfSet(f, "max-roi", lbMaxRoi),
			MinVlm:          floatIfSet(f, "min-vlm", lbMinVlm),
			MaxVlm:          floatIfSet(f, "max-vlm", lbMaxVlm),
			MinPrize:        floatIfSet(f, "min-prize", lbMinPrize),
			Live:            lbLive,
			LiveScan:        lbLiveScan,
			InMarket:        lbInMarket,
			Flat:            lbFlat,
			MinLiveEquity:   floatIfSet(f, "min-live-equity", lbMinLiveEq),
			MaxLiveEquity:   floatIfSet(f, "max-live-equity", lbMaxLiveEq),
			MaxLiveLeverage: floatIfSet(f, "max-live-leverage", lbMaxLiveLev),
		}
		return runRead("leaderboard", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.Leaderboard(ctx, p)
		})
	},
}

func init() {
	f := leaderboardCmd.Flags()
	f.StringVar(&lbWindow, "window", "day", "window for sort + metric filters: day|week|month|allTime")
	f.StringVar(&lbSort, "sort", "pnl", "sort key: pnl|roi|vlm|account_value|prize")
	f.StringVar(&lbOrder, "order", "desc", "sort order: desc|asc")
	f.IntVar(&lbLimit, "limit", 25, "max rows to return (0 = all)")
	f.IntVar(&lbOffset, "offset", 0, "skip this many ranked rows (pagination)")
	f.StringVar(&lbAddresses, "address", "", "drill-down: only these addresses (comma-separated)")
	f.BoolVar(&lbNamed, "named", false, "only rows with a public display name")
	f.BoolVar(&lbProfitable, "profitable", false, "only rows with window pnl > 0 AND roi > 0")
	f.StringVar(&lbProfitableIn, "profitable-in", "", "only rows with pnl > 0 in EACH listed window (e.g. day,week,month)")
	f.Float64Var(&lbMinAV, "min-account-value", 0, "minimum account_value (USD)")
	f.Float64Var(&lbMaxAV, "max-account-value", 0, "maximum account_value (USD)")
	f.Float64Var(&lbMinPnl, "min-pnl", 0, "minimum window pnl (USD)")
	f.Float64Var(&lbMaxPnl, "max-pnl", 0, "maximum window pnl (USD)")
	f.Float64Var(&lbMinRoi, "min-roi", 0, "minimum window roi (fraction: 0.1 = 10%)")
	f.Float64Var(&lbMaxRoi, "max-roi", 0, "maximum window roi (fraction: 0.1 = 10%)")
	f.Float64Var(&lbMinVlm, "min-vlm", 0, "minimum window volume (USD) — proxy for activity")
	f.Float64Var(&lbMaxVlm, "max-vlm", 0, "maximum window volume (USD)")
	f.Float64Var(&lbMinPrize, "min-prize", 0, "minimum contest prize")
	// Live enrichment (current on-chain state; one read per returned row, bounded).
	f.BoolVar(&lbLive, "live", false, "annotate each returned row with its CURRENT positions/equity/leverage (one extra read per row)")
	f.IntVar(&lbLiveScan, "live-scan", 25, "max addresses to enrich when --live (hard cap 100)")
	f.BoolVar(&lbInMarket, "in-market", false, "only addresses holding a position right now (implies --live)")
	f.BoolVar(&lbFlat, "flat", false, "only addresses currently in cash — watch their next trade (implies --live)")
	f.Float64Var(&lbMinLiveEq, "min-live-equity", 0, "minimum CURRENT equity (USD) — uses live state, not the stale board value (implies --live)")
	f.Float64Var(&lbMaxLiveEq, "max-live-equity", 0, "maximum CURRENT equity (USD) (implies --live)")
	f.Float64Var(&lbMaxLiveLev, "max-live-leverage", 0, "drop addresses whose current max leverage exceeds this (implies --live)")
	rootCmd.AddCommand(leaderboardCmd)
}
