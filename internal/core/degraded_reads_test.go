package core

// Reads-correctness cluster (review 2026-07-01, cluster NEXT-2), items 1–3:
//  1. a degraded account snapshot must be VISIBLE on every read surface built
//     from PortfolioView — portfolio/positions/orders carry the unreadable
//     inputs (degraded + degraded_dexs) instead of silently showing a smaller
//     book. The reads stay tolerant (exit 0) — only the gates fail closed;
//  2. order-status-by-cloid must never report a confident "absent" from a
//     DEGRADED open-orders sweep — the order may be resting on the unreadable
//     dex, and "absent → resubmit" is the documented double-place hazard.
//     Degraded + not found = retryable exit 40, not the stale historical row;
//  3. observeEquity must never record drawdown/daily-loss anchors from a
//     degraded snapshot, through ANY path (gates refuse; the read-only risk
//     status never writes).

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// degradedSubDexResp serves a main dex with one BTC short and an xyz sub-dex
// whose clearinghouse and/or open-orders sweep fail with a 500 — the transient
// partial-read case the surfaces must report instead of silently shrinking.
func degradedSubDexResp(failSubState, failSubOrders, failSpot bool) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, okOrder(`{"resting":{"oid":1}}`)
		}
		switch typ {
		case "clearinghouseState":
			if body["dex"] == "xyz" {
				if failSubState {
					return 500, "boom"
				}
				return 200, goldShortBare
			}
			return 200, emptyState
		case "frontendOpenOrders":
			if body["dex"] == "xyz" {
				if failSubOrders {
					return 500, "boom"
				}
				return 200, "[]"
			}
			return 200, "[" + openOrderJSON("BTC", 7, "") + "]"
		case "spotClearinghouseState":
			if failSpot {
				return 500, "boom"
			}
			return 200, `{"balances":[]}`
		case "allMids":
			return 200, `{"BTC":"60000"}`
		}
		return 200, "{}"
	}
}

func subDexCfg() *config.Config {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	return cfg
}

// ---- item 1: degradation is visible on portfolio ----

// A failed sub-dex clearinghouse read must be NAMED on the returned portfolio
// (degraded + degraded_dexs), while the read itself still succeeds.
func TestPortfolioSurfacesDegradedSubDexState(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(true, false, false))
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatalf("a degraded sub-dex must not fail the tolerant read: %v", err)
	}
	if len(pf.DegradedDexs) != 1 || pf.DegradedDexs[0] != "xyz" {
		t.Fatalf("degraded_dexs must name the unreadable sub-dex, got %v", pf.DegradedDexs)
	}
	if len(pf.Degraded) == 0 || !strings.Contains(strings.Join(pf.Degraded, ","), "sub-dex state (xyz)") {
		t.Fatalf("degraded must describe the unreadable input, got %v", pf.Degraded)
	}
}

// A failed sub-dex open-orders sweep is degradation too (invisible resting
// orders are invisible exposure).
func TestPortfolioSurfacesDegradedOpenOrdersSweep(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(false, true, false))
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.DegradedDexs) != 1 || pf.DegradedDexs[0] != "xyz" {
		t.Fatalf("degraded_dexs must name the dex whose order sweep failed, got %v", pf.DegradedDexs)
	}
	if !strings.Contains(strings.Join(pf.Degraded, ","), "open orders (xyz)") {
		t.Fatalf("degraded must name the failed order sweep, got %v", pf.Degraded)
	}
}

// A swallowed SpotUserState error is degradation (understated equity/balances)
// but not a sub-dex, so degraded_dexs stays empty.
func TestPortfolioSurfacesDegradedSpotState(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(false, false, true))
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(pf.Degraded, ","), "spot state") {
		t.Fatalf("degraded must name the unreadable spot state, got %v", pf.Degraded)
	}
	if len(pf.DegradedDexs) != 0 {
		t.Fatalf("spot degradation is not a dex, degraded_dexs must be empty, got %v", pf.DegradedDexs)
	}
}

// A clean read carries NO degradation markers (no warning noise).
func TestPortfolioCleanNoDegradedMarkers(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(false, false, false))
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Degraded) != 0 || len(pf.DegradedDexs) != 0 {
		t.Fatalf("clean read must carry no degradation, got degraded=%v dexs=%v", pf.Degraded, pf.DegradedDexs)
	}
	if len(pf.ReadMeta().EnvelopeWarnings()) != 0 {
		t.Fatalf("clean read must produce no warnings, got %v", pf.ReadMeta().EnvelopeWarnings())
	}
}

