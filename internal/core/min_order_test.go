package core

import (
	"errors"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// ---- gauntlet level ----

// The min-order floor rejects sub-minimum NEW exposure pre-flight (validation,
// exit 10), exempts reduce-only (dust closes), and — critically — never
// false-rejects when notional could not be priced (NotionalUSD 0): that is the
// "reject everything" trap the >0 guard defends against.
func TestPreTradeChecksMinOrderNotional(t *testing.T) {
	cfg := config.Default() // floor defaults to 10
	c := newCfgClient(t, cfg)
	floor := cfg.Risk.MinOrderNotionalUSD

	assertErr(t, c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 5, MinNotionalUSD: floor}),
		output.CatValidation, output.ExitValidation)

	if err := c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 50, MinNotionalUSD: floor}); err != nil {
		t.Errorf("at/above floor should pass, got %v", err)
	}
	if err := c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 5, MinNotionalUSD: floor, ReduceOnly: true}); err != nil {
		t.Errorf("reduce-only must bypass the floor (dust close), got %v", err)
	}
	if err := c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 0, MinNotionalUSD: floor}); err != nil {
		t.Errorf("notional 0 (un-priceable) must never min-reject, got %v", err)
	}
	if err := c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 1, MinNotionalUSD: 0}); err != nil {
		t.Errorf("floor 0 disables the check, got %v", err)
	}
}

// ---- engine level ----

// 0.0001 BTC @ 64000 = $6.40 < $10 floor => validation reject before signing.
func TestPlaceLimitBelowMinRejects(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`)))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.0001", Limit: "64000", Tif: "Gtc"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// 0.001 BTC @ 64000 = $64 clears the floor and rests.
func TestPlaceLimitAboveMinPasses(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":2}}`)))
	res, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.001", Limit: "64000", Tif: "Gtc"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resting" {
		t.Fatalf("want resting, got %+v", res)
	}
}

// A dust close (reduce-only, < $10) must place — the floor must never strand dust.
func TestPlaceReduceOnlyDustPasses(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":3}}`)))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Sell, Size: "0.0001", Limit: "64000", Tif: "Gtc", ReduceOnly: true})
	if err != nil {
		t.Fatalf("reduce-only dust must place, got %v", err)
	}
}

// Market order, floor active, no mid => fail closed (the floor can't be priced).
func TestPlaceMarketNoMidFloorFailsClosed(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", `{}`, "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01"})
	assertErr(t, err, output.CatRisk, output.ExitRisk) // no_ref_px
}

// Regression for the reject-everything fear: with ALL dollar guards off, the
// fail-closed no_ref_px refusal must NOT fire. A market order with no mid gets
// PAST the risk gauntlet and only then fails at the signing layer (the unrelated
// missing-mid reason) — proving the broadened fail-closed is conditional on a
// guard being active, not a blanket "reject everything when notional is 0".
func TestPlaceMarketNoGuardsSkipsFailClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MinOrderNotionalUSD = 0
	cfg.Risk.MaxOrderNotionalUSD = 0
	cfg.Risk.MaxPositionNotionalUSD = 0
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", `{}`, "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01"})
	if err == nil {
		t.Fatal("expected the signing-layer missing-mid error, got nil")
	}
	var oe *output.Error
	if errors.As(err, &oe) && oe.Category == output.CatRisk {
		t.Fatalf("guards off must NOT risk-reject (no_ref_px); the order should reach signing, got %v", err)
	}
}

// The bracket ENTRY leg is subject to the floor; a tiny entry rejects pre-flight.
func TestPlaceBracketTinyEntryRejects(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, bracketResp(okOrder(`{"resting":{"oid":1}}`), nil))
	_, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.0001", Limit: "64000", TP: "70000", SL: "60000"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// A reduce-only TWAP that reduces a dust position must bypass the floor.
func TestTwapReduceOnlyBypassesMin(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", twapRunning))
	_, _, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Sell, Size: "0.0001", Minutes: 5, ReduceOnly: true})
	if err != nil {
		t.Fatalf("reduce-only TWAP must bypass the floor, got %v", err)
	}
}

// A new-exposure TWAP below the floor is rejected pre-flight.
func TestTwapTinyNewExposureRejects(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", twapRunning))
	_, _, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Buy, Size: "0.0001", Minutes: 5})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// A reduce-only TWAP only shrinks a position, so — like reduce-only Place/Modify
// — it also bypasses the MAX notional caps (unwinding a large position must work).
func TestTwapReduceOnlyBypassesMaxCap(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 100 // 0.01 BTC @ 64000 = $640 would trip this if applied
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", twapRunning))
	_, _, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Sell, Size: "0.01", Minutes: 5, ReduceOnly: true})
	if err != nil {
		t.Fatalf("reduce-only TWAP must bypass the max cap (large unwind), got %v", err)
	}
}

// A reduce-only TWAP needs no reference price to unwind; with notional guards
// active and NO mid it must still pass (skip the notional block), not fail closed.
func TestTwapReduceOnlyNoMidPasses(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", `{}`, "", twapRunning))
	_, _, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Sell, Size: "0.01", Minutes: 5, ReduceOnly: true})
	if err != nil {
		t.Fatalf("reduce-only TWAP with no mid must pass (exempt from notional guards), got %v", err)
	}
}
