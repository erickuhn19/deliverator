package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	hl "github.com/erickuhn19/deliverator/internal/hl"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// Side is buy or sell.
type Side int

const (
	Buy Side = iota
	Sell
)

func (s Side) String() string {
	if s == Sell {
		return "sell"
	}
	return "buy"
}

// TriggerReq describes a TP/SL trigger order.
type TriggerReq struct {
	TriggerPx string
	IsMarket  bool
	Tpsl      string // "tp" | "sl"
}

// OrderReq is the core's order input. Strings in, validated/rounded internally.
type OrderReq struct {
	Coin       string
	Side       Side
	Size       string  // pre-round user input; "" => derive from Notional
	Notional   float64 // USD notional sizing (#50): when Size=="" && Notional>0, size = Notional/refPx
	Limit      string  // "" => market
	Tif        string  // Gtc | Ioc | Alo (limit only; default Gtc)
	ReduceOnly bool
	Trigger    *TriggerReq
	Cloid      string // "" => generated
	BuilderFee *int   // tenths-bps override; nil => config attach policy
	Priority   *int   // order-priority fee in bps; nil => config default (faster sequencing)
	Slippage   float64
	// Closing marks a reductive exit (spot close — spot has no reduce-only). It
	// exempts the order from the NEW-exposure guards (allowlist, limit_only, max
	// caps) like a perp reduce-only close, while keeping the $10 min floor. Set
	// only by closeSpot — never from CLI/JSON input.
	Closing bool
}

// BuilderApplied reports the builder fee attached to an order.
type BuilderApplied struct {
	Address      string `json:"address"`
	FeeTenthsBps int    `json:"fee_tenths_bps"`
}

// PlaceResult is the outcome of a write that creates/affects an order.
type PlaceResult struct {
	Cloid      string          `json:"cloid"`
	Oid        *int64          `json:"oid,omitempty"`
	Status     string          `json:"status"`          // resting | filled | submitted | dry_run | rejected
	Error      string          `json:"error,omitempty"` // per-leg reject reason (batch)
	Coin       string          `json:"coin"`
	Side       string          `json:"side"`
	Size       string          `json:"size"`
	LimitPx    string          `json:"limit_px,omitempty"`
	TriggerPx  string          `json:"trigger_px,omitempty"` // the (rounded) price that fires a trigger order
	Type       string          `json:"type"`                 // market | limit | trigger
	ReduceOnly bool            `json:"reduce_only,omitempty"`
	FilledSz   string          `json:"filled_sz,omitempty"`
	AvgPx      string          `json:"avg_px,omitempty"`
	Builder    *BuilderApplied `json:"builder,omitempty"`
	Rounded    *Rounding       `json:"rounded,omitempty"`
	DryRun     bool            `json:"dry_run,omitempty"`
}

// IsPartial reports a partial fill (exit 60).
func (r *PlaceResult) IsPartial() bool {
	if r.Status != "filled" || r.FilledSz == "" {
		return false
	}
	f, e1 := strconv.ParseFloat(r.FilledSz, 64)
	want, e2 := strconv.ParseFloat(r.Size, 64)
	return e1 == nil && e2 == nil && f+1e-12 < want
}

// ---------- cloid ----------

// GenCloid returns a fresh 0x + 32-hex (16-byte) client order id. It propagates
// an RNG error rather than swallowing it (audit #91 / S10): the cloid is HL's
// idempotency key, so a constant 0x000…0 from a half-filled buffer would make HL
// dedupe and silently drop distinct legs (e.g. on the copy path, which mints a
// fresh cloid per leg). The write fails closed before consuming a nonce.
//
// Note: on Go 1.24+ crypto/rand.Read never returns a non-nil error — it crashes
// the process on a CSPRNG failure — so this branch is forward/backward-compatible
// defense, not a live path on the current toolchain.
func GenCloid() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", output.Unknown("rng_failure", "failed to generate client order id (system RNG): "+err.Error())
	}
	return "0x" + hex.EncodeToString(b[:]), nil
}

func normalizeCloid(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return GenCloid()
	}
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "0x") && !strings.HasPrefix(t, "0X") {
		t = "0x" + t
	}
	if len(t) != 34 {
		return "", output.Validation("cloid", "cloid must be 0x + 32 hex chars (16 bytes)").
			WithHint("generate a 16-byte hex id, e.g. 0x" + "00000000000000000000000000000001")
	}
	if _, err := hex.DecodeString(t[2:]); err != nil {
		return "", output.Validation("cloid", "cloid is not valid hex")
	}
	return strings.ToLower(t), nil
}

func tifOf(s string) hl.Tif {
	switch strings.ToLower(s) {
	case "ioc":
		return hl.TifIoc
	case "alo":
		return hl.TifAlo
	default:
		return hl.TifGtc
	}
}

// ---------- signing wrapper (nonce-coordinated, §8) ----------

// signed runs fn with the exchange under the nonce flock: it floors the signer
// at the persisted high-water mark, then commits a fresh high-water afterward so
// overlapping processes never collide.
func (c *Client) signed(ctx context.Context, fn func(ex *hl.Exchange) error) error {
	ex, err := c.exchange(ctx)
	if err != nil {
		return err
	}
	h, err := c.nonce.Acquire()
	if err != nil {
		return output.Network("nonce_lock", "acquire nonce lock: "+err.Error())
	}
	defer h.Release()
	ex.SetLastNonce(h.Persisted())
	ferr := fn(ex)
	_ = h.Commit(time.Now().UnixMilli()) // high-water ≥ any nonce just used
	return ferr
}

// ---------- place ----------

// roundedOrder holds the validated, tick-rounded pieces of one order. It does not
// price a market order, run risk, or sign — callers do that. Shared by Place and
// PlaceBatch so size/price/trigger rounding (and strict-precision rejects) can
// never drift between the single-order and batch paths.
type roundedOrder struct {
	szOut, limitOut, triggerOut string
	szF, limitF, triggerF       float64
	cloid                       string
	typ                         string // market | limit | trigger
	isMarket                    bool
	rounding                    *Rounding
	warnings                    []string
}

// roundOrderReq validates + rounds size, limit, and trigger prices and normalizes
// the cloid for one order, honoring --strict (reject instead of round).
// applyNotional derives Size from Notional when Size is empty (#50: --notional).
// The reference price is the explicit limit, else the trigger price, else the live
// mid. The derived size is left UNROUNDED — roundOrderReq/RoundSize then apply the
// asset's lot precision and the risk layer sees the derived size like any other.
func (c *Client) applyNotional(ctx context.Context, mk Market, req OrderReq) (OrderReq, error) {
	if req.Size != "" || req.Notional <= 0 {
		return req, nil
	}
	var refPx float64
	switch {
	case req.Limit != "":
		refPx = parseFloatSafe(req.Limit)
	case req.Trigger != nil && req.Trigger.TriggerPx != "":
		refPx = parseFloatSafe(req.Trigger.TriggerPx)
	default:
		m, ok := c.midPrice(ctx, mk.Coin)
		if !ok {
			return req, output.Risk("no_ref_px",
				"cannot price --notional for "+mk.Coin+" — no mid available").
				WithHint("pass --limit alongside --notional, or retry when mids are available")
		}
		refPx = m
	}
	if refPx <= 0 {
		return req, output.Validation("bad_ref_px", "reference price for --notional must be positive")
	}
	req.Size = f2s(req.Notional / refPx)
	return req, nil
}

// roundMarketPrice rounds a price to the market's tick rule: the (0,1) probability
// rule for HIP-4 outcomes, else the standard perp/spot sig-fig rule.
func roundMarketPrice(mk Market, px string) (string, bool, error) {
	if mk.IsOutcome {
		return RoundOutcomePrice(px)
	}
	return RoundPrice(px, mk.SzDecimals, mk.IsSpot)
}

func (c *Client) roundOrderReq(mk Market, req OrderReq) (*roundedOrder, error) {
	ro := &roundedOrder{rounding: &Rounding{}}

	szOut, szChanged, err := RoundSize(req.Size, mk.SzDecimals)
	if err != nil {
		return nil, output.Validation("bad_size", err.Error())
	}
	if szChanged {
		if c.opts.Strict {
			return nil, output.Precision("sz_precision",
				fmt.Sprintf("size %s has too many decimals; %s allows %d (-> %s)", req.Size, mk.Coin, mk.SzDecimals, szOut)).
				WithHint("pass size " + szOut)
		}
		ro.rounding.Sz = &FromTo{From: req.Size, To: szOut}
		ro.warnings = append(ro.warnings, fmt.Sprintf("size rounded %s -> %s", req.Size, szOut))
	}
	ro.szOut = szOut
	ro.szF, _ = strconv.ParseFloat(szOut, 64)

	ro.cloid, err = normalizeCloid(req.Cloid)
	if err != nil {
		return nil, err
	}

	ro.isMarket = req.Limit == "" && req.Trigger == nil
	ro.typ = "market"
	if !ro.isMarket {
		px := req.Limit
		if px == "" && req.Trigger != nil {
			px = req.Trigger.TriggerPx
		}
		limitOut, pxChanged, perr := roundMarketPrice(mk, px)
		if perr != nil {
			return nil, output.Validation("bad_price", perr.Error())
		}
		if pxChanged {
			if c.opts.Strict {
				return nil, output.Precision("px_precision",
					fmt.Sprintf("price %s exceeds %d sig figs / %d decimals for %s (-> %s)", px, MaxSigFigs, mk.PxDecimals, mk.Coin, limitOut)).
					WithHint("pass price " + limitOut)
			}
			ro.rounding.Px = &FromTo{From: px, To: limitOut}
			ro.warnings = append(ro.warnings, fmt.Sprintf("price rounded %s -> %s", px, limitOut))
		}
		ro.limitOut = limitOut
		ro.limitF, _ = strconv.ParseFloat(limitOut, 64)
		if req.Trigger != nil {
			ro.typ = "trigger"
		} else {
			ro.typ = "limit"
		}
	}

	// A trigger order's trigger price must be rounded with the SAME rule as the
	// limit and the ROUNDED value wired — otherwise the order rests at the rounded
	// price while it fires at a raw, possibly tick-invalid one (and dry-run, the
	// agent's own preview, never shows the divergence).
	if req.Trigger != nil {
		if req.Limit == "" {
			// Pure trigger: the limit leg already rounded this exact input.
			ro.triggerOut, ro.triggerF = ro.limitOut, ro.limitF
		} else {
			triggerOut, tChanged, perr := roundMarketPrice(mk, req.Trigger.TriggerPx)
			if perr != nil {
				return nil, output.Validation("bad_trigger_px", perr.Error())
			}
			if tChanged {
				if c.opts.Strict {
					return nil, output.Precision("trigger_px_precision",
						fmt.Sprintf("trigger price %s exceeds %d sig figs / %d decimals for %s (-> %s)", req.Trigger.TriggerPx, MaxSigFigs, mk.PxDecimals, mk.Coin, triggerOut)).
						WithHint("pass --trigger " + triggerOut)
				}
				ro.rounding.TriggerPx = &FromTo{From: req.Trigger.TriggerPx, To: triggerOut}
				ro.warnings = append(ro.warnings, fmt.Sprintf("trigger price rounded %s -> %s", req.Trigger.TriggerPx, triggerOut))
			}
			ro.triggerOut = triggerOut
			ro.triggerF, _ = strconv.ParseFloat(triggerOut, 64)
		}
	}
	return ro, nil
}

// Place validates, rounds, risk-checks, attaches the builder, and submits one
// marketableGuardPx returns the price at which a non-trigger LIMIT order's dollar
// guards (min floor, max caps) must be evaluated. A MARKETABLE limit — one whose
// price crosses the mid — fills at the MARKET, not at its aggressive limit, so the
// guards are priced at the mid; otherwise a crossing order slips past
// max_order_notional, or is false-rejected by the min floor though HL would fill
// it. A resting (non-crossing) limit, or an unavailable mid, keeps the limit price.
// Only the GUARD reference moves — the signed wire keeps carrying the limit.
// Callers gate on pricingGuardsActive() and pass non-trigger limit prices. This is
// the one shared implementation; every limit-carrying write path (Place, PlaceBatch,
// PlaceBracket, Modify, ModifyBatch) uses it so the guard is consistent.
func (c *Client) marketableGuardPx(ctx context.Context, coin string, side Side, limitF float64) float64 {
	if m, ok := c.midPrice(ctx, coin); ok {
		if (side == Buy && limitF >= m) || (side == Sell && limitF <= m) {
			return m
		}
	}
	return limitF
}

// outcomeGuardPx returns the dollar-guard reference price for a non-trigger
// outcome order, and whether it could be determined. Outcome books are WIDE, so
// (unlike a perp) the mid badly misprices a crossing order — value it at the
// touch it actually fills at (ask for a buy, bid for a sell), and value a resting
// limit at its own price. ok=false only for a market order with no book touch, so
// the caller fails closed instead of bypassing the caps/minimum (audit #105).
func (c *Client) outcomeGuardPx(ctx context.Context, coin string, side Side, limitF float64, isMarket bool) (float64, bool) {
	var bid, ask float64
	if bbo, err := c.Bbo(ctx, coin); err == nil {
		bid, ask = parseFloatSafe(bbo.Bid), parseFloatSafe(bbo.Ask)
	}
	if isMarket {
		if side == Buy && ask > 0 {
			return ask, true
		}
		if side == Sell && bid > 0 {
			return bid, true
		}
		return 0, false
	}
	// Limit: marketable (crosses the touch) fills at the touch; else resting at limit.
	if side == Buy && ask > 0 && limitF >= ask {
		return ask, true
	}
	if side == Sell && bid > 0 && limitF <= bid {
		return bid, true
	}
	return limitF, true
}

