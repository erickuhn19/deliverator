package cmd

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/papertrade"
	"github.com/erickuhn19/deliverator/internal/state"
	"github.com/erickuhn19/deliverator/internal/wallet"
)

// Papertrade is a separate venue from Hyperliquid. These commands do not go
// through ClientAPI, and they do not change buy/sell/close. The session key is
// the existing agent key. Registration is signed by DELIVERATOR_MASTER_KEY and
// that secret is never stored.

var (
	ptAddress   string
	ptCoin      string
	ptSide      string
	ptMargin    string
	ptLeverage  int
	ptUseDebt   bool
	ptPositions []string
	ptIntent    string
	ptDays      int
)

var papertradeCmd = &cobra.Command{
	Use:   "papertrade",
	Short: "Trade Papertrade isolated perps on HyperEVM (real funds, relayer intents)",
	Long: `Papertrade commands talk to the live HyperEVM venue (chain 999), not to
Hyperliquid testnet. Opens fill at the Hyperliquid mid with no order book.
The agent key signs opens, closes, and cancels. It cannot withdraw.
Registration, which binds that key, is signed by the master wallet via
DELIVERATOR_MASTER_KEY and is not stored.

An exit 42 (or a post-sign exit 40) carries error.intent_ids. Check
'papertrade intent status' and resend that same intent. Do not open again.`,
	RunE: requireSubcommand,
}

func init() {
	rootCmd.AddCommand(papertradeCmd)
	papertradeCmd.PersistentFlags().StringVar(&ptAddress, "address", "", "user wallet (default: wallet.master_address)")

	papertradeCmd.AddCommand(ptMarketsCmd, ptSummaryCmd, ptAccountCmd, ptDepositCmd,
		ptOpenCmd, ptCloseCmd, ptCancelCmd, ptResendCmd, ptIntentCmd, ptSessionCmd)
	ptSessionCmd.AddCommand(ptRegisterCmd)

	ptOpenCmd.Flags().StringVar(&ptCoin, "coin", "", "market symbol, for example BTC")
	ptOpenCmd.Flags().StringVar(&ptSide, "side", "", "long or short")
	ptOpenCmd.Flags().StringVar(&ptMargin, "margin", "", "margin in USDC (18-decimal max), not coin size")
	ptOpenCmd.Flags().IntVar(&ptLeverage, "leverage", 0, "isolated leverage, 1 to the market max")
	ptOpenCmd.Flags().BoolVar(&ptUseDebt, "use-debt", false, "spend queued winnings instead of the balance")
	ptCloseCmd.Flags().StringArrayVar(&ptPositions, "position", nil, "position id (repeat for up to 12)")
	ptCancelCmd.Flags().StringVar(&ptIntent, "intent", "", "intent id to cancel")
	ptResendCmd.Flags().StringVar(&ptIntent, "intent", "", "stored intent id to resend")
	ptIntentStatusCmd.Flags().StringVar(&ptIntent, "intent", "", "intent id to look up")
	ptRegisterCmd.Flags().IntVar(&ptDays, "days", papertrade.DefaultSessionDays, "session length in days (1-30)")
}

var ptMarketsCmd = &cobra.Command{
	Use:   "markets",
	Short: "Papertrade markets, minimums, and which intents the relayer is accepting",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		markets, err := paperClient().Markets(ctx)
		if err != nil {
			return fail("papertrade.markets", output.Network("papertrade", err.Error()).Retry())
		}
		emitWarn("papertrade.markets", markets, paperReadWarnings()...)
		return nil
	},
}

var ptSummaryCmd = &cobra.Command{
	Use:   "summary",
	Short: "Protocol summary (TVL, open interest, PAPER supply) as published by the venue",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		sum, err := paperClient().Summary(ctx)
		if err != nil {
			return fail("papertrade.summary", output.Network("papertrade", err.Error()).Retry())
		}
		emitWarn("papertrade.summary", sum, paperReadWarnings()...)
		return nil
	},
}

var ptAccountCmd = &cobra.Command{
	Use:   "account",
	Short: "Balance, session keys, and open Papertrade positions",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		user, err := paperUser()
		if err != nil {
			return fail("papertrade.account", err)
		}
		view, warnings, err := paperAccountView(ctx, user, true)
		if err != nil {
			return fail("papertrade.account", err)
		}
		emitWarn("papertrade.account", view, append(paperReadWarnings(), warnings...)...)
		return nil
	},
}

