package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// matchesCoinFilter must tolerate the HIP-3 "<dex>:" prefix on either side so a
// sub-dex position/order is never silently dropped by a --coin filter, whatever
// form the clearinghouse and the user each use.
func TestMatchesCoinFilter(t *testing.T) {
	match := [][2]string{
		{"xyz:GOLD", "xyz:GOLD"}, // both prefixed
		{"GOLD", "GOLD"},         // both bare
		{"GOLD", "xyz:GOLD"},     // clearinghouse bare, user prefixed (the HIGH case)
		{"xyz:GOLD", "GOLD"},     // clearinghouse prefixed, user bare
		{"btc", "BTC"},           // case-insensitive main coin
	}
	for _, m := range match {
		if !matchesCoinFilter(m[0], m[1]) {
			t.Errorf("matchesCoinFilter(%q,%q) = false, want true", m[0], m[1])
		}
	}
	noMatch := [][2]string{
		{"xyz:GOLD", "xyz:SILVER"},
		{"BTC", "ETH"},
		{"PURR/USDC", "USDC"}, // a spot pair is not stripped at '/'
	}
	for _, m := range noMatch {
		if matchesCoinFilter(m[0], m[1]) {
			t.Errorf("matchesCoinFilter(%q,%q) = true, want false", m[0], m[1])
		}
	}
}

// goldShort is a sub-dex clearinghouse state reporting the coin in the BARE form
// ("GOLD") — the case coinMatches was built for and the read path used to drop.
const goldShortBare = `{"assetPositions":[{"position":{"coin":"GOLD","szi":"-0.01","positionValue":"41","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"8","leverage":{"type":"isolated","value":5}},"type":"oneWay"}],"marginSummary":{"accountValue":"50","totalMarginUsed":"8","totalNtlPos":"41","totalRawUsd":"50"},"crossMarginSummary":{"accountValue":"50","totalMarginUsed":"8","totalNtlPos":"41","totalRawUsd":"50"},"withdrawable":"42"}`

// subDexState routes the per-dex clearinghouse to a bare-coin GOLD position and
// leaves the main dex flat.
func subDexResp(t *testing.T) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, "{}"
		}
		switch typ {
		case "clearinghouseState":
			if body["dex"] == "xyz" {
				return 200, goldShortBare
			}
			return 200, emptyState
		case "frontendOpenOrders":
			return 200, "[]"
		case "spotClearinghouseState":
			return 200, `{"balances":[]}`
		}
		return 200, "{}"
	}
}

// The HIGH bug: a sub-dex position whose clearinghouse reports the BARE coin must
// (1) be normalized to the canonical "xyz:GOLD" in the output, and (2) be found
// by `positions --coin xyz:GOLD` — not silently dropped.
func TestSubDexPositionNormalizedAndFilterable(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	c, ctx := newTestClient(t, cfg, Options{}, subDexResp(t))

	all, err := c.Positions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var found *PositionView
	for i := range all {
		if all[i].Coin == "xyz:GOLD" {
			found = &all[i]
		}
		if all[i].Coin == "GOLD" {
			t.Fatalf("sub-dex coin must be normalized to xyz:GOLD, got bare %q", all[i].Coin)
		}
	}
	if found == nil {
		t.Fatalf("sub-dex GOLD position must appear (normalized), got %+v", all)
	}

	// The canonical prefixed filter must find it (this was returning 0 before).
	byPrefixed, err := c.Positions(ctx, "xyz:GOLD")
	if err != nil {
		t.Fatal(err)
	}
	if len(byPrefixed) != 1 || byPrefixed[0].Coin != "xyz:GOLD" {
		t.Fatalf("positions --coin xyz:GOLD must find the sub-dex position, got %+v", byPrefixed)
	}
	// The bare filter must also find it (symmetric tolerance).
	byBare, err := c.Positions(ctx, "GOLD")
	if err != nil {
		t.Fatal(err)
	}
	if len(byBare) != 1 {
		t.Fatalf("positions --coin GOLD must also find it, got %+v", byBare)
	}
}

// On a unified account, balance must flag collateral_shared and annotate each
// sub-dex block — so account_value 0.0 there isn't misread as "can't trade".
func TestBalanceFlagsSharedCollateral(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[{"coin":"USDC","token":0,"total":"500","hold":"0","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"480"]]}`
			case "frontendOpenOrders":
				return 200, "[]"
			}
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	bv, err := c.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bv.AvailableCollateral != "480" || !bv.CollateralShared {
		t.Fatalf("unified account must set available_collateral + collateral_shared: %+v", bv)
	}
	if bv.PerpDexs["xyz"].Note == "" {
		t.Errorf("each sub-dex block must carry the shared-collateral note: %+v", bv.PerpDexs["xyz"])
	}
}

// Without spot collateral, the shared flag/note are absent (omitempty) — we don't
// claim a unified pool we can't see.
func TestBalanceNoCollateralNoSharedFlag(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[]}` // no tokenToAvailableAfterMaintenance
			case "frontendOpenOrders":
				return 200, "[]"
			}
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	bv, err := c.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bv.CollateralShared {
		t.Errorf("no collateral -> collateral_shared must be false: %+v", bv)
	}
	if bv.PerpDexs["xyz"].Note != "" {
		t.Errorf("no collateral -> no shared-collateral note: %+v", bv.PerpDexs["xyz"])
	}
}

// portfolio carries the same unified-account flag + sub-dex note.
func TestPortfolioFlagsSharedCollateral(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[],"tokenToAvailableAfterMaintenance":[[0,"480"]]}`
			case "frontendOpenOrders":
				return 200, "[]"
			}
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pf.CollateralShared || pf.PerpDexs["xyz"].Note == "" {
		t.Fatalf("portfolio must flag shared collateral + annotate the sub-dex: %+v", pf)
	}
}

// portfolio must surface each configured sub-dex's margin summary, so the
// snapshot is internally consistent: a sub-dex position is listed AND its
// margin/notional is reported (the headline totals are main-dex only).
func TestPortfolioSurfacesSubDexMargin(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	c, ctx := newTestClient(t, cfg, Options{}, subDexResp(t))

	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pb, ok := pf.PerpDexs["xyz"]
	if !ok {
		t.Fatalf("portfolio must surface the xyz sub-dex margin, got %+v", pf.PerpDexs)
	}
	if pb.TotalMarginUsed != "8" || pb.TotalNotionalPos != "41" {
		t.Fatalf("sub-dex margin summary wrong: %+v", pb)
	}
	// And the position itself is listed (normalized).
	if len(pf.Positions) != 1 || pf.Positions[0].Coin != "xyz:GOLD" {
		t.Fatalf("portfolio must list the normalized sub-dex position, got %+v", pf.Positions)
	}
}