// ---- item 1: degradation is visible on positions / orders ----

// Positions must still return the readable book AND report the unreadable dex.
func TestPositionsReportsDegradedSubDex(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(true, false, false))
	pos, rm, err := c.Positions(ctx, "")
	if err != nil {
		t.Fatalf("positions must stay tolerant of a degraded sub-dex: %v", err)
	}
	_ = pos // main dex is flat here; the point is the degradation marker
	if len(rm.DegradedDexs) != 1 || rm.DegradedDexs[0] != "xyz" {
		t.Fatalf("positions must report the unreadable dex, got %+v", rm)
	}
	if len(rm.EnvelopeWarnings()) == 0 {
		t.Fatal("degraded positions read must emit a warning")
	}
}

// Orders must return the main-dex orders AND report the failed sub-dex sweep.
func TestOrdersReportsDegradedSweep(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(false, true, false))
	orders, rm, err := c.Orders(ctx, "")
	if err != nil {
		t.Fatalf("orders must stay tolerant of a degraded sub-dex sweep: %v", err)
	}
	if len(orders) != 1 || orders[0].Coin != "BTC" {
		t.Fatalf("main-dex orders must still be returned, got %+v", orders)
	}
	if len(rm.DegradedDexs) != 1 || rm.DegradedDexs[0] != "xyz" {
		t.Fatalf("orders must report the unreadable dex, got %+v", rm)
	}
	if len(rm.EnvelopeWarnings()) == 0 {
		t.Fatal("degraded orders read must emit a warning")
	}
}

// ---- item 2: degraded sweep + cloid not found is NEVER a confident absent ----

const staleCanceledFor = `{"status":"order","order":{"status":"canceled","statusTimestamp":1,"order":{"coin":"BTC","side":"B","limitPx":"64000","sz":"0.01","oid":42,"origSz":"0.01","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Gtc","children":[],"cloid":"%CLOID%"}}}`

// With the xyz sweep failing and the cloid not resting on the readable dexes,
// order status must return a RETRYABLE network error (exit 40) — never fall
// through to the historical query, whose stale "canceled" row would tell the
// agent to resubmit an order that may be live on the unreadable dex.
func TestOrderStatusDegradedSweepNotFoundIsRetryable(t *testing.T) {
	cl := "0x0000000000000000000000000000d111"
	sawHistorical := false
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, "{}"
		}
		switch typ {
		case "frontendOpenOrders":
			if body["dex"] == "xyz" {
				return 500, "boom"
			}
			return 200, "[]"
		case "orderStatus":
			sawHistorical = true
			return 200, strings.ReplaceAll(staleCanceledFor, "%CLOID%", cl)
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, subDexCfg(), Options{}, resp)
	res, err := c.OrderStatus(ctx, nil, cl)
	if err == nil {
		t.Fatalf("degraded sweep + not found must be an error, got %+v", res)
	}
	assertNetworkRetryable(t, err)
	if sawHistorical {
		t.Fatal("the historical orderStatus query must NOT be consulted on a degraded sweep — its stale row invites a double-place")
	}
}

// A cloid FOUND on a readable dex is still answered normally even while another
// sub-dex sweep is degraded — found is found.
func TestOrderStatusDegradedSweepFoundStillReturnsLive(t *testing.T) {
	cl := "0x0000000000000000000000000000d222"
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, "{}"
		}
		switch typ {
		case "frontendOpenOrders":
			if body["dex"] == "xyz" {
				return 500, "boom"
			}
			return 200, "[" + openOrderJSON("BTC", 99, cl) + "]"
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, subDexCfg(), Options{}, resp)
	oq, err := c.OrderStatus(ctx, nil, cl)
	if err != nil {
		t.Fatalf("a found live order must be returned despite the degraded sweep: %v", err)
	}
	if oq.Order.Status != "open" || oq.Order.Order.Oid != 99 {
		t.Fatalf("want the live order, got %+v", oq.Order)
	}
}

