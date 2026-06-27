package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// A MARKETABLE limit order fills at the mid, not its aggressive limit price — so
// the dollar guards must price it at the mid. Empirically: HL fills a $9-at-limit
// marketable sell at the market (~$15). (ETH mid is 3000 in the harness.)
func TestMarketableSellNotFloorRejected(t *testing.T) {
	// Sell limit 1000 (crosses mid 3000), size 0.005: limit-notional $5 (< $10 floor)
	// but mid-notional $15 (>= floor) -> must PLACE, not be floor-rejected.
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"filled":{"totalSz":"0.005","avgPx":"3000","oid":1}}`)))
	res, _, err := c.Place(ctx, OrderReq{Coin: "ETH", Side: Sell, Size: "0.005", Limit: "1000", Tif: "Ioc"})
	if err != nil {
		t.Fatalf("a marketable sell that fills above the floor must not be rejected: %v", err)
	}
	if res.Status != "filled" {
		t.Fatalf("expected fill, got %+v", res)
	}
}

// A RESTING limit (non-crossing) keeps its limit price for the floor: a sell above
// the mid that would rest at a sub-$10 value is correctly rejected.
func TestRestingSellBelowFloorStillRejected(t *testing.T) {
	// Sell limit 5000 (above mid 3000, rests), size 0.001 = $5 at the limit < floor.
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`)))
	_, _, err := c.Place(ctx, OrderReq{Coin: "ETH", Side: Sell, Size: "0.001", Limit: "5000", Tif: "Gtc"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// A marketable sell must not BYPASS the max-order cap: it fills at the mid (above
// its limit), so the cap is enforced at the mid, not the lower limit-notional.
func TestMarketableSellCapNotBypassed(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 5000
	// Sell limit 1000 (marketable), size 4: limit-notional $4000 (< cap) but
	// mid-notional $12000 (> cap) -> rejected at the mid.
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", okOrder(`{"filled":{"totalSz":"4","avgPx":"3000","oid":1}}`)))
	_, _, err := c.Place(ctx, OrderReq{Coin: "ETH", Side: Sell, Size: "4", Limit: "1000", Tif: "Ioc"})
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

// A marketable BUY (limit above mid) is priced at the mid, not its inflated limit,
// so a large buy isn't false-rejected by the order cap on the limit notional.
func TestMarketableBuyCapPricedAtMid(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 5000
	// Buy limit 9000 (above mid 3000, marketable), size 1: limit-notional $9000
	// (> cap) but mid-notional $3000 (< cap) -> must PLACE (priced at the mid).
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", okOrder(`{"filled":{"totalSz":"1","avgPx":"3000","oid":1}}`)))
	res, _, err := c.Place(ctx, OrderReq{Coin: "ETH", Side: Buy, Size: "1", Limit: "9000", Tif: "Ioc"})
	if err != nil {
		t.Fatalf("a marketable buy priced at the mid must pass the cap: %v", err)
	}
	if res.Status != "filled" {
		t.Fatalf("expected fill, got %+v", res)
	}
}
