package core

// Modify/ModifyBatch portfolio-gate coverage (review 2026-07-01, CRITICAL
// "Modify/ModifyBatch add unbounded new exposure while skipping every
// account-wide portfolio gate"): a modify REPLACES its own resting order, so
// every gate evaluates the book with the OLD order swapped for the REPLACEMENT
// at its new size/price —
//   - growing a resting order trips the account-wide gates (incl. an
//     already-tripped drawdown/daily-loss pause) exactly like Place;
//   - a routine re-price never double-counts the order against itself;
//   - a modify that does not raise the order's worst-case contribution is
//     de-risking (parity with the all-reduce-only batch rule) and is exempt;
//   - a reduce-only order keeps contributing zero on both sides of the swap;
//   - the per-coin max_position_notional cap sees the OTHER resting orders;
//   - ModifyBatch applies earlier legs' replacements before evaluating later
//     legs, with ONE open-orders fetch for the whole batch.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// modOrderJSON is one resting FrontendOpenOrder row with explicit size, price,
// side ("B" buy / "A" sell) and reduce-only flag.
func modOrderJSON(coin string, oid int64, sz, px, side string, ro bool) string {
	return fmt.Sprintf(`{"coin":%q,"oid":%d,"cloid":null,"limitPx":%q,"origSz":%q,"sz":%q,"side":%q,"reduceOnly":%v,"orderType":"Limit","tif":"Gtc","isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0","timestamp":1}`,
		coin, oid, px, sz, sz, side, ro)
}

// modifyGateResp serves the reads Modify's gates need: a clearinghouse with the
// given equity/positions, the given open-order book, and mids. It counts the
// open-orders fetches (the whole modify invocation must make exactly ONE) and
// flags any signed action reaching the wire.
func modifyGateResp(clearing, front string, wire *wireOrder, openOrderHits *int) respFn {
	return func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearing
			case "frontendOpenOrders":
				if openOrderHits != nil {
					*openOrderHits++
				}
				return 200, front
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
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
}

// seedTrippedDrawdown persists a prior equity peak for the test client's
// network+account, so a gated write against a much smaller live equity trips
// max_drawdown. Must run AFTER newTestClient (which points DELIVERATOR_HOME at
// the test's temp dir).
func seedTrippedDrawdown(t *testing.T, peak float64) {
	t.Helper()
	b, _ := json.Marshal(riskState{PeakEquity: peak, Day: "1970-01-01", DayAnchorEquity: peak, Basis: currentEquityBasis})
	_ = os.MkdirAll(filepath.Dir(riskStatePath("testnet", testMaster)), 0o700)
	if err := os.WriteFile(riskStatePath("testnet", testMaster), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The review's live repro: equity $1000, max_account_leverage 1. Place of 1 BTC
// @ $60k is rejected, but a tiny compliant $60 resting order could be MODIFIED
// up to the same $60k — the gates must see the replacement and reject exit 20.
// Also pins the one-fetch contract: the invocation reads open orders exactly
// once (the resolve fetch doubles as the gates' book).
func TestModifyGrowTripsPortfolioGates(t *testing.T) {
	var wire wireOrder
	var hits int
	front := "[" + modOrderJSON("BTC", 1, "0.001", "60000", "B", false) + "]" // $60 resting
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 1}), Options{},
		modifyGateResp(clearingWith("1000"), front, &wire, &hits))
	_, _, err := c.Modify(ctx, i64(1), "", "1", "") // grow to 1 BTC = $60k on $1k equity
	assertRiskCode(t, err, "max_account_leverage")
	if wire.seen {
		t.Fatal("a gate-rejected modify must never reach the wire")
	}
	if hits != 1 {
		t.Fatalf("open orders fetched %d times; a modify must fetch exactly once (resolve + gates share the read)", hits)
	}
}

