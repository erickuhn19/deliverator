package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// trade + management flags
var (
	wLimit         string
	wIoc           bool
	wAlo           bool
	wReduceOnly    bool
	wTp            string
	wSl            string
	wTrigger       string
	wTriggerType   string
	wTriggerMarket bool
	wCloid         string
	wBuilderFee    int
	wPriority      int
	wSlippage      float64
	wNotional      float64

	wAll      bool
	wOid      int64
	wOids     []int64
	wCloids   []string
	wSize     string
	wMarket   bool
	wCross    bool
	wIsolated bool
	wAdd      bool
	wRemove   bool
)

func addTradeFlags(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&wLimit, "limit", "", "limit price (omit for market)")
	f.BoolVar(&wIoc, "ioc", false, "immediate-or-cancel")
	f.BoolVar(&wAlo, "alo", false, "post-only (add liquidity only)")
	f.BoolVar(&wReduceOnly, "reduce-only", false, "reduce-only")
	f.StringVar(&wTp, "tp", "", "take-profit trigger price (bracket)")
	f.StringVar(&wSl, "sl", "", "stop-loss trigger price (bracket)")
	f.StringVar(&wTrigger, "trigger", "", "trigger price (this order is a trigger order)")
	f.StringVar(&wTriggerType, "trigger-type", "tp", "trigger type: tp|sl")
	f.BoolVar(&wTriggerMarket, "trigger-market", false, "trigger fires a market order")
	f.StringVar(&wCloid, "cloid", "", "client order id (0x+32hex; generated if omitted)")
	f.IntVar(&wBuilderFee, "builder-fee", 0, "builder fee in tenths-of-bps (overrides config)")
	f.IntVar(&wPriority, "priority-bps", 0, "order priority fee in bps for faster sequencing — IOC/market orders only, paid in HYPE from staking balance (max 8; overrides config; not with --tp/--sl)")
	f.Float64Var(&wSlippage, "slippage", 0, "max slippage for market orders (e.g. 0.01); 0 = 5% default, capped at 0.10")
	f.Float64Var(&wNotional, "notional", 0, "size the order by USD notional (size = notional/price); omit the size arg")
}

func buildOrderReq(cmd *cobra.Command, coin string, side core.Side, size string) core.OrderReq {
	req := core.OrderReq{
		Coin: coin, Side: side, Size: size, Notional: wNotional, Limit: wLimit,
		ReduceOnly: wReduceOnly, Cloid: wCloid, Slippage: wSlippage,
	}
	switch {
	case wAlo:
		req.Tif = "Alo"
	case wIoc:
		req.Tif = "Ioc"
	default:
		req.Tif = "Gtc"
	}
	if wTrigger != "" {
		req.Trigger = &core.TriggerReq{TriggerPx: wTrigger, IsMarket: wTriggerMarket, Tpsl: wTriggerType}
	}
	if cmd.Flags().Changed("builder-fee") {
		f := wBuilderFee
		req.BuilderFee = &f
	}
	if cmd.Flags().Changed("priority-bps") {
		p := wPriority
		req.Priority = &p
	}
	return req
}