// order (market | limit | trigger). It is idempotent on Cloid (§5.4).
func (c *Client) Place(ctx context.Context, req OrderReq) (*PlaceResult, []string, error) {
	mk, ok := c.meta.Lookup(req.Coin)
	if !ok {
		return nil, nil, unknownCoin(req.Coin)
	}
	req, nerr := c.applyNotional(ctx, mk, req)
	if nerr != nil {
		return nil, nil, nerr
	}
	ro, err := c.roundOrderReq(mk, req)
	if err != nil {
		return nil, nil, err
	}
	warnings := ro.warnings
	rounding := ro.rounding
	szOut, szF := ro.szOut, ro.szF
	cloid := ro.cloid
	isMarket, typ := ro.isMarket, ro.typ
	limitOut, limitF := ro.limitOut, ro.limitF
	triggerOut, triggerF := ro.triggerOut, ro.triggerF

	// Resolve + clamp the market slippage ONCE; reused by the worst-case-fill guard
	// below and the signed MarketOpen, so the cap is checked against the same band
	// the order can actually fill within (audit S4).
	effSlip := hl.DefaultSlippage
	if isMarket {
		s, serr := resolveSlippage(req.Slippage)
		if serr != nil {
			return nil, nil, serr
		}
		effSlip = s
	}

	// Notional + risk gate. Reduce-only orders can't increase exposure, so they
	// skip the notional caps entirely (and need no reference price for them).
	notional, posNotional := 0.0, 0.0
	if !req.ReduceOnly {
		refPx := limitF
		switch {
		case mk.IsOutcome && req.Trigger == nil:
			// Outcome books are wide, so value the guards at the touch the order
			// fills at (not the mid, which mis-states by the spread — audit #105).
			if px, ok := c.outcomeGuardPx(ctx, mk.Coin, req.Side, limitF, isMarket); ok {
				refPx = px
			} else if c.pricingGuardsActive() {
				// market order, no book touch -> fail closed (never bypass caps/min)
				return nil, nil, output.Risk("no_ref_px",
					"cannot determine a reference price for an outcome market order in "+mk.Coin+" — refusing to bypass notional caps/minimum").
					WithHint("retry when the book is available, or place a limit order with --limit")
			}
		case isMarket:
			m, ok := c.midPrice(ctx, mk.Coin)
			if !ok {
				// Fail CLOSED: an unknown reference price must never let a market
				// order slip past a configured dollar guard (a cap would see
				// notional 0 and pass anything; the min floor could not be enforced).
				if c.pricingGuardsActive() {
					return nil, nil, output.Risk("no_ref_px",
						"cannot determine a reference price for a market order in "+mk.Coin+" — refusing to bypass notional caps/minimum").
						WithHint("retry when mids are available, or place a limit order with --limit")
				}
			} else {
				// Price the guard at the worst-case fill (mid*(1+slip) buy / mid sell),
				// not the bare mid — else a fill can exceed the cap by the slippage band.
				refPx = marketGuardPx(m, req.Side == Buy, mk.IsOutcome, effSlip)
			}
		case req.Trigger == nil && c.pricingGuardsActive():
			// A marketable (crossing) limit fills at the market, so price the dollar
			// guards at the mid (shared with every other limit-carrying path).
			refPx = c.marketableGuardPx(ctx, mk.Coin, req.Side, limitF)
		}
		notional = refPx * szF
		if c.cfg.Risk.MaxPositionNotionalUSD > 0 {
			posNotional = notional + c.currentPositionNotional(ctx, mk.Coin)
		}
	}
	if err := c.preTradeChecks(riskCheck{Coin: mk.Coin, IsMarket: isMarket, NotionalUSD: notional, PositionNotionalUSD: posNotional, MinNotionalUSD: c.cfg.Risk.MinOrderNotionalUSD, ReduceOnly: req.ReduceOnly, Closing: req.Closing}); err != nil {
		return nil, nil, err
	}
	// Account-wide gates run against the resulting book; a reduce-only/closing
	// order adds no exposure, so it is exempt (it can only shrink the book).
	if !req.ReduceOnly && !req.Closing {
		if perr := c.checkPortfolioGates(ctx, []exposureDelta{{coin: mk.Coin, signedNotional: signedNotional(req.Side, notional)}}); perr != nil {
			return nil, nil, perr
		}
	} else if req.ReduceOnly {
		if ferr := c.reduceOnlyFlipErr(ctx, mk.Coin, szF); ferr != nil {
			return nil, nil, ferr
		}
	}

	builder, builderWarn := c.resolveBuilderApproved(ctx, req.BuilderFee)
	if builderWarn != "" {
		warnings = append(warnings, builderWarn)
	}
	priorityRate, prioWarn := c.resolvePriority(req.Priority)
	if prioWarn != "" {
		warnings = append(warnings, prioWarn)
	}
	if priorityRate > 0 {
		// Hyperliquid requires priority orders to be immediately executable (IOC):
		// a market order is marketable-IOC; a resting GTC/ALO limit or a trigger is
		// not. An explicit --priority-bps on a resting order is a hard reject; a
		// config-default priority is silently dropped (it only applies where valid).
		ioc := (req.Limit == "" && req.Trigger == nil) || strings.EqualFold(req.Tif, "Ioc")
		if !ioc {
			if req.Priority != nil {
				return nil, warnings, output.Validation("priority_requires_ioc",
					"order priority requires an IOC or market order — Hyperliquid rejects priority on resting GTC/ALO/trigger orders").
					WithHint("add --ioc, or drop --priority-bps")
			}
			warnings = append(warnings, "order priority skipped: it only applies to IOC/market orders")
			priorityRate = 0
		}
	}
	if priorityRate > 0 {
		warnings = append(warnings, fmt.Sprintf("order priority %d bps applied (faster sequencing, fee paid in HYPE from staking balance)", priorityRate/bpsToPriorityRate))
	}
	res := &PlaceResult{
		Cloid: cloid, Coin: mk.Coin, Side: req.Side.String(),
		Size: szOut, LimitPx: limitOut, TriggerPx: triggerOut, Type: typ, ReduceOnly: req.ReduceOnly,
	}
	if !rounding.Empty() {
		res.Rounded = rounding
	}
	if builder != nil {
		// Hyperliquid charges a spot BUY's taker fee in the BASE token, and does not
		// apply a (quote-denominated) builder fee on it — so a spot buy earns no
		// builder revenue. Don't claim a fee that won't be collected; warn instead.
		if mk.IsSpot && req.Side == Buy {
			warnings = append(warnings, "builder fee NOT earned on a spot BUY: Hyperliquid charges the taker fee in the base token, so no builder fee applies (spot sells and perps do earn it)")
		} else {
			res.Builder = &BuilderApplied{Address: builder.Builder, FeeTenthsBps: builder.Fee}
			warnings = append(warnings, fmt.Sprintf("builder fee %.3f%% applied", float64(builder.Fee)/1000.0))
		}
	}

	if c.opts.DryRun {
		res.DryRun = true
		res.Status = "dry_run"
		return res, warnings, nil
	}

	isBuy := req.Side == Buy
	var st hl.OrderStatus
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		if isMarket {
			st, e = ex.MarketOpen(ctx, mk.Coin, isBuy, szF, nil, effSlip, &cloid, builder)
		} else {
			cr := hl.CreateOrderRequest{
				Coin: mk.Coin, IsBuy: isBuy, Price: limitF, Size: szF,
				ReduceOnly: req.ReduceOnly, ClientOrderID: &cloid,
			}
			if req.Trigger != nil {
				cr.OrderType = hl.OrderType{Trigger: &hl.TriggerOrderType{
					TriggerPx: triggerF, IsMarket: req.Trigger.IsMarket, Tpsl: hl.Tpsl(req.Trigger.Tpsl),
				}}
			} else {
				cr.OrderType = hl.OrderType{Limit: &hl.LimitOrderType{Tif: tifOf(req.Tif)}}
			}
			st, e = ex.Order(ctx, cr, builder, priorityRate)
		}
		return e
	})
	if st.Error != nil {
		c.audit.Append(map[string]any{
			"action": "order", "cloid": cloid, "coin": mk.Coin, "side": req.Side.String(),
			"size": szOut, "limit_px": limitOut, "type": typ, "status": "rejected", "error": *st.Error,
		})
		return nil, warnings, mapOrderReject(*st.Error)
	}
	if serr != nil {
		c.audit.Append(map[string]any{
			"action": "order", "cloid": cloid, "coin": mk.Coin, "side": req.Side.String(),
			"size": szOut, "limit_px": limitOut, "type": typ, "status": "error", "error": serr.Error(),
		})
		return nil, warnings, mapExchangeErr(serr)
	}
	applyStatus(res, st)
	c.audit.Append(withFill(map[string]any{
		"action": "order", "cloid": cloid, "coin": mk.Coin, "side": req.Side.String(),
		"size": szOut, "limit_px": limitOut, "type": typ, "status": res.Status, "oid": res.Oid,
	}, res))
	return res, append(c.signerWarnings(), warnings...), nil
}

func applyStatus(res *PlaceResult, st hl.OrderStatus) {
	switch {
	case st.Filled != nil:
		res.Status = "filled"
		oid := int64(st.Filled.Oid)
		res.Oid = &oid
		res.FilledSz = st.Filled.TotalSz
		res.AvgPx = st.Filled.AvgPx
	case st.Resting != nil:
		res.Status = "resting"
		oid := st.Resting.Oid
		res.Oid = &oid
	case st.Status != "":
		// Bare-string status from a grouped bracket leg, e.g. "waitingForTrigger".
		res.Status = st.Status
	default:
		res.Status = "submitted"
	}
}

// withFill adds fill telemetry (the actually-filled size + avg price) to an audit
// map when the order filled, so the persisted trail can tell a partial fill from
// a full one — a "filled" status alone cannot.
func withFill(m map[string]any, res *PlaceResult) map[string]any {
	if res.FilledSz != "" {
		m["filled_sz"] = res.FilledSz
	}
	if res.AvgPx != "" {
		m["avg_px"] = res.AvgPx
	}
	return m
}

// auditLeg distills one order-write leg into a compact audit record, carrying the
// per-leg shape (type, reduce_only, trigger_px) and outcome (status, oid, fill)
// so a multi-leg row (batch / batch_modify) is as reconstructable from the trail
// as a single order/bracket row — not just an opaque count + coin list.
func auditLeg(r *PlaceResult) map[string]any {
	m := map[string]any{"coin": r.Coin, "side": r.Side, "size": r.Size, "type": r.Type, "status": r.Status}
	if r.ReduceOnly {
		m["reduce_only"] = true
	}
	if r.LimitPx != "" {
		m["limit_px"] = r.LimitPx
	}
	if r.TriggerPx != "" {
		m["trigger_px"] = r.TriggerPx
	}
	if r.Oid != nil {
		m["oid"] = *r.Oid
	}
	if r.Error != "" {
		m["error"] = r.Error
	}
	return withFill(m, r)
}

// auditLegs maps a slice of write results into compact audit records.
func auditLegs(results []*PlaceResult) []map[string]any {
	legs := make([]map[string]any, len(results))
	for i, r := range results {
		legs[i] = auditLeg(r)
	}
	return legs
}

// ---------- bracket (linked OCO tp/sl) ----------

// BracketReq is an entry order with linked take-profit / stop-loss legs, placed
// as ONE grouped (normalTpsl) action so the legs are a true OCO bracket: they
// activate when the entry fills, and a filled TP auto-cancels the SL.
type BracketReq struct {
	Coin     string
	Side     Side
	Size     string
	Limit    string // "" => market entry (marketable IOC at slippage)
	Tif      string // entry tif for a limit entry (Gtc|Alo|Ioc)
	TP       string // take-profit trigger price ("" => omit)
	SL       string // stop-loss trigger price ("" => omit)
	Slippage float64
	Cloid    string
}

// PlaceBracket submits an entry + linked tp/sl as one normalTpsl action.
func (c *Client) PlaceBracket(ctx context.Context, req BracketReq) ([]*PlaceResult, []string, error) {
	mk, ok := c.meta.Lookup(req.Coin)
	if !ok {
		return nil, nil, unknownCoin(req.Coin)
	}
	if req.TP == "" && req.SL == "" {
		return nil, nil, output.Validation("no_bracket", "a bracket needs --tp and/or --sl")
	}
	warnings := []string{}
	szOut, _, err := RoundSize(req.Size, mk.SzDecimals)
	if err != nil {
		return nil, nil, output.Validation("bad_size", err.Error())
	}
	szF, _ := strconv.ParseFloat(szOut, 64)
	cloid, err := normalizeCloid(req.Cloid)
	if err != nil {
		return nil, nil, err
	}

	// Entry price + tif. A market entry is a marketable IOC at the slippage price.
	isMarket := req.Limit == ""
	entryTif := tifOf(req.Tif)
	var entryPxF, marketGuard float64
	if isMarket {
		mid, ok := c.midPrice(ctx, mk.Coin)
		if !ok {
			return nil, nil, output.Risk("no_ref_px", "cannot determine a reference price for a market bracket entry in "+mk.Coin)
		}
		slip, serr := resolveSlippage(req.Slippage)
		if serr != nil {
			return nil, nil, serr
		}
		px := mid * (1 + slip)
		if req.Side == Sell {
			px = mid * (1 - slip)
		}
		out, perr := roundPxF(px, mk)
		if perr != nil {
			return nil, nil, perr
		}
		entryPxF, entryTif = out, hl.TifIoc
		marketGuard = marketGuardPx(mid, req.Side == Buy, mk.IsOutcome, slip)
	} else {
		out, perr := roundPxStr(req.Limit, mk)
		if perr != nil {
			return nil, nil, perr
		}
		entryPxF = out
	}

	// Risk gate on the entry exposure (the legs are reduce-only). A marketable
	// limit entry fills at the market, so price the guard at the mid (the wire
	// entryPxF still carries the limit); a market entry's entryPxF is already the
	// slippage estimate.
	refPx := entryPxF
	if isMarket {
		refPx = marketGuard // worst-case fill, not the slippage-limit order price
	} else if c.pricingGuardsActive() {
		refPx = c.marketableGuardPx(ctx, mk.Coin, req.Side, entryPxF)
	}
	notional := refPx * szF
	posNotional := 0.0
	if c.cfg.Risk.MaxPositionNotionalUSD > 0 {
		posNotional = notional + c.currentPositionNotional(ctx, mk.Coin)
	}
	if rerr := c.preTradeChecks(riskCheck{Coin: mk.Coin, IsMarket: isMarket, NotionalUSD: notional, PositionNotionalUSD: posNotional, MinNotionalUSD: c.cfg.Risk.MinOrderNotionalUSD}); rerr != nil {
		return nil, warnings, rerr
	}
	// The bracket ENTRY is the only new-exposure leg (tp/sl are reduce-only).
	if perr := c.checkPortfolioGates(ctx, []exposureDelta{{coin: mk.Coin, signedNotional: signedNotional(req.Side, notional)}}); perr != nil {
		return nil, warnings, perr
	}

	isBuy := req.Side == Buy
	builder, _ := c.resolveBuilderApproved(ctx, nil)
	if builder != nil {
		warnings = append(warnings, fmt.Sprintf("builder fee %.3f%% applied", float64(builder.Fee)/1000.0))
	}

	// [entry, tp?, sl?] — tp/sl are reduce-only triggers on the OPPOSITE side.
	orders := []hl.CreateOrderRequest{{
		Coin: mk.Coin, IsBuy: isBuy, Price: entryPxF, Size: szF, ClientOrderID: &cloid,
		OrderType: hl.OrderType{Limit: &hl.LimitOrderType{Tif: entryTif}},
	}}
	legs := []string{"entry"}
	addLeg := func(pxStr, tpsl string) error {
		f, perr := roundPxStr(pxStr, mk)
		if perr != nil {
			return perr
		}
		orders = append(orders, hl.CreateOrderRequest{
			Coin: mk.Coin, IsBuy: !isBuy, Price: f, Size: szF, ReduceOnly: true,
			OrderType: hl.OrderType{Trigger: &hl.TriggerOrderType{TriggerPx: f, IsMarket: true, Tpsl: hl.Tpsl(tpsl)}},
		})
		legs = append(legs, tpsl)
		return nil
	}
	if req.TP != "" {
		if e := addLeg(req.TP, "tp"); e != nil {
			return nil, warnings, e
		}
	}
	if req.SL != "" {
		if e := addLeg(req.SL, "sl"); e != nil {
			return nil, warnings, e
		}
	}

	if c.opts.DryRun {
		out := make([]*PlaceResult, len(orders))
		for i := range orders {
			out[i] = &PlaceResult{Cloid: cloid, Coin: mk.Coin, Side: legs[i], Size: szOut, Type: "bracket", Status: "dry_run", DryRun: true, ReduceOnly: i > 0}
		}
		return out, warnings, nil
	}

	var resp *hl.APIResponse[hl.OrderResponse]
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		resp, e = ex.BulkOrdersGrouped(ctx, orders, builder, hl.GroupingNormalTpsl, 0)
		return e
	})
	if serr != nil {
		return nil, warnings, mapExchangeErr(serr)
	}
	if resp == nil || len(resp.Data.Statuses) == 0 {
		return nil, warnings, output.Exchange("rejected", "bracket returned no statuses")
	}
	results := make([]*PlaceResult, 0, len(resp.Data.Statuses))
	for i, st := range resp.Data.Statuses {
		label := "leg"
		if i < len(legs) {
			label = legs[i]
		}
		r := &PlaceResult{Cloid: cloid, Coin: mk.Coin, Side: label, Size: szOut, Type: "bracket", ReduceOnly: i > 0}
		if st.Error != nil {
			r.Status = "rejected"
			results = append(results, r)
			continue
		}
		applyStatus(r, st)
		results = append(results, r)
	}
	// Record per-leg status + the entry's fill telemetry, so the trail can tell a
	// filled-entry armed bracket from a fully-rested one (like order/close rows do).
	legStatus := make([]string, len(results))
	for i, r := range results {
		legStatus[i] = r.Status
	}
	bm := map[string]any{"action": "bracket", "cloid": cloid, "coin": mk.Coin, "side": req.Side.String(), "size": szOut, "legs": legs, "leg_status": legStatus}
	if len(results) > 0 {
		bm = withFill(bm, results[0])
	}
	c.audit.Append(bm)
	return results, append(c.signerWarnings(), warnings...), nil
}

