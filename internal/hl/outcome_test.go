package hl

import (
	"context"
	"testing"
)

// HIP-4 outcome asset-id encoding: asset = 100_000_000 + (10*outcome + side),
// side ∈ {0=Yes, 1=No}. Verified against the live asset-ids doc and mainnet.
func TestOutcomeAssetEncoding(t *testing.T) {
	cases := []struct {
		outcome, side int
		wantEncoding  int
		wantAsset     int
		wantCoin      string
	}{
		{171, 0, 1710, 100_001_710, "#1710"}, // Fallback, Yes
		{171, 1, 1711, 100_001_711, "#1711"}, // Fallback, No
		{641, 0, 6410, 100_006_410, "#6410"}, // BTC daily binary, Yes
		{1, 0, 10, 100_000_010, "#10"},       // smallest non-zero
	}
	for _, c := range cases {
		if got := OutcomeEncoding(c.outcome, c.side); got != c.wantEncoding {
			t.Errorf("OutcomeEncoding(%d,%d) = %d, want %d", c.outcome, c.side, got, c.wantEncoding)
		}
		if got := OutcomeAsset(c.outcome, c.side); got != c.wantAsset {
			t.Errorf("OutcomeAsset(%d,%d) = %d, want %d", c.outcome, c.side, got, c.wantAsset)
		}
		if got := OutcomeCoin(c.outcome, c.side); got != c.wantCoin {
			t.Errorf("OutcomeCoin(%d,%d) = %q, want %q", c.outcome, c.side, got, c.wantCoin)
		}
	}
}

// The four asset-id bands must stay disjoint so a coin in one namespace can never
// collide with another: perp [0,10000), spot [10000,100000), HIP-3 sub-dex perps
// (>=100000, well below 100M), outcome [100_000_000, ...).
func TestOutcomeBandDisjoint(t *testing.T) {
	// A perp (asset 0), a spot id, and a far-out HIP-3 sub-dex id must all sit below
	// the outcome base; the smallest outcome asset must sit above them.
	perp := 0
	spot := spotAssetIndexOffset          // 10000
	subdex := PerpDexAsset(999, 9999)     // a deliberately extreme HIP-3 id
	smallestOutcome := OutcomeAsset(0, 0) // 100_000_000
	if !(perp < spotAssetIndexOffset && spot < perpDexAssetBase && subdex < outcomeAssetBase) {
		t.Fatalf("band ordering broken: perp=%d spot=%d subdex=%d outcomeBase=%d", perp, spot, subdex, outcomeAssetBase)
	}
	if smallestOutcome < outcomeAssetBase || smallestOutcome <= subdex {
		t.Fatalf("outcome band must sit above HIP-3: smallestOutcome=%d subdex=%d", smallestOutcome, subdex)
	}
}

// RegisterOutcomes wires both Yes/No sides of each outcome into coin->asset
// resolution with integer sizes (szDecimals 0); sides beyond 0/1 are ignored.
func TestRegisterOutcomes(t *testing.T) {
	info := NewInfo(context.Background(), "", true, &Meta{}, &SpotMeta{}, nil)
	info.RegisterOutcomes(&OutcomeMeta{Outcomes: []OutcomeInfo{
		{Outcome: 641, SideSpecs: []OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}},
		// A degenerate >2-side entry: only sides 0 and 1 must be registered.
		{Outcome: 7, SideSpecs: []OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}, {Name: "Maybe"}}},
	}})

	for coin, want := range map[string]int{
		"#6410": 100_006_410, "#6411": 100_006_411,
		"#70": 100_000_070, "#71": 100_000_071,
	} {
		got, ok := info.CoinToAsset(coin)
		if !ok || got != want {
			t.Errorf("CoinToAsset(%q) = %d ok=%v, want %d", coin, got, ok, want)
		}
		if dec := info.assetToDecimal[want]; dec != 0 {
			t.Errorf("assetToDecimal[%d] = %d, want 0 (integer sizes)", want, dec)
		}
	}
	// The invalid 3rd side ("Maybe", encoding 72) must NOT be registered.
	if _, ok := info.CoinToAsset("#72"); ok {
		t.Errorf("#72 (side 2) should not be registered — only sides 0/1 are valid")
	}
	// A nil meta must be a no-op, not a panic.
	info.RegisterOutcomes(nil)
}