func runTrade(cmd *cobra.Command, cmdName, coin string, side core.Side, size string) error {
	// Size sourcing: exactly one of the size arg or --notional (#50). Core derives
	// the size from --notional; --notional is not yet wired through brackets.
	if size != "" && wNotional > 0 {
		return fail(cmdName, output.Validation("size_xor_notional", "pass a size argument OR --notional, not both"))
	}
	if size == "" && wNotional <= 0 {
		return fail(cmdName, output.Validation("missing_size", "pass a size argument or --notional"))
	}
	if wNotional > 0 && (wTp != "" || wSl != "") {
		return fail(cmdName, output.Validation("notional_bracket", "--notional is not supported with --tp/--sl brackets yet; pass an explicit size"))
	}
	if wPriority != 0 && (wTp != "" || wSl != "") {
		// Order priority lives in the action's grouping, which a tp/sl bracket
		// already occupies (normalTpsl) — Hyperliquid makes them mutually exclusive.
		return fail(cmdName, output.Validation("priority_bracket", "--priority-bps is not supported with --tp/--sl brackets (priority and tp/sl grouping are mutually exclusive)"))
	}
	ctx, cancel := cmdCtx()
	defer cancel()
	c, err := newClient(ctx)
	if err != nil {
		return fail(cmdName, err)
	}
	// Linked OCO bracket: entry + tp/sl submitted as ONE grouped (normalTpsl)
	// action — the legs arm when the entry fills and a filled TP auto-cancels the SL.
	if wTp != "" || wSl != "" {
		tif := "Gtc"
		switch {
		case wAlo:
			tif = "Alo"
		case wIoc:
			tif = "Ioc"
		}
		results, warnings, berr := c.PlaceBracket(ctx, core.BracketReq{
			Coin: coin, Side: side, Size: size, Limit: wLimit, Tif: tif,
			TP: wTp, SL: wSl, Slippage: wSlippage, Cloid: wCloid,
		})
		if berr != nil {
			return fail(cmdName, berr)
		}
		emit(cmdName, results, warnings...)
		// The entry leg (results[0]) is a marketable IOC (market entry) or a limit —
		// a partial entry fill must surface exit 60 like buy/sell/close, not exit 0.
		// (A fully rejected entry already returns an error above via PlaceBracket.)
		if len(results) > 0 && results[0].IsPartial() {
			return output.ExitWith(output.ExitPartial)
		}
		return nil
	}

	req := buildOrderReq(cmd, coin, side, size)
	res, warnings, err := c.Place(ctx, req)
	if err != nil {
		return fail(cmdName, err)
	}
	emit(cmdName, res, warnings...)
	if res.IsPartial() {
		return output.ExitWith(output.ExitPartial)
	}
	return nil
}

var buyCmd = &cobra.Command{
	Use:   "buy <coin> [size]",
	Short: "Buy (market unless --limit/--alo/--ioc; omit size with --notional)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTrade(cmd, "buy", args[0], core.Buy, argAt(args, 1))
	},
}

var sellCmd = &cobra.Command{
	Use:   "sell <coin> [size]",
	Short: "Sell (market unless --limit/--alo/--ioc; omit size with --notional)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTrade(cmd, "sell", args[0], core.Sell, argAt(args, 1))
	},
}

// orderCmd is the generic placement command AND the parent of `order status`.
var orderCmd = &cobra.Command{
	Use:   "order <coin> <buy|sell> [size]",
	Short: "Generic order placement (omit size with --notional; also: `order status`)",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		side, err := parseSide(args[1])
		if err != nil {
			return fail("order", err)
		}
		return runTrade(cmd, "order", args[0], side, argAt(args, 2))
	},
}

// argAt returns args[i] or "" — lets the size positional be omitted with --notional.
func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func parseSide(s string) (core.Side, error) {
	switch strings.ToLower(s) {
	case "buy", "b", "long":
		return core.Buy, nil
	case "sell", "s", "short":
		return core.Sell, nil
	}
	return core.Buy, output.Validation("bad_side", "side must be buy or sell, got "+s)
}

var modifyCmd = &cobra.Command{
	Use:   "modify",
	Short: "Modify a resting order's size and/or limit price",
	RunE: func(cmd *cobra.Command, args []string) error {
		if wOid == 0 && wCloid == "" {
			return fail("modify", output.Validation("missing_id", "pass --oid or --cloid"))
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("modify", err)
		}
		var oid *int64
		if wOid != 0 {
			oid = ptrI64(wOid)
		}
		res, warnings, err := c.Modify(ctx, oid, wCloid, wSize, wLimit)
		if err != nil {
			return fail("modify", err)
		}
		emit("modify", res, warnings...)
		// A crossing modify can partial-fill — surface exit 60 like every other
		// fill-capable write (buy/sell/close/modify-batch), so an agent gating on
		// the exit code doesn't read a partial as a clean modify.
		if res.IsPartial() {
			return output.ExitWith(output.ExitPartial)
		}
		return nil
	},
}