// ---------- position-level tp/sl (positionTpsl) ----------

// PositionTpslReq attaches a take-profit and/or stop-loss to an EXISTING position.
// Unlike a bracket (whose tp/sl link to one entry order via normalTpsl), these are
// reduce-only triggers bound to the net position (positionTpsl grouping), so they
// protect it however it was built and regardless of later adds.
type PositionTpslReq struct {
	Coin  string
	TP    string // take-profit trigger price ("" => omit)
	SL    string // stop-loss trigger price ("" => omit)
	Size  string // "" => the whole current position; else a partial (must be <= position)
	Cloid string
}

// PlacePositionTpsl places reduce-only tp/sl triggers on the live position as one
// positionTpsl grouped action. Side and size are derived from the position (a long
// is protected by SELL triggers, a short by BUY); --size, if given, must not exceed
// it. The legs are reduce-only, so no new-exposure/portfolio gate or min-notional
// floor applies (like close, you can always de-risk). Triggers fire as market orders.
func (c *Client) PlacePositionTpsl(ctx context.Context, req PositionTpslReq) ([]*PlaceResult, []string, error) {
	mk, ok := c.meta.Lookup(req.Coin)
	if !ok {
		return nil, nil, unknownCoin(req.Coin)
	}
	if mk.IsSpot {
		return nil, nil, output.Validation("spot_unsupported", "position tp/sl is perp-only (spot has no position)")
	}
	if req.TP == "" && req.SL == "" {
		return nil, nil, output.Validation("no_tpsl", "a position tp/sl needs --tp and/or --sl")
	}
	if c.Halted() {
		return nil, nil, output.Halt("halted", "global halt active — position tp/sl rejected").WithHint("deliverator halt off")
	}
	if err := c.requireQueryAddr(); err != nil {
		return nil, nil, err
	}
	cloid, err := normalizeCloid(req.Cloid)
	if err != nil {
		return nil, nil, err
	}

	// Bind to the live position — it sets both the closing side and the size.
	szi, hasPos := c.positionSzi(ctx, mk.Coin)
	if !hasPos {
		return nil, nil, output.Exchange("no_position", "no open position in "+mk.Coin+" to protect")
	}
	isLong := szi > 0
	closeIsBuy := !isLong // a long is protected by SELL triggers; a short by BUY

	posSz := absF(szi)
	szF := posSz
	if req.Size != "" {
		f, perr := strconv.ParseFloat(req.Size, 64)
		if perr != nil {
			return nil, nil, output.Validation("bad_size", "size is not a number")
		}
		if f <= 0 {
			return nil, nil, output.Validation("bad_size", "size must be positive")
		}
		if f > posSz*(1+1e-9) {
			return nil, nil, output.Validation("size_exceeds_position",
				fmt.Sprintf("--size %s exceeds the open %s position of %s", req.Size, sideWord(isLong), f2s(posSz)))
		}
		szF = f
	}
	szOut, _, err := RoundSize(f2s(szF), mk.SzDecimals)
	if err != nil {
		return nil, nil, output.Validation("bad_size", err.Error())
	}
	szF, _ = strconv.ParseFloat(szOut, 64)
	// RoundSize already rejects a positive size that rounds to 0 at the coin's
	// precision (a dust position), so szF is > 0 here.

	warnings := []string{}
	mark, haveMark := c.midPrice(ctx, mk.Coin)
	builder, _ := c.resolveBuilderApproved(ctx, nil)
	if builder != nil {
		warnings = append(warnings, fmt.Sprintf("builder fee %.3f%% applied", float64(builder.Fee)/1000.0))
	}

	var orders []hl.CreateOrderRequest
	var legs []string
	addLeg := func(pxStr, tpsl string) error {
		f, perr := roundPxStr(pxStr, mk)
		if perr != nil {
			return perr
		}
		// A tp/sl on the wrong side of the mark would trigger immediately — warn,
		// don't reject (HL accepts it, and the operator may intend the level).
		if haveMark {
			wrong := (tpsl == "tp" && ((isLong && f <= mark) || (!isLong && f >= mark))) ||
				(tpsl == "sl" && ((isLong && f >= mark) || (!isLong && f <= mark)))
			if wrong {
				warnings = append(warnings, fmt.Sprintf("%s %s is on the wrong side of mark %s for a %s position — it may trigger immediately",
					strings.ToUpper(tpsl), f2s(f), f2s(mark), sideWord(isLong)))
			}
		}
		orders = append(orders, hl.CreateOrderRequest{
			Coin: mk.Coin, IsBuy: closeIsBuy, Price: f, Size: szF, ReduceOnly: true,
			OrderType: hl.OrderType{Trigger: &hl.TriggerOrderType{TriggerPx: f, IsMarket: true, Tpsl: hl.Tpsl(tpsl)}},
		})
		legs = append(legs, tpsl)
		return nil
	}
	if req.TP != "" {
		if e := addLeg(req.TP, "tp"); e != nil {
			return nil, warnings, e
		}
	}
	if req.SL != "" {
		if e := addLeg(req.SL, "sl"); e != nil {
			return nil, warnings, e
		}
	}
	// One cloid per bulk action (HL rejects a duplicate within one action): tag the
	// first leg, like the bracket tags only its entry. It is the idempotency key to
	// confirm on a timeout via `order status --cloid`.
	orders[0].ClientOrderID = &cloid

	if c.opts.DryRun {
		out := make([]*PlaceResult, len(orders))
		for i := range orders {
			out[i] = &PlaceResult{Cloid: cloid, Coin: mk.Coin, Side: legs[i], Size: szOut, Type: "position_tpsl", Status: "dry_run", DryRun: true, ReduceOnly: true}
		}
		return out, warnings, nil
	}

	var resp *hl.APIResponse[hl.OrderResponse]
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		resp, e = ex.BulkOrdersGrouped(ctx, orders, builder, hl.GroupingPositionTpsl, 0)
		return e
	})
	if serr != nil {
		return nil, warnings, mapExchangeErr(serr)
	}
	if resp == nil || len(resp.Data.Statuses) == 0 {
		return nil, warnings, output.Exchange("rejected", "position tp/sl returned no statuses")
	}
	results := make([]*PlaceResult, 0, len(resp.Data.Statuses))
	for i, st := range resp.Data.Statuses {
		label := "leg"
		if i < len(legs) {
			label = legs[i]
		}
		r := &PlaceResult{Cloid: cloid, Coin: mk.Coin, Side: label, Size: szOut, Type: "position_tpsl", ReduceOnly: true}
		// Echo the builder fee the eventual trigger fill earns, like close/buy/sell.
		if builder != nil {
			r.Builder = &BuilderApplied{Address: builder.Builder, FeeTenthsBps: builder.Fee}
		}
		if st.Error != nil {
			r.Status = "rejected"
			results = append(results, r)
			continue
		}
		applyStatus(r, st)
		results = append(results, r)
	}
	legStatus := make([]string, len(results))
	for i, r := range results {
		legStatus[i] = r.Status
	}
	am := map[string]any{
		"action": "position_tpsl", "cloid": cloid, "coin": mk.Coin,
		"side": sideWord(isLong), "size": szOut, "legs": legs, "leg_status": legStatus,
	}
	// A position tp/sl rests as a trigger, but echo fill telemetry for parity with
	// the other write audit rows (and the rare case a leg fills on arrival).
	if len(results) > 0 {
		am = withFill(am, results[0])
	}
	c.audit.Append(am)
	return results, append(c.signerWarnings(), warnings...), nil
}

// sideWord renders a position direction for messages and audit rows.
func sideWord(isLong bool) string {
	if isLong {
		return "long"
	}
	return "short"
}

// roundPxStr rounds a price string to the market's tick rule, returning a float.
func roundPxStr(px string, mk Market) (float64, error) {
	out, _, err := roundMarketPrice(mk, px)
	if err != nil {
		return 0, output.Validation("bad_price", err.Error())
	}
	f, _ := strconv.ParseFloat(out, 64)
	return f, nil
}

// roundPxF rounds a float price to the market's tick rule.
func roundPxF(px float64, mk Market) (float64, error) {
	return roundPxStr(strconv.FormatFloat(px, 'f', -1, 64), mk)
}

// ---------- batch (N independent orders in one signed action) ----------

// maxBatchOrders bounds a single batch action. HL tolerates large bulk actions,
// but a CLI batch this big is almost always a mistake; reject it loudly.
const maxBatchOrders = 100

// batchLegErr prefixes a pre-flight leg failure with its order index, preserving
// the error's category/code/hint so the exit code and remediation are unchanged.
func batchLegErr(i int, err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		ne := output.NewError(oe.Category, oe.Code, fmt.Sprintf("order %d: %s", i, oe.Message))
		if oe.Hint != "" {
			ne.WithHint(oe.Hint)
		}
		return ne
	}
	return fmt.Errorf("order %d: %w", i, err)
}

