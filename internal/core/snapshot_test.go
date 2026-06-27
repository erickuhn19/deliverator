package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

const macBTC = `[{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":40}],"marginTables":[],"collateralToken":0},[{"funding":"0.0001","openInterest":"100","prevDayPx":"63000","dayNtlVlm":"1000","premium":"0.001","oraclePx":"64100","markPx":"64050","midPx":"64055"}]]`

// macBTCETH: ctxs indexed by asset (BTC=0, ETH=1 in testMeta).
const macBTCETH = `[{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":40},{"name":"ETH","szDecimals":4,"maxLeverage":25}],"marginTables":[],"collateralToken":0},[{"funding":"0.0001","openInterest":"100","prevDayPx":"63000","dayNtlVlm":"1000","premium":"0.001","oraclePx":"64100","markPx":"64050","midPx":"64055"},{"funding":"0.0002","openInterest":"50","prevDayPx":"2000","dayNtlVlm":"500","premium":"0.001","oraclePx":"2010","markPx":"2005","midPx":"2006"}]]`

const rateLimitOK = `{"cumVlm":"1000","nRequestsUsed":50,"nRequestsCap":1000,"nRequestsSurplus":0}`

func TestSnapshotAllOK(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"clearinghouseState":     emptyState,
		"frontendOpenOrders":     `[]`,
		"spotClearinghouseState": `{"balances":[]}`,
		"userRateLimit":          rateLimitOK,
		"metaAndAssetCtxs":       macBTC,
	}))
	sv, _, err := c.Snapshot(ctx, []string{"BTC"})
	if err != nil {
		t.Fatal(err)
	}
	if !sv.Portfolio.OK || !sv.Limits.OK || !sv.BuilderStatus.OK {
		t.Fatalf("top sections not all ok: %+v", sv)
	}
	if sec, ok := sv.Ctx["BTC"]; !ok || !sec.OK {
		t.Fatalf("ctx[BTC] not ok: %+v", sv.Ctx)
	}
	if len(sv.Failed) != 0 {
		t.Fatalf("expected no failures, got %v", sv.Failed)
	}
}

func TestSnapshotAutoDiscoversCoins(t *testing.T) {
	clearing := clearingWith("100000", posWith("BTC", "0.1", "5000")) // BTC position
	ethOrder := `[{"coin":"ETH","oid":7,"limitPx":"2000","origSz":"1","sz":"1","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"clearinghouseState":     clearing,
		"frontendOpenOrders":     ethOrder,
		"spotClearinghouseState": `{"balances":[]}`,
		"userRateLimit":          rateLimitOK,
		"metaAndAssetCtxs":       macBTCETH,
	}))
	sv, _, err := c.Snapshot(ctx, nil) // no --coins => discover from positions+orders
	if err != nil {
		t.Fatal(err)
	}
	if len(sv.Coins) != 2 || sv.Coins[0] != "BTC" || sv.Coins[1] != "ETH" {
		t.Fatalf("auto-discover coins: want [BTC ETH], got %v", sv.Coins)
	}
	if !sv.Ctx["BTC"].OK || !sv.Ctx["ETH"].OK {
		t.Fatalf("both discovered ctx coins should be ok: %+v", sv.Ctx)
	}
}

func TestSnapshotPartialFailureIsolated(t *testing.T) {
	// limits fails (500) but every other section still returns; flat account so no ctx.
	resp := func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, emptyState
		case "frontendOpenOrders":
			return 200, `[]`
		case "spotClearinghouseState":
			return 200, `{"balances":[]}`
		case "userRateLimit":
			return 500, `upstream down`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	sv, warns, err := c.Snapshot(ctx, nil)
	if err != nil {
		t.Fatal(err) // a section failure must NOT be a top-level error
	}
	if sv.Limits.OK || sv.Limits.Error == nil {
		t.Fatalf("limits should carry an error: %+v", sv.Limits)
	}
	if !sv.Portfolio.OK {
		t.Fatalf("portfolio should still be ok: %+v", sv.Portfolio)
	}
	if len(sv.Failed) != 1 || sv.Failed[0] != "limits" {
		t.Fatalf("failed should be [limits], got %v", sv.Failed)
	}
	joined := ""
	for _, w := range warns {
		joined += w
	}
	if joined == "" {
		t.Fatal("expected a top-level warning listing the failed section")
	}
}

func TestSnapshotFlatAccountNoCtx(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"clearinghouseState":     emptyState,
		"frontendOpenOrders":     `[]`,
		"spotClearinghouseState": `{"balances":[]}`,
		"userRateLimit":          rateLimitOK,
	}))
	sv, _, err := c.Snapshot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sv.Coins) != 0 || len(sv.Ctx) != 0 {
		t.Fatalf("flat account should yield no ctx coins, got coins=%v ctx=%v", sv.Coins, sv.Ctx)
	}
	if sv.Ctx == nil {
		t.Fatal("ctx must be an empty map, not nil")
	}
}

func TestSnapshotNoQueryAddr(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{}))
	c.queryAddr = "" // no master/query address configured
	if _, _, err := c.Snapshot(ctx, []string{"BTC"}); err == nil {
		t.Fatal("snapshot with no query address must return a top-level error")
	}
}