var ptDepositCmd = &cobra.Command{
	Use:   "deposit-address",
	Short: "HyperCore USDC deposit proxy for this wallet (does not send funds)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		user, err := paperUser()
		if err != nil {
			return fail("papertrade.deposit-address", err)
		}
		proxy, err := paperClient().DepositProxy(ctx, user)
		if err != nil {
			return fail("papertrade.deposit-address", output.Network("papertrade", err.Error()).Retry())
		}
		emitWarn("papertrade.deposit-address", map[string]any{
			"user":            strings.ToLower(user.Hex()),
			"deposit_address": strings.ToLower(proxy.Hex()),
			"collateral":      papertrade.Collateral,
			"note":            "Send HyperCore USDC here from the master wallet. The first deposit of a new proxy costs 1 USDC to activate. Deliverator does not send it.",
		}, paperReadWarnings()...)
		return nil
	},
}

var ptOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open an isolated Papertrade position (session key)",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.open"
		if err := paperWritePre(); err != nil {
			return fail(name, err)
		}
		long, err := paperSide(ptSide)
		if err != nil {
			return fail(name, err)
		}
		if strings.TrimSpace(ptCoin) == "" {
			return fail(name, output.Validation("coin", "pass --coin"))
		}
		margin, err := paperMargin(ptMargin)
		if err != nil {
			return fail(name, err)
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
		plan, err := papertrade.PlanOpen(markets, account, ptCoin, long, margin, ptLeverage, ptUseDebt)
		if err != nil {
			return fail(name, err)
		}
		agent, agentErr := paperAgent()
		ready := false
		session := ""
		if agentErr == nil {
			session = strings.ToLower(agent.Address)
			ready = papertrade.SessionReady(account, common.HexToAddress(agent.Address), time.Now().Unix())
		}
		if !flagDryRun {
			if agentErr != nil {
				return fail(name, agentErr)
			}
			if !ready {
				return fail(name, output.Auth("session_key",
					"the agent key is not a live Papertrade session for "+strings.ToLower(user.Hex())).
					WithHint("the master wallet registers it once: set DELIVERATOR_MASTER_KEY and run `deliverator papertrade session register`"))
			}
		}
		if err := core.PaperOpenGate(Cfg, plan.Instrument.Symbol, plan.Leverage, plan.NotionalUSD, plan.CoinNotionalUSD); err != nil {
			if oe, ok := err.(*output.Error); ok && oe.Code == "limit_only" {
				oe.Hint = "Papertrade opens fill at the Hyperliquid mid. automation.limit_only blocks them."
			}
			return fail(name, err)
		}
		if err := paperPortfolio(ctx, plan.Instrument.Symbol, plan.IsLong, plan.NotionalUSD, len(account.OpenPositions)); err != nil {
			return fail(name, err)
		}
		warnings := paperReadWarnings()
		if !ready {
			warnings = append(warnings, "agent session is not registered; a live open will be rejected until `papertrade session register`")
		}
		if flagDryRun {
			emitWarn(name, map[string]any{
				"dry_run": true, "action": "openPosition", "coin": plan.Instrument.Symbol,
				"instrument_id": plan.Instrument.ID, "is_long": plan.IsLong,
				"margin": plan.Margin, "margin_raw": plan.MarginRaw, "leverage": plan.Leverage,
				"notional": plan.Notional, "use_debt": plan.UseDebt,
				"user": strings.ToLower(user.Hex()), "session_key": session, "session_ready": ready,
			}, warnings...)
			return nil
		}
		nonce, err := paperNonces().Take(papertrade.NonceKey(papertrade.Exchange, "session", user.Hex()))
		if err != nil {
			return fail(name, output.Network("nonce", err.Error()).Retry())
		}
		n, ok := new(big.Int).SetString(nonce, 10)
		if !ok {
			return fail(name, output.Unknown("nonce", "allocated nonce is not decimal"))
		}
		signed, err := papertrade.SignOpen(agent.Key, papertrade.OpenMessage{
			User: user, InstrumentID: uint32(plan.Instrument.ID), IsLong: plan.IsLong,
			SizeRaw: margin, Leverage: uint32(plan.Leverage), UseDebt: plan.UseDebt,
			Nonce: n, Deadline: paperDeadline(time.Now()),
		})
		if err != nil {
			return fail(name, output.Unknown("sign", err.Error()))
		}
		rec, err := paperSend(ctx, name, signed, "/intents", user, signed.Wire)
		if err != nil {
			return err
		}
		paperAudit(name, map[string]any{"intent_id": signed.IntentID, "coin": plan.Instrument.Symbol, "margin_raw": plan.MarginRaw, "state": rec.State})
		emitWarn(name, map[string]any{
			"action": "openPosition", "intent_id": signed.IntentID, "state": rec.State,
			"coin": plan.Instrument.Symbol, "instrument_id": plan.Instrument.ID, "is_long": plan.IsLong,
			"margin": plan.Margin, "margin_raw": plan.MarginRaw, "leverage": plan.Leverage,
			"notional": plan.Notional, "use_debt": plan.UseDebt, "nonce": nonce,
			"user": strings.ToLower(user.Hex()), "session_key": session, "queue_revision": rec.QueueRevision,
		}, warnings...)
		return nil
	},
}