// PlaceBatch submits up to maxBatchOrders independent orders (grouping "na") in
// ONE signed action — one nonce, one builder fee, one rate-cap charge. Local
// validation is ATOMIC and pre-flight: if any leg fails rounding or the risk
// gauntlet, NOTHING is signed. Hyperliquid may still reject individual legs at
// submit; those come back as per-leg results with status "rejected" (the action
// itself succeeds), leaving the caller to decide the exit code (all-rejected vs
// partial vs ok).
func (c *Client) PlaceBatch(ctx context.Context, reqs []OrderReq) ([]*PlaceResult, []string, error) {
	if len(reqs) == 0 {
		return nil, nil, output.Validation("empty_batch", "batch has no orders")
	}
	if len(reqs) > maxBatchOrders {
		return nil, nil, output.Validation("batch_too_large",
			fmt.Sprintf("batch has %d orders; max %d per action", len(reqs), maxBatchOrders)).
			WithHint(fmt.Sprintf("split into batches of <= %d", maxBatchOrders))
	}

	warnings := []string{}
	orders := make([]hl.CreateOrderRequest, 0, len(reqs))
	results := make([]*PlaceResult, 0, len(reqs))
	posSeed := map[string]float64{} // running resulting-position notional per coin
	seenCloid := map[string]bool{}
	var builder *hl.BuilderInfo
	builderResolved := false
	anyEarned := false                  // any leg that actually earns the builder fee (not a spot buy)
	var portfolioDeltas []exposureDelta // aggregate new exposure across non-reduce-only legs (#43)

	for i, req := range reqs {
		mk, ok := c.meta.Lookup(req.Coin)
		if !ok {
			return nil, warnings, batchLegErr(i, unknownCoin(req.Coin))
		}
		req, nerr := c.applyNotional(ctx, mk, req) // --notional per leg (#50)
		if nerr != nil {
			return nil, warnings, batchLegErr(i, nerr)
		}
		ro, err := c.roundOrderReq(mk, req)
		if err != nil {
			return nil, warnings, batchLegErr(i, err)
		}
		for _, w := range ro.warnings {
			warnings = append(warnings, fmt.Sprintf("order %d: %s", i, w))
		}
		if seenCloid[ro.cloid] {
			return nil, warnings, batchLegErr(i, output.Validation("dup_cloid",
				"duplicate client order id "+ro.cloid+" in batch"))
		}
		seenCloid[ro.cloid] = true

		// Price each leg. A market leg becomes a marketable IOC at the slippage
		// price — a bulk action cannot carry a bare market order.
		pxF := ro.limitF
		limitStr := ro.limitOut
		tif := tifOf(req.Tif)
		marketGuard := 0.0
		if ro.isMarket {
			mid, hasMid := c.midPrice(ctx, mk.Coin)
			if !hasMid {
				return nil, warnings, batchLegErr(i, output.Risk("no_ref_px",
					"cannot price a market leg in "+mk.Coin+" — no reference price available"))
			}
			slip, serr := resolveSlippage(req.Slippage)
			if serr != nil {
				return nil, warnings, batchLegErr(i, serr)
			}
			mpx := mid * (1 + slip)
			if req.Side == Sell {
				mpx = mid * (1 - slip)
			}
			out, perr := roundPxF(mpx, mk)
			if perr != nil {
				return nil, warnings, batchLegErr(i, perr)
			}
			pxF, tif = out, hl.TifIoc
			limitStr = strconv.FormatFloat(pxF, 'f', -1, 64)
			marketGuard = marketGuardPx(mid, req.Side == Buy, mk.IsOutcome, slip)
		}

		// Notional + risk: per-leg floor / order cap, cumulative position cap across
		// same-coin legs (seed each coin's current notional once). Reduce-only legs
		// are exempt from the notional guards.
		notional, posNotional := 0.0, 0.0
		if !req.ReduceOnly {
			// A marketable (crossing) limit leg fills at the market — price the guard
			// at the mid, same as single Place (a market leg's pxF is already the
			// slippage estimate; a trigger keeps its limit). Else the cap is
			// bypassable / the floor false-rejects by routing through batch/grid.
			refPx := pxF
			if ro.isMarket {
				refPx = marketGuard // worst-case fill, not the slippage-limit order price
			} else if req.Trigger == nil && c.pricingGuardsActive() {
				refPx = c.marketableGuardPx(ctx, mk.Coin, req.Side, pxF)
			}
			notional = refPx * ro.szF
			if _, seeded := posSeed[mk.Coin]; !seeded {
				posSeed[mk.Coin] = c.currentPositionNotional(ctx, mk.Coin)
			}
			posSeed[mk.Coin] += notional
			posNotional = posSeed[mk.Coin]
			portfolioDeltas = append(portfolioDeltas, exposureDelta{coin: mk.Coin, signedNotional: signedNotional(req.Side, notional)})
		} else if ferr := c.reduceOnlyFlipErr(ctx, mk.Coin, ro.szF); ferr != nil {
			return nil, warnings, batchLegErr(i, ferr)
		}
		if rerr := c.staticChecks(riskCheck{Coin: mk.Coin, IsMarket: ro.isMarket, NotionalUSD: notional, PositionNotionalUSD: posNotional, MinNotionalUSD: c.cfg.Risk.MinOrderNotionalUSD, ReduceOnly: req.ReduceOnly}); rerr != nil {
			return nil, warnings, batchLegErr(i, rerr)
		}

		// The builder fee is action-level on the wire: resolve it once (config policy,
		// or the first leg's explicit override). The per-leg "applied / not earned"
		// reporting happens below — a spot BUY leg earns no fee even though the wire
		// field is shared across the mixed action.
		if !builderResolved {
			var bwarn string
			builder, bwarn = c.resolveBuilderApproved(ctx, req.BuilderFee)
			if bwarn != "" {
				warnings = append(warnings, bwarn)
			}
			builderResolved = true
		}

		cloid := ro.cloid
		cr := hl.CreateOrderRequest{
			Coin: mk.Coin, IsBuy: req.Side == Buy, Price: pxF, Size: ro.szF,
			ReduceOnly: req.ReduceOnly, ClientOrderID: &cloid,
		}
		if req.Trigger != nil {
			cr.OrderType = hl.OrderType{Trigger: &hl.TriggerOrderType{
				TriggerPx: ro.triggerF, IsMarket: req.Trigger.IsMarket, Tpsl: hl.Tpsl(req.Trigger.Tpsl),
			}}
		} else {
			cr.OrderType = hl.OrderType{Limit: &hl.LimitOrderType{Tif: tif}}
		}
		orders = append(orders, cr)

		res := &PlaceResult{
			Cloid: ro.cloid, Coin: mk.Coin, Side: req.Side.String(),
			Size: ro.szOut, LimitPx: limitStr, TriggerPx: ro.triggerOut, Type: ro.typ, ReduceOnly: req.ReduceOnly,
		}
		if !ro.rounding.Empty() {
			res.Rounded = ro.rounding
		}
		if builder != nil {
			// A spot BUY earns no builder fee (HL takes the taker fee in the base
			// token), even inside a mixed batch where the wire carries the field —
			// so don't claim it on this leg. Parity with single-order Place.
			if mk.IsSpot && req.Side == Buy {
				warnings = append(warnings, fmt.Sprintf("order %d: builder fee NOT earned on a spot BUY (Hyperliquid charges the taker fee in the base token; spot sells and perps do earn it)", i))
			} else {
				res.Builder = &BuilderApplied{Address: builder.Builder, FeeTenthsBps: builder.Fee}
				anyEarned = true
			}
		}
		results = append(results, res)
	}

	// One "builder fee applied" note for the action, only if at least one leg
	// actually earns it (an all-spot-buy batch earns none — don't claim it).
	if builder != nil && anyEarned {
		warnings = append(warnings, fmt.Sprintf("builder fee %.3f%% applied", float64(builder.Fee)/1000.0))
	}

	// Account-wide gates run ONCE against the aggregate new exposure of the whole
	// batch (a grid is a batch), so a book-leverage/net-exposure cap can't be
	// tunneled under by fragmenting one position across many legs.
	if perr := c.checkPortfolioGates(ctx, portfolioDeltas); perr != nil {
		return nil, warnings, perr
	}

	// All legs passed pre-flight. One signed action = one rate-cap charge — taken
	// now, AFTER atomic validation, so a rejected batch burns nothing (parity with
	// Place, whose preTradeChecks charges the cap only after staticChecks pass).
	// Charged before the dry-run short-circuit, matching Place's dry-run behavior.
	if err := c.checkRateCap(); err != nil {
		return nil, warnings, err
	}

	// Order priority is action-level, so a batch carries one rate (the config
	// default — there is no per-leg priority on the wire). HL requires priority
	// orders to be IOC, so apply it only when every leg is IOC; otherwise drop the
	// config default (don't fail a GTC/ALO batch over a global priority setting).
	priorityRate, prioWarn := c.resolvePriority(nil)
	if prioWarn != "" {
		warnings = append(warnings, prioWarn)
	}
	if priorityRate > 0 {
		allIOC := true
		for _, o := range orders {
			if o.OrderType.Limit == nil || o.OrderType.Limit.Tif != hl.TifIoc {
				allIOC = false
				break
			}
		}
		if allIOC {
			warnings = append(warnings, fmt.Sprintf("order priority %d bps applied to the batch (fee paid in HYPE)", priorityRate/bpsToPriorityRate))
		} else {
			warnings = append(warnings, "order priority skipped: it only applies when every batch leg is IOC")
			priorityRate = 0
		}
	}

	if c.opts.DryRun {
		for _, r := range results {
			r.DryRun = true
			r.Status = "dry_run"
		}
		return results, warnings, nil
	}

	var resp *hl.APIResponse[hl.OrderResponse]
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		resp, e = ex.BulkOrdersGrouped(ctx, orders, builder, hl.GroupingNA, priorityRate)
		return e
	})
	// A non-nil response with statuses → map them (incl. partial per-leg rejects,
	// which BulkOrdersGrouped surfaces by returning a non-nil serr WITH full
	// statuses). An empty/absent statuses list is a hard, all-or-nothing failure:
	// a network/signing error (serr != nil), or HL rejecting the WHOLE action at
	// the envelope level ({"status":"err","response":"<reason>"} → Ok=false, Err
	// set, no statuses), or an ok-but-empty body. Map serr if present; otherwise
	// surface resp.Err (the action reason) — never deref a nil serr.
	if resp == nil || len(resp.Data.Statuses) == 0 {
		if serr != nil {
			return nil, warnings, mapExchangeErr(serr)
		}
		msg := "batch returned no statuses"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return nil, warnings, mapOrderReject(msg)
	}
	// Map each status back to its leg. HL returns 1:1 statuses for GroupingNA; if
	// it ever returns FEWER, the unmatched trailing legs are flagged "unknown"
	// rather than left blank and silently read as success.
	statuses := resp.Data.Statuses
	for i, r := range results {
		switch {
		case i >= len(statuses):
			r.Status = "unknown"
			r.Error = "no status returned by exchange for this leg"
		case statuses[i].Error != nil:
			r.Status = "rejected"
			r.Error = *statuses[i].Error
		default:
			applyStatus(r, statuses[i])
		}
	}
	c.audit.Append(map[string]any{"action": "batch", "orders": len(orders), "legs": auditLegs(results)})
	return results, append(c.signerWarnings(), warnings...), nil
}

// GridReq describes a ladder of evenly-spaced limit orders on one coin.
type GridReq struct {
	Coin       string
	Side       Side
	Levels     int
	FromPx     string // price of the first level
	ToPx       string // price of the last level (inclusive)
	TotalSize  string // split evenly across levels
	Tif        string
	ReduceOnly bool
}

// BuildGrid expands a grid into Levels OrderReqs: prices evenly spaced from FromPx
// to ToPx (inclusive), TotalSize split evenly. Per-leg tick/lot rounding and the
// risk gauntlet (incl. the min-order floor) are applied later by PlaceBatch — so
// a level whose slice falls below the $10 minimum is rejected there, loudly.
func (c *Client) BuildGrid(req GridReq) ([]OrderReq, error) {
	if req.Levels < 1 {
		return nil, output.Validation("bad_levels", "grid needs --levels >= 1")
	}
	if req.Levels > maxBatchOrders {
		return nil, output.Validation("too_many_levels",
			fmt.Sprintf("grid has %d levels; max %d", req.Levels, maxBatchOrders))
	}
	from, ferr := strconv.ParseFloat(req.FromPx, 64)
	to, terr := strconv.ParseFloat(req.ToPx, 64)
	total, serr := strconv.ParseFloat(req.TotalSize, 64)
	if ferr != nil || terr != nil || from <= 0 || to <= 0 {
		return nil, output.Validation("bad_price", "--from and --to must be positive numbers")
	}
	if serr != nil || total <= 0 {
		return nil, output.Validation("bad_size", "--size must be a positive number")
	}
	per := strconv.FormatFloat(total/float64(req.Levels), 'f', -1, 64)
	out := make([]OrderReq, 0, req.Levels)
	for i := 0; i < req.Levels; i++ {
		px := from
		if req.Levels > 1 {
			px = from + (to-from)*float64(i)/float64(req.Levels-1)
		}
		out = append(out, OrderReq{
			Coin: req.Coin, Side: req.Side, Size: per,
			Limit: strconv.FormatFloat(px, 'f', -1, 64), Tif: req.Tif, ReduceOnly: req.ReduceOnly,
		})
	}
	return out, nil
}

// ---------- close ----------

// Close flattens (or reduces) a position. Market close uses MarketClose; a limit
// close places a reduce-only limit on the opposite side of the current position.
func (c *Client) Close(ctx context.Context, coin, size string, market bool, limit, cloidIn string) (*PlaceResult, []string, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, nil, unknownCoin(coin)
	}
	cloid, err := normalizeCloid(cloidIn)
	if err != nil {
		return nil, nil, err
	}
	if c.Halted() {
		return nil, nil, output.Halt("halted", "global halt active — close rejected").WithHint("deliverator halt off")
	}

	// A HIP-4 outcome "close" sells the held Yes/No side back (a token balance, no
	// reduce-only concept) — like a spot close.
	if mk.IsOutcome {
		return c.closeOutcome(ctx, mk, size, market, limit, cloid)
	}

	// Spot has no position / reduce-only: a "close" sells the base token for USDC.
	if mk.IsSpot {
		return c.closeSpot(ctx, mk, size, market, limit, cloid)
	}

	// Limit close: derive side from the live position, place reduce-only limit.
	if !market && limit != "" {
		szi, hasPos := c.positionSzi(ctx, mk.Coin)
		if !hasPos {
			return nil, nil, output.Exchange("no_position", "no open position in "+mk.Coin+" to close")
		}
		side := Sell
		if szi < 0 {
			side = Buy
		}
		sz := size
		if sz == "" {
			sz = strconv.FormatFloat(absF(szi), 'f', -1, 64)
		}
		return c.Place(ctx, OrderReq{Coin: mk.Coin, Side: side, Size: sz, Limit: limit, ReduceOnly: true, Cloid: cloid})
	}

	// Market close via the internal/hl MarketClose helper.
	var szPtr *float64
	if size != "" {
		f, perr := strconv.ParseFloat(size, 64)
		if perr != nil {
			return nil, nil, output.Validation("bad_size", "size is not a number")
		}
		szPtr = &f
	}
	res := &PlaceResult{Cloid: cloid, Coin: mk.Coin, Side: "close", Type: "market", ReduceOnly: true}
	if size != "" {
		res.Size = size
	} else if szi, ok := c.positionSzi(ctx, mk.Coin); ok {
		// Record the size being flattened so a partial fill is detectable (exit 60).
		res.Size = strconv.FormatFloat(absF(szi), 'f', -1, 64)
	}
	// A close charges the builder fee too (the fill carries it); echo the builder
	// on the result like buy/sell so the operator can see the close earns revenue.
	builder, _ := c.resolveBuilderApproved(ctx, nil)
	var warnings []string
	if builder != nil {
		res.Builder = &BuilderApplied{Address: builder.Builder, FeeTenthsBps: builder.Fee}
		warnings = append(warnings, fmt.Sprintf("builder fee %.3f%% applied", float64(builder.Fee)/1000.0))
	}
	if c.opts.DryRun {
		res.DryRun = true
		res.Status = "dry_run"
		return res, warnings, nil
	}
	var st hl.OrderStatus
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		st, e = ex.MarketClose(ctx, mk.Coin, szPtr, nil, hl.DefaultSlippage, &cloid, builder)
		return e
	})
	if st.Error != nil {
		return nil, warnings, mapOrderReject(*st.Error)
	}
	if serr != nil {
		return nil, warnings, mapExchangeErr(serr)
	}
	applyStatus(res, st)
	c.audit.Append(withFill(map[string]any{"action": "close", "cloid": cloid, "coin": mk.Coin, "size": res.Size, "status": res.Status, "oid": res.Oid}, res))
	return res, append(c.signerWarnings(), warnings...), nil
}

// closeSpot exits a spot holding by SELLING the base token for USDC. Spot has no
// position/reduce-only concept, so the sell size comes from the (Total − Hold)
// balance and the order is a plain sell — subject to the full risk gauntlet,
// including the $10 floor (HL rejects sub-minimum spot orders too).
func (c *Client) closeSpot(ctx context.Context, mk Market, size string, market bool, limit, cloid string) (*PlaceResult, []string, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, nil, err
	}
	sz := size
	if sz == "" {
		base, ok := c.meta.SpotBaseToken(mk.Coin)
		if !ok {
			return nil, nil, output.Validation("spot_base_unknown", "cannot resolve the base token of "+mk.Coin)
		}
		st, err := c.info.SpotUserState(ctx, c.queryAddr)
		if err != nil {
			return nil, nil, mapNetwork("spot_user_state", err)
		}
		var sellable decimal.Decimal
		found := false
		for _, b := range st.Balances {
			if b.Token == base {
				total, _ := decimal.NewFromString(b.Total)
				hold, _ := decimal.NewFromString(b.Hold)
				sellable = total.Sub(hold)
				found = true
				break
			}
		}
		// FLOOR to the lot size — never round UP, which would request more than the
		// balance and get rejected. (RoundSize in Place rounds half-up, so the size
		// must already be a sellable, lot-aligned amount before it gets there.)
		sellable = sellable.Truncate(int32(mk.SzDecimals))
		if !found || !sellable.IsPositive() {
			return nil, nil, output.Exchange("no_spot_balance", "no sellable balance to close in "+mk.Coin).
				WithHint("(Total − Hold), floored to the lot size, is 0 — a resting order may be holding it, or it's dust below one lot")
		}
		sz = sellable.String()
	}
	req := OrderReq{Coin: mk.Coin, Side: Sell, Size: sz, Cloid: cloid, Closing: true}
	if !market {
		req.Limit = limit
	}
	return c.Place(ctx, req)
}

