package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// ---- #1: marketable-limit guard pricing must hold on EVERY limit-carrying path,
// not just single Place. A crossing limit fills at the market, so the dollar guards
// price at the mid — else the cap is bypassable / the floor false-rejects by
// routing the order through batch / grid / bracket / modify. (ETH mid 3000.)

// Batch: a marketable sell must NOT bypass the max-order cap (priced at the mid).
func TestBatchMarketableLimitCapNotBypassed(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 5000
	// sell limit 1000 (marketable), size 4: limit-notional $4000 (< cap) but
	// mid-notional $12000 (> cap) -> reject at the mid, like single Place.
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", okOrders(`{"filled":{"totalSz":"4","avgPx":"3000","oid":1}}`)))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{{Coin: "ETH", Side: Sell, Size: "4", Limit: "1000", Tif: "Ioc"}})
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

// Batch: a small marketable sell that fills above the floor must PLACE, not be
// false-rejected by the $10 min on its (lower) limit-notional.
func TestBatchMarketableLimitFloorNotRejected(t *testing.T) {
	// sell limit 1000 (marketable), size 0.005: limit-notional $5 (< floor) but
	// mid-notional $15 (>= floor) -> must place.
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrders(`{"filled":{"totalSz":"0.005","avgPx":"3000","oid":1}}`)))
	res, _, err := c.PlaceBatch(ctx, []OrderReq{{Coin: "ETH", Side: Sell, Size: "0.005", Limit: "1000", Tif: "Ioc"}})
	if err != nil {
		t.Fatalf("a marketable batch sell above the floor must place: %v", err)
	}
	if len(res) != 1 || res[0].Status != "filled" {
		t.Fatalf("expected one filled leg, got %+v", res)
	}
}

// Batch: a RESTING (non-crossing) sub-$10 sell stays rejected — the fix must not
// price-shift non-marketable legs (parity with TestRestingSellBelowFloorStillRejected).
func TestBatchRestingBelowFloorStillRejected(t *testing.T) {
	// sell limit 5000 (above mid 3000, rests), size 0.001 = $5 at the limit < floor.
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`)))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{{Coin: "ETH", Side: Sell, Size: "0.001", Limit: "5000", Tif: "Gtc"}})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// Bracket: a marketable limit ENTRY is priced at the mid for the cap, so a large
// crossing entry isn't false-rejected on its inflated limit notional.
func TestBracketMarketableLimitEntryPricedAtMid(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 5000
	// buy limit 9000 (marketable, mid 3000), size 1: limit-notional $9000 (> cap)
	// but mid-notional $3000 (< cap) -> entry must place (priced at the mid).
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":[` +
		`{"filled":{"totalSz":"1","avgPx":"3000","oid":1}},"waitingForTrigger","waitingForTrigger"]}}}`
	var grouping string
	c, ctx := newTestClient(t, cfg, Options{}, bracketResp(exResp, &grouping))
	_, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "ETH", Side: Buy, Size: "1", Limit: "9000", Tif: "Ioc", TP: "10000", SL: "1000"})
	if err != nil {
		t.Fatalf("a marketable bracket entry priced at the mid must pass the cap: %v", err)
	}
}

// Modify: re-pricing a resting order to a marketable (crossing) limit prices the
// cap at the mid — the OLD code rejected on the inflated limit notional.
func TestModifyMarketableLimitPricedAtMid(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 4000
	// existing BTC buy; modify to limit 100000 (marketable, mid 64000), size 0.05:
	// limit-notional $5000 (> cap) but mid-notional $3200 (< cap) -> must place.
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Gtc","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", front, okOrder(`{"resting":{"oid":99}}`)))
	oid := int64(42)
	if _, _, err := c.Modify(ctx, &oid, "", "0.05", "100000"); err != nil {
		t.Fatalf("a marketable modify priced at the mid must pass the cap: %v", err)
	}
}

// ---- #2: a spot-BUY batch/grid leg must NOT claim a builder fee HL won't collect
// (parity with single-order Place), while perp and spot-sell legs do.

func TestBatchSpotBuyNoBuilderPerpEarns(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`))))
	res, w, err := c.PlaceBatch(ctx, []OrderReq{
		{Coin: "PURR/USDC", Side: Buy, Size: "150", Limit: "0.1", Tif: "Gtc"}, // spot buy: NO fee
		{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "60000", Tif: "Gtc"},    // perp: earns fee
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Builder != nil {
		t.Errorf("spot-buy batch leg must not claim a builder fee, got %+v", res[0].Builder)
	}
	if res[1].Builder == nil {
		t.Errorf("perp batch leg must earn the builder fee")
	}
	if !warningsContain(w, "NOT earned") {
		t.Errorf("expected a NOT-earned warning for the spot-buy leg: %v", w)
	}
}

func TestBatchSpotSellEarnsBuilder(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`))))
	res, _, err := c.PlaceBatch(ctx, []OrderReq{{Coin: "PURR/USDC", Side: Sell, Size: "150", Limit: "0.2", Tif: "Gtc"}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Builder == nil {
		t.Errorf("spot-sell batch leg SHOULD earn the builder fee")
	}
}

// An all-spot-buy batch earns no fee anywhere — it must not emit the action-level
// "fee applied" note (which would be a false revenue claim).
func TestBatchAllSpotBuyNoAppliedWarning(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`))))
	_, w, err := c.PlaceBatch(ctx, []OrderReq{{Coin: "PURR/USDC", Side: Buy, Size: "150", Limit: "0.1", Tif: "Gtc"}})
	if err != nil {
		t.Fatal(err)
	}
	if warningsContain(w, "% applied") {
		t.Errorf("an all-spot-buy batch earns no fee; must not warn 'applied': %v", w)
	}
}

// ---- #4: a crossing modify that partial-fills must be flagged IsPartial so the
// modify command can surface exit 60 (the data the cmd handler now keys on).

func TestModifyPartialFillFlagged(t *testing.T) {
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.02","sz":"0.02","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Gtc","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	// A crossing modify fills only part of the requested size (0.01 of 0.02).
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, okOrder(`{"filled":{"totalSz":"0.01","avgPx":"64000","oid":99}}`)))
	oid := int64(42)
	res, _, err := c.Modify(ctx, &oid, "", "0.02", "64000")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsPartial() {
		t.Fatalf("a partially-filled crossing modify must be IsPartial(): %+v", res)
	}
}
