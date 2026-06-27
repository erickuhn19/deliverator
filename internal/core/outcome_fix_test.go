package core

// Coverage for the two outcome-gauntlet fixes:
//   #105 — outcomeGuardPx values a marketable outcome order at the book TOUCH
//          (ask buy / bid sell), not the wide-spread mid.
//   #104 — positions surfaces held outcome tokens by lazily loading the universe
//          when a "+<enc>" balance is present (was invisible when not pre-loaded).

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

func TestOutcomeGuardPx(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, typ string, _ map[string]any) (int, string) {
		if typ == "l2Book" {
			// bid 0.33 / ask 0.41 — a WIDE book (mid 0.37), so touch != mid.
			return 200, `{"coin":"#6520","time":1,"levels":[[{"px":"0.33","sz":"100","n":1}],[{"px":"0.41","sz":"100","n":1}]]}`
		}
		return 200, `{}`
	})
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 652, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})

	cases := []struct {
		name     string
		side     Side
		limitF   float64
		isMarket bool
		want     float64
	}{
		{"marketable buy -> ask", Buy, 0.42, false, 0.41},
		{"marketable sell -> bid", Sell, 0.30, false, 0.33},
		{"resting buy -> own limit", Buy, 0.20, false, 0.20},
		{"resting sell -> own limit", Sell, 0.80, false, 0.80},
		{"market buy -> ask", Buy, 0, true, 0.41},
		{"market sell -> bid", Sell, 0, true, 0.33},
	}
	for _, tc := range cases {
		px, ok := c.outcomeGuardPx(ctx, "#6520", tc.side, tc.limitF, tc.isMarket)
		if !ok || px != tc.want {
			t.Errorf("%s: got (%v, %v), want %v", tc.name, px, ok, tc.want)
		}
	}
}

// A marketable outcome buy whose mid-valued notional is below the $10 floor but
// whose TOUCH-valued notional clears it must NOT be falsely rejected (#105).
func TestOutcomeMarketableBuyNotionalUsesTouch(t *testing.T) {
	// 28 shares: mid 0.33 -> $9.24 (would false-reject); ask 0.41 -> $11.48 (clears).
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "l2Book":
			return 200, `{"coin":"#6520","time":1,"levels":[[{"px":"0.33","sz":"500","n":1}],[{"px":"0.41","sz":"500","n":1}]]}`
		case "allMids":
			return 200, `{"#6520":"0.33"}`
		}
		return 200, `{}`
	})
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 652, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})
	// Dry-run so nothing signs; the min-notional gate runs before the dry-run exit.
	_, _, err := c.Place(ctx, OrderReq{Coin: "#6520", Side: Buy, Size: "28", Limit: "0.42", Tif: "Ioc"})
	if err != nil {
		t.Fatalf("marketable outcome buy valued at the touch ($11.48) must clear the $10 floor, got: %v", err)
	}
}

// positions must surface a held outcome token even when the outcome universe was
// not pre-enabled — it lazily loads on detecting a "+<enc>" balance (#104).
func TestPositionsSurfacesOutcomeHoldingLazily(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, emptyState
		case "spotClearinghouseState":
			return 200, `{"balances":[{"coin":"+6520","token":5,"hold":"0","total":"30","entryNtl":"0"}]}`
		case "outcomeMeta":
			return 200, `{"outcomes":[{"outcome":652,"sideSpecs":[{"name":"Yes"},{"name":"No"}],"quoteToken":"USDC"}]}`
		case "allMids":
			return 200, `{"#6520":"0.4"}`
		}
		return 200, `{}`
	})
	// Deliberately do NOT AddOutcomes — rely on the lazy load triggered by the
	// held "+6520" balance.
	if c.Meta().OutcomeMeta() != nil {
		t.Fatal("precondition: outcomes must start unloaded")
	}
	pos, err := c.Positions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pos {
		if p.Coin == "#6520" && p.Class == "outcome" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a held outcome token must surface as a class:outcome position, got %+v", pos)
	}
}
