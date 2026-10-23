package cmd

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/papertrade"
)

// Papertrade builder codes. The fee is the feeAddress on RegisterSessionKey,
// not a field of open or close. Default is the zero address. See
// internal/papertrade/builder.go for the protocol sources.

var (
	ptBuilder     string
	ptAppName     string
	ptWebsite     string
	ptContactKind string
	ptContact     string
	ptIntegration string
)

func init() {
	papertradeCmd.AddCommand(ptBuilderCmd)
	ptBuilderCmd.AddCommand(ptBuilderClaimCmd, ptBuilderApplyCmd)
	ptRegisterCmd.Flags().StringVar(&ptBuilder, "builder", "", "builder code for this session key (0x address or none; default: papertrade.builder_address, else no builder)")
	ptBuilderApplyCmd.Flags().StringVar(&ptAppName, "name", "", "app or builder name (1-64 characters)")
	ptBuilderApplyCmd.Flags().StringVar(&ptWebsite, "website", "", "https website")
	ptBuilderApplyCmd.Flags().StringVar(&ptContactKind, "contact-kind", "", "telegram, x, or email")
	ptBuilderApplyCmd.Flags().StringVar(&ptContact, "contact", "", "contact handle or email")
	ptBuilderApplyCmd.Flags().StringVar(&ptIntegration, "integration", "", "how traders will use this builder code")
}

var ptBuilderCmd = &cobra.Command{
	Use:   "builder",
	Short: "Papertrade builder code: fee rate, approval, and unclaimed fees",
	Long: `Show the live Papertrade builder fee and one address.

The address is --address, otherwise papertrade.builder_address, otherwise
wallet.master_address. Approval and unclaimed fees are read from the
contracts. Routing totals and the application status come from the exchange
query API. Open and close do not take a builder; the fee is chosen when the
session key is registered.

Nothing here is the Hyperliquid builder fee.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.builder"
		ctx, cancel := cmdCtx()
		defer cancel()
		who, err := paperBuilderTarget()
		if err != nil {
			return fail(name, err)
		}
		view, warnings, err := paperBuilderView(ctx, who)
		if err != nil {
			return fail(name, err)
		}
		emitWarn(name, view, append(paperReadWarnings(), warnings...)...)
		return nil
	},
}

var ptBuilderClaimCmd = &cobra.Command{
	Use:   "claim",
	Short: "Claim Papertrade builder fees to the exchange balance (builder wallet signs; needs --yes)",
	Long: `Send claimBuilderFees() on the Papertrade Exchange from the builder
address. The signature is DELIVERATOR_MASTER_KEY for this command only; that
key is not stored, and its address must be the builder. --address, when set,
must be that same address. wallet.master_address is the trader and is not
required to match.

The call credits the builder's Papertrade exchange balance. It does not
withdraw. Gas is HYPE on HyperEVM. A zero builderFees balance is refused
before anything is signed. If a transaction from this address is already
pending, this command stops.

On exit 42, error.tx_hash is the transaction to watch. Do not claim again
until that transaction is mined or dropped.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.builder.claim"
		if err := paperWritePre(); err != nil {
			return fail(name, err)
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		var from common.Address
		var key *ecdsa.PrivateKey
		if flagDryRun {
			who, err := paperBuilderTarget()
			if err != nil {
				return fail(name, err)
			}
			from = who
		} else {
			k, addr, err := loadMasterKey(
				"builder fee claims are signed by the builder wallet",
				"set DELIVERATOR_MASTER_KEY to the builder address for this command only. It is not stored. The agent key cannot claim builder fees.",
			)
			if err != nil {
				return fail(name, err)
			}
			if strings.TrimSpace(ptAddress) != "" && !strings.EqualFold(ptAddress, addr.Hex()) {
				return fail(name, output.Validation("master_address", "--address does not match the wallet that DELIVERATOR_MASTER_KEY controls"))
			}
			key, from = k, addr
		}
		plan, err := paperClient().PlanClaim(ctx, from)
		if err != nil {
			return fail(name, paperChainFail(err))
		}
		preview := claimPreview(plan)
		if flagDryRun {
			preview["dry_run"] = true
			preview["note"] = "A live claim reads DELIVERATOR_MASTER_KEY, signs claimBuilderFees(), and pays HYPE gas. Fees land in the exchange balance. Deliverator does not withdraw them."
			emitWarn(name, preview, paperReadWarnings()...)
			return nil
		}
		if !flagYes {
			return fail(name, output.Validation("confirm", "claimBuilderFees moves accrued builder fees into the exchange balance — pass --yes").
				WithHint("run with --dry-run first. This spends HYPE on gas and does not withdraw."))
		}
		res, err := paperClient().ClaimBuilderFees(ctx, key)
		if err != nil {
			return fail(name, paperChainFail(err))
		}
		out := claimPreview(plan)
		out["tx_hash"] = res.Hash.Hex()
		out["explorer_url"] = "https://hyperevmscan.io/tx/" + res.Hash.Hex()
		out["status"] = "success"
		var warnings []string
		if res.HasEvent {
			out["amount_raw"] = res.Amount.String()
			out["amount"] = papertrade.FormatRaw18(res.Amount)
			out["paid_raw"] = res.Paid.String()
			out["paid"] = papertrade.FormatRaw18(res.Paid)
			queued := new(big.Int).Sub(res.Amount, res.Paid)
			if queued != nil && queued.Sign() >= 0 {
				out["queued_raw"] = queued.String()
				out["queued"] = papertrade.FormatRaw18(queued)
			}
			if res.Paid.Cmp(res.Amount) < 0 {
				warnings = append(warnings, "part of this claim joined the payout queue because the LP could not cover all of it")
			}
		} else {
			warnings = append(warnings, "the transaction succeeded but the receipt had no BuilderFeesClaimed log for this builder")
		}
		paperAudit(name, map[string]any{"builder": strings.ToLower(from.Hex()), "tx_hash": res.Hash.Hex(), "status": "success"})
		emitWarn(name, out, append(paperReadWarnings(), warnings...)...)
		return nil
	},
}

var ptBuilderApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Submit a Papertrade builder application (builder wallet signs; needs --yes)",
	Long: `Sign the same personal message the builders page signs and POST it to
/query/builders/applications. This does not approve the builder. The venue
reviews the application, and the contract owner schedules an on-chain
approval through the 24-hour timelock.

The signature is DELIVERATOR_MASTER_KEY for this command only. That key is
the address traders will name as feeAddress. It moves no funds.

If the POST outcome is unknown (exit 42), run papertrade builder and read
application_status before applying again.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.builder.apply"
		if err := paperWritePre(); err != nil {
			return fail(name, err)
		}
		app, err := papertrade.NormalizeApplication(ptAppName, ptWebsite, ptContactKind, ptContact, ptIntegration)
		if err != nil {
			var fe *papertrade.FieldError
			if errors.As(err, &fe) {
				return fail(name, output.Validation(fe.Field, err.Error()).
					WithHint("contact-kind is telegram, x, or email. website must be an https URL."))
			}
			return fail(name, output.Validation("application", err.Error()))
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		if flagDryRun {
			who, err := paperBuilderTarget()
			if err != nil {
				return fail(name, err)
			}
			now := time.Now().UnixMilli()
			emitWarn(name, map[string]any{
				"dry_run": true, "address": strings.ToLower(who.Hex()),
				"environment": papertrade.BuilderApplicationEnvironment,
				"app_name":    app.AppName, "website_url": app.WebsiteURL,
				"contact_kind": app.ContactKind, "contact": app.Contact,
				"integration": app.Integration,
				"message":     papertrade.ApplicationMessage(strings.ToLower(who.Hex()), papertrade.BuilderApplicationEnvironment, app, now),
				"note":        "A live apply re-reads the clock, signs this text with DELIVERATOR_MASTER_KEY, and POSTs it. The timestamp will be the moment of that signature. Approval is a later timelock action by the contract owner, not this command.",
			}, paperReadWarnings()...)
			return nil
		}
		if !flagYes {
			return fail(name, output.Validation("confirm", "apply submits a signed builder application — pass --yes").
				WithHint("run with --dry-run to read the message. It moves no funds and does not approve the builder."))
		}
		key, signer, err := loadMasterKey(
			"builder applications are signed by the builder wallet",
			"set DELIVERATOR_MASTER_KEY to the address traders will name. It is not stored.",
		)
		if err != nil {
			return fail(name, err)
		}
		if strings.TrimSpace(ptAddress) != "" && !strings.EqualFold(ptAddress, signer.Hex()) {
			return fail(name, output.Validation("master_address", "--address does not match the wallet that DELIVERATOR_MASTER_KEY controls"))
		}
		now := time.Now().UnixMilli()
		msg := papertrade.ApplicationMessage(strings.ToLower(signer.Hex()), papertrade.BuilderApplicationEnvironment, app, now)
		sig, err := papertrade.SignPersonal(key, msg)
		if err != nil {
			return fail(name, output.Unknown("sign", err.Error()))
		}
		rec, err := paperClient().SubmitApplication(ctx, signer, app, now, sig)
		if err != nil {
			return fail(name, paperApplicationFail(err))
		}
		paperAudit(name, map[string]any{
			"builder": strings.ToLower(signer.Hex()), "status": rec.Status, "app_name": app.AppName,
		})
		emitWarn(name, map[string]any{
			"address": rec.Address, "status": rec.Status,
			"submitted_at_ms": rec.SubmittedAtMs, "next_submission_at_ms": rec.NextSubmissionAtMs,
			"environment": papertrade.BuilderApplicationEnvironment, "app_name": app.AppName,
			"note": "The application is pending review. On-chain approval is a timelock action by the contract owner. Traders name this address with papertrade session register --builder.",
		}, append(paperReadWarnings(), "this signature only submitted an application; it approved nothing and moved no funds")...)
		return nil
	},
}

func paperBuilderConfigWarning(old, neu string) string {
	show := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "(none)"
		}
		return s
	}
	if strings.TrimSpace(neu) == "" {
		return fmt.Sprintf("papertrade.builder_address cleared: %s → (none) — new session registrations name no builder unless --builder is passed. Keys already registered keep their fee address until you register again. Confirm the account operator approved this.", show(old))
	}
	return fmt.Sprintf("papertrade.builder_address changed: %s → %s — new session registrations name this builder unless --builder overrides it. While it stays approved, winning closes signed by that key pay the builder fee (live rate, cap 1%% of adjusted PnL). Keys already registered keep their fee address until you register again. Deliverator does not block this; confirm the account operator approved it.", show(old), neu)
}

func paperBuilderTarget() (common.Address, error) {
	if s := strings.TrimSpace(ptAddress); s != "" {
		if !common.IsHexAddress(s) {
			return common.Address{}, output.Validation("address", "--address must be 0x and 40 hex characters")
		}
		return common.HexToAddress(s), nil
	}
	if Cfg != nil {
		if s := strings.TrimSpace(Cfg.Papertrade.BuilderAddress); s != "" {
			a, err := papertrade.ParseBuilderAddress(s)
			if err != nil || a == (common.Address{}) {
				return common.Address{}, output.Validation("builder", "papertrade.builder_address is not a builder address")
			}
			return a, nil
		}
	}
	return paperUser()
}

func paperBuilderView(ctx context.Context, who common.Address) (map[string]any, []string, error) {
	client := paperClient()
	out := map[string]any{
		"address":          strings.ToLower(who.Hex()),
		"fee_rate_cap":     papertrade.FormatFeeRate(mustRaw(papertrade.BuilderFeeRateCapRaw)),
		"fee_rate_cap_raw": papertrade.BuilderFeeRateCapRaw,
		"chain_ok":         false,
		"query_ok":         false,
	}
	var warnings []string
	reg, err := client.BuilderRegistryAddress(ctx)
	if err != nil {
		warnings = append(warnings, "builder registry read failed: "+err.Error())
	} else {
		out["chain_ok"] = true
		out["codes_live"] = true
		out["registry"] = strings.ToLower(reg.Hex())
		if !strings.EqualFold(reg.Hex(), papertrade.BuilderRegistryFallback) {
			warnings = append(warnings, "SessionKeyManager.builderRegistry() is "+strings.ToLower(reg.Hex())+", not the documented "+papertrade.BuilderRegistryFallback)
		}
		if rate, err := client.BuilderFeeRate(ctx, reg); err != nil {
			warnings = append(warnings, "builder fee rate read failed: "+err.Error())
		} else {
			out["fee_rate_raw"] = rate.String()
			out["fee_rate"] = papertrade.FormatFeeRate(rate)
		}
		if ok, err := client.BuilderApproved(ctx, reg, who); err != nil {
			warnings = append(warnings, "builder approval read failed: "+err.Error())
		} else {
			out["approved"] = ok
		}
		if fees, err := client.UnclaimedBuilderFees(ctx, who); err != nil {
			warnings = append(warnings, "unclaimed builder fees read failed: "+err.Error())
		} else {
			out["unclaimed_raw"] = fees.String()
			out["unclaimed"] = papertrade.FormatRaw18(fees)
		}
	}
	if board, err := client.BuilderLeaderboard(ctx); err != nil {
		warnings = append(warnings, "builder leaderboard read failed: "+err.Error())
	} else {
		out["query_ok"] = true
		out["as_of_ms"] = board.AsOfMs
		out["totals"] = map[string]any{
			"builders": board.Builders, "users": board.Users, "closes": board.Closes, "accrued_raw": board.AccruedRaw,
		}
		if board.Registry != "" {
			out["query_registry"] = board.Registry
			if _, ok := out["registry"]; !ok {
				out["registry"] = board.Registry
				out["codes_live"] = true
			}
		}
		if board.FeeRateRaw != "" {
			if _, ok := out["fee_rate_raw"]; !ok {
				out["fee_rate_raw"] = board.FeeRateRaw
				if n := mustRaw(board.FeeRateRaw); n != nil {
					out["fee_rate"] = papertrade.FormatFeeRate(n)
				}
			}
		}
		for _, row := range board.Rows {
			if strings.EqualFold(row.Address, who.Hex()) && row.Name != "" {
				out["name"] = row.Name
			}
		}
	}
	if stats, err := client.BuilderStats(ctx, who); err != nil {
		warnings = append(warnings, "builder stats read failed: "+err.Error())
	} else {
		out["query_ok"] = true
		out["approved_query"] = stats.Approved
		if _, ok := out["approved"]; !ok {
			out["approved"] = stats.Approved
		}
		if stats.UnclaimedRaw != "" {
			if _, ok := out["unclaimed_raw"]; !ok {
				out["unclaimed_raw"] = stats.UnclaimedRaw
				if n := mustRaw(stats.UnclaimedRaw); n != nil {
					out["unclaimed"] = papertrade.FormatRaw18(n)
				}
			}
		}
		out["routing"] = map[string]any{
			"users": stats.Users, "session_keys": stats.SessionKeys, "closes": stats.Closes,
			"notional_raw": stats.NotionalRaw, "as_of_ms": stats.AsOfMs,
		}
		if stats.HasEarnings {
			out["earnings"] = map[string]any{
				"accrued_raw": stats.AccruedRaw, "claimed_raw": stats.ClaimedRaw, "paid_raw": stats.PaidRaw,
			}
		}
	}
	if app, err := client.BuilderApplication(ctx, who); err != nil {
		warnings = append(warnings, "builder application read failed: "+err.Error())
	} else {
		out["query_ok"] = true
		out["application_known"] = app.StatusKnown
		if app.StatusKnown {
			if app.Status == "" {
				out["application_status"] = nil
			} else {
				out["application_status"] = app.Status
			}
		}
		if app.HasSubmitted {
			out["application_submitted_at_ms"] = app.SubmittedAtMs
		}
	}
	if out["chain_ok"] != true && out["query_ok"] != true {
		return nil, nil, output.Network("papertrade", "builder status could not be read from the chain or the exchange").Retry()
	}
	if _, ok := out["codes_live"]; !ok {
		out["codes_live"] = false
	}
	return out, warnings, nil
}

func claimPreview(plan *papertrade.ClaimPlan) map[string]any {
	return map[string]any{
		"action": "claimBuilderFees", "builder": strings.ToLower(plan.From.Hex()),
		"exchange": strings.ToLower(plan.To.Hex()), "chain_id": papertrade.ChainID,
		"data": "0x" + hex.EncodeToString(plan.Data), "nonce": plan.Nonce, "gas": plan.Gas,
		"max_fee_per_gas": plan.FeeCap.String(),
		"unclaimed_raw":   plan.Unclaimed.String(), "unclaimed": papertrade.FormatRaw18(plan.Unclaimed),
		"hype_balance": papertrade.FormatRaw18(plan.Balance),
	}
}

func paperChainFail(err error) *output.Error {
	var na *papertrade.NotApprovedError
	if errors.As(err, &na) {
		return output.Validation("builder_not_approved", na.Error()).
			WithHint("register with no --builder, or name an address the contract owner has approved. `papertrade builder --address` shows approval. There is no separate user approval transaction.")
	}
	var ce *papertrade.ChainError
	if !errors.As(err, &ce) {
		return output.Network("papertrade", err.Error()).Retry()
	}
	switch ce.Phase {
	case papertrade.ChainPending:
		return output.Timeout("papertrade_tx", ce.Message).WithTxHash(ce.Hash).
			WithHint("do not send claimBuilderFees again until this transaction is mined or dropped. Check error.tx_hash, then `papertrade builder`.")
	case papertrade.ChainReverted:
		return output.Exchange("claim_reverted", ce.Message).WithTxHash(ce.Hash).
			WithHint("the transaction was mined and reverted. Do not resend it unchanged.")
	case papertrade.ChainSimulated, papertrade.ChainRejected:
		return output.Exchange("papertrade_tx", ce.Message).WithHint("nothing was broadcast")
	case papertrade.ChainRefused:
		return output.Validation("papertrade_tx", ce.Message)
	default:
		return output.Network("papertrade", ce.Message).Retry()
	}
}

func paperApplicationFail(err error) *output.Error {
	var te *papertrade.TransportError
	if errors.As(err, &te) {
		if te.Sent {
			return output.Timeout("application_unconfirmed", "the application POST was not confirmed. It may have been saved.").
				WithHint("run `deliverator papertrade builder` and read application_status before applying again. Do not assume it failed.")
		}
		return output.Network("papertrade", te.Error()).Retry()
	}
	var ae *papertrade.ApplicationError
	if !errors.As(err, &ae) {
		return output.Network("papertrade", err.Error()).Retry()
	}
	if ae.Unknown {
		return output.Timeout("application_unconfirmed", ae.Error()).
			WithHint("run `deliverator papertrade builder` and read application_status before applying again.")
	}
	switch ae.Code {
	case "rate_limited":
		return output.RateLimit("rate_limited", ae.Error()).WithRetryAfter(2000)
	case "application_cooldown":
		msg := "builder application is in its cooldown"
		if ae.Message != "" {
			msg = ae.Message
		}
		e := output.RateLimit("application_cooldown", msg)
		if ae.HasNext {
			wait := ae.NextAtMs - time.Now().UnixMilli()
			if wait < 0 {
				wait = 0
			}
			e = e.WithRetryAfter(wait)
		}
		return e
	case "application_approved":
		return output.Validation("application_approved", "this address is already an approved builder").
			WithHint("traders name it with `papertrade session register --builder`. A new application is not required.")
	case "signature_expired":
		return output.Exchange("signature_expired", "the application signature expired").Retry().
			WithHint("sign again. The timestamp is the moment of the signature.")
	case "signature_mismatch":
		return output.Exchange("signature_mismatch", "the venue rejected the application signature")
	case "application_invalid":
		code := "application"
		msg := "the venue rejected the application"
		if ae.Field != "" {
			code = ae.Field
			msg = "the venue rejected application field " + ae.Field
		}
		return output.Validation(code, msg)
	default:
		return output.Exchange("application_rejected", ae.Error()).
			WithHint("the application was not saved. Fix the fields and sign again.")
	}
}

func paperAnnotateSession(ctx context.Context, user common.Address, view papertrade.AccountView, out map[string]any) []string {
	if view.SessionKey == "" {
		return nil
	}
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	binding, err := paperClient().SessionBinding(quick, user, common.HexToAddress(view.SessionKey))
	if err != nil {
		return []string{"could not read this session key's builder from SessionKeyManager"}
	}
	out["builder_fee_address"] = strings.ToLower(binding.FeeAddress.Hex())
	out["chain_expiry_unix"] = binding.ExpiryUnix
	var warnings []string
	listed := false
	for _, k := range view.SessionKeys {
		if strings.EqualFold(k.Address, view.SessionKey) {
			listed = true
			break
		}
	}
	if listed && binding.ExpiryUnix == 0 && binding.FeeAddress == (common.Address{}) {
		warnings = append(warnings, "SessionKeyManager.sessionKeys returned an empty record for this user and agent key, so the builder on that key could not be confirmed")
	}
	if msg := papertrade.BoundFeeWarning(binding.FeeAddress); msg != "" {
		warnings = append(warnings, msg)
	}
	return warnings
}

func paperBoundBuilderWarning(ctx context.Context, user common.Address) string {
	ag, err := paperAgent()
	if err != nil {
		return ""
	}
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	binding, err := paperClient().SessionBinding(quick, user, common.HexToAddress(ag.Address))
	if err != nil {
		return ""
	}
	return papertrade.BoundFeeWarning(binding.FeeAddress)
}

func mustRaw(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil
	}
	return n
}
