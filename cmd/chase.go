package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
)

// chase (#51) is the passive maker / limit-following helper: place a post-only
// limit pegged to the BBO and re-price it as the touch moves (via modify, which
// preserves the cloid), so the order keeps following the book instead of going
// stale. Like `watch`/`stream` it emits NDJSON (one line per step) and runs until
// the order fills, you Ctrl-C, the --timeout elapses, or --max-reprices is hit.

var (
	chaseOffset      float64
	chaseTif         string
	chaseInterval    time.Duration
	chaseMaxReprices int
	chaseTimeout     time.Duration
	chaseLeave       bool
)

var chaseCmd = &cobra.Command{
	Use:   "chase <coin> <buy|sell> <size>",
	Short: "Place a BBO-pegged limit and re-price it as the book moves (passive maker)",
	Long: `Place a post-only limit pegged to the BBO and keep it pegged: poll the book
every --interval and re-price (via modify, preserving the cloid) whenever the
rounded peg moves, so a resting maker order follows the touch instead of going
stale.

  deliverator chase BTC buy 0.01                      # join the bid, follow it up/down
  deliverator chase ETH sell 1 --offset 0.5           # rest 0.5 behind the ask
  deliverator chase BTC buy 0.01 --max-reprices 20 --timeout 5m

--offset is the distance BEHIND the touch (>=0 passive): a buy pegs at bid-offset,
a sell at ask+offset. Default tif is Alo (post-only) so a reprice never crosses.
Runs until the order fully fills, Ctrl-C, --timeout, or --max-reprices; on exit
without a full fill the resting order is canceled unless --leave-resting. Emits
NDJSON (one envelope per step). Place/modify enforce the usual risk gates.`,
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		side, err := parseSide(args[1])
		if err != nil {
			return fail("chase", err)
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		c, err := newClient(ctx)
		if err != nil {
			return fail("chase", err)
		}

		p := core.ChaseParams{
			Coin: args[0], Side: side, Size: args[2],
			Offset: chaseOffset, Tif: chaseTif,
			Interval: chaseInterval, MaxReprices: chaseMaxReprices,
			Timeout: chaseTimeout, LeaveResting: chaseLeave, Cloid: wCloid,
		}
		if cerr := c.Chase(ctx, p, func(ev core.ChaseEvent) { emit("chase", ev) }); cerr != nil {
			return fail("chase", cerr)
		}
		return nil
	},
}

func init() {
	chaseCmd.Flags().Float64Var(&chaseOffset, "offset", 0, "price distance behind the touch (>=0 passive): buy=bid-offset, sell=ask+offset")
	chaseCmd.Flags().StringVar(&chaseTif, "tif", "Alo", "time-in-force: Alo (post-only, default) | Gtc")
	chaseCmd.Flags().DurationVar(&chaseInterval, "interval", 2*time.Second, "poll/reprice cadence")
	chaseCmd.Flags().IntVar(&chaseMaxReprices, "max-reprices", 0, "stop after this many reprices (0 = unlimited)")
	chaseCmd.Flags().DurationVar(&chaseTimeout, "timeout", 0, "stop after this long (0 = until filled/interrupted)")
	chaseCmd.Flags().BoolVar(&chaseLeave, "leave-resting", false, "on exit without a full fill, keep the order (default: cancel it)")
	chaseCmd.Flags().StringVar(&wCloid, "cloid", "", "client order id (0x+32hex; generated if omitted)")
	rootCmd.AddCommand(chaseCmd)
}