// OutcomeMeta parses the live mainnet response shape: {outcomes[], questions[]},
// outcomes carry only {outcome,name,description,sideSpecs,quoteToken}, questions
// group named outcomes. (Sampled from api.hyperliquid.xyz on 2026-06-24.)
func TestOutcomeMetaParse(t *testing.T) {
	body := `{
		"outcomes":[
			{"outcome":171,"name":"Fallback","description":"","sideSpecs":[{"name":"Yes"},{"name":"No"}],"quoteToken":"USDC"},
			{"outcome":641,"name":"Recurring","description":"class:priceBinary|underlying:BTC|expiry:20260625-0600|targetPrice:62857|period:1d","sideSpecs":[{"name":"Yes"},{"name":"No"}],"quoteToken":"USDC"}
		],
		"questions":[
			{"question":32,"name":"2026 World Cup Champion","description":"...metadata=category:sports|subCategory:football","fallbackOutcome":171,"namedOutcomes":[172,173,174],"settledNamedOutcomes":[]}
		]
	}`
	info, ctx := testInfo(t, func(typ string, _ map[string]any) (int, string) {
		if typ == "outcomeMeta" {
			return 200, body
		}
		return 200, `{}`
	})
	om, err := info.OutcomeMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(om.Outcomes) != 2 || len(om.Questions) != 1 {
		t.Fatalf("counts: outcomes=%d questions=%d", len(om.Outcomes), len(om.Questions))
	}
	if om.Outcomes[1].Outcome != 641 || om.Outcomes[1].QuoteToken != "USDC" ||
		len(om.Outcomes[1].SideSpecs) != 2 || om.Outcomes[1].SideSpecs[0].Name != "Yes" {
		t.Fatalf("outcome[1]: %+v", om.Outcomes[1])
	}
	q := om.Questions[0]
	if q.Question != 32 || q.FallbackOutcome != 171 || len(q.NamedOutcomes) != 3 || len(q.SettledNamedOutcomes) != 0 {
		t.Fatalf("question: %+v", q)
	}
}

// SlippagePrice for outcomes uses ADDITIVE slippage clamped into (0,1): a
// multiplicative mid*1.05 would push a high-priced No leg past 1.0 (invalid).
func TestSlippagePriceOutcome(t *testing.T) {
	ex, ctx := testExchangeSpot(t, noInfo)
	ex.Info().RegisterOutcomes(&OutcomeMeta{Outcomes: []OutcomeInfo{
		{Outcome: 641, SideSpecs: []OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}},
	}})
	near := func(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

	yes := 0.04
	if got, _ := ex.SlippagePrice(ctx, "#6410", true, 0.05, &yes); !near(got, 0.09) {
		t.Errorf("outcome Yes buy: got %v want 0.09 (0.04+0.05, additive)", got)
	}
	// Yes sell: 0.04-0.05 = -0.01 -> clamped to the valid minimum, still > 0.
	if got, _ := ex.SlippagePrice(ctx, "#6410", false, 0.05, &yes); !near(got, 0.00001) {
		t.Errorf("outcome Yes sell must clamp into (0,1): got %v want 0.00001", got)
	}
	// No leg ~0.97: multiplicative 0.97*1.05 = 1.0185 would be invalid; additive +
	// clamp keeps it < 1 and still marketable.
	no := 0.97
	if got, _ := ex.SlippagePrice(ctx, "#6411", true, 0.05, &no); !near(got, 0.99999) {
		t.Errorf("outcome No buy must clamp below 1.0: got %v want 0.99999", got)
	}
}

// The "#<enc>" mid key resolves from the main allMids (outcomes carry no dex prefix).
func TestSlippagePriceOutcomeMid(t *testing.T) {
	ex, ctx := testExchangeSpot(t, func(typ string, _ map[string]any) (int, string) {
		if typ == "allMids" {
			return 200, `{"BTC":"65000","#6410":"0.20"}`
		}
		return 200, `{}`
	})
	ex.Info().RegisterOutcomes(&OutcomeMeta{Outcomes: []OutcomeInfo{
		{Outcome: 641, SideSpecs: []OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}},
	}})
	got, err := ex.SlippagePrice(ctx, "#6410", true, 0.05, nil) // mid 0.20 + 0.05
	if err != nil {
		t.Fatalf("outcome mid should resolve from allMids: %v", err)
	}
	if d := got - 0.25; d > 1e-9 || d < -1e-9 {
		t.Errorf("outcome buy from mid: got %v want 0.25", got)
	}
}
