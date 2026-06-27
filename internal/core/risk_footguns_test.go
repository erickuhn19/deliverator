package core

// Tests for the Tier-1 risk-layer footguns (audit S2/S3/S4): a poisoned mid must
// not bypass the caps, the per-coin cap must count an existing outcome holding, and
// market caps must be priced at the worst-case fill with a slippage clamp.

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// --- S2: non-finite / non-positive mid must fail closed, not slip the caps ---

func TestParseFloatSafeRejectsNonFinite(t *testing.T) {
	for _, s := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity"} {
		if got := parseFloatSafe(s); got != 0 {
			t.Errorf("parseFloatSafe(%q) = %v, want 0", s, got)
		}
	}
	if got := parseFloatSafe("12.5"); got != 12.5 {
		t.Errorf("parseFloatSafe(12.5) = %v", got)
	}
}

func TestPlaceMarketRejectsPoisonedMid(t *testing.T) {
	// strconv.ParseFloat accepts "NaN"/"Inf"; a "0" mid is garbage. Each must make a
	// market order fail closed (no_ref_px) rather than compute a NaN/0 notional that
	// passes every cap (NaN>cap and 0>cap are false) and gets signed.
	for _, mid := range []string{`{"BTC":"NaN"}`, `{"BTC":"Inf"}`, `{"BTC":"0"}`} {
		c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", mid, "", `{}`))
		_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01"})
		if err == nil {
			t.Fatalf("mid %s: market order must fail closed, not sign", mid)
		}
		assertErr(t, err, output.CatRisk, output.ExitRisk)
	}
}

// --- S4: market caps priced at the worst-case fill + slippage clamp ---

func TestResolveSlippage(t *testing.T) {
	if s, err := resolveSlippage(0); err != nil || s != hl.DefaultSlippage {
		t.Errorf("unset should default: got %v err %v", s, err)
	}
	if s, err := resolveSlippage(0.03); err != nil || s != 0.03 {
		t.Errorf("in-range should pass through: got %v err %v", s, err)
	}
	if _, err := resolveSlippage(maxSlippage + 0.01); err == nil {
		t.Error("over-max slippage must error")
	}
}

func TestMarketGuardPx(t *testing.T) {
	if p := marketGuardPx(100, true, false, 0.05); p != 105 {
		t.Errorf("perp buy worst-case = %v, want 105", p)
	}
	if p := marketGuardPx(100, false, false, 0.05); p != 100 {
		t.Errorf("perp sell worst-case = %v, want 100 (a sell fills <= mid)", p)
	}
	if p := marketGuardPx(0.5, true, true, 0.2); p != 0.7 {
		t.Errorf("outcome buy worst-case = %v, want 0.7", p)
	}
	if p := marketGuardPx(0.95, true, true, 0.2); p != 1 {
		t.Errorf("outcome buy must clamp to 1, got %v", p)
	}
}

func TestPlaceMarketGuardsAtWorstCaseFill(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 100
	// BTC mid 64000, default 5% slippage. size 0.0015: notional at the MID = $96
	// (would have passed); at the worst-case fill 64000*1.05*0.0015 = $100.8 > $100,
	// so it must reject — the guard is priced at the fill, not the mid (audit S4).
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", `{"BTC":"64000"}`, "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.0015"})
	if err == nil {
		t.Fatal("market buy must be capped at the worst-case fill, not the mid")
	}
	assertErr(t, err, output.CatRisk, output.ExitRisk)

	// A size whose worst-case fill stays under the cap ($94.08 < $100) passes.
	c2, ctx2 := newTestClient(t, cfg, Options{}, engineResp("", `{"BTC":"64000"}`, "", okOrder(`{"filled":{"totalSz":"0.0014","avgPx":"64010","oid":7}}`)))
	if _, _, err := c2.Place(ctx2, OrderReq{Coin: "BTC", Side: Buy, Size: "0.0014"}); err != nil {
		t.Fatalf("worst-case fill under the cap should pass: %v", err)
	}
}

func TestPlaceRejectsExcessiveSlippage(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Slippage: 0.5})
	if err == nil {
		t.Fatal("--slippage 0.5 (> max) must be rejected")
	}
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// --- S3: per-coin position cap counts an existing HIP-4 outcome holding ---

func TestOutcomePositionCapCountsHolding(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxPositionNotionalUSD = 80
	resp := func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "spotClearinghouseState":
			return 200, `{"balances":[{"coin":"+6410","total":"200","hold":"0","entryNtl":"40"}]}`
		case "allMids":
			return 200, `{"#6410":"0.25"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, cfg, Options{DryRun: true}, resp)
	c.Meta().AddOutcomes(outcome641)

	// Existing holding = 200 shares × mid 0.25 = $50. A new $40 at-stake buy (160 ×
	// 0.25) makes the resulting position $90 > $80 — must reject because the spot
	// "+6410" holding is now counted (perp clearinghouse alone reports 0 for it, S3).
	if _, _, err := c.Place(ctx, OrderReq{Coin: "#6410", Side: Buy, Size: "160", Limit: "0.25"}); err == nil {
		t.Fatal("per-coin cap must include the existing outcome holding ($50 + $40 > $80)")
	}
	// A $25 buy → resulting $75 < $80 → passes.
	if _, _, err := c.Place(ctx, OrderReq{Coin: "#6410", Side: Buy, Size: "100", Limit: "0.25"}); err != nil {
		t.Fatalf("resulting $75 under the $80 cap should pass: %v", err)
	}
}
