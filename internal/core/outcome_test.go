package core

import (
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// EnsureOutcomes is the lazy-load entry: a no-op once the outcome universe is
// loaded, so a command can call it freely without re-fetching (audit: outcomes
// load on demand, not via a config flag that defaulted off and hid the markets).
func TestEnsureOutcomesIdempotentWhenLoaded(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, _ string, _ map[string]any) (int, string) {
		return 200, `{}`
	})
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 641, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})
	if c.Meta().OutcomeMeta() == nil {
		t.Fatal("precondition: outcomes should be loaded")
	}
	// Already loaded -> returns nil immediately and must not clear what's loaded.
	if err := c.EnsureOutcomes(ctx); err != nil {
		t.Fatalf("EnsureOutcomes when already loaded must be a no-op, got %v", err)
	}
	if c.Meta().OutcomeMeta() == nil {
		t.Fatal("EnsureOutcomes must not drop the loaded outcome universe")
	}
}

// AddOutcomes makes "#<encoding>" outcome coins resolve and placeable with rich
// fields, surfaced via OutcomeMarkets() — but kept OUT of the default `markets`
// (ordered) listing.
func TestMetaStoreAddOutcomes(t *testing.T) {
	ms := testMeta()
	before := len(ms.Markets())

	ms.AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 641, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})

	yes, ok := ms.Lookup("#6410")
	if !ok {
		t.Fatal("#6410 (outcome 641 Yes) should resolve after AddOutcomes")
	}
	if yes.AssetIndex != 100_006_410 || yes.SzDecimals != 0 || yes.IsSpot || !yes.IsOutcome {
		t.Fatalf("outcome Yes market wrong: %+v", yes)
	}
	if yes.Class != "outcome" || yes.Side != "Yes" || yes.Outcome != 641 ||
		yes.ResolutionStatus != "open" || yes.PriceBound != "0..1" || yes.QuoteToken != "USDC" {
		t.Errorf("outcome Yes rich fields wrong: %+v", yes)
	}
	no, ok := ms.Lookup("#6411")
	if !ok || no.AssetIndex != 100_006_411 || no.Side != "No" {
		t.Fatalf("#6411 (outcome 641 No) wrong: %+v ok=%v", no, ok)
	}

	// Surfaced via OutcomeMarkets() (both legs), NOT the default `markets` listing.
	if got := len(ms.OutcomeMarkets()); got != 2 {
		t.Errorf("OutcomeMarkets() = %d, want 2 (Yes+No)", got)
	}
	if got := len(ms.Markets()); got != before {
		t.Errorf("Markets() grew by %d; outcomes must stay out of the default listing", got-before)
	}
	if ms.OutcomeMeta() == nil || len(ms.OutcomeMeta().Outcomes) != 1 {
		t.Errorf("OutcomeMeta() should report the loaded universe")
	}
}

// A priceBinary description is parsed into underlying/target/expiry + a readable title.
func TestAddOutcomesPriceBinary(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{
			Outcome: 641, Name: "Recurring",
			Description: "class:priceBinary|underlying:BTC|expiry:20260625-0600|targetPrice:62857|period:1d",
			SideSpecs:   []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC",
		},
	}})
	yes, _ := ms.Lookup("#6410")
	if yes.Underlying != "BTC" || yes.TargetPrice != "62857" || yes.Expiry != "2026-06-25 06:00Z" {
		t.Errorf("priceBinary parse wrong: underlying=%q target=%q expiry=%q", yes.Underlying, yes.TargetPrice, yes.Expiry)
	}
	if !strings.Contains(yes.Title, "BTC above 62857") || !strings.HasSuffix(yes.Title, "Yes") {
		t.Errorf("priceBinary title wrong: %q", yes.Title)
	}
}