var ptCloseCmd = &cobra.Command{
	Use:   "close",
	Short: "Close 1 to 12 Papertrade positions (session key, full close only)",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.close"
		if err := paperWritePre(); err != nil {
			return fail(name, err)
		}
		ids, err := paperPositionIDs(ptPositions)
		if err != nil {
			return fail(name, err)
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
		if !markets.ActionEnabled("closePositions") {
			return fail(name, output.Exchange("intake_action_paused", "Papertrade is not accepting closes right now").Retry())
		}
		var missing []string
		wireIDs := make([]string, len(ids))
		for i, id := range ids {
			wireIDs[i] = id.String()
			if !account.HasPosition(id.String()) {
				missing = append(missing, id.String())
			}
		}
		if len(missing) > 0 {
			return fail(name, output.Validation("position", "not an open position: "+strings.Join(missing, ", ")).
				WithHint("run `deliverator papertrade account` and pass those position ids"))
		}
		if flagDryRun {
			emitWarn(name, map[string]any{
				"dry_run": true, "action": "closePositions", "position_ids": wireIDs,
				"user": strings.ToLower(user.Hex()),
			})
			return nil
		}
		agent, err := paperAgent()
		if err != nil {
			return fail(name, err)
		}
		if !papertrade.SessionReady(account, common.HexToAddress(agent.Address), time.Now().Unix()) {
			return fail(name, output.Auth("session_key", "the agent key is not a live Papertrade session").
				WithHint("run `deliverator papertrade session register` from the master wallet"))
		}
		nonce, err := paperNonces().Take(papertrade.NonceKey(papertrade.Exchange, "session", user.Hex()))
		if err != nil {
			return fail(name, output.Network("nonce", err.Error()).Retry())
		}
		n, ok := new(big.Int).SetString(nonce, 10)
		if !ok {
			return fail(name, output.Unknown("nonce", "allocated nonce is not decimal"))
		}
		signed, err := papertrade.SignClose(agent.Key, papertrade.CloseMessage{
			User: user, PositionIDs: ids, Nonce: n, Deadline: paperDeadline(time.Now()),
		})
		if err != nil {
			return fail(name, output.Unknown("sign", err.Error()))
		}
		rec, err := paperSend(ctx, name, signed, "/intents", user, signed.Wire)
		if err != nil {
			return err
		}
		paperAudit(name, map[string]any{"intent_id": signed.IntentID, "position_ids": wireIDs, "state": rec.State})
		emit(name, map[string]any{
			"action": "closePositions", "intent_id": signed.IntentID, "state": rec.State,
			"position_ids": wireIDs, "nonce": nonce, "user": strings.ToLower(user.Hex()),
			"queue_revision": rec.QueueRevision,
		})
		return nil
	},
}

