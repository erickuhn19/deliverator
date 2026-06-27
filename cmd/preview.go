package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
)

var (
	pvLimit    string
	pvLeverage int
)

// previewCmd is a no-sign what-if: project the resulting position, account
// leverage, margin, and an estimated liquidation price for a proposed order (#46).
var previewCmd = &cobra.Command{
	Use:   "preview <coin> <buy|sell> <size>",
	Short: "What-if: project resulting leverage / margin / liq for an order (no signing)",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		side, err := parseSide(args[1])
		if err != nil {
			return fail("preview", err)
		}
		return runRead("preview", func(ctx context.Context, c core.ClientAPI) (any, error) {
			return c.Preview(ctx, args[0], side, args[2], pvLimit, pvLeverage)
		})
	},
}

func init() {
	previewCmd.Flags().StringVar(&pvLimit, "limit", "", "entry price to model (default: live mark/mid)")
	previewCmd.Flags().IntVar(&pvLeverage, "leverage", 0, "leverage to model (default: the position's, else asset max)")
	rootCmd.AddCommand(previewCmd)
}