var cancelCmd = &cobra.Command{
	Use:   "cancel",
	Short: "Cancel by --oid, --cloid, --oids/--cloids (batch), or --all [--coin]",
	RunE: func(cmd *cobra.Command, args []string) error {
		if wOid == 0 && wCloid == "" && !wAll && len(wOids) == 0 && len(wCloids) == 0 {
			return fail("cancel", output.Validation("missing_target", "pass --oid, --cloid, --oids, --cloids, or --all"))
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("cancel", err)
		}
		req := core.CancelReq{Coin: rCoin, All: wAll, Cloid: wCloid, Oids: wOids, Cloids: wCloids}
		if wOid != 0 {
			req.Oid = ptrI64(wOid)
		}
		res, err := c.Cancel(ctx, req)
		if err != nil {
			return fail("cancel", err)
		}
		emit("cancel", res)
		return nil
	},
}

var closeCmd = &cobra.Command{
	Use:   "close <coin>",
	Short: "Close (flatten/reduce) a position",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("close", err)
		}
		market := wMarket || wLimit == ""
		res, warnings, err := c.Close(ctx, args[0], wSize, market, wLimit, wCloid)
		if err != nil {
			return fail("close", err)
		}
		emit("close", res, warnings...)
		if res.IsPartial() {
			return output.ExitWith(output.ExitPartial)
		}
		return nil
	},
}

var leverageCmd = &cobra.Command{
	Use:   "leverage <coin> <x>",
	Short: "Set leverage (cross by default)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		x, perr := strconv.Atoi(args[1])
		if perr != nil {
			return fail("leverage", output.Validation("bad_leverage", "leverage must be an integer"))
		}
		cross := !wIsolated
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("leverage", err)
		}
		res, err := c.SetLeverage(ctx, args[0], x, cross)
		if err != nil {
			return fail("leverage", err)
		}
		emit("leverage", res)
		return nil
	},
}

var marginCmd = &cobra.Command{
	Use:   "margin <coin> <usd>",
	Short: "Adjust isolated margin (--add default, --remove to subtract)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		usd, perr := strconv.ParseFloat(args[1], 64)
		if perr != nil || usd < 0 {
			return fail("margin", output.Validation("bad_amount", "usd must be a non-negative number"))
		}
		if wRemove {
			usd = -usd
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("margin", err)
		}
		res, err := c.AdjustMargin(ctx, args[0], usd)
		if err != nil {
			return fail("margin", err)
		}
		emit("margin", res)
		return nil
	},
}

// ---- twap ----

var (
	wMinutes   int
	wRandomize bool
	wTwapID    int64
)

var twapCmd = &cobra.Command{
	Use:   "twap <coin> <buy|sell> <size>",
	Short: "TWAP order sliced over --minutes (also: `twap cancel`)",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		side, err := parseSide(args[1])
		if err != nil {
			return fail("twap", err)
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("twap", err)
		}
		res, warnings, err := c.Twap(ctx, core.TwapReq{
			Coin: args[0], Side: side, Size: args[2],
			ReduceOnly: wReduceOnly, Minutes: wMinutes, Randomize: wRandomize,
		})
		if err != nil {
			return fail("twap", err)
		}
		emit("twap", res, warnings...)
		return nil
	},
}

var twapStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show live TWAPs and their slice fills (optional --coin / --id filters)",
	Long: `Report the user's live TWAPs with progress (executed size/notional, % done)
and their per-slice fills.

A TWAP drops off the running list once it completes, so an id that returns no
running entry has finished (or never started). On a TWAP place TIMEOUT (exit 42)
the outcome is unknown — the twapOrder action carries no client-order-id, so the
confirmation key is the coin: run "twap status --coin <coin>" and check whether a
TWAP started (and when) before resubmitting, rather than blind-resending.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("twap.status", err)
		}
		res, err := c.TwapStatus(ctx, rCoin, wTwapID)
		if err != nil {
			return fail("twap.status", err)
		}
		emit("twap.status", res)
		return nil
	},
}

var twapCancelCmd = &cobra.Command{
	Use:   "cancel",
	Short: "Cancel a running TWAP by --coin and --id",
	RunE: func(cmd *cobra.Command, args []string) error {
		if wTwapID == 0 || rCoin == "" {
			return fail("twap.cancel", output.Validation("missing_target", "pass --coin and --id"))
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("twap.cancel", err)
		}
		res, err := c.TwapCancel(ctx, rCoin, wTwapID)
		if err != nil {
			return fail("twap.cancel", err)
		}
		emit("twap.cancel", res)
		return nil
	},
}

var (
	wPosTP   string
	wPosSL   string
	wPosSize string
)

var positionTpslCmd = &cobra.Command{
	Use:   "position-tpsl <coin>",
	Short: "Attach reduce-only take-profit / stop-loss to your whole open position",
	Long: `Place reduce-only take-profit and/or stop-loss triggers bound to your EXISTING
position (Hyperliquid's positionTpsl grouping). Unlike a bracket — whose tp/sl
link to one entry order — these protect the NET position however it was built,
sized to it at placement (re-run after you materially change the position size).

  deliverator position-tpsl BTC --sl 58000              # stop-loss on the whole position
  deliverator position-tpsl BTC --tp 72000 --sl 58000   # both, one signed action
  deliverator position-tpsl BTC --tp 72000 --size 0.5   # protect part of the position

Side is derived from the live position (a long is protected by SELL triggers, a
short by BUY). --size defaults to the full position and must not exceed it; the
triggers fire as market orders. Reduce-only, so no portfolio gate or $10 floor
applies. Works on HIP-3 sub-dex coins (xyz:*) too.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("position-tpsl", err)
		}
		results, warnings, err := c.PlacePositionTpsl(ctx, core.PositionTpslReq{
			Coin: args[0], TP: wPosTP, SL: wPosSL, Size: wPosSize, Cloid: wCloid,
		})
		if err != nil {
			return fail("position-tpsl", err)
		}
		emit("position-tpsl", results, warnings...)
		for _, r := range results {
			if r.Status == "rejected" {
				return output.ExitWith(output.ExitPartial)
			}
		}
		return nil
	},
}

// ---- dead-man's switch ----

type dmsState struct {
	Secs       int   `json:"secs"`
	DeadlineMs int64 `json:"deadline_ms"`
	SetAtMs    int64 `json:"set_at_ms"`
}

func dmsPath() string { return filepath.Join(config.Dir(), "dms.json") }

func writeDMS(s dmsState) {
	_ = os.MkdirAll(config.Dir(), 0o700)
	b, _ := json.Marshal(s)
	_ = os.WriteFile(dmsPath(), b, 0o600)
}

func readDMS() (dmsState, bool) {
	b, err := os.ReadFile(dmsPath())
	if err != nil {
		return dmsState{}, false
	}
	var s dmsState
	if json.Unmarshal(b, &s) != nil {
		return dmsState{}, false
	}
	return s, true
}