// A TOTAL live-sweep failure (main dex read down) is the same hazard: no
// confident historical fallback, retryable error instead.
func TestOrderStatusMainSweepFailureIsRetryable(t *testing.T) {
	cl := "0x0000000000000000000000000000d333"
	sawHistorical := false
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, "{}"
		}
		switch typ {
		case "frontendOpenOrders":
			return 500, "boom"
		case "orderStatus":
			sawHistorical = true
			return 200, strings.ReplaceAll(staleCanceledFor, "%CLOID%", cl)
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	_, err := c.OrderStatus(ctx, nil, cl)
	if err == nil {
		t.Fatal("a failed live sweep must be an error, not a silent historical fallback")
	}
	assertNetworkRetryable(t, err)
	if sawHistorical {
		t.Fatal("historical orderStatus must not be consulted when the live sweep failed")
	}
}

// ---- item 3: observeEquity never records from a degraded snapshot ----

// With a trajectory gate configured and the sub-dex unreadable, the gated write
// must refuse (retryable) BEFORE observeEquity runs — no risk-state file may be
// created from the degraded (understated) equity.
func TestGateNeverAnchorsEquityFromDegradedSnapshot(t *testing.T) {
	cfg := subDexCfg()
	cfg.Risk = config.Risk{MaxDrawdownPct: 50}
	c, ctx := newTestClient(t, cfg, Options{}, degradedSubDexResp(true, false, false))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.5", Limit: "60000"})
	if err == nil {
		t.Fatal("a gated write on a degraded snapshot must refuse")
	}
	assertNetworkRetryable(t, err)
	if _, serr := os.Stat(riskStatePath(c.network, c.queryAddr)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("no risk-state anchors may be recorded from a degraded snapshot (stat err=%v)", serr)
	}
}

// Once anchored from a HEALTHY snapshot, a later degraded snapshot must refuse
// the write and leave the persisted anchors byte-identical (no re-anchor, no
// poisoned day anchor).
func TestDegradedSnapshotNeverMutatesExistingAnchors(t *testing.T) {
	cfg := subDexCfg()
	cfg.Risk = config.Risk{MaxDrawdownPct: 50}
	degraded := false
	resp := func(path, typ string, body map[string]any) (int, string) {
		if degraded {
			return degradedSubDexResp(true, false, false)(path, typ, body)
		}
		return degradedSubDexResp(false, false, false)(path, typ, body)
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)

	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"}); err != nil {
		t.Fatalf("healthy gated write should pass: %v", err)
	}
	before, err := os.ReadFile(riskStatePath(c.network, c.queryAddr))
	if err != nil {
		t.Fatalf("healthy write must anchor the risk state: %v", err)
	}

	degraded = true
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000"}); err == nil {
		t.Fatal("degraded snapshot must refuse the gated write")
	}
	after, err := os.ReadFile(riskStatePath(c.network, c.queryAddr))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("degraded snapshot mutated the persisted anchors:\nbefore %s\nafter  %s", before, after)
	}
}

// The read-only risk status (also the console polling path) must never create or
// mutate the anchors — even when fed a degraded portfolio — and must carry the
// degradation markers through to its own view.
func TestRiskStatusFromDegradedPortfolioIsReadOnlyAndMarked(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(true, false, false))
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rv := c.RiskStatusFromPortfolio(pf)
	if len(rv.DegradedDexs) != 1 || rv.DegradedDexs[0] != "xyz" {
		t.Fatalf("risk view must carry the portfolio's degradation, got %+v", rv.DegradedDexs)
	}
	if _, serr := os.Stat(riskStatePath(c.network, c.queryAddr)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the read-only risk view must never write risk state (stat err=%v)", serr)
	}
}

// Source-level pin: observeEquity (the ONLY writer of the drawdown/daily-loss
// anchors) may be called from exactly one place — checkPortfolioGatesBook, which
// sits behind portfolioEquitySnapshot's degraded-snapshot refusal. A new caller
// must consciously update this test AND prove it cannot consume degraded equity.
func TestObserveEquityHasExactlyOneCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`observeEquity\(`)
	calls := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func observeEquity(") {
				continue
			}
			calls += len(re.FindAllString(line, -1))
		}
	}
	if calls != 1 {
		t.Fatalf("observeEquity must have exactly ONE call site (checkPortfolioGatesBook, behind the degraded-snapshot refusal); found %d — a new caller must be proven unable to record degraded equity", calls)
	}
}
