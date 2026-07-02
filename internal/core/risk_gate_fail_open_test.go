package core

// Risk-gate fail-open cluster (review 2026-07-01, cluster NEXT-1):
//  1. a failed (429/500) or address-less position read must FAIL CLOSED on the
//     per-coin position cap for exposure-ADDING orders — retryable exit 40, not
//     a silent pass against a phantom-flat book. Reduce-only orders stay allowed
//     (they carry r:true on the wire, so the exchange guarantees they reduce);
//  2. the notional/portfolio caps must count resting NON-reduce-only orders as
//     worst-case future exposure, or sequential single orders tunnel arbitrarily
//     far past every cap; an open-orders read failure fails closed like (1);
//  3. automation.limit_only must block a trigger order with is_market=true — it
//     EXECUTES as a market order when it fires (instantly, if already crossed);
//  4. the drawdown/daily-loss anchors are keyed per network+account; the legacy
//     unkeyed risk_state.json is ignored (fresh anchors + a loud warning), so a
//     testnet peak can never brick mainnet trading (or vice versa).

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// assertNetworkRetryable asserts err is a category-network (exit 40) error
// marked retryable — the "back off and retry" shape, NOT exit 20's "respect
// the cap, stop trying".
func assertNetworkRetryable(t *testing.T, err error) {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %v", err)
	}
	if oe.Category != output.CatNetwork {
		t.Fatalf("want category network (exit 40), got %s/%s (%v)", oe.Category, oe.Code, oe)
	}
	if !oe.Retryable {
		t.Fatalf("a transient read failure must be retryable:true, got %v", oe)
	}
}

// restingBTC is a frontendOpenOrders body with n resting 0.5 BTC orders at
// $60k each on side ("B" bid/buy, "A" ask/sell), non-reduce-only unless ro.
func restingBTC(side string, n int, ro bool) string {
	roStr := "false"
	if ro {
		roStr = "true"
	}
	rows := make([]string, n)
	for i := range rows {
		rows[i] = `{"coin":"BTC","oid":` + strconv.Itoa(i+1) + `,"limitPx":"60000","origSz":"0.5","sz":"0.5","side":"` + side + `","reduceOnly":` + roStr + `,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0.0"}`
	}
	return `[` + strings.Join(rows, ",") + `]`
}

// restingBTCBuys is a frontendOpenOrders body with n resting 0.5 BTC buys at
// $60k each (non-reduce-only unless ro).
func restingBTCBuys(n int, ro bool) string { return restingBTC("B", n, ro) }

// ---- finding 1: position-read failure fails CLOSED for adds, OPEN for reduces ----

// A 500/429 on the clearinghouse read with risk.max_position_notional_usd set
// must reject an exposure-adding order with a RETRYABLE network error (exit 40)
// and sign nothing — not evaluate the cap against a phantom-flat book.
func TestPositionReadFailureFailsClosedForAdds(t *testing.T) {
	var wire wireOrder
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 500, `upstream down`
			case "frontendOpenOrders":
				return 200, `[]`
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		wire.seen = true
		return 200, okOrder(`{"resting":{"oid":1}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{}, resp)
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"})
	if err == nil {
		t.Fatal("an exposure-adding order must FAIL CLOSED when the position read fails and a cap is configured")
	}
	assertNetworkRetryable(t, err)
	if wire.seen {
		t.Fatal("nothing may be signed/sent when the cap's position read failed")
	}
}

// The same unreadable state must NOT block de-risking: a reduce-only order
// carries r:true on the wire, so the exchange itself guarantees it can only
// reduce — it goes out even when the position cannot be read locally.
func TestPositionReadFailureStillAllowsReduceOnly(t *testing.T) {
	var wire wireOrder
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 500, `upstream down`
			case "allMids":
				return 200, `{"BTC":"60000"}`
			case "frontendOpenOrders":
				return 200, `[]`
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if orders, ok := action["orders"].([]any); ok && len(orders) > 0 {
				if o, ok := orders[0].(map[string]any); ok {
					wire.seen = true
					wire.r, _ = o["r"].(bool)
				}
			}
		}
		return 200, okOrder(`{"filled":{"totalSz":"0.05","avgPx":"60000","oid":9}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{}, resp)
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Sell, Size: "0.05", ReduceOnly: true})
	if err != nil {
		t.Fatalf("a reduce-only order must still pass on unreadable position state (the wire r:true guarantees it reduces): %v", err)
	}
	if !wire.seen || !wire.r {
		t.Fatalf("the reduce-only order must reach the wire with r:true (seen=%v r=%v)", wire.seen, wire.r)
	}
}

