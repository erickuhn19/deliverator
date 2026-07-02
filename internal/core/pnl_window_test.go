package core

// NEXT-2 items 4 + 5: PnlAttribution must (4) apply ONE window to fills, fees,
// and funding — the default (no --since) is the current UTC day, stated in the
// output — and (5) never sum a base-token fee quantity as USD: non-USDC fee
// tokens are converted via a live mid when possible, else excluded and reported
// separately.

import (
	"strings"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// pnlWindowResp records the startTime each time-bounded read was called with.
func pnlWindowResp(t *testing.T, fills, funding, mids string, fillsStart, fundingStart *[]int64) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" {
			return 200, "{}"
		}
		switch typ {
		case "userFills":
			t.Error("PnlAttribution must use the time-bounded fills read (one window), never full history")
			return 200, `[]`
		case "userFillsByTime":
			if s, ok := body["startTime"].(float64); ok {
				*fillsStart = append(*fillsStart, int64(s))
			}
			return 200, fills
		case "userFunding":
			if s, ok := body["startTime"].(float64); ok {
				*fundingStart = append(*fundingStart, int64(s))
			}
			return 200, funding
		case "allMids":
			return 200, mids
		}
		return 200, "{}"
	}
}

// Default (since=nil): fills AND funding must be fetched from the SAME window
// start — the current UTC midnight — and the view must state the window.
func TestPnlAttributionDefaultWindowGovernsAllComponents(t *testing.T) {
	var fillsStarts, fundingStarts []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, `[]`, `[]`, `{}`, &fillsStarts, &fundingStarts))

	midBefore := utcMidnightMs()
	v, _, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	midAfter := utcMidnightMs()

	if len(fillsStarts) == 0 || len(fundingStarts) == 0 {
		t.Fatalf("both fills and funding must be time-bounded: fills=%v funding=%v", fillsStarts, fundingStarts)
	}
	if fillsStarts[0] != fundingStarts[0] {
		t.Fatalf("ONE window must govern fills and funding: fills since %d, funding since %d", fillsStarts[0], fundingStarts[0])
	}
	if fillsStarts[0] != midBefore && fillsStarts[0] != midAfter {
		t.Fatalf("default window must anchor at UTC midnight (%d or %d), got %d", midBefore, midAfter, fillsStarts[0])
	}
	if v.WindowStartMs != fillsStarts[0] {
		t.Fatalf("view must state the effective window start: window_start_ms=%d, fetched=%d", v.WindowStartMs, fillsStarts[0])
	}
	if v.Window == "" || !strings.Contains(strings.ToLower(v.Window), "utc") {
		t.Fatalf("view must describe the default window (UTC day), got %q", v.Window)
	}
}

// An explicit --since governs all components identically.
func TestPnlAttributionExplicitSinceGovernsAllComponents(t *testing.T) {
	var fillsStarts, fundingStarts []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, `[]`, `[]`, `{}`, &fillsStarts, &fundingStarts))

	since := int64(12345)
	v, _, err := c.PnlAttribution(ctx, &since, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fillsStarts) != 1 || len(fundingStarts) != 1 || fillsStarts[0] != 12345 || fundingStarts[0] != 12345 {
		t.Fatalf("explicit since must govern both reads: fills=%v funding=%v", fillsStarts, fundingStarts)
	}
	if v.SinceMs != 12345 || v.WindowStartMs != 12345 {
		t.Fatalf("view must state the explicit window: %+v", v)
	}
}

