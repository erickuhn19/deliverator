package cmd

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

var rCoins string // snapshot --coins

// snapshotCmd is the unified one-moment read an agent calls once per tick instead
// of chaining portfolio + limits + ctx + builder. Each section carries its own
// ok/error; a partial failure is a top-level warning, not a failed command (#45).
var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Unified one-moment read: portfolio, limits, ctx[coins], builder status (one call)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReadWarn("snapshot", func(ctx context.Context, c core.ClientAPI) (any, []string, error) {
			return c.Snapshot(ctx, splitCoins(rCoins))
		})
	},
}

// splitCoins parses a comma-separated --coins value, trimming and dropping empties.
func splitCoins(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// shared read-command flags
var (
	rCoin     string
	rSince    int64
	rLimit    int
	rLevels   int
	rInterval string
	rWindow   string
	rOid      int64
	rCloid    string
	rClass    string
	rStatus   string
)

func sincePtr() *int64 {
	if rSince > 0 {
		return ptrI64(rSince)
	}
	return nil
}

var portfolioCmd = &cobra.Command{
	Use:   "portfolio",
	Short: "Full snapshot: positions, open orders, balances, margin, uPnL",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("portfolio", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Portfolio(ctx) })
	},
}

var positionsCmd = &cobra.Command{
	Use:   "positions",
	Short: "Open perp positions",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("positions", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Positions(ctx, rCoin) })
	},
}

var ordersCmd = &cobra.Command{
	Use:   "orders",
	Short: "Resting open orders",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("orders", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Orders(ctx, rCoin) })
	},
}

var fillsCmd = &cobra.Command{
	Use:   "fills",
	Short: "Recent fills (incl. fee, builderFee, closedPnl)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("fills", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Fills(ctx, sincePtr(), rLimit) })
	},
}

var fundingCmd = &cobra.Command{
	Use:   "funding",
	Short: "Funding payments",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("funding", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Funding(ctx, sincePtr()) })
	},
}

var ledgerCmd = &cobra.Command{
	Use:   "ledger",
	Short: "Deposits/withdrawals/transfers (non-funding)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("ledger", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Ledger(ctx, sincePtr()) })
	},
}

var balanceCmd = &cobra.Command{
	Use:   "balance",
	Short: "Perp + spot balances, withdrawable",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("balance", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Balance(ctx) })
	},
}

var pnlCmd = &cobra.Command{
	Use:   "pnl",
	Short: "Account-value / PnL / volume time series (also: `pnl attribution`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("pnl", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Pnl(ctx) })
	},
}

// pnlAttributionCmd nets realized PnL − fees − builder fee + funding, per coin.
var pnlAttributionCmd = &cobra.Command{
	Use:   "attribution",
	Short: "Net session P&L by coin + source: realized, trading fees, builder fee, funding",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("pnl.attribution", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.PnlAttribution(ctx, sincePtr(), rCoin)
		})
	},
}

var bookCmd = &cobra.Command{
	Use:   "book <coin>",
	Short: "L2 order book",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("book", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Book(ctx, args[0], rLevels) })
	},
}

var bboCmd = &cobra.Command{
	Use:   "bbo <coin>",
	Short: "Best bid/offer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("bbo", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Bbo(ctx, args[0]) })
	},
}

var midsCmd = &cobra.Command{
	Use:   "mids",
	Short: "All mid prices",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("mids", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Mids(ctx) })
	},
}

var candlesCmd = &cobra.Command{
	Use:   "candles <coin>",
	Short: "OHLCV candles",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("candles", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.Candles(ctx, args[0], rInterval, sincePtr())
		})
	},
}

var ctxCmd = &cobra.Command{
	Use:   "ctx <coin>",
	Short: "Mark, oracle, funding rate, OI, premium",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("ctx", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Ctx(ctx, args[0]) })
	},
}

var limitsCmd = &cobra.Command{
	Use:   "limits",
	Short: "Per-address rate-limit budget (remaining requests)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("limits", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.Limits(ctx) })
	},
}

var predictedFundingsCmd = &cobra.Command{
	Use:   "predicted-fundings",
	Short: "Forward (next-interval) funding forecast per coin and venue (carry signal)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("predicted-fundings", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.PredictedFundings(ctx, rCoin)
		})
	},
}

var historicalOrdersCmd = &cobra.Command{
	Use:   "historical-orders",
	Short: "Closed-order lifecycle (filled/canceled/rejected/expired) for reconciliation",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("historical-orders", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.HistoricalOrders(ctx, rLimit)
		})
	},
}

var marketsCmd = &cobra.Command{
	Use:   "markets",
	Short: "Tradable universe + precision/leverage rules (--class perp|spot|outcome|all, --status)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("markets", func(ctx context.Context, c core.ClientAPI) (any, error) {
			if rCoin != "" {
				mk, ok := c.Meta().Lookup(rCoin)
				if !ok {
					return nil, output.Validation("unknown_coin", "unknown coin "+rCoin)
				}
				return mk, nil
			}
			return marketsFiltered(c.Meta(), rClass, rStatus)
		})
	},
}