// ---- finding 2: resting non-reduce-only orders count against the caps ----

// respWithOrders serves a flat account whose open-order book is openOrders.
func respWithOrders(openOrders string, wire *wireOrder) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearingWith("1000000")
			case "frontendOpenOrders":
				return 200, openOrders
			case "spotClearinghouseState":
				return 200, noSpot
			case "allMids":
				return 200, `{"BTC":"60000","ETH":"3000"}`
			}
			return 200, `{}`
		}
		if wire != nil {
			wire.seen = true
		}
		return 200, okOrder(`{"resting":{"oid":9}}`)
	}
}

// Three resting 0.5 BTC buys ($90k pending) + flat position + a $100k per-coin
// cap: a 4th 0.5 BTC buy ($30k) makes the worst-case position $120k and must be
// rejected exit 20 — before the fix each order was checked against the FILLED
// position only ($30k) and passed, tunneling past the cap.
func TestPositionCapCountsRestingOrders(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{},
		respWithOrders(restingBTCBuys(3, false), &wire))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"})
	assertRiskCode(t, err, "max_position_notional")
	if wire.seen {
		t.Fatal("the over-cap order must never reach the wire")
	}
}

// Reduce-only resting orders (e.g. bracket TP/SL children) can only SHRINK the
// position — they contribute zero pending exposure and must not false-reject.
func TestPositionCapIgnoresReduceOnlyRestingOrders(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{},
		respWithOrders(restingBTCBuys(3, true), &wire))
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"}); err != nil {
		t.Fatalf("reduce-only resting orders must not count as pending exposure: %v", err)
	}
	if !wire.seen {
		t.Fatal("the in-cap order should have reached the wire")
	}
}

// A coin with NO position but a resting non-reduce-only order becomes a
// position on fill — max_open_positions must count it.
func TestMaxOpenPositionsCountsRestingOnlyCoin(t *testing.T) {
	ethResting := `[{"coin":"ETH","oid":7,"limitPx":"3000","origSz":"1.0","sz":"1.0","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0.0"}]`
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxOpenPositions: 1}), Options{},
		respWithOrders(ethResting, &wire))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"})
	assertRiskCode(t, err, "max_open_positions")
	if wire.seen {
		t.Fatal("the over-count order must never reach the wire")
	}
}