// A spot-buy fee charged in the BASE token (feeToken PURR) must be converted to
// USD via the live mid — never summed at face value as dollars.
func TestPnlAttributionConvertsBaseTokenFee(t *testing.T) {
	// 10,000 PURR bought at $0.30; fee 7 PURR (real cost $2.10 at the live mid).
	fills := `[{"coin":"PURR/USDC","px":"0.30","sz":"10000","side":"B","time":3,"oid":3,"hash":"0x","fee":"7","feeToken":"PURR","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":3}]`
	var fs, us []int64
	// PURR/USDC is spot index 0 in testMeta → allMids key "@0".
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, fills, `[]`, `{"@0":"0.3"}`, &fs, &us))
	v, _, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.TradingFees != "-2.1" {
		t.Fatalf("7 PURR fee at mid 0.3 must be -2.1 USD, got %q (face-value summing = money bug)", v.Totals.TradingFees)
	}
	if len(v.FeeTokens) != 1 {
		t.Fatalf("fee_tokens breakdown must be present, got %+v", v.FeeTokens)
	}
	ft := v.FeeTokens[0]
	if ft.Token != "PURR" || ft.Amount != "7" || ft.UsdValue != "2.1" || !ft.Converted {
		t.Fatalf("fee_tokens row wrong: %+v", ft)
	}
}

// An unconvertible fee token (no <TOKEN>/USDC mid) must be EXCLUDED from the
// USD sums, reported in fee_tokens, and warned about — never silently summed.
func TestPnlAttributionExcludesUnconvertibleFeeToken(t *testing.T) {
	fills := `[{"coin":"PURR/USDC","px":"0.30","sz":"10000","side":"B","time":3,"oid":3,"hash":"0x","fee":"500","feeToken":"FOO","closedPnl":"10","startPosition":"0","dir":"Open Long","crossed":true,"tid":3}]`
	var fs, us []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, fills, `[]`, `{}`, &fs, &us))
	v, rm, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.TradingFees != "0" {
		t.Fatalf("an unconvertible fee must be excluded from trading_fees, got %q", v.Totals.TradingFees)
	}
	if v.Totals.NetSessionPnl != "10" {
		t.Fatalf("net must exclude the unvalued fee, got %q", v.Totals.NetSessionPnl)
	}
	if len(v.FeeTokens) != 1 || v.FeeTokens[0].Token != "FOO" || v.FeeTokens[0].Converted || v.FeeTokens[0].Amount != "500" {
		t.Fatalf("fee_tokens must report the excluded token, got %+v", v.FeeTokens)
	}
	warned := false
	for _, w := range rm.EnvelopeWarnings() {
		if strings.Contains(w, "FOO") && strings.Contains(strings.ToUpper(w), "EXCLUDED") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("an excluded fee token must be warned about, got %v", rm.EnvelopeWarnings())
	}
}

// On mainnet nearly every spot pair EXCEPT PURR/USDC has an "@<index>" universe
// name (HYPE/USDC is "@156"): a "<TOKEN>/USDC" name lookup fails even though the
// pair — and its mid — exists. The fee token must be resolved token-name → token
// index → the pair whose tokens are [token, USDC], so the available mid is used
// instead of wrongly excluding the fee (understated trading_fees / net).
func TestPnlAttributionConvertsFeeViaAtIndexSpotPair(t *testing.T) {
	meta := &hl.Meta{Universe: []hl.AssetInfo{{Name: "BTC", SzDecimals: 5, MaxLeverage: 40}}}
	spot := &hl.SpotMeta{
		Universe: []hl.SpotAssetInfo{{Name: "@156", Index: 156, Tokens: []int{150, 0}}},
		Tokens: []hl.SpotTokenInfo{
			{Name: "USDC", Index: 0, SzDecimals: 2},
			{Name: "HYPE", Index: 150, SzDecimals: 2},
		},
	}
	// Buy 10 HYPE at $40 on the "@156" pair; fee 0.5 HYPE (real cost $20).
	fills := `[{"coin":"@156","px":"40","sz":"10","side":"B","time":3,"oid":3,"hash":"0x","fee":"0.5","feeToken":"HYPE","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":3}]`
	var fs, us []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, fills, `[]`, `{"@156":"40.0"}`, &fs, &us))
	c.meta = NewMetaStore("testnet", meta, spot, time.Now())
	v, rm, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.TradingFees != "-20" {
		t.Fatalf("0.5 HYPE fee at the @156 mid 40 must be -20 USD, got %q (excluding an available mid under-reports costs)", v.Totals.TradingFees)
	}
	if len(v.FeeTokens) != 1 || v.FeeTokens[0].Token != "HYPE" || !v.FeeTokens[0].Converted || v.FeeTokens[0].Mid != "40" {
		t.Fatalf("fee_tokens must report the HYPE fee as converted at mid 40, got %+v", v.FeeTokens)
	}
	for _, w := range rm.EnvelopeWarnings() {
		if strings.Contains(strings.ToUpper(w), "EXCLUDED") {
			t.Fatalf("a convertible fee must not warn about exclusion: %v", rm.EnvelopeWarnings())
		}
	}
}