var ptCancelCmd = &cobra.Command{
	Use:   "cancel",
	Short: "Cancel one queued Papertrade intent (session key)",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.cancel"
		if err := paperWritePre(); err != nil {
			return fail(name, err)
		}
		id, err := paperIntentHash(ptIntent)
		if err != nil {
			return fail(name, err)
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		user, err := paperUser()
		if err != nil {
			return fail(name, err)
		}
		if flagDryRun {
			emit(name, map[string]any{
				"dry_run": true, "action": "cancelIntent", "intent_id": strings.ToLower(id.Hex()),
				"user": strings.ToLower(user.Hex()),
			})
			return nil
		}
		account, err := paperClient().Account(ctx, user)
		if err != nil {
			return fail(name, output.Network("papertrade", err.Error()).Retry())
		}
		agent, err := paperAgent()
		if err != nil {
			return fail(name, err)
		}
		if !papertrade.SessionReady(account, common.HexToAddress(agent.Address), time.Now().Unix()) {
			return fail(name, output.Auth("session_key", "the agent key is not a live Papertrade session").
				WithHint("run `deliverator papertrade session register` from the master wallet"))
		}
		signed, err := papertrade.SignCancel(agent.Key, papertrade.CancelMessage{
			User: user, IntentID: id, Deadline: paperDeadline(time.Now()),
		})
		if err != nil {
			return fail(name, output.Unknown("sign", err.Error()))
		}
		path := "/intents/" + strings.ToLower(id.Hex()) + "/cancel"
		rec, err := paperSend(ctx, name, signed, path, user, signed.Wire)
		if err != nil {
			return err
		}
		paperAudit(name, map[string]any{"intent_id": signed.IntentID, "state": rec.State})
		emit(name, map[string]any{
			"action": "cancelIntent", "intent_id": signed.IntentID, "state": rec.State,
			"user": strings.ToLower(user.Hex()),
		})
		return nil
	},
}

var ptResendCmd = &cobra.Command{
	Use:   "resend",
	Short: "POST a previously signed Papertrade intent again (same nonce, same signature)",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.resend"
		if err := paperRequireMainnet(); err != nil {
			return fail(name, err)
		}
		id, err := paperIntentHash(ptIntent)
		if err != nil {
			return fail(name, err)
		}
		stored, err := paperIntents().Find(strings.ToLower(id.Hex()))
		if err != nil {
			return fail(name, output.Validation("intent", "no stored intent "+strings.ToLower(id.Hex())).
				WithHint("only an intent this process signed can be resent"))
		}
		if stored.Deadline != "" {
			if dl, ok := new(big.Int).SetString(stored.Deadline, 10); ok && time.Now().Unix() > dl.Int64() {
				return fail(name, output.Validation("deadline_expired", "the stored intent's deadline has passed").
					WithHint("run `papertrade intent status` — if it is absent, the trade did not land and a new open is a new position"))
			}
		}
		if flagDryRun {
			emit(name, map[string]any{"dry_run": true, "intent_id": stored.IntentID, "action": stored.Action, "path": stored.Path})
			return nil
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		rec, err := paperClient().PostIntent(ctx, stored.Path, stored.Body)
		if ferr := paperClassify(name, stored.IntentID, rec, err); ferr != nil {
			return ferr
		}
		stored.State = rec.State
		_ = paperIntents().Put(stored)
		emit(name, map[string]any{"intent_id": stored.IntentID, "action": stored.Action, "state": rec.State, "queue_revision": rec.QueueRevision})
		return nil
	},
}

var ptIntentCmd = &cobra.Command{
	Use:   "intent",
	Short: "Inspect a stored or on-account Papertrade intent",
	RunE:  requireSubcommand,
}

var ptIntentStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Whether an intent is stored locally and whether an open position carries it",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.intent.status"
		id, err := paperIntentHash(ptIntent)
		if err != nil {
			return fail(name, err)
		}
		hexID := strings.ToLower(id.Hex())
		out := map[string]any{"intent_id": hexID, "stored": false}
		if stored, err := paperIntents().Find(hexID); err == nil {
			out["stored"] = true
			out["action"] = stored.Action
			out["deadline"] = stored.Deadline
			out["state"] = stored.State
			if stored.Deadline != "" {
				if dl, ok := new(big.Int).SetString(stored.Deadline, 10); ok {
					out["deadline_expired"] = time.Now().Unix() > dl.Int64()
				}
			}
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		user, uerr := paperUser()
		if uerr == nil {
			account, err := paperClient().Account(ctx, user)
			if err != nil {
				return fail(name, output.Network("papertrade", err.Error()).Retry())
			}
			out["seen_on_open_position"] = account.HasIntent(hexID)
		}
		emitWarn(name, out, paperReadWarnings()...)
		return nil
	},
}

var ptSessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Whether the agent key is a live Papertrade session",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.session"
		ctx, cancel := cmdCtx()
		defer cancel()
		user, err := paperUser()
		if err != nil {
			return fail(name, err)
		}
		view, warnings, err := paperAccountView(ctx, user, true)
		if err != nil {
			return fail(name, err)
		}
		emitWarn(name, map[string]any{
			"user": view.Address, "session_key": view.SessionKey, "session_ready": view.SessionReady,
			"session_keys": view.SessionKeys, "indexed_through_timestamp": view.IndexedThroughTimestamp,
		}, append(paperReadWarnings(), warnings...)...)
		return nil
	},
}

var ptRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register the agent key as a Papertrade session (master wallet signs once)",
	RunE: func(cmd *cobra.Command, args []string) error {
		const name = "papertrade.session.register"
		if err := paperWritePre(); err != nil {
			return fail(name, err)
		}
		days, err := sessionDays()
		if err != nil {
			return fail(name, err)
		}
		agent, err := paperAgent()
		if err != nil {
			return fail(name, err)
		}
		now := time.Now()
		expiry := big.NewInt(now.Unix() + int64(days)*86400)
		preview := map[string]any{
			"action": "registerSessionKey", "session_key": strings.ToLower(agent.Address),
			"expiry_unix": expiry.String(), "days": days, "fee_address": papertrade.FeeAddress,
		}
		if flagDryRun {
			if addr, err := paperUser(); err == nil {
				preview["user"] = strings.ToLower(addr.Hex())
			}
			preview["dry_run"] = true
			preview["note"] = "A live register reads DELIVERATOR_MASTER_KEY, signs once, and does not store that key."
			emit(name, preview)
			return nil
		}
		master, user, err := paperMaster()
		if err != nil {
			return fail(name, err)
		}
		if err := paperMasterMatches(user); err != nil {
			return fail(name, err)
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		markets, err := paperClient().Markets(ctx)
		if err != nil {
			return fail(name, output.Network("papertrade", err.Error()).Retry())
		}
		if !markets.ActionEnabled("registerSessionKey") {
			return fail(name, output.Exchange("intake_action_paused", "Papertrade is not accepting session registration right now").Retry())
		}
		nonce, err := paperNonces().Take(papertrade.NonceKey(papertrade.Exchange, "session", user.Hex()))
		if err != nil {
			return fail(name, output.Network("nonce", err.Error()).Retry())
		}
		n, ok := new(big.Int).SetString(nonce, 10)
		if !ok {
			return fail(name, output.Unknown("nonce", "allocated nonce is not decimal"))
		}
		signed, err := papertrade.SignRegister(master, papertrade.RegisterMessage{
			User: user, Key: common.HexToAddress(agent.Address), Expiry: expiry,
			FeeAddress: common.HexToAddress(papertrade.FeeAddress),
			Nonce:      n, Deadline: paperDeadline(now),
		})
		if err != nil {
			return fail(name, output.Unknown("sign", err.Error()))
		}
		if !strings.EqualFold(signed.Signer, user.Hex()) {
			return fail(name, output.Unknown("sign", "registration recovered a different signer than the master key"))
		}
		rec, err := paperSend(ctx, name, signed, "/intents", user, signed.Wire)
		if err != nil {
			return err
		}
		paperAudit(name, map[string]any{"intent_id": signed.IntentID, "session_key": strings.ToLower(agent.Address), "state": rec.State})
		emit(name, map[string]any{
			"action": "registerSessionKey", "intent_id": signed.IntentID, "state": rec.State,
			"user": strings.ToLower(user.Hex()), "session_key": strings.ToLower(agent.Address),
			"expiry_unix": expiry.String(), "nonce": nonce, "queue_revision": rec.QueueRevision,
		})
		return nil
	},
}

func init() {
	ptIntentCmd.AddCommand(ptIntentStatusCmd)
}

func paperClient() *papertrade.Client { return papertrade.NewClient(flagTimeout) }

func paperNonces() *papertrade.NonceStore {
	return papertrade.NewNonceStore(filepath.Join(config.Dir(), "papertrade"))
}

func paperIntents() *papertrade.IntentStore {
	return papertrade.NewIntentStore(filepath.Join(config.Dir(), "papertrade", "intents"))
}