// An open-orders read failure blinds the resting-exposure math — exposure-adding
// orders must fail CLOSED (retryable exit 40), exactly like a position-read failure.
func TestOpenOrdersReadFailureFailsClosedForAdds(t *testing.T) {
	var wire wireOrder
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearingWith("1000000")
			case "frontendOpenOrders":
				return 429, `rate limited`
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		wire.seen = true
		return 200, okOrder(`{"resting":{"oid":1}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{}, resp)
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"})
	if err == nil {
		t.Fatal("an exposure-adding order must FAIL CLOSED when the open-orders read fails and a cap is configured")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %v", err)
	}
	// 429 maps to rate-limited (41) via the typed transport error; a 500 maps to
	// network (40) — both are retryable back-off shapes, never a silent pass and
	// never exit 20's "stop trying".
	if oe.Category != output.CatNetwork && oe.Category != output.CatRateLimit {
		t.Fatalf("want a retryable network/rate-limit category, got %s/%s (%v)", oe.Category, oe.Code, oe)
	}
	if !oe.Retryable {
		t.Fatalf("must be retryable:true, got %v", oe)
	}
	if wire.seen {
		t.Fatal("nothing may be signed/sent when the open-orders read failed")
	}
}

// PlaceBatch legs must see (position + resting + earlier-legs-in-batch): with
// $90k already resting, the batch's first in-cap leg is fine but the cumulative
// second leg breaches and the WHOLE batch is rejected pre-sign.
func TestPlaceBatchSeesRestingPlusEarlierLegs(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 130000}), Options{},
		respWithOrders(restingBTCBuys(3, false), &wire))
	// resting $90k; leg1 $30k => $120k (ok); leg2 $30k => $150k > $130k cap.
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"},
		{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"},
	})
	assertRiskCode(t, err, "max_position_notional")
	if wire.seen {
		t.Fatal("an atomically-rejected batch must sign nothing")
	}
}

// The batch paths must carry the SAME retryable fail-closed shape as single
// Place: batchLegErr may re-label a leg failure but must NOT strip retryable /
// retry_after_ms — an agent keying on the envelope's retryable field would
// treat a transient 429/500 burst as permanent and stop.
func TestPositionReadFailureFailsClosedForAddsPlaceBatch(t *testing.T) {
	var wire wireOrder
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 500, `upstream down`
			case "frontendOpenOrders":
				return 200, `[]`
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		wire.seen = true
		return 200, okOrder(`{"resting":{"oid":1}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{}, resp)
	_, _, err := c.PlaceBatch(ctx, []OrderReq{{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"}})
	if err == nil {
		t.Fatal("an exposure-adding batch leg must FAIL CLOSED when the position read fails and a cap is configured")
	}
	assertNetworkRetryable(t, err)
	if wire.seen {
		t.Fatal("nothing may be signed/sent when the cap's position read failed")
	}
}

// Same contract on the batch-modify path (its per-leg cap base is the filled
// position): a failed position read rejects retryably, not as a permanent 40.
func TestPositionReadFailureFailsClosedModifyBatch(t *testing.T) {
	var wire wireOrder
	front := "[" + openOrderJSON("BTC", 1, "") + "]"
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 500, `upstream down`
			case "frontendOpenOrders":
				return 200, front
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		wire.seen = true
		return 200, okOrder(`{"resting":{"oid":1}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{}, resp)
	_, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}})
	if err == nil {
		t.Fatal("a non-reduce-only modify must FAIL CLOSED when the position read fails and a cap is configured")
	}
	assertNetworkRetryable(t, err)
	if wire.seen {
		t.Fatal("nothing may be signed/sent when the cap's position read failed")
	}
}

// A tiny opposite-side FIRST leg must not dodge the resting-order accounting:
// the pending term is evaluated per (coin, side of THIS leg), never seeded once
// from the first leg's side. With $90k of non-RO buys resting and a $100k cap,
// the buy leg's worst case is $120k+ — rejected, whole batch unsigned. Before
// the fix the batch seeded base = sell-side pending (0) and signed.
func TestPlaceBatchMixedSideStillSeesRestingOrders(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{},
		respWithOrders(restingBTCBuys(3, false), &wire))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		{Coin: "BTC", Side: Sell, Size: "0.001", Limit: "60000"}, // $60 — fine on its own side
		{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"},    // $30k + $90k resting buys > cap
	})
	assertRiskCode(t, err, "max_position_notional")
	if wire.seen {
		t.Fatal("the mixed-side batch must sign nothing — it breaches the cap's worst case")
	}
}

// ---- finding 2 fix-up: resting orders may TIGHTEN net exposure, never LOOSEN it ----

// A resting non-reduce-only SELL must not offset a buy in the net-exposure
// math: pending orders fill independently, so a parked sell (priced never to
// fill, or an innocent one-sided grid) must not un-gate buys past
// max_net_exposure_usd. Flat book, $10k cap, one resting $30k sell: a $30k buy
// nets to 0 under signed netting and reached the wire — it must reject exit 20
// (worst case: only the buys fill → net +$30k).
func TestNetExposureNotLoosenedByRestingSells(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxNetExposureUSD: 10000}), Options{},
		respWithOrders(restingBTC("A", 1, false), &wire))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"})
	assertRiskCode(t, err, "max_net_exposure")
	if wire.seen {
		t.Fatal("a resting sell must never un-gate a buy past max_net_exposure — nothing may be signed")
	}
}

// The pre-resting-orders behavior is restored for a long book: long $100k +
// $30k buy = $130k net, over a $110k cap — a resting non-RO $30k sell (which
// may never fill) must not net that back down to $100k and pass. A reduce-only
// resting sell contributes zero everywhere, so genuine TP/SL exits never
// tighten (or loosen) the measured net either.
func TestNetExposureRestingSellCannotOffsetLongBook(t *testing.T) {
	orders := restingBTC("A", 1, false)
	resp := func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, clearingWith("1000000", posWith("BTC", "2", "100000"))
		case "frontendOpenOrders":
			return 200, orders
		case "spotClearinghouseState":
			return 200, noSpot
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxNetExposureUSD: 110000}), Options{}, resp)
	assertRiskCode(t, gateErr(c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 30000}})), "max_net_exposure")

	// The same sell as reduce-only is a genuine exit: zero pending contribution,
	// net = $100k + $5k = $105k <= $110k → passes.
	orders = restingBTC("A", 1, true)
	if err := gateErr(c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 5000}})); err != nil {
		t.Fatalf("a reduce-only resting sell contributes zero pending exposure: %v", err)
	}

	// Mirror direction still tightens: on a short book a non-RO resting sell adds
	// to the worst-case short side (this held before the fix too — pin it).
	orders = restingBTC("A", 1, false)
	shortResp := func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, clearingWith("1000000", posWith("BTC", "-2", "100000"))
		case "frontendOpenOrders":
			return 200, orders
		case "spotClearinghouseState":
			return 200, noSpot
		}
		return 200, `{}`
	}
	c2, ctx2 := newTestClient(t, riskCfg(config.Risk{MaxNetExposureUSD: 110000}), Options{}, shortResp)
	assertRiskCode(t, gateErr(c2.checkPortfolioGates(ctx2, []exposureDelta{{"BTC", -10000}})), "max_net_exposure")
}

// ---- follow-up: ONE open-orders fetch per gated write (429 budget) ----

// countingResp serves a healthy flat account and counts frontendOpenOrders hits.
func countingResp(hits *int) respFn {
	return func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearingWith("1000000")
			case "frontendOpenOrders":
				*hits++
				return 200, `[]`
			case "spotClearinghouseState":
				return 200, noSpot
			case "allMids":
				return 200, `{"BTC":"60000","ETH":"3000"}`
			}
			return 200, `{}`
		}
		return 200, okOrder(`{"resting":{"oid":9}}`)
	}
}

// With BOTH risk.max_position_notional_usd and a portfolio gate configured, one
// Place must hit the open-orders endpoint exactly ONCE (weight 20): the cap's
// fetch is threaded through to the portfolio snapshot instead of re-fetching.
func TestPlaceFetchesOpenOrdersOnceWithBothCapTypes(t *testing.T) {
	var hits int
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000, MaxAccountLeverage: 5}),
		Options{}, countingResp(&hits))
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("open orders fetched %d times; a write with both cap types must fetch exactly once", hits)
	}
}

// Same one-fetch contract on the batch path: the whole batch shares one read
// across the per-coin cap AND the portfolio gates.
func TestPlaceBatchFetchesOpenOrdersOnceWithBothCapTypes(t *testing.T) {
	var hits int
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/exchange" {
			return 200, okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`)
		}
		return countingResp(&hits)(path, typ, body)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000, MaxAccountLeverage: 5}),
		Options{}, resp)
	if _, _, err := c.PlaceBatch(ctx, []OrderReq{
		{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"},
		{Coin: "ETH", Side: Buy, Size: "0.1", Limit: "3000"},
	}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("open orders fetched %d times; a batch must fetch exactly once", hits)
	}
}

// ---- finding 2 pin: a silently-degraded SUB-DEX read fails the gates CLOSED ----

// A sub-dex clearinghouse or open-orders sweep that fails while a portfolio
// gate is configured under-counts the book (PortfolioView.degraded). An
// exposure-ADDING order must be rejected with the retryable exit-40 shape —
// never gated against the smaller book — while reduce-only still passes.
func TestSubDexDegradedFailsClosedForAdds(t *testing.T) {
	for _, failType := range []string{"clearinghouseState", "frontendOpenOrders"} {
		t.Run(failType, func(t *testing.T) {
			var wire wireOrder
			resp := func(path, typ string, body map[string]any) (int, string) {
				if path == "/info" {
					dex, _ := body["dex"].(string)
					if typ == failType && dex == "xyz" {
						return 500, `upstream down`
					}
					switch typ {
					case "clearinghouseState":
						return 200, clearingWith("100000", posWith("BTC", "0.1", "6000"))
					case "frontendOpenOrders":
						return 200, `[]`
					case "spotClearinghouseState":
						return 200, noSpot
					case "allMids":
						return 200, `{"BTC":"60000"}`
					}
					return 200, `{}`
				}
				wire.seen = true
				return 200, okOrder(`{"filled":{"totalSz":"0.05","avgPx":"60000","oid":9}}`)
			}
			cfg := riskCfg(config.Risk{MaxAccountLeverage: 5})
			cfg.PerpDexs = []string{"xyz"}
			c, ctx := newTestClient(t, cfg, Options{}, resp)

			_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"})
			if err == nil {
				t.Fatal("an exposure-adding order must FAIL CLOSED when a sub-dex read degrades under a portfolio gate")
			}
			assertNetworkRetryable(t, err)
			if wire.seen {
				t.Fatal("nothing may be signed against a degraded account snapshot")
			}

			// De-risking still works: the reduce-only exit skips the gates entirely.
			if _, _, rerr := c.Place(ctx, OrderReq{Coin: "BTC", Side: Sell, Size: "0.05", ReduceOnly: true}); rerr != nil {
				t.Fatalf("reduce-only must still pass on a degraded sub-dex snapshot: %v", rerr)
			}
			if !wire.seen {
				t.Fatal("the reduce-only exit should have reached the wire")
			}
		})
	}
}

// ---- finding 3: automation.limit_only covers trigger-market orders ----

func limitOnlyCfg(r config.Risk) *config.Config {
	cfg := riskCfg(r)
	cfg.Automation.LimitOnly = true
	return cfg
}

// A trigger order with is_market=true EXECUTES as a market order when it fires
// (instantly, if the trigger is already crossed) — limit_only must block it.
func TestLimitOnlyBlocksTriggerMarketPlace(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, limitOnlyCfg(config.Risk{}), Options{},
		respWithOrders(`[]`, &wire))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01",
		Trigger: &TriggerReq{TriggerPx: "64001", IsMarket: true, Tpsl: "sl"}})
	assertRiskCode(t, err, "limit_only")
	if wire.seen {
		t.Fatal("a limit_only-blocked trigger-market order must never reach the wire")
	}
}