var dmsCmd = &cobra.Command{
	Use:   "dms (set <secs> | heartbeat | clear | status)",
	Short: "Dead-man's switch: schedule-cancel that auto-flattens if not refreshed",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sub := strings.ToLower(args[0])
		ctx, cancel := cmdCtx()
		defer cancel()

		switch sub {
		case "status":
			s, ok := readDMS()
			if !ok {
				emit("dms.status", map[string]any{"armed": false})
				return nil
			}
			emit("dms.status", map[string]any{
				"armed":         s.DeadlineMs > time.Now().UnixMilli(),
				"secs":          s.Secs,
				"deadline_ms":   s.DeadlineMs,
				"set_at_ms":     s.SetAtMs,
				"expires_in_ms": s.DeadlineMs - time.Now().UnixMilli(),
			})
			return nil
		case "clear":
			c, err := newClient(ctx)
			if err != nil {
				return fail("dms.clear", err)
			}
			if err := c.ScheduleCancel(ctx, nil); err != nil {
				return fail("dms.clear", err)
			}
			_ = os.Remove(dmsPath())
			core.AuditDMS(Cfg, flagNoAudit, "clear", 0, 0)
			emit("dms.clear", map[string]any{"cleared": true})
			return nil
		case "set", "heartbeat":
			secs := Cfg.Risk.DeadManSwitchSecs
			if sub == "set" {
				if len(args) < 2 {
					return fail("dms.set", output.Validation("missing_secs", "usage: dms set <secs>"))
				}
				n, perr := strconv.Atoi(args[1])
				if perr != nil || n < 5 {
					return fail("dms.set", output.Validation("bad_secs", "secs must be an integer >= 5 (exchange minimum)"))
				}
				secs = n
			} else if s, ok := readDMS(); ok && s.Secs > 0 {
				secs = s.Secs
			}
			if secs < 5 {
				return fail("dms."+sub, output.Validation("bad_secs", "dead_man_switch_secs must be >= 5; set one or pass `dms set <secs>`"))
			}
			deadline := time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
			c, err := newClient(ctx)
			if err != nil {
				return fail("dms."+sub, err)
			}
			if err := c.ScheduleCancel(ctx, ptrI64(deadline)); err != nil {
				return fail("dms."+sub, err)
			}
			writeDMS(dmsState{Secs: secs, DeadlineMs: deadline, SetAtMs: time.Now().UnixMilli()})
			core.AuditDMS(Cfg, flagNoAudit, sub, secs, deadline)
			emit("dms."+sub, map[string]any{"armed": true, "secs": secs, "deadline_ms": deadline})
			return nil
		}
		return fail("dms", output.Validation("bad_subcommand", "want: set|heartbeat|clear|status"))
	},
}

// ---- halt ----

var haltCmd = &cobra.Command{
	Use:   "halt (on | off | status)",
	Short: "Global emergency stop — rejects all new orders instantly",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch strings.ToLower(args[0]) {
		case "on":
			if err := core.SetHalt(true); err != nil {
				return fail("halt", output.Unknown("halt", err.Error()))
			}
			core.AuditHalt(Cfg, flagNoAudit, true)
			emit("halt", map[string]any{"halted": true})
		case "off":
			if err := core.SetHalt(false); err != nil {
				return fail("halt", output.Unknown("halt", err.Error()))
			}
			core.AuditHalt(Cfg, flagNoAudit, false)
			emit("halt", map[string]any{"halted": false})
		case "status":
			ctx, cancel := cmdCtx()
			defer cancel()
			c, err := newClient(ctx)
			if err != nil {
				// halt status should work even offline; fall back to file check
				emit("halt", map[string]any{"halted": haltedByFile()})
				return nil
			}
			emit("halt", map[string]any{"halted": c.Halted()})
		default:
			return fail("halt", output.Validation("bad_arg", "want: on | off | status"))
		}
		return nil
	},
}

func haltedByFile() bool {
	_, err := os.Stat(filepath.Join(config.Dir(), "halt"))
	return err == nil
}

// ---- panic: cancel-all + flatten-all ----

