package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
)

// referralCode is the Deliverator referral code. New users get it via the
// /join/<code> onboarding link; existing accounts apply it with `referral apply`.
// Referred accounts get a fee discount; the operator earns referral rewards.
const referralCode = "DELIVERATOR"

var referralCmd = &cobra.Command{Use: "referral", Short: "Apply or check the Deliverator referral code", RunE: requireSubcommand}

var referralStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether this account has a referrer set",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRead("referral.status", func(ctx context.Context, c core.ClientAPI) (any, error) {
			ri, err := c.ReferralStatus(ctx)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"referred":         ri.IsReferred(),
				"cum_volume":       ri.CumVlm,
				"deliverator_code": referralCode,
			}
			if ri.ReferredBy != nil {
				out["referred_by"] = ri.ReferredBy
			}
			return out, nil
		})
	},
}

var referralApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply the Deliverator referral code (one-time; 4% off your fees)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("referral.apply", err)
		}
		// HL allows only one referrer, set once — skip cleanly if already set.
		if ri, rerr := c.ReferralStatus(ctx); rerr == nil && ri.IsReferred() {
			emit("referral.apply", map[string]any{
				"applied": false, "reason": "account already has a referrer", "referred_by": ri.ReferredBy,
			})
			return nil
		}
		if err := c.SetReferrer(ctx, referralCode); err != nil {
			return fail("referral.apply", err)
		}
		emit("referral.apply", map[string]any{"applied": true, "code": referralCode},
			"referral applied — you now get 4% off Hyperliquid fees")
		return nil
	},
}

func init() {
	referralCmd.AddCommand(referralStatusCmd, referralApplyCmd)
	rootCmd.AddCommand(referralCmd)
}
