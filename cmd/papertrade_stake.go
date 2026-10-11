package cmd

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/papertrade"
)

// PAPER staking. The venue signs stake, unstake, and claim with the user's
// wallet (the master key), not the session key, in the PaperStaking EIP-712
// domain. See internal/papertrade/staking.go for the evidence trail.

var ptAmount string

var ptStakeCmd = &cobra.Command{
	Use:   "stake",
	Short: "Stake PAPER for USDC rewards (master wallet signs; needs --yes)",
	Long: `Stake PAPER through the Papertrade relayer. The intent is signed by the
user wallet via DELIVERATOR_MASTER_KEY (read for this command only, never stored).
The agent session key cannot stake. The relayer enforces a per-stake minimum
(see 'papertrade account' staking.minimum_stake). Staked PAPER earns USDC.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return paperStakingWrite("papertrade.stake", "paperStake")
	},
}

var ptUnstakeCmd = &cobra.Command{
	Use:   "unstake",
	Short: "Unstake PAPER (master wallet signs; needs --yes)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return paperStakingWrite("papertrade.unstake", "paperUnstake")
	},
}

var ptClaimCmd = &cobra.Command{
	Use:   "claim",
	Short: "Claim accrued USDC staking rewards (master wallet signs; needs --yes)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return paperStakingWrite("papertrade.claim", "paperClaim")
	},
}

func init() {
	papertradeCmd.AddCommand(ptStakeCmd, ptUnstakeCmd, ptClaimCmd)
	ptStakeCmd.Flags().StringVar(&ptAmount, "amount", "", "PAPER to stake (up to 18 decimals)")
	ptUnstakeCmd.Flags().StringVar(&ptAmount, "amount", "", "PAPER to unstake (up to 18 decimals)")
}

func paperStakingWrite(name, action string) error {
	if err := paperWritePre(); err != nil {
		return fail(name, err)
	}
	var amount *big.Int
	if action != "paperClaim" {
		a, err := papertrade.ParsePaper(ptAmount)
		if err != nil || a.Sign() <= 0 {
			return fail(name, output.Validation("amount", "pass --amount as a positive PAPER amount, up to 18 decimals").
				WithHint("example: --amount 1000"))
		}
		amount = a
	}
	ctx, cancel := cmdCtx()
	defer cancel()
	user, err := paperUser()
	if err != nil {
		return fail(name, err)
	}
	markets, account, err := paperReadBook(ctx, user)
	if err != nil {
		return fail(name, err)
	}
	if !markets.ActionEnabled(action) {
		return fail(name, output.Exchange("intake_action_paused", "Papertrade is not accepting "+action+" right now").Retry())
	}
	staking := paperStakingSummary(account, markets)
	preview := map[string]any{
		"action": action, "user": strings.ToLower(user.Hex()),
		"staking_contract": papertrade.PaperStaking, "staking": staking,
	}
	if err := paperStakingCheck(action, amount, account, markets, staking); err != nil {
		return fail(name, err)
	}
	if amount != nil {
		preview["amount_raw"] = amount.String()
		preview["amount"] = papertrade.FormatRaw18(amount)
	}
	if flagDryRun {
		preview["dry_run"] = true
		preview["note"] = "A live run reads DELIVERATOR_MASTER_KEY, signs once, and does not store that key."
		emit(name, preview)
		return nil
	}
	if !flagYes {
		return fail(name, output.Validation("confirm", action+" is signed by the master wallet — pass --yes to confirm").
			WithHint("run with --dry-run first to see the plan"))
	}
	master, signer, err := paperMaster()
	if err != nil {
		return fail(name, err)
	}
	if err := paperMasterMatches(signer); err != nil {
		return fail(name, err)
	}
	if signer != user {
		return fail(name, output.Validation("master_address", "DELIVERATOR_MASTER_KEY does not control the Papertrade user wallet"))
	}
	nonce, err := paperNonces().Take(papertrade.NonceKey(papertrade.PaperStaking, papertrade.NonceRoutePaper, user.Hex()))
	if err != nil {
		return fail(name, output.Network("nonce", err.Error()).Retry())
	}
	n, ok := new(big.Int).SetString(nonce, 10)
	if !ok {
		return fail(name, output.Unknown("nonce", "allocated nonce is not decimal"))
	}
	deadline := paperDeadline(time.Now())
	var signed *papertrade.Signed
	switch action {
	case "paperStake":
		signed, err = papertrade.SignStake(master, papertrade.StakeMessage{User: user, Amount: amount, Nonce: n, Deadline: deadline})
	case "paperUnstake":
		signed, err = papertrade.SignUnstake(master, papertrade.StakeMessage{User: user, Amount: amount, Nonce: n, Deadline: deadline})
	default:
		signed, err = papertrade.SignClaim(master, papertrade.ClaimMessage{User: user, Nonce: n, Deadline: deadline})
	}
	if err != nil {
		return fail(name, output.Unknown("sign", err.Error()))
	}
	if !strings.EqualFold(signed.Signer, user.Hex()) {
		return fail(name, output.Unknown("sign", "staking intent recovered a different signer than the user wallet"))
	}
	rec, err := paperSend(ctx, name, signed, "/intents", user, signed.Wire)
	if err != nil {
		return err
	}
	out := map[string]any{
		"action": action, "intent_id": signed.IntentID, "state": rec.State,
		"user": strings.ToLower(user.Hex()), "nonce": nonce, "queue_revision": rec.QueueRevision,
	}
	if amount != nil {
		out["amount_raw"] = amount.String()
		out["amount"] = papertrade.FormatRaw18(amount)
	}
	paperAudit(name, map[string]any{"intent_id": signed.IntentID, "action": action, "amount_raw": out["amount_raw"], "state": rec.State})
	emit(name, out)
	return nil
}

// paperStakingCheck mirrors the venue client's form checks so a doomed intent
// never burns a nonce: stake >= minimum and <= free PAPER, unstake <= staked,
// claim only with something pending.
func paperStakingCheck(action string, amount *big.Int, a papertrade.Account, m papertrade.Markets, s *papertrade.Staking) error {
	bal, _ := new(big.Int).SetString(orZero(a.PaperBalance), 10)
	staked, _ := new(big.Int).SetString(orZero(a.PaperStaked), 10)
	switch action {
	case "paperStake":
		if min, ok := new(big.Int).SetString(orZero(m.Intake.MinimumPaperStakeRaw), 10); ok && amount.Cmp(min) < 0 {
			return output.Validation("below_minimum_paper_stake", "stake is under the venue minimum of "+papertrade.FormatRaw18(min)+" PAPER")
		}
		if bal == nil || amount.Cmp(bal) > 0 {
			return output.Validation("amount", "amount exceeds available PAPER ("+papertrade.FormatRaw18(orBig(bal))+")")
		}
	case "paperUnstake":
		if staked == nil || amount.Cmp(staked) > 0 {
			return output.Validation("amount", "amount exceeds staked PAPER ("+papertrade.FormatRaw18(orBig(staked))+")")
		}
	case "paperClaim":
		if s.PendingRewardsUSDCRaw == "" || s.PendingRewardsUSDCRaw == "0" {
			return output.Validation("nothing_to_claim", "no pending USDC staking rewards")
		}
	}
	return nil
}

func paperStakingSummary(a papertrade.Account, m papertrade.Markets) *papertrade.Staking {
	s := &papertrade.Staking{
		PaperBalance: fmtRaw(a.PaperBalance), PaperStaked: fmtRaw(a.PaperStaked),
		StakingContract: papertrade.PaperStaking,
	}
	if m.AccRewardPerShareRaw != "" {
		if p, ok := papertrade.PendingRewards(a.PaperStaked, a.PaperPendingBase, a.PaperRewardDebt, m.AccRewardPerShareRaw); ok {
			s.PendingRewardsUSDCRaw = p.String()
			s.PendingRewardsUSDC = papertrade.FormatRaw18(p)
		}
	}
	if m.Intake.MinimumPaperStakeRaw != "" {
		s.MinimumStakeRaw = m.Intake.MinimumPaperStakeRaw
		s.MinimumStake = fmtRaw(m.Intake.MinimumPaperStakeRaw)
	}
	return s
}

// paperAttachStaking adds the staking block to an account view. A markets read
// failure only drops pending rewards and the minimum, with a warning.
func paperAttachStaking(ctx context.Context, view *papertrade.AccountView) []string {
	markets, err := paperClient().Markets(ctx)
	if err != nil {
		view.Staking = paperStakingSummary(view.Account, papertrade.Markets{})
		return []string{"trading state unavailable, so pending staking rewards were not computed: " + err.Error()}
	}
	view.Staking = paperStakingSummary(view.Account, markets)
	return nil
}

func fmtRaw(s string) string {
	n, ok := new(big.Int).SetString(orZero(s), 10)
	if !ok {
		return s
	}
	return papertrade.FormatRaw18(n)
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

func orBig(n *big.Int) *big.Int {
	if n == nil {
		return big.NewInt(0)
	}
	return n
}