// Same bypass through the batch path: one trigger-market leg rejects the batch.
func TestLimitOnlyBlocksTriggerMarketPlaceBatch(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, limitOnlyCfg(config.Risk{}), Options{},
		respWithOrders(`[]`, &wire))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"},
		{Coin: "BTC", Side: Buy, Size: "0.01",
			Trigger: &TriggerReq{TriggerPx: "64001", IsMarket: true, Tpsl: "sl"}},
	})
	assertRiskCode(t, err, "limit_only")
	if wire.seen {
		t.Fatal("a limit_only-blocked batch must sign nothing")
	}
}

// A trigger LIMIT order (is_market=false) rests at its limit when it fires —
// it stays allowed under limit_only.
func TestLimitOnlyAllowsTriggerLimit(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, limitOnlyCfg(config.Risk{}), Options{},
		respWithOrders(`[]`, &wire))
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "59000",
		Trigger: &TriggerReq{TriggerPx: "59500", IsMarket: false, Tpsl: "sl"}}); err != nil {
		t.Fatalf("a trigger LIMIT order must pass under limit_only: %v", err)
	}
	if !wire.seen {
		t.Fatal("the trigger-limit order should have reached the wire")
	}
}

// The documented exemption is preserved: an exit (reduce-only) may market out
// under limit_only.
func TestLimitOnlyStillExemptsReduceOnlyMarket(t *testing.T) {
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearingWith("100000", posWith("BTC", "0.1", "6400"))
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "frontendOpenOrders":
				return 200, `[]`
			case "spotClearinghouseState":
				return 200, noSpot
			}
			return 200, `{}`
		}
		return 200, okOrder(`{"filled":{"totalSz":"0.05","avgPx":"64000","oid":9}}`)
	}
	c, ctx := newTestClient(t, limitOnlyCfg(config.Risk{}), Options{}, resp)
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Sell, Size: "0.05", ReduceOnly: true}); err != nil {
		t.Fatalf("a reduce-only market exit must stay exempt from limit_only: %v", err)
	}
}