// A spot-buy BUILDER fee is denominated in the fill's single feeToken exactly
// like the trading fee (there is no separate builder-fee token on the wire) —
// it must be converted at the same mid, never summed at face value as USD
// (fix-up of item 5: builder_fees previously summed a token QUANTITY as dollars).
func TestPnlAttributionConvertsBaseTokenBuilderFee(t *testing.T) {
	// 10,000 PURR bought at $0.30; trading fee 7 PURR, builder fee 1 PURR
	// (real costs $2.10 and $0.30 at the live mid — NOT $7 and $1).
	fills := `[{"coin":"PURR/USDC","px":"0.30","sz":"10000","side":"B","time":3,"oid":3,"hash":"0x","fee":"7","feeToken":"PURR","builderFee":"1","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":3}]`
	var fs, us []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, fills, `[]`, `{"@0":"0.3"}`, &fs, &us))
	v, _, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.BuilderFees != "-0.3" {
		t.Fatalf("1 PURR builder fee at mid 0.3 must be -0.3 USD, got %q (face-value summing = money bug)", v.Totals.BuilderFees)
	}
	if v.Totals.TradingFees != "-2.1" {
		t.Fatalf("7 PURR trading fee at mid 0.3 must be -2.1 USD, got %q", v.Totals.TradingFees)
	}
	if v.Totals.NetSessionPnl != "-2.4" {
		t.Fatalf("net must be -2.4 (both fees at the mid), got %q", v.Totals.NetSessionPnl)
	}
	// fee_tokens itemizes the FULL token quantity — trading + builder: 7+1=8 PURR = $2.4.
	if len(v.FeeTokens) != 1 {
		t.Fatalf("fee_tokens breakdown must be present, got %+v", v.FeeTokens)
	}
	ft := v.FeeTokens[0]
	if ft.Token != "PURR" || ft.Amount != "8" || ft.UsdValue != "2.4" || !ft.Converted {
		t.Fatalf("fee_tokens must cover trading+builder quantities (8 PURR = $2.4), got %+v", ft)
	}
}

// When the fee token has no readable USDC mid, the BUILDER fee must be excluded
// from builder_fees/net exactly like the trading fee — and reported in fee_tokens.
func TestPnlAttributionExcludesUnconvertibleBuilderFee(t *testing.T) {
	fills := `[{"coin":"PURR/USDC","px":"0.30","sz":"10000","side":"B","time":3,"oid":3,"hash":"0x","fee":"500","feeToken":"FOO","builderFee":"100","closedPnl":"10","startPosition":"0","dir":"Open Long","crossed":true,"tid":3}]`
	var fs, us []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, fills, `[]`, `{}`, &fs, &us))
	v, rm, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.BuilderFees != "0" {
		t.Fatalf("an unconvertible builder fee must be excluded from builder_fees, got %q", v.Totals.BuilderFees)
	}
	if v.Totals.TradingFees != "0" || v.Totals.NetSessionPnl != "10" {
		t.Fatalf("net must exclude every unvalued fee, got %+v", v.Totals)
	}
	if len(v.FeeTokens) != 1 || v.FeeTokens[0].Token != "FOO" || v.FeeTokens[0].Converted || v.FeeTokens[0].Amount != "600" {
		t.Fatalf("fee_tokens must report the excluded trading+builder quantity (600 FOO), got %+v", v.FeeTokens)
	}
	warned := false
	for _, w := range rm.EnvelopeWarnings() {
		if strings.Contains(w, "FOO") && strings.Contains(strings.ToUpper(w), "EXCLUDED") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("an excluded fee token must be warned about, got %v", rm.EnvelopeWarnings())
	}
}