// A re-price must not double-count the order against itself: the gates see the
// book MINUS the old order PLUS the replacement. One $1200 resting buy
// re-priced to $1220 on $1000 equity is 1.22x — within the 2x cap. Counting
// old + new (2.42x) would false-reject a routine re-price.
func TestModifyRepriceDoesNotDoubleCountItself(t *testing.T) {
	var wire wireOrder
	front := "[" + modOrderJSON("BTC", 1, "0.02", "60000", "B", false) + "]" // $1200 resting
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 2}), Options{},
		modifyGateResp(clearingWith("1000"), front, &wire, nil))
	if _, _, err := c.Modify(ctx, i64(1), "", "", "61000"); err != nil { // same size, $1220
		t.Fatalf("a re-price must not double-count the modified order: %v", err)
	}
	if !wire.seen {
		t.Fatal("the in-cap re-price should have reached the wire")
	}
}

// With the drawdown pause tripped, GROWING via modify is rejected (the bypass
// the review demonstrated) while SHRINKING still passes — shrinking is
// de-risking, parity with the all-reduce-only batch rule.
func TestModifyShrinkExemptFromTrippedDrawdown(t *testing.T) {
	var wire wireOrder
	front := "[" + modOrderJSON("BTC", 1, "0.001", "60000", "B", false) + "]"
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDrawdownPct: 20}), Options{},
		modifyGateResp(clearingWith("1000"), front, &wire, nil))
	seedTrippedDrawdown(t, 10000) // live equity 1000 => 90% drawdown, gate tripped

	_, _, err := c.Modify(ctx, i64(1), "", "0.002", "") // grow: must hit the pause
	assertRiskCode(t, err, "max_drawdown")
	if wire.seen {
		t.Fatal("a drawdown-paused grow-modify must never reach the wire")
	}

	if _, _, serr := c.Modify(ctx, i64(1), "", "0.0005", ""); serr != nil { // shrink: de-risking
		t.Fatalf("a shrinking modify must pass an already-tripped drawdown gate: %v", serr)
	}
	if !wire.seen {
		t.Fatal("the shrinking modify should have reached the wire")
	}
}

// A reduce-only order contributes zero worst-case exposure before AND after the
// swap — modifying one (even growing it) stays exempt from a tripped gate, like
// every other reduce-only de-risking path.
func TestModifyReduceOnlyExemptFromTrippedDrawdown(t *testing.T) {
	var wire wireOrder
	front := "[" + modOrderJSON("BTC", 1, "0.05", "60000", "A", true) + "]" // RO sell on a long
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDrawdownPct: 20}), Options{},
		modifyGateResp(clearingWith("1000", posWith("BTC", "0.1", "6000")), front, &wire, nil))
	seedTrippedDrawdown(t, 10000)
	if _, _, err := c.Modify(ctx, i64(1), "", "0.08", ""); err != nil {
		t.Fatalf("a reduce-only modify contributes zero exposure and must pass a tripped gate: %v", err)
	}
	if !wire.seen {
		t.Fatal("the reduce-only modify should have reached the wire")
	}
}

// The per-coin max_position_notional cap on a modify must count the OTHER
// resting orders (the modified order swaps for its replacement, everything else
// stays): two $30k resting buys, modify one to $90k => worst case $120k > the
// $100k cap. Before the fix the cap saw only the FILLED book ($0) and passed.
func TestModifyPerCoinCapSeesRestingOrders(t *testing.T) {
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{},
		respWithOrders(restingBTCBuys(2, false), &wire))
	_, _, err := c.Modify(ctx, i64(1), "", "1.5", "") // 0.5 -> 1.5 BTC @ 60000: $90k + $30k other resting
	assertRiskCode(t, err, "max_position_notional")
	if wire.seen {
		t.Fatal("the over-cap modify must never reach the wire")
	}
}