// closeOutcome exits a HIP-4 outcome holding by SELLING the held side back. The
// holding is a "+<enc>" token balance (no perp position / reduce-only concept), so
// the sell size is its (Total − Hold), and the order is a plain Closing sell —
// exempt from the new-exposure guards but still subject to the $10 floor (HL rejects
// sub-minimum orders, so a tiny remaining bet can only settle at expiry).
func (c *Client) closeOutcome(ctx context.Context, mk Market, size string, market bool, limit, cloid string) (*PlaceResult, []string, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, nil, err
	}
	sz := size
	if sz == "" {
		tok := "+" + strings.TrimPrefix(mk.Coin, "#") // "#6410" -> "+6410"
		st, err := c.info.SpotUserState(ctx, c.queryAddr)
		if err != nil {
			return nil, nil, mapNetwork("spot_user_state", err)
		}
		var sellable decimal.Decimal
		found := false
		for _, b := range st.Balances {
			if strings.EqualFold(b.Coin, tok) {
				total, _ := decimal.NewFromString(b.Total)
				hold, _ := decimal.NewFromString(b.Hold)
				sellable = total.Sub(hold)
				found = true
				break
			}
		}
		sellable = sellable.Truncate(0) // outcome shares are integers (szDecimals 0)
		if !found || !sellable.IsPositive() {
			return nil, nil, output.Exchange("no_outcome_balance", "no sellable balance to close in "+mk.Coin).
				WithHint("(Total − Hold) for " + tok + " is 0 — a resting order may be holding it, or it has settled")
		}
		sz = sellable.String()
	}
	req := OrderReq{Coin: mk.Coin, Side: Sell, Size: sz, Cloid: cloid, Closing: true}
	if !market {
		req.Limit = limit
	}
	return c.Place(ctx, req)
}

// ---------- cancel ----------

// CancelReq selects orders to cancel.
type CancelReq struct {
	Oid    *int64
	Cloid  string
	Oids   []int64  // batch cancel by oid list (one signed action)
	Cloids []string // batch cancel by cloid list (one signed action)
	All    bool
	Coin   string
}

// CancelResult reports cancellations.
type CancelResult struct {
	Canceled int          `json:"canceled"`
	Oids     []int64      `json:"oids,omitempty"`
	Cloids   []string     `json:"cloids,omitempty"`
	Failed   []CancelFail `json:"failed,omitempty"` // legs that did not cancel (e.g. already gone)
	DryRun   bool         `json:"dry_run,omitempty"`
}

// CancelFail is one leg of a batch cancel that did not cancel.
type CancelFail struct {
	Oid   *int64 `json:"oid,omitempty"`
	Cloid string `json:"cloid,omitempty"`
	Error string `json:"error"`
}

// Cancel cancels by oid, by cloid, or all (optionally per coin). Batches (§7).
// Only the paths that must READ open orders (--all, or --cloid without --coin)
// need the master address; a plain oid+coin cancel is a pure agent-signed write.
func (c *Client) Cancel(ctx context.Context, req CancelReq) (*CancelResult, error) {
	if len(req.Oids) > 0 || len(req.Cloids) > 0 {
		return c.cancelBatch(ctx, req)
	}
	res := &CancelResult{}

	if req.All {
		if err := c.requireQueryAddr(); err != nil {
			return nil, err
		}
		orders, err := c.allOpenOrders(ctx)
		if err != nil {
			return nil, mapNetwork("open_orders", err)
		}
		var reqs []hl.CancelOrderRequest
		for _, o := range orders {
			if req.Coin != "" && !strings.EqualFold(o.Coin, req.Coin) {
				continue
			}
			reqs = append(reqs, hl.CancelOrderRequest{Coin: o.Coin, OrderID: o.Oid})
			res.Oids = append(res.Oids, o.Oid)
		}
		res.Canceled = len(reqs)
		if c.opts.DryRun {
			res.DryRun = true
			return res, nil
		}
		if len(reqs) == 0 {
			return res, nil
		}
		serr := c.signed(ctx, func(ex *hl.Exchange) error {
			_, e := ex.BulkCancel(ctx, reqs)
			return e
		})
		if serr != nil {
			return nil, mapExchangeErr(serr)
		}
		c.audit.Append(map[string]any{"action": "cancel_all", "coin": req.Coin, "canceled": res.Canceled, "oids": res.Oids})
		return res, nil
	}

	// Resolve the coin for a single cancel if not supplied (needs a read).
	coin := req.Coin
	if coin == "" {
		if err := c.requireQueryAddr(); err != nil {
			return nil, err
		}
		var found bool
		coin, found = c.resolveOrderCoin(ctx, req.Oid, req.Cloid)
		if !found {
			return nil, output.Validation("order_not_found", "could not find the order to resolve its coin").
				WithHint("pass --coin, or the order may already be gone")
		}
	}
	if c.opts.DryRun {
		res.DryRun = true
		res.Canceled = 1
		if req.Oid != nil {
			res.Oids = []int64{*req.Oid}
		}
		if req.Cloid != "" {
			res.Cloids = []string{req.Cloid}
		}
		return res, nil
	}
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		if req.Oid != nil {
			_, e := ex.Cancel(ctx, coin, *req.Oid)
			return e
		}
		cl, err := normalizeCloid(req.Cloid)
		if err != nil {
			return err
		}
		_, e := ex.CancelByCloid(ctx, coin, cl)
		return e
	})
	if serr != nil {
		return nil, mapExchangeErr(serr)
	}
	res.Canceled = 1
	if req.Oid != nil {
		res.Oids = []int64{*req.Oid}
	}
	if req.Cloid != "" {
		res.Cloids = []string{req.Cloid}
	}
	c.audit.Append(map[string]any{"action": "cancel", "coin": coin, "oid": req.Oid, "cloid": req.Cloid})
	return res, nil
}

// cancelBatch cancels a caller-supplied list of oids OR cloids in ONE signed
// action (not both — they are different action types). Coins are resolved from a
// single open-orders read unless --coin pins them; an oid/cloid no longer resting
// is reported in Failed (already filled/canceled) rather than failing the batch.
func (c *Client) cancelBatch(ctx context.Context, req CancelReq) (*CancelResult, error) {
	if req.Oid != nil || req.Cloid != "" {
		return nil, output.Validation("mixed_cancel",
			"pass a single --oid/--cloid OR a batch --oids/--cloids, not both")
	}
	if len(req.Oids) > 0 && len(req.Cloids) > 0 {
		return nil, output.Validation("mixed_cancel",
			"cancel a batch by --oids OR --cloids, not both (they are different signed actions)")
	}
	res := &CancelResult{}

	// Resolve coins. --coin pins every leg (no read); otherwise read open orders
	// once and map oid/cloid -> coin. A leg absent from the book is already gone.
	oidCoin := map[int64]string{}
	cloidCoin := map[string]string{}
	if req.Coin == "" {
		if err := c.requireQueryAddr(); err != nil {
			return nil, err
		}
		orders, err := c.allOpenOrders(ctx)
		if err != nil {
			return nil, mapNetwork("open_orders", err)
		}
		for _, o := range orders {
			oidCoin[o.Oid] = o.Coin
			if o.Cloid != nil {
				cloidCoin[strings.ToLower(*o.Cloid)] = o.Coin
			}
		}
	}

	if len(req.Oids) > 0 {
		var creqs []hl.CancelOrderRequest
		var sent []int64
		for _, oid := range req.Oids {
			o := oid
			coin := req.Coin
			if coin == "" {
				cc, ok := oidCoin[oid]
				if !ok {
					res.Failed = append(res.Failed, CancelFail{Oid: &o, Error: "order not found (already filled/canceled?)"})
					continue
				}
				coin = cc
			}
			// Validate the asset against the SAME source BulkCancel uses, so one
			// unknown coin fails only its leg rather than aborting the whole action.
			if _, ok := c.info.CoinToAsset(coin); !ok {
				res.Failed = append(res.Failed, CancelFail{Oid: &o, Error: "unknown asset for coin " + coin})
				continue
			}
			creqs = append(creqs, hl.CancelOrderRequest{Coin: coin, OrderID: oid})
			sent = append(sent, oid)
		}
		if c.opts.DryRun {
			res.DryRun, res.Oids, res.Canceled = true, sent, len(sent)
			return res, nil
		}
		if len(creqs) == 0 {
			return res, nil
		}
		var resp *hl.APIResponse[hl.CancelOrderResponse]
		serr := c.signed(ctx, func(ex *hl.Exchange) error {
			var e error
			resp, e = ex.BulkCancel(ctx, creqs)
			return e
		})
		if resp == nil || len(resp.Data.Statuses) == 0 {
			if serr != nil {
				return nil, mapExchangeErr(serr)
			}
			return nil, output.Exchange("cancel_failed", "batch cancel returned no statuses")
		}
		for i, oid := range sent {
			o := oid
			// A leg with no returned status is UNCONFIRMED — report it, never count
			// it canceled (HL returns 1:1, but a short array must not over-report).
			if i >= len(resp.Data.Statuses) {
				res.Failed = append(res.Failed, CancelFail{Oid: &o, Error: "no status returned (cancel unconfirmed)"})
				continue
			}
			if msg := cancelLegError(resp.Data.Statuses[i]); msg != "" {
				res.Failed = append(res.Failed, CancelFail{Oid: &o, Error: msg})
				continue
			}
			res.Oids = append(res.Oids, oid)
		}
		res.Canceled = len(res.Oids)
		c.audit.Append(map[string]any{"action": "cancel_batch", "by": "oid", "canceled": res.Canceled, "oids": res.Oids, "failed": failedIDs(res.Failed)})
		return res, nil
	}

	// cloid list
	var creqs []hl.CancelOrderRequestByCloid
	var sent []string
	for _, raw := range req.Cloids {
		// Reject an empty cloid HERE — normalizeCloid("") generates a RANDOM cloid
		// (for the place path), which would otherwise be signed as a phantom cancel.
		if strings.TrimSpace(raw) == "" {
			return nil, output.Validation("bad_cloid", "empty cloid in --cloids list")
		}
		cl, err := normalizeCloid(raw)
		if err != nil {
			return nil, output.Validation("bad_cloid", err.Error())
		}
		coin := req.Coin
		if coin == "" {
			cc, ok := cloidCoin[cl]
			if !ok {
				res.Failed = append(res.Failed, CancelFail{Cloid: cl, Error: "order not found (already filled/canceled?)"})
				continue
			}
			coin = cc
		}
		if _, ok := c.info.CoinToAsset(coin); !ok {
			res.Failed = append(res.Failed, CancelFail{Cloid: cl, Error: "unknown asset for coin " + coin})
			continue
		}
		creqs = append(creqs, hl.CancelOrderRequestByCloid{Coin: coin, Cloid: cl})
		sent = append(sent, cl)
	}
	if c.opts.DryRun {
		res.DryRun, res.Cloids, res.Canceled = true, sent, len(sent)
		return res, nil
	}
	if len(creqs) == 0 {
		return res, nil
	}
	var resp *hl.APIResponse[hl.CancelOrderResponse]
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		resp, e = ex.BulkCancelByCloids(ctx, creqs)
		return e
	})
	if resp == nil || len(resp.Data.Statuses) == 0 {
		if serr != nil {
			return nil, mapExchangeErr(serr)
		}
		return nil, output.Exchange("cancel_failed", "batch cancel returned no statuses")
	}
	for i, cl := range sent {
		if i >= len(resp.Data.Statuses) {
			res.Failed = append(res.Failed, CancelFail{Cloid: cl, Error: "no status returned (cancel unconfirmed)"})
			continue
		}
		if msg := cancelLegError(resp.Data.Statuses[i]); msg != "" {
			res.Failed = append(res.Failed, CancelFail{Cloid: cl, Error: msg})
			continue
		}
		res.Cloids = append(res.Cloids, cl)
	}
	res.Canceled = len(res.Cloids)
	c.audit.Append(map[string]any{"action": "cancel_batch", "by": "cloid", "canceled": res.Canceled, "cloids": res.Cloids, "failed": failedIDs(res.Failed)})
	return res, nil
}

// failedIDs distills cancel failures into compact audit records (the identifier
// that failed + why), so the trail names WHICH cancels missed — not just how many.
func failedIDs(fails []CancelFail) []map[string]any {
	out := make([]map[string]any, len(fails))
	for i, f := range fails {
		m := map[string]any{"error": f.Error}
		if f.Oid != nil {
			m["oid"] = *f.Oid
		}
		if f.Cloid != "" {
			m["cloid"] = f.Cloid
		}
		out[i] = m
	}
	return out
}

// cancelLegError returns the error message for one cancel status leg, or "" if it
// canceled successfully ("success").
func cancelLegError(mv hl.MixedValue) string {
	if s, ok := mv.String(); ok {
		if s == "success" {
			return ""
		}
		return s
	}
	if obj, ok := mv.Object(); ok {
		if v, ok := obj["error"]; ok {
			if msg, ok := v.(string); ok && msg != "" {
				return msg
			}
		}
	}
	return "cancel failed"
}

// ---------- modify ----------

