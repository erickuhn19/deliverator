package core

import (
	"math"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

func TestIsolatedLiqPrice(t *testing.T) {
	// long 10x, mmf 1%: 100*(1-0.1)/(1-0.01) = 90.909
	if got := isolatedLiqPrice(100, 1, 10, 0.01); math.Abs(got-90.9091) > 0.001 {
		t.Fatalf("long liq: want ~90.909, got %v", got)
	}
	// short 10x, mmf 1%: 100*(1+0.1)/(1+0.01) = 108.911
	if got := isolatedLiqPrice(100, -1, 10, 0.01); math.Abs(got-108.9109) > 0.001 {
		t.Fatalf("short liq: want ~108.911, got %v", got)
	}
}

func TestMaintenanceMarginFraction(t *testing.T) {
	m := testMeta()
	if got := m.MaintenanceMarginFraction("BTC", 1000); math.Abs(got-0.0125) > 1e-9 { // 1/(2*40)
		t.Fatalf("BTC mmf: want 0.0125, got %v", got)
	}
	if got := m.MaintenanceMarginFraction("ETH", 1000); math.Abs(got-0.02) > 1e-9 { // 1/(2*25)
		t.Fatalf("ETH mmf: want 0.02, got %v", got)
	}
}

func TestPositionDistanceToLiq(t *testing.T) {
	// BTC long: szi 0.1, positionValue 5000 (mark 50000), liq 45000 => 10% away.
	clearing := `{"assetPositions":[{"position":{"coin":"BTC","szi":"0.1","positionValue":"5000","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"500","liquidationPx":"45000","leverage":{"type":"isolated","value":10}},"type":"oneWay"}],"marginSummary":{"accountValue":"5000","totalMarginUsed":"500","totalNtlPos":"5000","totalRawUsd":"5000"},"crossMarginSummary":{"accountValue":"5000","totalMarginUsed":"500","totalNtlPos":"5000","totalRawUsd":"5000"},"withdrawable":"4500"}`
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, typ string, _ map[string]any) (int, string) {
		if typ == "clearinghouseState" {
			return 200, clearing
		}
		return 200, `[]`
	})
	pos, err := c.Positions(ctx, "BTC")
	if err != nil || len(pos) != 1 {
		t.Fatalf("positions: err=%v n=%d", err, len(pos))
	}
	if pos[0].DistanceToLiqPct != "10" {
		t.Fatalf("distance_to_liq_pct: want 10, got %q", pos[0].DistanceToLiqPct)
	}
}

func TestPreviewProjects(t *testing.T) {
	clearing := clearingWith("1000") // flat, equity 1000
	resp := func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, clearing
		case "frontendOpenOrders":
			return 200, `[]`
		case "spotClearinghouseState":
			return 200, noSpot
		case "allMids":
			return 200, `{"BTC":"50000"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	pv, err := c.Preview(ctx, "BTC", Buy, "0.01", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Leverage != 10 || pv.OrderNotional != "500" || pv.MarginRequired != "50" {
		t.Fatalf("projection wrong: lev=%d notional=%q margin=%q", pv.Leverage, pv.OrderNotional, pv.MarginRequired)
	}
	if pv.ResultingAccountLeverage != "0.5" { // 500 / 1000 equity
		t.Fatalf("resulting account leverage: want 0.5, got %q", pv.ResultingAccountLeverage)
	}
	if liq := parseFloatSafe(pv.EstLiquidationPx); math.Abs(liq-45569.6) > 1.0 { // 50000*0.9/0.9875
		t.Fatalf("est liq: want ~45569.6, got %v", liq)
	}
}

// Preview on a HIP-4 outcome reports at-stake (max loss) + max-gain — fully
// collateralized, so leverage/liquidation fields are omitted (N/A).
func TestPreviewOutcome(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, _ string, _ map[string]any) (int, string) {
		return 200, `{}`
	})
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 641, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})

	// Buy 200 @ 0.25: at-stake $50 (max loss), max-gain $150 (200 × 0.75).
	pv, err := c.Preview(ctx, "#6410", Buy, "200", "0.25", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.IsOutcome || pv.AtStakeUSD != "50" || pv.MaxGainUSD != "150" {
		t.Fatalf("outcome payoff wrong: at_stake=%q max_gain=%q (%+v)", pv.AtStakeUSD, pv.MaxGainUSD, pv)
	}
	if pv.OrderNotional != "50" || pv.MarginRequired != "50" {
		t.Errorf("outcome notional/margin should equal at-stake (size×price): %+v", pv)
	}
	// Leverage / liquidation are N/A and must be omitted, never a misleading 0/value.
	if pv.Leverage != 0 || pv.EstLiquidationPx != "" || pv.EstDistanceToLiqPct != "" || pv.ResultingAccountLeverage != "" {
		t.Errorf("outcome preview must omit leverage/liq fields: %+v", pv)
	}
}

// A SELL (exit/short the side) inverts the payoff vs a buy: at-stake and max-gain
// swap. Reporting buy semantics for a sell understated the risk (audit #91 / T3-preview).
func TestPreviewOutcomeSellInverts(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, _ string, _ map[string]any) (int, string) {
		return 200, `{}`
	})
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 641, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})

	// Sell 200 @ 0.25: at-stake $150 (200 × 0.75, resolves to 1 against the seller),
	// max-gain $50 (200 × 0.25, keep the premium if it resolves to 0).
	pv, err := c.Preview(ctx, "#6410", Sell, "200", "0.25", 0)
	if err != nil {
		t.Fatal(err)
	}
	if pv.AtStakeUSD != "150" || pv.MaxGainUSD != "50" {
		t.Fatalf("sell payoff not inverted: at_stake=%q max_gain=%q (want 150 / 50)", pv.AtStakeUSD, pv.MaxGainUSD)
	}
}