// ModifyBatch: growing legs trip the portfolio gates atomically (nothing
// signed), with the replacements — not the old orders — in the evaluated book.
func TestModifyBatchGrowTripsPortfolioGates(t *testing.T) {
	var wire wireOrder
	var hits int
	front := "[" + modOrderJSON("BTC", 1, "0.001", "60000", "B", false) + "," +
		modOrderJSON("BTC", 2, "0.001", "60000", "B", false) + "]"
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 1}), Options{},
		modifyGateResp(clearingWith("1000"), front, &wire, &hits))
	_, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Size: "0.5"}, {Oid: i64(2), Size: "0.5"}})
	assertRiskCode(t, err, "max_account_leverage") // 2 × $30k replacements = 60x on $1k
	if wire.seen {
		t.Fatal("a gate-rejected batch modify must sign nothing")
	}
	if hits != 1 {
		t.Fatalf("open orders fetched %d times; a batch modify must fetch exactly once", hits)
	}
}

// Each ModifyBatch leg sees the resting book with all EARLIER legs'
// replacements applied: leg 1 shrinks $30k->$6k, so leg 2's $72k grow is
// evaluated against $6k (78k <= 100k cap, passes). Against the un-replaced book
// ($30k) it would false-reject at $102k.
func TestModifyBatchEarlierLegReplacementsApplied(t *testing.T) {
	front := "[" + modOrderJSON("BTC", 1, "0.5", "60000", "B", false) + "," +
		modOrderJSON("BTC", 2, "0.5", "60000", "B", false) + "]"
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, clearingWith("1000000")
			case "frontendOpenOrders":
				return 200, front
			case "spotClearinghouseState":
				return 200, noSpot
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		return 200, okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`)
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxPositionNotionalUSD: 100000}), Options{}, resp)
	res, _, err := c.ModifyBatch(ctx, []ModifyReq{
		{Oid: i64(1), Size: "0.1"}, // shrink $30k -> $6k
		{Oid: i64(2), Size: "1.2"}, // grow  $30k -> $72k; $72k + $6k = $78k <= cap
	})
	if err != nil {
		t.Fatalf("later legs must see earlier legs' replacements (not the stale book): %v", err)
	}
	if len(res) != 2 || res[0].Status != "resting" || res[1].Status != "resting" {
		t.Fatalf("batch modify results wrong: %+v", res)
	}
}

// A degraded SUB-DEX open-orders sweep hides resting exposure, so an
// exposure-INCREASING modify fails CLOSED (retryable exit 40) — while a
// shrinking modify of a main-dex order still passes: de-risking must never be
// blocked by a read failure.
func TestModifyIncreaseFailsClosedOnStaleSubDex(t *testing.T) {
	var wire wireOrder
	front := "[" + modOrderJSON("BTC", 1, "0.001", "60000", "B", false) + "]"
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			dex, _ := body["dex"].(string)
			switch typ {
			case "clearinghouseState":
				return 200, clearingWith("1000000")
			case "frontendOpenOrders":
				if dex == "xyz" {
					return 500, `upstream down`
				}
				return 200, front
			case "spotClearinghouseState":
				return 200, noSpot
			case "allMids":
				return 200, `{"BTC":"60000"}`
			}
			return 200, `{}`
		}
		wire.seen = true
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	cfg := riskCfg(config.Risk{MaxAccountLeverage: 5})
	cfg.PerpDexs = []string{"xyz"}
	c, ctx := newTestClient(t, cfg, Options{}, resp)

	_, _, err := c.Modify(ctx, i64(1), "", "0.002", "") // grow: needs the COMPLETE book
	if err == nil {
		t.Fatal("an exposure-increasing modify must FAIL CLOSED when a sub-dex sweep degrades under a portfolio gate")
	}
	assertNetworkRetryable(t, err)
	if wire.seen {
		t.Fatal("nothing may be signed on a degraded book")
	}

	if _, _, serr := c.Modify(ctx, i64(1), "", "0.0005", ""); serr != nil { // shrink: exempt
		t.Fatalf("a shrinking modify must still pass on a degraded sub-dex sweep: %v", serr)
	}
	if !wire.seen {
		t.Fatal("the shrinking modify should have reached the wire")
	}
}