func paperWritePre() error {
	if err := paperRequireMainnet(); err != nil {
		return err
	}
	return core.GlobalHaltError()
}

func paperRequireMainnet() error {
	if Cfg == nil || Cfg.Network != config.NetworkMainnet {
		return output.Validation("papertrade_mainnet",
			"Papertrade writes target production HyperEVM (chain 999) and need network = mainnet").
			WithHint("set network to mainnet only when you intend to trade the live Papertrade venue")
	}
	return nil
}

func paperReadWarnings() []string {
	if Cfg != nil && Cfg.Network != config.NetworkMainnet {
		return []string{"Papertrade data is production HyperEVM (chain 999). The Hyperliquid network flag does not retarget it."}
	}
	return nil
}

func paperUser() (common.Address, error) {
	s := strings.TrimSpace(ptAddress)
	if s == "" && Cfg != nil {
		s = strings.TrimSpace(Cfg.Wallet.MasterAddress)
	}
	if !common.IsHexAddress(s) {
		return common.Address{}, output.Validation("master_address",
			"pass --address or set wallet.master_address to the Papertrade user wallet")
	}
	return common.HexToAddress(s), nil
}

func paperAgent() (*wallet.Agent, error) {
	ag, err := wallet.Load(accountOrDefault())
	if err != nil {
		return nil, output.Auth("agent_key", "no agent key for this account").
			WithHint("run `deliverator onboard` or set DELIVERATOR_AGENT_KEY. That key is the Papertrade session key and cannot withdraw.")
	}
	return ag, nil
}

func paperMaster() (*ecdsa.PrivateKey, common.Address, error) {
	raw := strings.TrimSpace(os.Getenv("DELIVERATOR_MASTER_KEY"))
	if raw == "" {
		return nil, common.Address{}, output.Auth("master_key",
			"session registration is signed by the master wallet").
			WithHint("set DELIVERATOR_MASTER_KEY for this command only. It is not written to the keychain. The agent key cannot register itself.")
	}
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(raw, "0x"))
	if err != nil {
		return nil, common.Address{}, output.Auth("master_key", "DELIVERATOR_MASTER_KEY is not a 32-byte hex private key")
	}
	return pk, crypto.PubkeyToAddress(pk.PublicKey), nil
}

func paperMasterMatches(master common.Address) error {
	if strings.TrimSpace(ptAddress) != "" && !strings.EqualFold(ptAddress, master.Hex()) {
		return output.Validation("master_address", "--address does not match the wallet that DELIVERATOR_MASTER_KEY controls")
	}
	if Cfg != nil && strings.TrimSpace(Cfg.Wallet.MasterAddress) != "" && !strings.EqualFold(Cfg.Wallet.MasterAddress, master.Hex()) {
		return output.Validation("master_address", "wallet.master_address does not match the wallet that DELIVERATOR_MASTER_KEY controls")
	}
	return nil
}

func paperSide(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "long", "buy":
		return true, nil
	case "short", "sell":
		return false, nil
	default:
		return false, output.Validation("side", "pass --side long or --side short")
	}
}

func paperMargin(s string) (*big.Int, error) {
	n, err := parsePaperMargin(s)
	if err != nil {
		return nil, output.Validation("margin", err.Error()).WithHint("example: --margin 1000")
	}
	return n, nil
}

func paperPositionIDs(ids []string) ([]*big.Int, error) {
	if len(ids) < 1 || len(ids) > 12 {
		return nil, output.Validation("position", "pass 1 to 12 --position values")
	}
	seen := map[string]bool{}
	out := make([]*big.Int, 0, len(ids))
	for _, raw := range ids {
		raw = strings.TrimSpace(raw)
		for _, r := range raw {
			if r < '0' || r > '9' {
				return nil, output.Validation("position", "position id must be a decimal integer")
			}
		}
		n, ok := new(big.Int).SetString(raw, 10)
		if !ok {
			return nil, output.Validation("position", "position id must be a decimal integer")
		}
		if seen[n.String()] {
			return nil, output.Validation("position", "duplicate position id "+n.String())
		}
		seen[n.String()] = true
		out = append(out, n)
	}
	return out, nil
}