// ---- finding 4: risk-state anchors keyed per network+account ----

// A testnet equity peak must not gate mainnet trading (and vice versa), and two
// accounts must not share anchors: each network+account context re-anchors
// independently.
func TestRiskStateAnchorsKeyedPerNetworkAndAccount(t *testing.T) {
	clearing := clearingWith("10000")
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDrawdownPct: 20}), Options{}, gateResp(&clearing, noSpot))
	if err := gateErr(c.checkPortfolioGates(ctx, nil)); err != nil { // testnet/master peak = 10000
		t.Fatalf("first observation sets the peak: %v", err)
	}

	// Same home dir, mainnet, 10x smaller equity: with a shared unkeyed file this
	// reads as a 90% drawdown and bricks trading; keyed anchors must pass.
	clearing = clearingWith("1000")
	c.network = "mainnet"
	if err := gateErr(c.checkPortfolioGates(ctx, nil)); err != nil {
		t.Fatalf("a mainnet account must not be judged against the testnet peak: %v", err)
	}

	// Different account on the original network: independent anchors too.
	c.network = "testnet"
	c.queryAddr = "0xfeedfacefeedfacefeedfacefeedfacefeedface"
	if err := gateErr(c.checkPortfolioGates(ctx, nil)); err != nil {
		t.Fatalf("a second account must not inherit the first account's peak: %v", err)
	}
}