var panicCmd = &cobra.Command{
	Use:   "panic",
	Short: "Cancel ALL orders, cancel running TWAPs, and flatten ALL positions",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !flagYes && !flagDryRun {
			return fail("panic", output.Validation("confirm", "panic flattens everything — pass --yes to confirm"))
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("panic", err)
		}
		res, err := c.Panic(ctx)
		if err != nil {
			return fail("panic", err)
		}
		emit("panic", res)
		// A teardown that could not be confirmed flat (a degraded dex read, a
		// failed cancel/close, or remaining orders) exits non-zero so an operator
		// never reads an incomplete emergency-flatten as success.
		if !res.Complete {
			return output.ExitWith(output.ExitPartial)
		}
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{buyCmd, sellCmd, orderCmd, closeCmd} {
		addTradeFlags(c)
	}
	modifyCmd.Flags().Int64Var(&wOid, "oid", 0, "order id")
	modifyCmd.Flags().StringVar(&wCloid, "cloid", "", "client order id")
	modifyCmd.Flags().StringVar(&wSize, "size", "", "new size")
	modifyCmd.Flags().StringVar(&wLimit, "limit", "", "new limit price")

	cancelCmd.Flags().Int64Var(&wOid, "oid", 0, "order id")
	cancelCmd.Flags().StringVar(&wCloid, "cloid", "", "client order id")
	cancelCmd.Flags().Int64SliceVar(&wOids, "oids", nil, "batch cancel these order ids (comma-separated)")
	cancelCmd.Flags().StringSliceVar(&wCloids, "cloids", nil, "batch cancel these client order ids (comma-separated)")
	cancelCmd.Flags().BoolVar(&wAll, "all", false, "cancel all orders")
	cancelCmd.Flags().StringVar(&rCoin, "coin", "", "pin coin (for --all, or to skip the lookup for --oids/--cloids)")

	closeCmd.Flags().StringVar(&wSize, "size", "", "size to close (default: full)")
	closeCmd.Flags().BoolVar(&wMarket, "market", false, "market close (default)")

	leverageCmd.Flags().BoolVar(&wCross, "cross", false, "cross margin (default)")
	leverageCmd.Flags().BoolVar(&wIsolated, "isolated", false, "isolated margin")
	leverageCmd.MarkFlagsMutuallyExclusive("cross", "isolated")

	marginCmd.Flags().BoolVar(&wAdd, "add", false, "add margin (default)")
	marginCmd.Flags().BoolVar(&wRemove, "remove", false, "remove margin")
	marginCmd.MarkFlagsMutuallyExclusive("add", "remove")

	twapCmd.Flags().IntVar(&wMinutes, "minutes", 30, "minutes to slice the TWAP over")
	twapCmd.Flags().BoolVar(&wReduceOnly, "reduce-only", false, "reduce-only")
	twapCmd.Flags().BoolVar(&wRandomize, "randomize", false, "randomize slice sizing/timing")
	twapCancelCmd.Flags().StringVar(&rCoin, "coin", "", "coin of the TWAP to cancel")
	twapCancelCmd.Flags().Int64Var(&wTwapID, "id", 0, "twap id to cancel")
	twapStatusCmd.Flags().StringVar(&rCoin, "coin", "", "filter to this coin")
	twapStatusCmd.Flags().Int64Var(&wTwapID, "id", 0, "filter to this twap id")
	twapCmd.AddCommand(twapCancelCmd, twapStatusCmd)

	positionTpslCmd.Flags().StringVar(&wPosTP, "tp", "", "take-profit trigger price")
	positionTpslCmd.Flags().StringVar(&wPosSL, "sl", "", "stop-loss trigger price")
	positionTpslCmd.Flags().StringVar(&wPosSize, "size", "", "size to protect (default: the whole position)")
	positionTpslCmd.Flags().StringVar(&wCloid, "cloid", "", "client order id (0x+32hex; generated if omitted)")

	rootCmd.AddCommand(buyCmd, sellCmd, orderCmd, modifyCmd, cancelCmd, closeCmd,
		leverageCmd, marginCmd, twapCmd, positionTpslCmd, dmsCmd, haltCmd, panicCmd)
}