func paperIntentHash(s string) (common.Hash, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return common.Hash{}, output.Validation("intent", "pass --intent 0x…")
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		s = "0x" + s
	}
	raw := s[2:]
	if len(raw) != 64 {
		return common.Hash{}, output.Validation("intent", "intent id must be 32 bytes of hex")
	}
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return common.Hash{}, output.Validation("intent", "intent id must be 32 bytes of hex")
		}
	}
	return common.HexToHash(s), nil
}

func sessionDays() (int, error) {
	d := ptDays
	if d == 0 {
		d = papertrade.DefaultSessionDays
	}
	if d < 1 || d > papertrade.DefaultSessionDays {
		return 0, output.Validation("expiry", "session length must be from 1 to 30 days")
	}
	return d, nil
}

func paperDeadline(now time.Time) *big.Int {
	return big.NewInt(now.Unix() + papertrade.IntentTTLSeconds)
}

func paperReadBook(ctx context.Context, user common.Address) (papertrade.Markets, papertrade.Account, error) {
	markets, err := paperClient().Markets(ctx)
	if err != nil {
		return papertrade.Markets{}, papertrade.Account{}, output.Network("papertrade", err.Error()).Retry()
	}
	account, err := paperClient().Account(ctx, user)
	if err != nil {
		return papertrade.Markets{}, papertrade.Account{}, output.Network("papertrade", err.Error()).Retry()
	}
	return markets, account, nil
}

func paperAccountView(ctx context.Context, user common.Address, loadKey bool) (papertrade.AccountView, []string, error) {
	account, err := paperClient().Account(ctx, user)
	if err != nil {
		return papertrade.AccountView{}, nil, output.Network("papertrade", err.Error()).Retry()
	}
	view := papertrade.AccountView{Account: account}
	var warnings []string
	if loadKey {
		if ag, err := paperAgent(); err == nil {
			view.SessionKey = strings.ToLower(ag.Address)
			ready := papertrade.SessionReady(account, common.HexToAddress(ag.Address), time.Now().Unix())
			view.SessionReady = &ready
		} else {
			warnings = append(warnings, "no agent key loaded, so session_ready was not evaluated")
		}
	}
	return view, warnings, nil
}

func paperPortfolio(ctx context.Context, coin string, isLong bool, notional float64, paperOpen int) error {
	if !core.PortfolioGatesConfigured(Cfg) {
		return nil
	}
	api, err := newClient(ctx)
	if err != nil {
		if oe, ok := err.(*output.Error); ok {
			return oe.WithHint("portfolio risk gates are on, so a Papertrade open fails closed when the Hyperliquid book cannot be read")
		}
		return output.Network("portfolio_state", err.Error()).Retry()
	}
	hc, ok := api.(*core.Client)
	if !ok {
		return output.Network("portfolio_state", "portfolio risk gates need the Hyperliquid client").Retry()
	}
	signed := notional
	if !isLong {
		signed = -notional
	}
	return hc.GatePaperExposure(ctx, coin, signed, paperOpen)
}

// paperSend stores the signed body, posts it, and turns a transport failure
// into exit 40 or 42 with the intent id attached. The returned error is already
// a rendered *output.CmdError.
func paperSend(ctx context.Context, cmdName string, signed *papertrade.Signed, path string, user common.Address, wire any) (*papertrade.Receipt, error) {
	var body []byte
	var err error
	if signed.Action == "cancelIntent" {
		body, err = json.Marshal(wire)
	} else {
		body, err = json.Marshal(struct {
			Action string `json:"action"`
			Signed any    `json:"signed"`
		}{Action: signed.Action, Signed: wire})
	}
	if err != nil {
		return nil, fail(cmdName, output.Unknown("sign", err.Error()))
	}
	deadline := wireDeadline(wire)
	stored := papertrade.StoredIntent{
		IntentID: signed.IntentID, Action: signed.Action, Path: path, Body: body,
		Deadline: deadline, User: strings.ToLower(user.Hex()), CreatedMs: time.Now().UnixMilli(),
	}
	if err := paperIntents().Put(stored); err != nil {
		return nil, fail(cmdName, output.Network("intent_store", "signed intent was not sent because it could not be stored: "+err.Error()).
			WithIntentIDs(signed.IntentID).
			WithHint("fix the state directory and submit a new intent; this nonce was consumed"))
	}
	rec, err := paperClient().PostIntent(ctx, path, body)
	if ferr := paperClassify(cmdName, signed.IntentID, rec, err); ferr != nil {
		return nil, ferr
	}
	stored.State = rec.State
	_ = paperIntents().Put(stored)
	return rec, nil
}