// The legacy unkeyed risk_state.json is IGNORED: its anchors are never adopted
// for any context (wrong anchors are worse than fresh ones), the file is left
// untouched on disk (downgrade-safe), and the first run of a network+account
// anchors fresh with a loud envelope warning (fresh anchors under-protect until
// the next UTC day).
func TestLegacyRiskStateIgnoredFreshAnchorWarned(t *testing.T) {
	clearing := clearingWith("1000")
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearing
			case "frontendOpenOrders":
				return 200, `[]`
			case "spotClearinghouseState":
				return 200, noSpot
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		return 200, okOrder(`{"resting":{"oid":1}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDrawdownPct: 20}), Options{}, resp)

	// Seed a LEGACY unkeyed file with a 10x peak (e.g. from a testnet faucet).
	legacy := filepath.Join(config.Dir(), "risk_state.json")
	_ = os.MkdirAll(filepath.Dir(legacy), 0o700)
	seed := []byte(`{"peak_equity":10000,"day":"1970-01-01","day_anchor_equity":10000,"basis":2}`)
	if err := os.WriteFile(legacy, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	_, warnings, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"})
	if err != nil {
		t.Fatalf("the legacy peak must be ignored (fresh anchor at current equity), got %v", err)
	}
	var warned bool
	for _, w := range warnings {
		if strings.Contains(w, "anchor") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("re-initialized anchors must emit an envelope warning (operator must know the gates under-protect until re-anchored); warnings=%v", warnings)
	}
	after, rerr := os.ReadFile(legacy)
	if rerr != nil || string(after) != string(seed) {
		t.Fatalf("the legacy risk_state.json must be left untouched on disk (err=%v)", rerr)
	}
}
