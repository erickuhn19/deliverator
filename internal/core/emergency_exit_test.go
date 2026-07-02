package core

// Emergency-exit contract (review 2026-07-01, cluster P0-C): the emergency
// levers must never be the thing a tripped safety gate blocks.
//  1. Panic must flatten positions DURING a global halt — its flatten legs
//     bypass the halt gate via a core-internal path (never a CLI flag), while
//     plain close and plain orders stay halt-rejected (exit 21).
//  2. A batch whose EVERY leg is reduce-only adds no exposure, so it is exempt
//     from the account-wide portfolio gates (parity with Place/Twap) —
//     de-risking via `grid --reduce-only` / a multi-leg reduce must work
//     exactly when a drawdown/daily-loss gate has paused trading. One
//     exposure-adding leg keeps the whole batch gated, and reduce-only legs
//     still hit the flip guard.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// haltedPanicResp serves a halted-panic scenario: one resting order + a long
// 0.1 BTC position until the flatten order hits /exchange, then clean reads so
// the strict re-verification can confirm the teardown. wire captures the
// flatten leg's reduce-only flag.
func haltedPanicResp(wire *wireOrder) respFn {
	long := clearingWith("100000", posWith("BTC", "0.1", "6400"))
	openOrder := `[{"coin":"BTC","oid":7,"limitPx":"60000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Gtc","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	flattened := false
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				if flattened {
					return 200, emptyState
				}
				return 200, long
			case "frontendOpenOrders":
				if flattened {
					return 200, `[]`
				}
				return 200, openOrder
			case "spotClearinghouseState":
				return 200, noSpot
			case "webData2":
				return 200, `{"twapStates":[]}`
			}
			return 200, `{}`
		}
		switch typ {
		case "cancel":
			return 200, cancelOk
		case "order":
			if a, ok := body["action"].(map[string]any); ok {
				if orders, ok := a["orders"].([]any); ok && len(orders) > 0 {
					if o, ok := orders[0].(map[string]any); ok {
						wire.seen = true
						wire.r, _ = o["r"].(bool)
					}
				}
			}
			flattened = true
			return 200, okOrder(`{"filled":{"totalSz":"0.1","avgPx":"64000","oid":9}}`)
		}
		return 200, defaultEx
	}
}

// The review's repro: halt on, then panic. The flatten legs must bypass the
// halt gate (panic must work during a halt) — before the fix every Close leg
// died with 'global halt active — close rejected' and every position stayed
// open while the operator believed the emergency lever was pulled.
func TestPanicFlattensDuringHalt(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, config.Default(), Options{}, haltedPanicResp(&wire))
	if err := SetHalt(true); err != nil {
		t.Fatal(err)
	}
	res, err := c.Panic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 1 {
		t.Errorf("panic must still cancel resting orders under a halt: %+v", res)
	}
	if len(res.Closed) != 1 {
		t.Fatalf("want 1 flatten leg, got %+v", res.Closed)
	}
	if m, ok := res.Closed[0].(map[string]any); ok {
		t.Fatalf("flatten leg failed under halt: %v", m["error"])
	}
	if !wire.seen {
		t.Fatal("no flatten order reached the wire under a halt")
	}
	if !wire.r {
		t.Fatal("the panic flatten must be reduce-only on the wire")
	}
	if !res.Complete {
		t.Errorf("a clean halted teardown should verify Complete: %+v", res)
	}
}

// The bypass is core-internal to panic ONLY: a plain close and a plain order
// during a halt must still reject with exit 21.
func TestHaltStillRejectsPlainCloseAndOrder(t *testing.T) {
	long := clearingWith("100000", posWith("BTC", "0.1", "6400"))
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp(long, "", "", `SHOULD_NOT_BE_CALLED`))
	if err := SetHalt(true); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Close(ctx, "BTC", "", true, "", "")
	assertErr(t, err, output.CatHalt, output.ExitHalt)
	_, _, err = c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatHalt, output.ExitHalt)
}

// trippedDrawdownClient builds a client whose max_drawdown gate is ALREADY
// breached: persisted peak equity 5000 vs live equity 1000 (80% > the 20% cap),
// holding a long 0.1 BTC. /exchange answers any order action with one resting
// status per leg.
func trippedDrawdownClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	long := clearingWith("1000", posWith("BTC", "0.1", "3000"))
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, long
			case "frontendOpenOrders":
				return 200, `[]`
			case "spotClearinghouseState":
				return 200, noSpot
			}
			return 200, `{}`
		}
		if a, ok := body["action"].(map[string]any); ok {
			if orders, ok := a["orders"].([]any); ok && len(orders) > 0 {
				sts := make([]string, len(orders))
				for i := range sts {
					sts[i] = `{"resting":{"oid":` + strconv.Itoa(i+1) + `}}`
				}
				return 200, okOrders(sts...)
			}
		}
		return 200, defaultEx
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDrawdownPct: 20}), Options{}, resp)
	seed, _ := json.Marshal(riskState{PeakEquity: 5000, Day: "1970-01-01", DayAnchorEquity: 5000, Basis: currentEquityBasis})
	_ = os.MkdirAll(filepath.Dir(riskStatePath("testnet", testMaster)), 0o700)
	if err := os.WriteFile(riskStatePath("testnet", testMaster), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	return c, ctx
}

func roSell(size string) OrderReq {
	return OrderReq{Coin: "BTC", Side: Sell, Size: size, Limit: "64000", Tif: "Gtc", ReduceOnly: true}
}

// With the drawdown gate tripped, a batch whose EVERY leg is reduce-only adds
// no exposure and must pass — before the fix the empty delta set was still
// evaluated against the breached book and the whole de-risking batch was
// rejected exit 20, exactly when trading was paused.
func TestPlaceBatchAllReduceOnlyExemptFromTrippedGates(t *testing.T) {
	c, ctx := trippedDrawdownClient(t)
	res, _, err := c.PlaceBatch(ctx, []OrderReq{roSell("0.04"), roSell("0.04")})
	if err != nil {
		t.Fatalf("an all-reduce-only batch must pass a tripped portfolio gate: %v", err)
	}
	for i, r := range res {
		if r.Status != "resting" {
			t.Errorf("leg %d should rest: %+v", i, r)
		}
	}
}

// One exposure-adding leg keeps the WHOLE batch fully gated (exit 20), and the
// single-order paths keep their existing behavior: a plain order is rejected by
// the tripped gate, a single reduce-only order stays exempt.
func TestPlaceBatchMixedAndSingleOrderStayGated(t *testing.T) {
	c, ctx := trippedDrawdownClient(t)
	// Mixed: reduce-only + a plain buy => the batch stays gated.
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		roSell("0.04"),
		{Coin: "BTC", Side: Buy, Size: "0.001", Limit: "60000", Tif: "Gtc"},
	})
	assertRiskCode(t, err, "max_drawdown")
	// Single plain order: rejected by the tripped gate (unchanged).
	_, _, err = c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.001", Limit: "60000", Tif: "Gtc"})
	assertRiskCode(t, err, "max_drawdown")
	// Single reduce-only order: exempt (unchanged).
	if _, _, err := c.Place(ctx, roSell("0.04")); err != nil {
		t.Fatalf("a single reduce-only order must stay exempt: %v", err)
	}
}

// The portfolio-gate exemption must NOT skip the flip guard: a reduce-only
// batch leg that cannot actually reduce (same side as the position, or crossing
// zero) still rejects exit 20 even while the gates are tripped.
func TestPlaceBatchReduceOnlyLegsStillFlipGuarded(t *testing.T) {
	c, ctx := trippedDrawdownClient(t)
	// Same side as the long: adds exposure under a reduce-only label.
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		roSell("0.04"),
		{Coin: "BTC", Side: Buy, Size: "0.04", Limit: "60000", Tif: "Gtc", ReduceOnly: true},
	})
	assertRiskCode(t, err, "reduce_only_same_side")
	// Oversized on the reducing side: could only fill by crossing zero.
	_, _, err = c.PlaceBatch(ctx, []OrderReq{roSell("0.2")})
	assertRiskCode(t, err, "reduce_only_flip")
}
