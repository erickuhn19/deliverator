package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// ---- fixtures ----

func clearingWith(accountValue string, positions ...string) string {
	av := accountValue
	return `{"assetPositions":[` + strings.Join(positions, ",") +
		`],"marginSummary":{"accountValue":"` + av + `","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"` + av +
		`"},"crossMarginSummary":{"accountValue":"` + av + `","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"` + av +
		`"},"withdrawable":"` + av + `"}`
}

// posWith encodes a position: szi sign sets long/short, positionValue is the
// unsigned notional (matching HL's shape).
func posWith(coin, szi, notional string) string {
	return `{"position":{"coin":"` + coin + `","szi":"` + szi + `","positionValue":"` + notional +
		`","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"0","leverage":{"type":"cross","value":1}},"type":"oneWay"}`
}

// gateResp serves the reads Portfolio() makes; clearing is a pointer so a test
// can change the live equity between calls (drawdown/daily-loss).
func gateResp(clearing *string, spot string) respFn {
	return func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, *clearing
		case "frontendOpenOrders":
			return 200, `[]`
		case "spotClearinghouseState":
			return 200, spot
		}
		return 200, `{}`
	}
}

func riskCfg(r config.Risk) *config.Config {
	c := config.Default()
	c.Risk = r // start from all-off so each test isolates its gate
	return c
}

func assertRiskCode(t *testing.T, err error, code string) {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %v", err)
	}
	if oe.Category != output.CatRisk || oe.Code != code {
		t.Fatalf("want risk/%s, got %s/%s (%v)", code, oe.Category, oe.Code, oe)
	}
}

const noSpot = `{"balances":[]}`

// ---- snapshot exposure gates ----

func TestGateAccountLeverage(t *testing.T) {
	clearing := clearingWith("1000", posWith("BTC", "0.02", "1000")) // long $1000, equity 1000 => 1x
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 2}), Options{}, gateResp(&clearing, noSpot))
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 400}}); err != nil {
		t.Fatalf("1.4x within the 2x cap must pass: %v", err)
	}
	assertRiskCode(t, c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 1500}}), "max_account_leverage") // 2.5x
}

func TestGateNetExposure(t *testing.T) {
	clearing := clearingWith("100000") // flat, high equity so leverage never trips
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxNetExposureUSD: 1000}), Options{}, gateResp(&clearing, noSpot))
	// A hedged book nets out: +1500 long, -800 short => |net| 700, within cap.
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 1500}, {"ETH", -800}}); err != nil {
		t.Fatalf("net 700 within 1000 cap must pass: %v", err)
	}
	assertRiskCode(t, c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 1500}}), "max_net_exposure")
}

func TestGateConcentration(t *testing.T) {
	clearing := clearingWith("1000")
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxConcentrationPctPerCoin: 100}), Options{}, gateResp(&clearing, noSpot))
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 800}}); err != nil {
		t.Fatalf("80%% of equity within the 100%% cap must pass: %v", err)
	}
	assertRiskCode(t, c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 1500}}), "max_concentration") // 150%
}

// ---- trajectory gates (persistent peak / daily anchor) ----

func TestGateDrawdown(t *testing.T) {
	clearing := clearingWith("1000")
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDrawdownPct: 20}), Options{}, gateResp(&clearing, noSpot))
	if err := c.checkPortfolioGates(ctx, nil); err != nil { // peak = 1000
		t.Fatalf("first observation sets the peak, must pass: %v", err)
	}
	clearing = clearingWith("700") // 30% below peak
	assertRiskCode(t, c.checkPortfolioGates(ctx, nil), "max_drawdown")
}

func TestGateDailyLoss(t *testing.T) {
	clearing := clearingWith("1000")
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxDailyLossUSD: 100}), Options{}, gateResp(&clearing, noSpot))
	if err := c.checkPortfolioGates(ctx, nil); err != nil { // anchor = 1000
		t.Fatalf("first observation sets the daily anchor, must pass: %v", err)
	}
	clearing = clearingWith("850") // $150 loss > $100 cap
	assertRiskCode(t, c.checkPortfolioGates(ctx, nil), "max_daily_loss")
}

func TestObserveEquityDayRolloverAndPeak(t *testing.T) {
	testHome(t)
	seed, _ := json.Marshal(riskState{PeakEquity: 5000, Day: "1970-01-01", DayAnchorEquity: 5000, Basis: currentEquityBasis})
	_ = os.MkdirAll(filepath.Dir(riskStatePath()), 0o700)
	if err := os.WriteFile(riskStatePath(), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	dd, dlUSD, dlPct, err := observeEquity(1000)
	if err != nil {
		t.Fatal(err)
	}
	if dlUSD != 0 || dlPct != 0 {
		t.Fatalf("a new UTC day must re-anchor the daily figure, got %.2f/%.2f", dlUSD, dlPct)
	}
	if dd < 79.9 || dd > 80.1 { // all-time peak 5000 persists across the day rollover
		t.Fatalf("drawdown from the persistent peak should be ~80%%, got %.2f", dd)
	}
}

// ---- fail-closed + unified-account equity fallback ----

func TestGateFailsClosedOnUnreadableState(t *testing.T) {
	resp := func(_, typ string, _ map[string]any) (int, string) {
		if typ == "clearinghouseState" {
			return 500, `upstream down`
		}
		return 200, `[]`
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 2}), Options{}, resp)
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 100}}); err == nil {
		t.Fatal("an unreadable account snapshot must fail closed (reject), not pass")
	}
}