// Modify changes the size and/or limit price of a resting order. The existing
// order is fetched to recover its coin/side/tif.
func (c *Client) Modify(ctx context.Context, oid *int64, cloid, newSize, newLimit string) (*PlaceResult, []string, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, nil, err
	}
	// Normalize the cloid to its canonical 0x+lowercase form before matching, so
	// findResting (which compares against frontendOpenOrders) and the rebuilt
	// order agree — a bare 32-hex cloid is valid input but would otherwise miss.
	if oid == nil && cloid != "" {
		norm, err := normalizeCloid(cloid)
		if err != nil {
			return nil, nil, output.Validation("bad_cloid", err.Error()).
				WithHint("cloid must be 0x + 32 hex chars")
		}
		cloid = norm
	}
	existing, ok := c.findResting(ctx, oid, cloid)
	if !ok {
		return nil, nil, output.Validation("order_not_found", "no resting order matches that oid/cloid").
			WithHint("list with `deliverator orders`")
	}
	// A modify rebuilds the order as a plain limit; HL rejects modifying a
	// trigger order anyway ("Attempted to modify trigger order"). Refuse early
	// with an actionable error instead of burning a nonce on a doomed action.
	if existing.IsTrigger {
		return nil, nil, output.Validation("modify_trigger_unsupported",
			"cannot modify a trigger order in place — its trigger condition can't be changed via modify").
			WithHint("cancel the order and place a new trigger")
	}
	mk, ok := c.meta.Lookup(existing.Coin)
	if !ok {
		return nil, nil, unknownCoin(existing.Coin)
	}
	side := existing.Side
	size := newSize
	if size == "" {
		size = existing.OrigSz
	}
	px := newLimit
	if px == "" {
		px = existing.LimitPx
	}
	szOut, szChanged, err := RoundSize(size, mk.SzDecimals)
	if err != nil {
		return nil, nil, output.Validation("bad_size", err.Error())
	}
	pxOut, pxChanged, err := roundMarketPrice(mk, px)
	if err != nil {
		return nil, nil, output.Validation("bad_price", err.Error())
	}
	warnings := []string{}
	// HL's modify action has no builder field: a modify cancels the order and
	// re-places it WITHOUT the builder, so a previously fee-bearing order stops
	// earning. Warn when a builder would otherwise apply.
	if b, _ := c.resolveBuilderApproved(ctx, nil); b != nil {
		warnings = append(warnings, "builder fee dropped: Hyperliquid does not support builder fees on modify — the replacement order carries no fee")
	}
	rounding := &Rounding{}
	if szChanged {
		rounding.Sz = &FromTo{From: size, To: szOut}
		warnings = append(warnings, fmt.Sprintf("size rounded %s -> %s", size, szOut))
	}
	if pxChanged {
		rounding.Px = &FromTo{From: px, To: pxOut}
		warnings = append(warnings, fmt.Sprintf("price rounded %s -> %s", px, pxOut))
	}
	szF, _ := strconv.ParseFloat(szOut, 64)
	pxF, _ := strconv.ParseFloat(pxOut, 64)

	// A modify can grow a resting order to any size/price — gate it with the
	// same gauntlet Place uses (halt, allowlist, notional caps, rate cap). A
	// reduce-only order can't increase exposure, so it skips the notional caps.
	notional, posNotional := 0.0, 0.0
	if !existing.ReduceOnly {
		// A modify to a crossing limit fills at the market — price the guard at the
		// mid (the wire pxF still carries the new limit), same as Place/PlaceBatch.
		refPx := pxF
		if c.pricingGuardsActive() {
			refPx = c.marketableGuardPx(ctx, mk.Coin, side, pxF)
		}
		notional = refPx * szF
		if c.cfg.Risk.MaxPositionNotionalUSD > 0 {
			posNotional = notional + c.currentPositionNotional(ctx, mk.Coin)
		}
	}
	if rerr := c.preTradeChecks(riskCheck{Coin: mk.Coin, IsMarket: false, NotionalUSD: notional, PositionNotionalUSD: posNotional, MinNotionalUSD: c.cfg.Risk.MinOrderNotionalUSD, ReduceOnly: existing.ReduceOnly}); rerr != nil {
		return nil, warnings, rerr
	}

	// Preserve the order's client id across the modify. A modify cancels the old
	// order and places a replacement; without re-attaching the cloid the new
	// order would carry none, silently breaking status/cancel-by-cloid and the
	// timeout-retry protocol — the whole idempotency contract. Carry the existing
	// order's cloid; when modifying by cloid, fall back to that identifier.
	preservedCloid := existing.Cloid
	if preservedCloid == "" && cloid != "" {
		preservedCloid = cloid
	}

	res := &PlaceResult{Cloid: preservedCloid, Coin: mk.Coin, Side: side.String(), Size: szOut, LimitPx: pxOut, Type: "limit", ReduceOnly: existing.ReduceOnly}
	if !rounding.Empty() {
		res.Rounded = rounding
	}
	if c.opts.DryRun {
		res.DryRun = true
		res.Status = "dry_run"
		return res, warnings, nil
	}
	var st hl.OrderStatus
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		newOrder := hl.CreateOrderRequest{
			Coin: mk.Coin, IsBuy: side == Buy, Price: pxF, Size: szF,
			ReduceOnly: existing.ReduceOnly,
			// Preserve the resting order's time-in-force (e.g. Alo/post-only)
			// across the modify; default to Gtc when unknown.
			OrderType: hl.OrderType{Limit: &hl.LimitOrderType{Tif: tifOf(existing.Tif)}},
		}
		if preservedCloid != "" {
			cl, cerr := normalizeCloid(preservedCloid)
			if cerr != nil {
				return cerr
			}
			newOrder.ClientOrderID = &cl
		}
		mr := hl.ModifyOrderRequest{Order: newOrder}
		if oid != nil {
			mr.Oid = oid
		} else {
			cl, cerr := normalizeCloid(cloid)
			if cerr != nil {
				return cerr
			}
			mr.Cloid = &hl.Cloid{Value: cl}
		}
		var e error
		st, e = ex.ModifyOrder(ctx, mr)
		return e
	})
	if st.Error != nil {
		return nil, warnings, mapOrderReject(*st.Error)
	}
	if serr != nil {
		return nil, warnings, mapExchangeErr(serr)
	}
	applyStatus(res, st)
	// Log the cloid the REPLACEMENT order actually carries (preservedCloid), not the
	// input cloid (empty when modifying by oid), plus the new size/price/side — so
	// the trail can tie the modify to the order and recover the post-modify params.
	c.audit.Append(map[string]any{
		"action": "modify", "coin": mk.Coin, "oid": oid, "cloid": preservedCloid,
		"side": res.Side, "size": res.Size, "limit_px": res.LimitPx, "status": res.Status,
	})
	return res, append(c.signerWarnings(), warnings...), nil
}

// ModifyReq is one leg of a batch modify: re-price and/or re-size a resting
// order identified by oid or cloid. Empty Size/Limit keep the existing value.
type ModifyReq struct {
	Oid   *int64
	Cloid string
	Size  string
	Limit string
}

// ModifyBatch modifies N resting orders in ONE signed action (batchModify).
// Pre-flight validation is ATOMIC (any leg failing resolution/rounding/risk
// rejects the whole batch before signing); HL may still reject individual legs
// at submit, surfaced as per-leg results. The rate cap is charged once. Like the
// single modify, HL drops the builder fee on the replacement orders.
func (c *Client) ModifyBatch(ctx context.Context, reqs []ModifyReq) ([]*PlaceResult, []string, error) {
	if len(reqs) == 0 {
		return nil, nil, output.Validation("empty_batch", "batch has no modifies")
	}
	if len(reqs) > maxBatchOrders {
		return nil, nil, output.Validation("batch_too_large",
			fmt.Sprintf("batch has %d modifies; max %d per action", len(reqs), maxBatchOrders))
	}
	if err := c.requireQueryAddr(); err != nil {
		return nil, nil, err
	}
	orders, err := c.allOpenOrders(ctx)
	if err != nil {
		return nil, nil, mapNetwork("open_orders", err)
	}

	warnings := []string{}
	if b, _ := c.resolveBuilderApproved(ctx, nil); b != nil {
		warnings = append(warnings, "builder fee dropped: Hyperliquid does not support builder fees on modify — the replacement orders carry no fee")
	}

	mreqs := make([]hl.ModifyOrderRequest, 0, len(reqs))
	results := make([]*PlaceResult, 0, len(reqs))
	posSeed := map[string]float64{}
	seen := map[string]bool{}
	for i, req := range reqs {
		cloid := req.Cloid
		if req.Oid == nil {
			if cloid == "" {
				return nil, warnings, batchLegErr(i, output.Validation("missing_id", "each modify needs an oid or cloid"))
			}
			norm, nerr := normalizeCloid(cloid)
			if nerr != nil {
				return nil, warnings, batchLegErr(i, output.Validation("bad_cloid", nerr.Error()))
			}
			cloid = norm
		}
		existing, ok := matchResting(orders, req.Oid, cloid)
		if !ok {
			return nil, warnings, batchLegErr(i, output.Validation("order_not_found", "no resting order matches that oid/cloid"))
		}
		// Reject a duplicate target — dedup on the RESOLVED order's oid so the oid
		// and cloid forms of the same order collapse to one key (you cannot modify
		// one order twice in a single batch).
		key := strconv.FormatInt(existing.Oid, 10)
		if seen[key] {
			return nil, warnings, batchLegErr(i, output.Validation("dup_target", "duplicate order targeted twice in one batch"))
		}
		seen[key] = true
		if existing.IsTrigger {
			return nil, warnings, batchLegErr(i, output.Validation("modify_trigger_unsupported",
				"cannot modify a trigger order in place — cancel and re-place instead"))
		}
		mk, ok := c.meta.Lookup(existing.Coin)
		if !ok {
			return nil, warnings, batchLegErr(i, unknownCoin(existing.Coin))
		}
		size := req.Size
		if size == "" {
			size = existing.OrigSz
		}
		px := req.Limit
		if px == "" {
			px = existing.LimitPx
		}
		szOut, szChanged, rerr := RoundSize(size, mk.SzDecimals)
		if rerr != nil {
			return nil, warnings, batchLegErr(i, output.Validation("bad_size", rerr.Error()))
		}
		pxOut, pxChanged, perr := roundMarketPrice(mk, px)
		if perr != nil {
			return nil, warnings, batchLegErr(i, output.Validation("bad_price", perr.Error()))
		}
		szF, _ := strconv.ParseFloat(szOut, 64)
		pxF, _ := strconv.ParseFloat(pxOut, 64)

		notional, posNotional := 0.0, 0.0
		if !existing.ReduceOnly {
			// A modify to a crossing limit fills at the market — price the guard at
			// the mid (the wire pxF still carries the new limit), same as Modify.
			refPx := pxF
			if c.pricingGuardsActive() {
				refPx = c.marketableGuardPx(ctx, mk.Coin, existing.Side, pxF)
			}
			notional = refPx * szF
			// A modify REPLACES a resting order — unlike PlaceBatch, legs are NOT
			// summed, or a same-coin grid re-price would be rejected though exposure
			// is unchanged. Each leg is evaluated against the filled position alone,
			// exactly like a single Modify. Cache the position per coin, fetched only
			// when the cap is active.
			if c.cfg.Risk.MaxPositionNotionalUSD > 0 {
				base, seeded := posSeed[mk.Coin]
				if !seeded {
					base = c.currentPositionNotional(ctx, mk.Coin)
					posSeed[mk.Coin] = base
				}
				posNotional = notional + base
			}
		}
		if gerr := c.staticChecks(riskCheck{Coin: mk.Coin, NotionalUSD: notional, PositionNotionalUSD: posNotional, MinNotionalUSD: c.cfg.Risk.MinOrderNotionalUSD, ReduceOnly: existing.ReduceOnly}); gerr != nil {
			return nil, warnings, batchLegErr(i, gerr)
		}

		preservedCloid := existing.Cloid
		if preservedCloid == "" && cloid != "" && req.Oid == nil {
			preservedCloid = cloid
		}
		newOrder := hl.CreateOrderRequest{
			Coin: mk.Coin, IsBuy: existing.Side == Buy, Price: pxF, Size: szF, ReduceOnly: existing.ReduceOnly,
			OrderType: hl.OrderType{Limit: &hl.LimitOrderType{Tif: tifOf(existing.Tif)}},
		}
		if preservedCloid != "" {
			cl, cerr := normalizeCloid(preservedCloid)
			if cerr != nil {
				return nil, warnings, batchLegErr(i, output.Validation("bad_cloid", cerr.Error()))
			}
			newOrder.ClientOrderID = &cl
		}
		mr := hl.ModifyOrderRequest{Order: newOrder}
		if req.Oid != nil {
			mr.Oid = req.Oid
		} else {
			mr.Cloid = &hl.Cloid{Value: cloid}
		}
		mreqs = append(mreqs, mr)

		r := &PlaceResult{Cloid: preservedCloid, Coin: mk.Coin, Side: existing.Side.String(), Size: szOut, LimitPx: pxOut, Type: "limit", ReduceOnly: existing.ReduceOnly}
		rounding := &Rounding{}
		if szChanged {
			rounding.Sz = &FromTo{From: size, To: szOut}
			warnings = append(warnings, fmt.Sprintf("order %d: size rounded %s -> %s", i, size, szOut))
		}
		if pxChanged {
			rounding.Px = &FromTo{From: px, To: pxOut}
			warnings = append(warnings, fmt.Sprintf("order %d: price rounded %s -> %s", i, px, pxOut))
		}
		if !rounding.Empty() {
			r.Rounded = rounding
		}
		results = append(results, r)
	}

	// One signed action = one rate-cap charge, after the batch fully validates.
	if err := c.checkRateCap(); err != nil {
		return nil, warnings, err
	}

	if c.opts.DryRun {
		for _, r := range results {
			r.DryRun = true
			r.Status = "dry_run"
		}
		return results, warnings, nil
	}

	var resp *hl.APIResponse[hl.OrderResponse]
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		resp, e = ex.BatchModify(ctx, mreqs)
		return e
	})
	if resp == nil || len(resp.Data.Statuses) == 0 {
		if serr != nil {
			return nil, warnings, mapExchangeErr(serr)
		}
		msg := "batch modify returned no statuses"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return nil, warnings, mapOrderReject(msg)
	}
	statuses := resp.Data.Statuses
	for i, r := range results {
		switch {
		case i >= len(statuses):
			r.Status = "unknown"
			r.Error = "no status returned by exchange for this leg"
		case statuses[i].Error != nil:
			r.Status = "rejected"
			r.Error = *statuses[i].Error
		default:
			applyStatus(r, statuses[i])
		}
	}
	c.audit.Append(map[string]any{"action": "batch_modify", "count": len(mreqs), "legs": auditLegs(results)})
	return results, append(c.signerWarnings(), warnings...), nil
}

// ---------- panic (emergency cancel-all + flatten-all) ----------

// PanicResult is the outcome of an emergency teardown.
type PanicResult struct {
	Canceled      int      `json:"canceled"`
	Closed        []any    `json:"closed"`
	TwapsCanceled []int64  `json:"twaps_canceled,omitempty"`
	Degraded      []string `json:"degraded,omitempty"` // dexes whose flat state could not be confirmed
	Complete      bool     `json:"complete"`           // false => teardown NOT verified flat; re-run / inspect
	CancelError   string   `json:"cancel_error,omitempty"`
}

