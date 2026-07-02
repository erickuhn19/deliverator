package core

// NEXT-2 items 1 + 8 on the snapshot surface:
//  - a degraded portfolio (unreadable sub-dex) must surface as snapshot
//    warnings, not silently shrink the one-call tick read;
//  - the per-coin ctx fan-out must fetch each dex's metaAndAssetCtxs ONCE and
//    slice out every requested coin locally — not once per coin, concurrently,
//    self-inflicting the per-IP 429s the snapshot exists to avoid.

import (
	"strings"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

const macThree = `[{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":40},{"name":"ETH","szDecimals":4,"maxLeverage":25},{"name":"SOL","szDecimals":2,"maxLeverage":20}],"marginTables":[],"collateralToken":0},[` +
	`{"funding":"0.0001","openInterest":"100","prevDayPx":"63000","dayNtlVlm":"1000","premium":"0.001","oraclePx":"64100","markPx":"64050","midPx":"64055"},` +
	`{"funding":"0.0002","openInterest":"50","prevDayPx":"2000","dayNtlVlm":"500","premium":"0.001","oraclePx":"2010","markPx":"2005","midPx":"2006"},` +
	`{"funding":"0.0003","openInterest":"25","prevDayPx":"150","dayNtlVlm":"250","premium":"0.001","oraclePx":"151","markPx":"150.5","midPx":"150.6"}]]`

// Snapshot with 3 same-dex perp coins must hit metaAndAssetCtxs exactly ONCE.
func TestSnapshotCtxFetchesMetaAndAssetCtxsOnce(t *testing.T) {
	macCalls := 0
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, "{}"
		}
		switch typ {
		case "clearinghouseState":
			return 200, emptyState
		case "frontendOpenOrders":
			return 200, `[]`
		case "spotClearinghouseState":
			return 200, `{"balances":[]}`
		case "userRateLimit":
			return 200, rateLimitOK
		case "metaAndAssetCtxs":
			macCalls++
			return 200, macThree
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	// Extend the universe to three perps for this test.
	meta := &hl.Meta{Universe: []hl.AssetInfo{
		{Name: "BTC", SzDecimals: 5, MaxLeverage: 40},
		{Name: "ETH", SzDecimals: 4, MaxLeverage: 25},
		{Name: "SOL", SzDecimals: 2, MaxLeverage: 20},
	}}
	c.meta = NewMetaStore("testnet", meta, testMeta().SpotMeta(), time.Now())

	sv, _, err := c.Snapshot(ctx, []string{"BTC", "ETH", "SOL"})
	if err != nil {
		t.Fatal(err)
	}
	for _, coin := range []string{"BTC", "ETH", "SOL"} {
		if sec, ok := sv.Ctx[coin]; !ok || !sec.OK {
			t.Fatalf("ctx[%s] must be ok from the shared fetch: %+v", coin, sv.Ctx)
		}
	}
	if macCalls != 1 {
		t.Fatalf("3 same-dex coins must cost exactly ONE metaAndAssetCtxs fetch, got %d", macCalls)
	}
}

// A degraded portfolio (unreadable sub-dex clearinghouse) must surface as a
// snapshot warning while the section itself stays ok (tolerant read) — AND the
// snapshot's own ReadMeta must hoist degraded/degraded_dexs, so the envelope
// carries the top-level degraded_dexs field TOOLS.md promises for the tick read.
func TestSnapshotWarnsOnDegradedPortfolio(t *testing.T) {
	c, ctx := newTestClient(t, subDexCfg(), Options{}, degradedSubDexResp(true, false, false))
	sv, rm, err := c.Snapshot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sv.Portfolio.OK {
		t.Fatalf("a degraded (partial) portfolio must still be an ok section: %+v", sv.Portfolio)
	}
	joined := strings.Join(rm.EnvelopeWarnings(), " | ")
	if !strings.Contains(joined, "sub-dex state (xyz)") {
		t.Fatalf("snapshot must warn about the unreadable sub-dex, got %q", joined)
	}
	if len(rm.DegradedDexs) != 1 || rm.DegradedDexs[0] != "xyz" {
		t.Fatalf("snapshot ReadMeta must hoist the portfolio's degraded_dexs (top-level envelope field), got %v", rm.DegradedDexs)
	}
}