func TestGateEquityFallsBackToCollateralWhenFlat(t *testing.T) {
	// Unified account: perp account_value 0 while $500 USDC sits in spot.
	clearing := clearingWith("0")
	spot := `{"balances":[{"coin":"USDC","token":0,"hold":"0","total":"500","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"500"]]}`
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 2}), Options{}, gateResp(&clearing, spot))
	// equity falls back to 500: +800 => 1.6x, within 2x.
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 800}}); err != nil {
		t.Fatalf("equity should fall back to $500 collateral (1.6x ok): %v", err)
	}
	// +1200 => 2.4x > 2x.
	assertRiskCode(t, c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 1200}}), "max_account_leverage")
}

// Live-found bug: a unified account WITH a position reads a tiny perp
// account_value ($3) while the real equity ($145) sits in collateral. Equity must
// be the GREATER of the two, or every gate over-rejects the moment a position
// opens. This fails on the old account_value-only logic (would compute ~10x).
func TestGateEquityUsesGreaterOfAccountValueAndCollateral(t *testing.T) {
	clearing := clearingWith("3.04", posWith("SOL", "0.22", "15.21")) // perp margin slice only
	spot := `{"balances":[{"coin":"USDC","token":0,"hold":"0","total":"145","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"145.11"]]}`
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 0.5}), Options{}, gateResp(&clearing, spot))
	// Add ~$15.2: resulting SOL ~$30.4 / equity max(3.04,145.11)=145.11 = 0.21x < 0.5 => pass.
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"SOL", 15.2}}); err != nil {
		t.Fatalf("equity must use the $145 collateral (0.21x), not the $3 margin slice: %v", err)
	}
}

// ---- max open positions ----

func TestGateMaxOpenPositions(t *testing.T) {
	clearing := clearingWith("100000", posWith("BTC", "0.1", "5000"), posWith("ETH", "1", "3000")) // 2 open
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxOpenPositions: 2}), Options{}, gateResp(&clearing, noSpot))
	// Adding to an existing coin keeps the count at 2 → allowed.
	if err := c.checkPortfolioGates(ctx, []exposureDelta{{"BTC", 1000}}); err != nil {
		t.Fatalf("adding to an existing position must pass: %v", err)
	}
	// Opening a 3rd distinct coin → 3 > cap 2 → reject.
	assertRiskCode(t, c.checkPortfolioGates(ctx, []exposureDelta{{"SOL", 1000}}), "max_open_positions")
}

// ---- reduce-only flip guard ----

func TestReduceOnlyFlipGuard(t *testing.T) {
	clearing := clearingWith("100000", posWith("BTC", "0.1", "5000")) // long 0.1 BTC
	c, ctx := newTestClient(t, riskCfg(config.Risk{}), Options{}, gateResp(&clearing, noSpot))
	// Within the position → ok.
	if err := c.reduceOnlyFlipErr(ctx, "BTC", 0.05); err != nil {
		t.Fatalf("a reduce within the position must pass: %v", err)
	}
	// Exceeds the position → would cross zero → reject.
	assertRiskCode(t, c.reduceOnlyFlipErr(ctx, "BTC", 0.2), "reduce_only_flip")
	// Flat coin → skipped, so a bracket tp/sl or pre-armed stop placed before the
	// position exists is never false-rejected.
	if err := c.reduceOnlyFlipErr(ctx, "ETH", 0.5); err != nil {
		t.Fatalf("reduce-only when flat must be allowed (bracket/pre-armed stop): %v", err)
	}
}

// ---- wiring: Place trips the gate; reduce-only is exempt ----

func TestPlaceGateWiringAndReduceOnlyExempt(t *testing.T) {
	clearing := clearingWith("1000") // flat, equity 1000
	resp := func(path, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, clearing
		case "frontendOpenOrders":
			return 200, `[]`
		case "spotClearinghouseState":
			return 200, noSpot
		case "allMids":
			return 200, `{"BTC":"60000"}`
		}
		if path == "/exchange" {
			return 200, okOrder(`{"resting":{"oid":1}}`)
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxAccountLeverage: 1}), Options{}, resp)

	// Market buy 0.05 BTC @ ~60000 = $3000 notional, equity 1000 => 3x > 1x cap.
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.05"}); err == nil {
		t.Fatal("a 3x order must be rejected by the account-leverage gate")
	} else {
		assertRiskCode(t, err, "max_account_leverage")
	}

	// The same size as a reduce-only order adds no exposure → gate is skipped.
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.05", ReduceOnly: true}); err != nil {
		t.Fatalf("reduce-only must be exempt from the portfolio gate, got %v", err)
	}
}