// Panic is the emergency lever: cancel ALL resting orders (every dex), cancel
// every RUNNING TWAP (which would otherwise keep slicing and rebuild a position
// after the flatten), flatten ALL positions, then strictly RE-READ every dex to
// confirm the teardown. Any dex whose read fails is surfaced in Degraded and sets
// Complete=false, so a partial emergency-flatten is never reported as success. Not
// halt-gated (panic must work during a halt). Audited as one "panic" row.
func (c *Client) Panic(ctx context.Context) (*PanicResult, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	res := &PanicResult{Complete: true, Closed: []any{}}

	// 1. Cancel all resting orders (main + sub-dex).
	if cr, cerr := c.Cancel(ctx, CancelReq{All: true}); cerr != nil {
		res.CancelError = cerr.Error()
		res.Complete = false
	} else {
		res.Canceled = cr.Canceled
	}

	// 2. Cancel every running TWAP — a live TWAP keeps slicing and rebuilds a
	// position seconds after the flatten reports done.
	if twaps, terr := c.info.RunningTwaps(ctx, c.queryAddr); terr != nil {
		res.Complete = false
		res.Degraded = append(res.Degraded, "twaps")
	} else {
		for _, tw := range twaps {
			if _, e := c.TwapCancel(ctx, tw.Coin, tw.ID); e == nil {
				res.TwapsCanceled = append(res.TwapsCanceled, tw.ID)
			} else {
				res.Complete = false
			}
		}
	}

	// 3. Flatten every position (main + sub-dex), best-effort per leg.
	if positions, perr := c.Positions(ctx, ""); perr != nil {
		res.Complete = false
	} else {
		for _, p := range positions {
			if p.Side == "flat" {
				continue
			}
			if r, _, e := c.Close(ctx, p.Coin, "", true, "", ""); e != nil {
				res.Complete = false
				res.Closed = append(res.Closed, map[string]any{"coin": p.Coin, "error": e.Error()})
			} else {
				res.Closed = append(res.Closed, r)
			}
		}
	}

	// 4. Strict verification: re-read every dex. A read error means that dex's
	// orders/positions could NOT be confirmed (the best-effort sweeps above skip a
	// failed dex silently); any still-resting order means a leg was missed.
	for _, dex := range append([]string{""}, c.cfg.PerpDexs...) {
		d := strings.ToLower(strings.TrimSpace(dex))
		label := d
		if label == "" {
			label = "main"
		}
		oo, oerr := c.info.FrontendOpenOrdersForDex(ctx, c.queryAddr, d)
		st, uerr := c.info.UserStateForDex(ctx, c.queryAddr, d)
		if oerr != nil || uerr != nil {
			res.Degraded = append(res.Degraded, label)
			res.Complete = false
			continue
		}
		if len(oo) > 0 {
			res.Complete = false
		}
		for _, ap := range st.AssetPositions {
			if sideFromSzi(ap.Position.Szi) != "flat" {
				res.Complete = false
			}
		}
	}

	// 4b. The per-dex re-read above only sees perp positions; HIP-4 outcome
	// holdings are spot "+<enc>" tokens that step 3 closes via Positions but the
	// verification missed — so a missed/no-op outcome close would falsely report
	// complete. Re-read spot (main dex only — sub-dexes have no spot) and flag any
	// residual outcome balance. Gated on outcomes being enabled, so the common
	// path adds zero calls (audit #91 / T3-flatten).
	if c.meta.OutcomeMeta() != nil {
		if ss, serr := c.info.SpotUserState(ctx, c.queryAddr); serr != nil {
			res.Degraded = append(res.Degraded, "outcomes")
			res.Complete = false
		} else if ss != nil && hasOutcomeBalance(ss.Balances) {
			res.Complete = false
		}
	}

	c.audit.Append(map[string]any{
		"action": "panic", "canceled": res.Canceled, "closed": len(res.Closed),
		"twaps_canceled": len(res.TwapsCanceled), "degraded": res.Degraded, "complete": res.Complete,
	})
	return res, nil
}

// ---------- leverage / margin ----------

// LeverageResult reports a leverage change.
type LeverageResult struct {
	Coin     string `json:"coin"`
	Leverage int    `json:"leverage"`
	Mode     string `json:"mode"` // cross | isolated
	DryRun   bool   `json:"dry_run,omitempty"`
}

// SetLeverage updates leverage for a coin (capped by risk.max_leverage).
func (c *Client) SetLeverage(ctx context.Context, coin string, x int, cross bool) (*LeverageResult, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	if c.Halted() {
		return nil, output.Halt("halted", "global halt is active — leverage change rejected").WithHint("deliverator halt off")
	}
	if x < 1 {
		return nil, output.Validation("bad_leverage", "leverage must be >= 1")
	}
	if mk.MaxLeverage > 0 && x > mk.MaxLeverage {
		return nil, output.Risk("exceeds_market_max",
			fmt.Sprintf("leverage %dx exceeds %s max %dx", x, mk.Coin, mk.MaxLeverage))
	}
	if err := c.checkLeverage(x); err != nil {
		return nil, err
	}
	mode := "cross"
	if !cross {
		mode = "isolated"
	}
	res := &LeverageResult{Coin: mk.Coin, Leverage: x, Mode: mode}
	if c.opts.DryRun {
		res.DryRun = true
		return res, nil
	}
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		_, e := ex.UpdateLeverage(ctx, x, mk.Coin, cross)
		return e
	})
	if serr != nil {
		return nil, mapExchangeErr(serr)
	}
	c.audit.Append(map[string]any{"action": "leverage", "coin": mk.Coin, "leverage": x, "mode": mode})
	return res, nil
}

// MarginResult reports an isolated-margin adjustment.
type MarginResult struct {
	Coin   string  `json:"coin"`
	USD    float64 `json:"usd"` // positive = add, negative = remove
	DryRun bool    `json:"dry_run,omitempty"`
}

// AdjustMargin adds (positive) or removes (negative) isolated margin, in USD.
func (c *Client) AdjustMargin(ctx context.Context, coin string, usd float64) (*MarginResult, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	if c.Halted() {
		return nil, output.Halt("halted", "global halt is active — margin change rejected").WithHint("deliverator halt off")
	}
	if usd == 0 {
		return nil, output.Validation("bad_amount", "margin amount must be non-zero (+add / -remove)")
	}
	res := &MarginResult{Coin: mk.Coin, USD: usd}
	if c.opts.DryRun {
		res.DryRun = true
		return res, nil
	}
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		_, e := ex.UpdateIsolatedMargin(ctx, usd, mk.Coin)
		return e
	})
	if serr != nil {
		return nil, mapExchangeErr(serr)
	}
	c.audit.Append(map[string]any{"action": "margin", "coin": mk.Coin, "usd": usd})
	return res, nil
}

// ---------- schedule-cancel (dead-man's switch) ----------

// ScheduleCancel arms (absolute epoch ms) or clears (nil) the dead-man's switch.
func (c *Client) ScheduleCancel(ctx context.Context, deadlineMs *int64) error {
	if c.opts.DryRun {
		return nil
	}
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		_, e := ex.ScheduleCancel(ctx, deadlineMs)
		return e
	})
	if serr != nil {
		// Map exchange rejections (e.g. the volume requirement on scheduleCancel)
		// to a proper category/exit code instead of leaking as exit 1 (unknown).
		return mapExchangeErr(serr)
	}
	return nil
}

// ---------- referral ----------

// SetReferrer applies a referral code to the account (one-time, agent-signed).
// It is idempotent in spirit: callers should check ReferralStatus first, since
// HL rejects a second referrer.
func (c *Client) SetReferrer(ctx context.Context, code string) error {
	if c.opts.DryRun {
		return nil
	}
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		return ex.SetReferrer(ctx, code)
	})
	if serr != nil {
		return mapExchangeErr(serr)
	}
	c.audit.Append(map[string]any{"action": "set_referrer", "code": code})
	return nil
}

// ReferralStatus reports whether the query account already has a referrer.
func (c *Client) ReferralStatus(ctx context.Context) (*hl.ReferralInfo, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	ri, err := c.info.Referral(ctx, c.queryAddr)
	if err != nil {
		return nil, mapNetwork("referral", err)
	}
	return ri, nil
}

// ---------- twap ----------

const (
	twapMinMinutes = 1
	twapMaxMinutes = 1440 // 24h — guard against a fat-fingered duration
)

// TwapReq is the core's TWAP input. The order is sliced over Minutes.
type TwapReq struct {
	Coin       string
	Side       Side
	Size       string // total size across all slices (pre-round)
	ReduceOnly bool
	Minutes    int
	Randomize  bool
}

// TwapResult is the outcome of a TWAP submission.
type TwapResult struct {
	TwapID     *int64    `json:"twap_id,omitempty"`
	Coin       string    `json:"coin"`
	Side       string    `json:"side"`
	Size       string    `json:"size"`
	Minutes    int       `json:"minutes"`
	Randomize  bool      `json:"randomize,omitempty"`
	ReduceOnly bool      `json:"reduce_only,omitempty"`
	Status     string    `json:"status"` // running | dry_run
	Rounded    *Rounding `json:"rounded,omitempty"`
	DryRun     bool      `json:"dry_run,omitempty"`
}

// Twap submits a TWAP order. It runs the SAME gauntlet as Place (size rounding,
// notional caps fail-closed without a mid, halt/allowlist/rate). TWAP executes
// as passive slices, so it is treated as a non-market order for risk: it is not
// blocked by automation.limit_only but is subject to the allowlist and caps.
func (c *Client) Twap(ctx context.Context, req TwapReq) (*TwapResult, []string, error) {
	mk, ok := c.meta.Lookup(req.Coin)
	if !ok {
		return nil, nil, unknownCoin(req.Coin)
	}
	if req.Minutes < twapMinMinutes || req.Minutes > twapMaxMinutes {
		return nil, nil, output.Validation("bad_minutes",
			fmt.Sprintf("twap minutes must be between %d and %d", twapMinMinutes, twapMaxMinutes)).
			WithHint(fmt.Sprintf("pass --minutes between %d and %d", twapMinMinutes, twapMaxMinutes))
	}

	warnings := []string{}
	rounding := &Rounding{}
	szOut, szChanged, err := RoundSize(req.Size, mk.SzDecimals)
	if err != nil {
		return nil, nil, output.Validation("bad_size", err.Error())
	}
	if szChanged {
		if c.opts.Strict {
			return nil, nil, output.Precision("sz_precision",
				fmt.Sprintf("size %s has too many decimals; %s allows %d (-> %s)", req.Size, mk.Coin, mk.SzDecimals, szOut)).
				WithHint("pass size " + szOut)
		}
		rounding.Sz = &FromTo{From: req.Size, To: szOut}
		warnings = append(warnings, fmt.Sprintf("size rounded %s -> %s", req.Size, szOut))
	}
	szF, _ := strconv.ParseFloat(szOut, 64)

	// Notional + risk gate, fail-CLOSED like Place: an unknown reference price
	// must never let a TWAP slip past a configured dollar guard. Reduce-only
	// TWAPs only shrink a position, so — like reduce-only Place/Modify — they skip
	// the notional math entirely and need no reference price to unwind.
	notional, posNotional := 0.0, 0.0
	if !req.ReduceOnly {
		refPx, hasMid := c.midPrice(ctx, mk.Coin)
		if !hasMid {
			if c.pricingGuardsActive() {
				return nil, nil, output.Risk("no_ref_px",
					"cannot determine a reference price for a TWAP in "+mk.Coin+" — refusing to bypass notional caps/minimum").
					WithHint("retry when mids are available")
			}
			refPx = 0
		}
		notional = refPx * szF
		if c.cfg.Risk.MaxPositionNotionalUSD > 0 {
			posNotional = notional + c.currentPositionNotional(ctx, mk.Coin)
		}
	}
	if err := c.preTradeChecks(riskCheck{Coin: mk.Coin, IsMarket: false, NotionalUSD: notional, PositionNotionalUSD: posNotional, MinNotionalUSD: c.cfg.Risk.MinOrderNotionalUSD, ReduceOnly: req.ReduceOnly}); err != nil {
		return nil, warnings, err
	}
	if !req.ReduceOnly {
		if perr := c.checkPortfolioGates(ctx, []exposureDelta{{coin: mk.Coin, signedNotional: signedNotional(req.Side, notional)}}); perr != nil {
			return nil, warnings, perr
		}
	}

	// Hyperliquid's twapOrder action has no builder field, so TWAP volume earns no
	// builder fee. Warn when a builder would otherwise attach so the operator knows.
	if c.resolveBuilder(nil) != nil {
		warnings = append(warnings, "builder fee NOT applied: Hyperliquid does not support builder fees on TWAP orders")
	}

	res := &TwapResult{
		Coin: mk.Coin, Side: req.Side.String(), Size: szOut,
		Minutes: req.Minutes, Randomize: req.Randomize, ReduceOnly: req.ReduceOnly,
	}
	if !rounding.Empty() {
		res.Rounded = rounding
	}
	if c.opts.DryRun {
		res.DryRun = true
		res.Status = "dry_run"
		return res, warnings, nil
	}

	isBuy := req.Side == Buy
	var st hl.TwapStatus
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		var e error
		st, e = ex.TwapOrder(ctx, mk.Coin, isBuy, szF, req.ReduceOnly, req.Minutes, req.Randomize)
		return e
	})
	if st.Error != nil {
		c.audit.Append(map[string]any{
			"action": "twap", "coin": mk.Coin, "side": req.Side.String(),
			"size": szOut, "minutes": req.Minutes, "status": "rejected", "error": *st.Error,
		})
		return nil, warnings, mapOrderReject(*st.Error)
	}
	if serr != nil {
		c.audit.Append(map[string]any{
			"action": "twap", "coin": mk.Coin, "side": req.Side.String(),
			"size": szOut, "minutes": req.Minutes, "status": "error", "error": serr.Error(),
		})
		return nil, warnings, mapExchangeErr(serr)
	}
	// Only report running when the exchange actually issued a twap id; otherwise a
	// degenerate/mis-parsed response must not be reported as a live TWAP.
	if st.Running == nil || st.Running.TwapID == 0 {
		return nil, warnings, output.Exchange("twap_no_id",
			"TWAP was accepted but no twap id was returned — treat as not started").
			WithHint("check `deliverator orders` / fills before retrying")
	}
	res.Status = "running"
	id := st.Running.TwapID
	res.TwapID = &id
	c.audit.Append(map[string]any{
		"action": "twap", "coin": mk.Coin, "side": req.Side.String(),
		"size": szOut, "minutes": req.Minutes, "twap_id": res.TwapID,
	})
	return res, warnings, nil
}

// TwapCancelResult reports a TWAP cancellation.
type TwapCancelResult struct {
	Coin     string `json:"coin"`
	TwapID   int64  `json:"twap_id"`
	Canceled bool   `json:"canceled"`
	DryRun   bool   `json:"dry_run,omitempty"`
}

// TwapCancel stops a running TWAP by id. Like other cancel paths it is not
// halt-gated — stopping an in-flight TWAP must work during a halt.
func (c *Client) TwapCancel(ctx context.Context, coin string, twapID int64) (*TwapCancelResult, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	res := &TwapCancelResult{Coin: mk.Coin, TwapID: twapID}
	if c.opts.DryRun {
		res.DryRun = true
		return res, nil
	}
	serr := c.signed(ctx, func(ex *hl.Exchange) error {
		_, e := ex.TwapCancel(ctx, mk.Coin, twapID)
		return e
	})
	if serr != nil {
		return nil, mapExchangeErr(serr)
	}
	res.Canceled = true
	c.audit.Append(map[string]any{"action": "twap_cancel", "coin": mk.Coin, "twap_id": twapID})
	return res, nil
}

// ---------- helpers ----------

func (c *Client) resolveBuilder(override *int) *hl.BuilderInfo {
	addr := c.cfg.Builder.Address
	if addr == "" {
		return nil
	}
	fee := c.cfg.Builder.FeeTenthsBps
	if override != nil {
		fee = *override
	}
	if override == nil && c.cfg.Builder.AttachMode != config.AttachAll {
		return nil // manual mode + no explicit flag -> don't attach
	}
	if fee <= 0 {
		return nil
	}
	// Lowercase the builder address on the wire: approveBuilderFee / maxBuilderFee
	// are keyed on the lowercased address (the read path already lowercases), so a
	// checksummed config address must be normalized here or HL can treat the
	// attached builder as un-approved and reject every order.
	return &hl.BuilderInfo{Builder: strings.ToLower(addr), Fee: fee}
}