// Questions group outcomes (title + question fields) and mark settled legs.
func TestAddOutcomesQuestionGrouping(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(&hl.OutcomeMeta{
		Outcomes: []hl.OutcomeInfo{
			{Outcome: 178, Name: "Brazil", SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
			{Outcome: 171, Name: "Fallback", SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
		},
		Questions: []hl.OutcomeQuestion{
			{
				Question: 32, Name: "2026 World Cup Champion", FallbackOutcome: 171,
				NamedOutcomes: []int{178}, SettledNamedOutcomes: []int{171},
			},
		},
	})
	brz, _ := ms.Lookup("#1780") // outcome 178 Yes
	if brz.Question != 32 || brz.QuestionName != "2026 World Cup Champion" {
		t.Errorf("question grouping wrong: %+v", brz)
	}
	if brz.Title != "2026 World Cup Champion: Brazil — Yes" {
		t.Errorf("event title wrong: %q", brz.Title)
	}
	if fb, _ := ms.Lookup("#1710"); fb.ResolutionStatus != "settled" {
		t.Errorf("settled outcome should be marked settled, got %q", fb.ResolutionStatus)
	}
}

func TestParseOutcomeDescription(t *testing.T) {
	u, tg, e := parseOutcomeDescription("class:priceBinary|underlying:ETH|expiry:20260101-1200|targetPrice:3000.5|period:1d")
	if u != "ETH" || tg != "3000.5" || e != "2026-01-01 12:00Z" {
		t.Errorf("parse = (%q,%q,%q)", u, tg, e)
	}
	// Non-priceBinary descriptions yield empty strings.
	if u, tg, e := parseOutcomeDescription("This resolves Yes if Brazil wins."); u != "" || tg != "" || e != "" {
		t.Errorf("english desc should parse empty, got (%q,%q,%q)", u, tg, e)
	}
	if e := formatOutcomeExpiry("bogus"); e != "bogus" {
		t.Errorf("bad expiry should pass through, got %q", e)
	}
}

// ctx on an outcome returns the probability (mid), the complement, best bid/ask,
// and descriptive fields — and omits perp-only funding/OI/oracle.
func TestOutcomeCtx(t *testing.T) {
	resp := func(path, typ string, body map[string]any) (int, string) {
		switch typ {
		case "allMids":
			return 200, `{"BTC":"65000","#6410":"0.20","#6411":"0.80"}`
		case "l2Book":
			return 200, `{"coin":"#6410","time":1,"levels":[[{"px":"0.19","sz":"100","n":1}],[{"px":"0.21","sz":"100","n":1}]]}`
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{
			Outcome: 641, Name: "Recurring",
			Description: "class:priceBinary|underlying:BTC|expiry:20260625-0600|targetPrice:62857|period:1d",
			SideSpecs:   []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC",
		},
	}})

	cv, err := c.Ctx(ctx, "#6410")
	if err != nil {
		t.Fatal(err)
	}
	if !cv.IsOutcome || cv.MidPx != "0.20" || cv.MarkPx != "0.20" {
		t.Fatalf("outcome ctx mid wrong: %+v", cv)
	}
	if cv.ComplementMid != "0.8" { // 1 - 0.20
		t.Errorf("complement mid = %q, want 0.8", cv.ComplementMid)
	}
	if cv.BestBid != "0.19" || cv.BestAsk != "0.21" {
		t.Errorf("best bid/ask wrong: bid=%q ask=%q", cv.BestBid, cv.BestAsk)
	}
	if cv.Side != "Yes" || cv.ResolutionStatus != "open" || cv.Expiry != "2026-06-25 06:00Z" {
		t.Errorf("outcome ctx descriptive fields wrong: %+v", cv)
	}
	if cv.Funding != "" || cv.OpenInterest != "" || cv.OraclePx != "" {
		t.Errorf("outcome ctx must omit perp-only fields: %+v", cv)
	}
}

// Risk caps treat an outcome buy's at-stake (size×price) as the notional — the
// fully-collateralized max loss — not a leveraged exposure.
func TestOutcomeRiskCapAtStake(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 30 // cap interpreted as at-stake (size×price)
	cfg.Risk.MinOrderNotionalUSD = 10
	c, ctx := newTestClient(t, cfg, Options{DryRun: true}, func(_, _ string, _ map[string]any) (int, string) {
		return 200, `{}`
	})
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 641, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})

	// at-stake = 200 × 0.25 = $50 > $30 cap -> rejected on the at-stake amount.
	if _, _, err := c.Place(ctx, OrderReq{Coin: "#6410", Side: Buy, Size: "200", Limit: "0.25"}); err == nil {
		t.Fatal("expected a cap reject for $50 at-stake > $30 cap")
	}
	// at-stake = 100 × 0.25 = $25 — within the cap and above the $10 floor -> passes.
	res, _, err := c.Place(ctx, OrderReq{Coin: "#6410", Side: Buy, Size: "100", Limit: "0.25"})
	if err != nil {
		t.Fatalf("$25 at-stake within caps should pass: %v", err)
	}
	if res == nil || res.Status != "dry_run" {
		t.Fatalf("expected dry_run pass, got %+v", res)
	}
}

var outcome641 = &hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
	{
		Outcome: 641, Name: "Recurring",
		Description: "class:priceBinary|underlying:BTC|expiry:20260625-0600|targetPrice:62857|period:1d",
		SideSpecs:   []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC",
	},
}}

// positions surfaces a held "+<enc>" outcome token as a class:"outcome" row with
// side/mark/at-stake/max-gain.
func TestOutcomePositions(t *testing.T) {
	resp := func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, clearingWith("100") // flat perp account
		case "spotClearinghouseState":
			return 200, `{"balances":[{"coin":"USDC","token":0,"total":"100","hold":"0","entryNtl":"0"},{"coin":"+6410","total":"200","hold":"0","entryNtl":"40"}]}`
		case "allMids":
			return 200, `{"BTC":"65000","#6410":"0.25"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	c.Meta().AddOutcomes(outcome641)

	pos, err := c.Positions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var oc *PositionView
	for i := range pos {
		if pos[i].Class == "outcome" {
			oc = &pos[i]
			break
		}
	}
	if oc == nil {
		t.Fatal("a held outcome should surface as a class:outcome position")
	}
	if oc.Coin != "#6410" || oc.OutcomeSide != "Yes" || oc.Szi != "200" {
		t.Fatalf("outcome position identity wrong: %+v", oc)
	}
	// mark 0.25 → value 200×0.25=50; at-stake (cost) 40; max-gain 200−40=160.
	if oc.MarkPx != "0.25" || oc.AtStakeUSD != "40" || oc.PositionValue != "50" || oc.MaxGainUSD != "160" {
		t.Fatalf("outcome payoff/mark wrong: %+v", oc)
	}
	// No perp/leverage/liq fields on an outcome position.
	if oc.LiquidationPx != "" || oc.LeverageValue != 0 {
		t.Errorf("outcome position must omit liq/leverage: %+v", oc)
	}
}

// close on an outcome sells the full held side (size pulled from the +<enc> balance).
func TestCloseOutcome(t *testing.T) {
	resp := func(_, typ string, _ map[string]any) (int, string) {
		if typ == "spotClearinghouseState" {
			return 200, `{"balances":[{"coin":"+6410","total":"280","hold":"0","entryNtl":"40"}]}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, resp)
	c.Meta().AddOutcomes(outcome641)

	res, _, err := c.Close(ctx, "#6410", "", false, "0.04", "") // no size → full balance
	if err != nil {
		t.Fatal(err)
	}
	if res.Coin != "#6410" || res.Side != "sell" || res.Size != "280" || res.Status != "dry_run" {
		t.Fatalf("outcome close wrong: %+v", res)
	}
}