func paperClassify(cmdName, intentID string, rec *papertrade.Receipt, err error) error {
	var te *papertrade.TransportError
	if errors.As(err, &te) {
		if te.Sent {
			return fail(cmdName, output.Timeout("intent_unconfirmed",
				"the relayer did not confirm the intent. It may have landed.").
				WithIntentIDs(intentID).
				WithHint("run `deliverator papertrade intent status --intent "+intentID+"`. If it is absent, `papertrade resend --intent "+intentID+"`. Do not open a second position."))
		}
		return fail(cmdName, output.Network("papertrade", te.Error()).Retry().
			WithIntentIDs(intentID).
			WithHint("the request did not leave this host. Resend the stored intent: `deliverator papertrade resend --intent "+intentID+"`"))
	}
	if err != nil {
		return fail(cmdName, output.Network("papertrade", err.Error()).Retry().WithIntentIDs(intentID))
	}
	if rec == nil {
		return fail(cmdName, output.Timeout("intent_unconfirmed", "empty relayer response").
			WithIntentIDs(intentID).
			WithHint("run `deliverator papertrade intent status --intent "+intentID+"` before opening again"))
	}
	if rec.Status == 429 || rec.Code == "rate_limited" || rec.Error == "rate_limited" {
		return fail(cmdName, output.RateLimit("rate_limited", paperMessage(rec)).WithRetryAfter(2000).WithIntentIDs(intentID))
	}
	if code := paperCode(rec); code != "" || (rec.OK != nil && !*rec.OK) {
		if code == "" {
			code = "rejected"
		}
		return fail(cmdName, output.Exchange(code, paperMessage(rec)).WithIntentIDs(intentID).
			WithHint("the relayer rejected this intent. Do not resend it unchanged."))
	}
	if rec.IntentID != "" && !strings.EqualFold(rec.IntentID, intentID) && cmdName != "papertrade.cancel" && cmdName != "papertrade.resend" {
		return fail(cmdName, output.Timeout("intent_unconfirmed", "the relayer returned a different intent id").
			WithIntentIDs(intentID))
	}
	switch rec.State {
	case "queued", "prepared", "confirmed":
		return nil
	case "failed", "reverted", "expired", "cancelled":
		return fail(cmdName, output.Exchange(rec.State, "intent "+rec.State).WithIntentIDs(intentID))
	default:
		return fail(cmdName, output.Timeout("intent_unconfirmed", "relayer state "+fmt.Sprintf("%q", rec.State)+" is not a confirmation").
			WithIntentIDs(intentID).
			WithHint("run `deliverator papertrade intent status --intent "+intentID+"` before opening again"))
	}
}

func paperCode(rec *papertrade.Receipt) string {
	if rec.Code != "" {
		return rec.Code
	}
	return rec.Error
}

func paperMessage(rec *papertrade.Receipt) string {
	if rec.Message != "" {
		return rec.Message
	}
	if rec.Reason != "" {
		return rec.Reason
	}
	if paperCode(rec) != "" {
		return paperCode(rec)
	}
	return "relayer rejected the intent"
}

func wireDeadline(wire any) string {
	b, err := json.Marshal(wire)
	if err != nil {
		return ""
	}
	var probe struct {
		Deadline string `json:"deadline"`
	}
	_ = json.Unmarshal(b, &probe)
	return probe.Deadline
}

func paperAudit(action string, fields map[string]any) {
	if Cfg == nil || flagNoAudit || !Cfg.State.Audit || Cfg.State.AuditPath == "" {
		return
	}
	fields["ts"] = output.Now()
	fields["cmd"] = action
	fields["venue"] = "papertrade"
	state.NewAudit(config.ExpandPath(Cfg.State.AuditPath), true).Append(fields)
}

func emitWarn(cmd string, data any, warnings ...string) {
	if warnings == nil {
		warnings = []string{}
	}
	output.Emit(output.Response{Cmd: cmd, Data: data, Warnings: warnings, Meta: RootMeta(0)})
}

// parsePaperMargin is a wrapper so the command package does not export the
// venue parser's error type. The real parser lives in papertrade.
func parsePaperMargin(s string) (*big.Int, error) {
	return papertrade.ParseMargin(s)
}