// builderApprTTL bounds how long a fetched master-approval is reused. A CLI run is
// usually one order (one fetch), but a long-running command (watch/twap/chase) that
// re-resolves the builder should occasionally re-check, so a freshly-approved fee
// starts applying without a restart. The inverse — a master REVOKE mid-process — can
// likewise take up to one TTL to take effect (a stale "approved" memo could then
// attach a now-unapproved fee and have HL reject the order). This is bounded and
// effectively unreachable in shipped commands (the only multi-order ones either
// attach the builder once on the initial Place, like chase, or never attach it, like
// twap/modify), so the TTL is the accepted trade-off vs a per-order read.
const builderApprTTL = 120 * time.Second

// builderApprovedMax returns the trader's master-approved maximum builder fee
// (tenths-of-bps) for the given builder, via the `maxBuilderFee` info endpoint,
// memoized per builder for builderApprTTL. The query is keyed on the READ address
// (the master/sub the orders act for), exactly like `builder status`.
func (c *Client) builderApprovedMax(ctx context.Context, builder string) (int, error) {
	if c.queryAddr == "" {
		return 0, fmt.Errorf("no query address to check builder approval")
	}
	builder = strings.ToLower(builder)
	c.builderApprMu.Lock()
	defer c.builderApprMu.Unlock()
	if c.builderApprOK && c.builderApprFor == builder && time.Since(c.builderApprAt) < builderApprTTL {
		return c.builderApprMax, nil
	}
	var raw float64
	if err := c.InfoPost(ctx, map[string]any{
		"type":    "maxBuilderFee",
		"user":    c.queryAddr,
		"builder": builder,
	}, &raw); err != nil {
		return 0, err
	}
	c.builderApprMax = int(raw)
	c.builderApprFor = builder
	c.builderApprOK = true
	c.builderApprAt = time.Now()
	return c.builderApprMax, nil
}

// resolveBuilderApproved is the graceful (approval-aware) builder resolver every
// order path uses. It returns the builder to attach per config (resolveBuilder),
// but only if the trader's master wallet has approved it up to at least the
// configured fee. If it is NOT approved — or the approval check fails — the fee is
// SKIPPED (nil) and the order is placed fee-free. Hyperliquid rejects an order that
// carries an unapproved builder fee, so attaching it anyway would block the trade
// entirely; skipping it means a user who hasn't run the one-time master-signed
// approveBuilderFee can still trade (just without funding the builder).
//
// The config-default skip is SILENT by design — the invitation to approve lives in
// onboard/connect/`builder status`, not on every order. But when the caller passed
// an EXPLICIT --builder-fee override, the user is actively asking for a fee, so a
// drop returns a one-line warning explaining why it didn't apply.
func (c *Client) resolveBuilderApproved(ctx context.Context, override *int) (*hl.BuilderInfo, string) {
	b := c.resolveBuilder(override)
	if b == nil {
		return nil, ""
	}
	approvedMax, err := c.builderApprovedMax(ctx, b.Builder)
	if err == nil && approvedMax >= b.Fee {
		return b, ""
	}
	// Dropped: unapproved or unverifiable -> place fee-free, never block the trade.
	if override != nil {
		if err != nil {
			return nil, "--builder-fee skipped: could not verify master approval — order placed fee-free"
		}
		return nil, fmt.Sprintf("--builder-fee %d skipped: master approved only %d tenths-bps for this builder — order placed fee-free (raise it with approveBuilderFee)", b.Fee, approvedMax)
	}
	return nil, ""
}

// bpsToPriorityRate converts basis points to Hyperliquid's order-priority `p`
// (rate = p/1e8; 1 bp = 0.0001 = 10000/1e8).
const bpsToPriorityRate = 10000

// resolvePriority returns the order-priority rate (`p`, where rate = p/1e8) for
// an order, applying the per-order override or the config default, clamped to
// risk.max_priority_bps (and HL's 8 bps hard cap). Returns 0 (no priority) when
// unset, plus a warning string when a requested value was clamped down.
func (c *Client) resolvePriority(override *int) (rate int, warning string) {
	bps := c.cfg.Automation.PriorityBps
	if override != nil {
		bps = *override
	}
	if bps <= 0 {
		return 0, ""
	}
	max := c.cfg.Risk.MaxPriorityBps
	if max <= 0 || max > config.HLMaxPriorityBps {
		max = config.HLMaxPriorityBps
	}
	if bps > max {
		warning = fmt.Sprintf("order priority %d bps clamped to the %d bps cap", bps, max)
		bps = max
	}
	return bps * bpsToPriorityRate, warning
}

// midOK parses a mid-price string and reports it usable only when it parsed AND is
// finite AND strictly positive. strconv.ParseFloat accepts "NaN"/"Inf", so a
// poisoned /info mid would otherwise return (NaN, true) — letting a market order
// compute a NaN/0 notional that slips every cap (NaN>cap and 0>cap are both false).
// Returning ok=false makes the market-order path fail closed via no_ref_px (audit S2).
func midOK(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, false
	}
	return f, true
}

func (c *Client) midPrice(ctx context.Context, coin string) (float64, bool) {
	// Sub-dex (HIP-3) mids come from that dex's allMids, keyed by "<dex>:<coin>".
	if dex := dexOf(coin); dex != "" {
		mids, err := c.info.AllMidsForDex(ctx, dex)
		if err != nil {
			return 0, false
		}
		if s, ok := mids[coin]; ok {
			return midOK(s)
		}
		return 0, false
	}
	mids, err := c.info.AllMids(ctx)
	if err != nil {
		return 0, false
	}
	if s, ok := mids[coin]; ok {
		return midOK(s)
	}
	// Non-canonical spot pairs are keyed by "@<index>", not by the pair name.
	if mk, ok := c.meta.Lookup(coin); ok && mk.IsSpot {
		if s, ok := mids["@"+strconv.Itoa(mk.AssetIndex-10000)]; ok {
			return midOK(s)
		}
	}
	return 0, false
}

// maxSlippage caps operator/agent-supplied market slippage so a market order cannot
// fill arbitrarily far past the dollar guard (which is priced at the worst-case
// fill). 10% is generous for liquid perps; outcome slippage is additive but clamped
// into (0,1) regardless (audit S4).
const maxSlippage = 0.10

// resolveSlippage returns the effective market slippage: DefaultSlippage when unset
// (<=0), the value when in (0, maxSlippage], else a validation error.
func resolveSlippage(s float64) (float64, error) {
	if s <= 0 {
		return hl.DefaultSlippage, nil
	}
	if s > maxSlippage {
		return 0, output.Validation("bad_slippage",
			fmt.Sprintf("--slippage %.4g exceeds the %.0f%% maximum", s, maxSlippage*100)).
			WithHint(fmt.Sprintf("use --slippage <= %.2f, or place a limit order with --limit", maxSlippage))
	}
	return s, nil
}

// marketGuardPx prices the dollar/at-stake guards at the WORST-CASE fill of a market
// order rather than the mid: a BUY can fill up to mid*(1+slip) (outcome: mid+slip,
// clamped <1); a SELL sweeps bids <= mid, so the mid itself is its worst case. This
// stops a market fill from exceeding max_order_notional / at-stake by the slippage
// band (audit S4). The wire order price is still the slippage limit, set separately.
func marketGuardPx(mid float64, isBuy, isOutcome bool, slip float64) float64 {
	if !isBuy {
		return mid
	}
	if isOutcome {
		if p := mid + slip; p < 1 {
			return p
		}
		return 1
	}
	return mid * (1 + slip)
}

// dexOf returns the sub-dex prefix of a "dex:coin" symbol, or "" for main/spot.
func dexOf(coin string) string {
	if i := strings.IndexByte(coin, ':'); i > 0 {
		return coin[:i]
	}
	return ""
}

// coinMatches compares a position's coin against a requested symbol, tolerating
// the dex prefix (a sub-dex clearinghouse may report "BRENTOIL" or "xyz:BRENTOIL").
func coinMatches(posCoin, want string) bool {
	if strings.EqualFold(posCoin, want) {
		return true
	}
	if i := strings.IndexByte(want, ':'); i > 0 {
		return strings.EqualFold(posCoin, want[i+1:])
	}
	return false
}

func (c *Client) positionSzi(ctx context.Context, coin string) (float64, bool) {
	if c.queryAddr == "" {
		return 0, false
	}
	st, err := c.info.UserStateForDex(ctx, c.queryAddr, dexOf(coin))
	if err != nil {
		return 0, false
	}
	for _, ap := range st.AssetPositions {
		if coinMatches(ap.Position.Coin, coin) {
			f, err := strconv.ParseFloat(ap.Position.Szi, 64)
			return f, err == nil && f != 0
		}
	}
	return 0, false
}

func (c *Client) currentPositionNotional(ctx context.Context, coin string) float64 {
	if c.queryAddr == "" {
		return 0
	}
	// A HIP-4 outcome holding is a spot "+<enc>" token, not a perp AssetPosition, so
	// the perp clearinghouse below reports 0 for it — letting an agent accumulate an
	// unbounded outcome stake by splitting buys each under max_position_notional.
	// Count the existing outcome holding's current value so the per-coin cap sees it
	// (audit S3).
	if strings.HasPrefix(coin, "#") {
		var sum float64
		for _, pv := range c.outcomePositionsFromSpot(ctx, coin) {
			sum += absF(parseFloatSafe(pv.PositionValue))
		}
		return sum
	}
	st, err := c.info.UserStateForDex(ctx, c.queryAddr, dexOf(coin))
	if err != nil {
		return 0
	}
	for _, ap := range st.AssetPositions {
		if coinMatches(ap.Position.Coin, coin) {
			f, _ := strconv.ParseFloat(ap.Position.PositionValue, 64)
			return absF(f)
		}
	}
	return 0
}

// restingOrder is a normalized view of an existing order, sourced from
// FrontendOpenOrders for both the oid and cloid lookups (the only query that
// carries the order's tif/isTrigger).
type restingOrder struct {
	Oid        int64
	Coin       string
	Side       Side
	OrigSz     string
	LimitPx    string
	ReduceOnly bool
	Cloid      string // the order's client id, if it carries one (preserved across a modify)
	Tif        string // Gtc | Ioc | Alo — preserved across a modify ("" => Gtc)
	IsTrigger  bool   // trigger orders cannot be modified in place (HL rejects it)
}

func sideOf(s hl.OrderSide) Side {
	if s == hl.OrderSideAsk {
		return Sell
	}
	return Buy
}

func (c *Client) findResting(ctx context.Context, oid *int64, cloid string) (restingOrder, bool) {
	// Source from frontendOpenOrders for both the oid and cloid paths: it is the
	// only query that reliably carries the order's TIF and trigger flag
	// (orderStatus-by-cloid returns an empty tif), and modify only ever targets a
	// resting order, so the open-orders set is the right place to look.
	orders, err := c.allOpenOrders(ctx)
	if err != nil {
		return restingOrder{}, false
	}
	return matchResting(orders, oid, cloid)
}

// matchResting resolves an oid/cloid against an already-fetched open-orders set,
// so a batch can resolve every leg from a single read.
func matchResting(orders []hl.FrontendOpenOrder, oid *int64, cloid string) (restingOrder, bool) {
	for _, o := range orders {
		match := (oid != nil && o.Oid == *oid) ||
			(oid == nil && o.Cloid != nil && strings.EqualFold(*o.Cloid, cloid))
		if !match {
			continue
		}
		return restingOrder{
			Oid:  o.Oid,
			Coin: o.Coin, Side: sideOf(o.Side),
			OrigSz:     strconv.FormatFloat(o.OrigSz, 'f', -1, 64),
			LimitPx:    strconv.FormatFloat(o.LimitPx, 'f', -1, 64),
			ReduceOnly: o.ReduceOnly,
			Cloid:      derefStr(o.Cloid),
			Tif:        string(o.Tif),
			IsTrigger:  o.IsTrigger,
		}, true
	}
	return restingOrder{}, false
}

// derefStr returns the pointed-to string, or "" if nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (c *Client) resolveOrderCoin(ctx context.Context, oid *int64, cloid string) (string, bool) {
	orders, err := c.allOpenOrders(ctx)
	if err != nil {
		return "", false
	}
	for _, o := range orders {
		if oid != nil && o.Oid == *oid {
			return o.Coin, true
		}
	}
	// cloid path: query order status to get the coin.
	if cloid != "" {
		if res, err := c.info.QueryOrderByCloid(ctx, c.queryAddr, cloid); err == nil && res.Status == hl.OrderQueryStatusSuccess {
			return res.Order.Order.Coin, true
		}
	}
	return "", false
}

func mapOrderReject(msg string) error {
	s := strings.ToLower(msg)
	switch {
	case strings.Contains(s, "tick") || strings.Contains(s, "divisible") || strings.Contains(s, "decimal") || strings.Contains(s, "significant"):
		return output.Exchange("tick_reject", msg).WithHint("check `deliverator markets` precision and retry")
	case strings.Contains(s, "reduce only") || strings.Contains(s, "reduce-only"):
		return output.Exchange("reduce_only", msg)
	case strings.Contains(s, "minimum value") || strings.Contains(s, "minimum order"):
		return output.Exchange("min_order_value", msg).
			WithHint("increase size; Hyperliquid enforces a ~$10 minimum order value (see risk.min_order_notional_usd)")
	case strings.Contains(s, "insufficient") || strings.Contains(s, "margin"):
		return output.Exchange("margin", msg)
	default:
		return output.Exchange("order_rejected", msg)
	}
}

func mapExchangeErr(err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "deadline") || strings.Contains(s, "timeout") || strings.Contains(s, "timed out"):
		return output.Timeout("timeout", "request timed out — order outcome is UNKNOWN").
			WithHint("run `deliverator order status --cloid <id>` before resubmitting (§5.4)")
	case strings.Contains(s, "too many") || strings.Contains(s, "rate limit"):
		return output.RateLimit("rate_limited", err.Error()).WithRetryAfter(10000)
	case strings.Contains(s, "nonce"):
		// The L1 nonce is derived from local time; HL rejects nonces outside its
		// accepted window. That is a clock problem the operator must fix, not a
		// "read the message and retry" exchange reject (exit 70, not 50).
		return output.Clock("nonce_window", "exchange rejected the nonce — likely clock skew: "+err.Error()).
			WithHint("sync the system clock (NTP); the nonce is derived from local time")
	case strings.Contains(s, "tick") || strings.Contains(s, "divisible") || strings.Contains(s, "decimal"):
		return output.Exchange("tick_reject", err.Error())
	case strings.Contains(s, "insufficient") || strings.Contains(s, "margin"):
		return output.Exchange("margin", err.Error())
	default:
		return output.Exchange("rejected", err.Error())
	}
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