// marketsFiltered returns the tradable universe filtered by --class. The default
// (no class) is perps+spot — HIP-4 outcomes number in the hundreds and rotate
// daily, so they are excluded unless requested via `--class outcome` (or
// `--class all`), which lazily loads the outcome universe in newClient (no config
// flag needed); `--status` further filters outcomes by resolution_status (open|settled).
func marketsFiltered(m *core.MetaStore, class, status string) (any, error) {
	class = strings.ToLower(strings.TrimSpace(class))
	status = strings.ToLower(strings.TrimSpace(status))
	out := []core.Market{}
	addOutcomes := func() {
		for _, mk := range m.OutcomeMarkets() {
			if status != "" && !strings.EqualFold(mk.ResolutionStatus, status) {
				continue
			}
			out = append(out, mk)
		}
	}
	switch class {
	case "", "perp", "spot":
		for _, mk := range m.Markets() {
			if class == "perp" && mk.IsSpot {
				continue
			}
			if class == "spot" && !mk.IsSpot {
				continue
			}
			out = append(out, mk)
		}
	case "outcome":
		addOutcomes()
	case "all":
		out = append(out, m.Markets()...)
		addOutcomes()
	default:
		return nil, output.Validation("bad_class", "class must be perp|spot|outcome|all, got "+class)
	}
	return out, nil
}

var builderCmd = &cobra.Command{Use: "builder", Short: "Builder fee status", RunE: requireSubcommand}

var builderStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Approved max, configured fee + attach mode",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("builder.status", func(ctx context.Context, c core.ClientAPI) (any, error) { return c.BuilderStatus(ctx) })
	},
}

var orderStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Query one order by --oid or --cloid",
	RunE: func(cmd *cobra.Command, args []string) error {
		if rOid == 0 && rCloid == "" {
			return fail("order.status", output.Validation("missing_id", "pass --oid or --cloid"))
		}
		return runRead("order.status", func(ctx context.Context, c core.ClientAPI) (any, error) {
			var oid *int64
			if rOid != 0 {
				oid = ptrI64(rOid)
			}
			return c.OrderStatus(ctx, oid, rCloid)
		})
	},
}

func init() {
	positionsCmd.Flags().StringVar(&rCoin, "coin", "", "filter by coin")
	ordersCmd.Flags().StringVar(&rCoin, "coin", "", "filter by coin")
	marketsCmd.Flags().StringVar(&rCoin, "coin", "", "show one coin")
	marketsCmd.Flags().StringVar(&rClass, "class", "", "filter: perp|spot|outcome|all (default: perp+spot)")
	marketsCmd.Flags().StringVar(&rStatus, "status", "", "outcome resolution_status filter: open|settled")

	fillsCmd.Flags().Int64Var(&rSince, "since", 0, "only fills since this unix-ms time")
	fillsCmd.Flags().IntVar(&rLimit, "limit", 100, "max fills to return")
	fundingCmd.Flags().Int64Var(&rSince, "since", 0, "since this unix-ms time (default 7d)")
	ledgerCmd.Flags().Int64Var(&rSince, "since", 0, "since this unix-ms time (default 30d)")

	bookCmd.Flags().IntVar(&rLevels, "levels", 10, "levels per side")
	candlesCmd.Flags().StringVar(&rInterval, "interval", "1m", "candle interval (1m,5m,1h,...)")
	candlesCmd.Flags().Int64Var(&rSince, "since", 0, "since this unix-ms time (default 24h)")
	pnlCmd.Flags().StringVar(&rWindow, "window", "7d", "time window (informational)")
	pnlAttributionCmd.Flags().Int64Var(&rSince, "since", 0, "only fills/funding since this unix-ms time (default: all available)")
	pnlAttributionCmd.Flags().StringVar(&rCoin, "coin", "", "filter by coin (e.g. BTC, xyz:GOLD)")
	pnlCmd.AddCommand(pnlAttributionCmd)

	orderStatusCmd.Flags().Int64Var(&rOid, "oid", 0, "order id")
	orderStatusCmd.Flags().StringVar(&rCloid, "cloid", "", "client order id")

	snapshotCmd.Flags().StringVar(&rCoins, "coins", "", "comma-separated coins for ctx (default: auto-discover from positions+orders)")

	predictedFundingsCmd.Flags().StringVar(&rCoin, "coin", "", "filter by coin")
	historicalOrdersCmd.Flags().IntVar(&rLimit, "limit", 100, "max orders to return (newest first; 0 = all)")

	builderCmd.AddCommand(builderStatusCmd)
	orderCmd.AddCommand(orderStatusCmd) // orderCmd defined in writes.go

	rootCmd.AddCommand(
		portfolioCmd, positionsCmd, ordersCmd, fillsCmd, fundingCmd, ledgerCmd,
		balanceCmd, pnlCmd, bookCmd, bboCmd, midsCmd, candlesCmd, ctxCmd,
		limitsCmd, marketsCmd, builderCmd, snapshotCmd,
		predictedFundingsCmd, historicalOrdersCmd,
	)
}