// SpotPairForToken resolves both a canonical "<TOKEN>/USDC" name and an
// "@<index>" universe name, and refuses unknown tokens / non-USDC quotes.
func TestSpotPairForToken(t *testing.T) {
	spot := &hl.SpotMeta{
		Universe: []hl.SpotAssetInfo{
			{Name: "PURR/USDC", Index: 0, Tokens: []int{1, 0}},
			{Name: "@156", Index: 156, Tokens: []int{150, 0}},
			{Name: "@200", Index: 200, Tokens: []int{150, 3}}, // HYPE quoted in a non-USDC token
		},
		Tokens: []hl.SpotTokenInfo{
			{Name: "USDC", Index: 0, SzDecimals: 2},
			{Name: "PURR", Index: 1, SzDecimals: 2},
			{Name: "OTHER", Index: 3, SzDecimals: 2},
			{Name: "HYPE", Index: 150, SzDecimals: 2},
		},
	}
	m := NewMetaStore("testnet", &hl.Meta{}, spot, time.Now())
	if mk, ok := m.SpotPairForToken("PURR"); !ok || mk.Coin != "PURR/USDC" {
		t.Fatalf("canonical pair must resolve by name: %+v ok=%v", mk, ok)
	}
	if mk, ok := m.SpotPairForToken("HYPE"); !ok || mk.Coin != "@156" || mk.AssetIndex != 10156 {
		t.Fatalf("@-named pair must resolve via the token index: %+v ok=%v", mk, ok)
	}
	if _, ok := m.SpotPairForToken("NOPE"); ok {
		t.Fatal("unknown token must not resolve")
	}
}

// USDC fees (and blank feeToken, the perp default) keep the existing behavior.
func TestPnlAttributionUSDCFeesUnchanged(t *testing.T) {
	fills := `[{"coin":"BTC","px":"60000","sz":"0.01","side":"A","time":3,"oid":3,"hash":"0x","fee":"0.10","feeToken":"USDC","builderFee":"0.05","closedPnl":"50","startPosition":"0.01","dir":"Close","crossed":true,"tid":3}]`
	var fs, us []int64
	c, ctx := newTestClient(t, config.Default(), Options{},
		pnlWindowResp(t, fills, `[]`, `{}`, &fs, &us))
	v, _, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.TradingFees != "-0.1" || v.Totals.BuilderFees != "-0.05" || v.Totals.NetSessionPnl != "49.85" {
		t.Fatalf("USDC fee handling regressed: %+v", v.Totals)
	}
	if len(v.FeeTokens) != 0 {
		t.Fatalf("all-USDC fees need no fee_tokens breakdown, got %+v", v.FeeTokens)
	}
}

// utcMidnightMs sanity: it is 00:00:00.000 UTC of the current day.
func TestUtcMidnightMs(t *testing.T) {
	ms := utcMidnightMs()
	tm := time.UnixMilli(ms).UTC()
	if tm.Hour() != 0 || tm.Minute() != 0 || tm.Second() != 0 || tm.Nanosecond() != 0 {
		t.Fatalf("utcMidnightMs not midnight: %v", tm)
	}
	now := time.Now().UTC()
	if tm.Year() != now.Year() || tm.YearDay() != now.YearDay() {
		// tolerate a midnight rollover between the two calls
		if !now.Add(-time.Second).Before(tm.Add(24 * time.Hour)) {
			t.Fatalf("utcMidnightMs wrong day: %v vs %v", tm, now)
		}
	}
}
