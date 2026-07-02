package core

// NEXT-2 item 6: portfolio.margin_ratio and preview.resulting_account_leverage
// must use the SAME corrected unified-equity basis the risk gates use
// (accountEquity, basis 2 — commit 2c000c9), not the deprecated equityOf max()
// heuristic, which understates equity once margin is reserved and so overstates
// liquidation proximity / leverage.

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// unifiedMarginResp models a unified account with margin in use: true equity is
// the spot USDC ($10k, uPnL 0), but the perp account_value reads only the $8k
// margin slice and available_collateral only the unreserved $2k — both equityOf
// inputs are understated.
func unifiedMarginResp() respFn {
	clearing := `{"assetPositions":[{"position":{"coin":"BTC","szi":"0.66667","entryPx":"60000","positionValue":"40000","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"8000","leverage":{"type":"cross","value":5}},"type":"oneWay"}],"marginSummary":{"accountValue":"8000","totalMarginUsed":"8000","totalNtlPos":"40000","totalRawUsd":"8000"},"crossMarginSummary":{"accountValue":"8000","totalMarginUsed":"8000","totalNtlPos":"40000","totalRawUsd":"8000"},"withdrawable":"0"}`
	spot := `{"balances":[{"coin":"USDC","token":0,"total":"10000","hold":"0","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"2000"]]}`
	return infoMap(map[string]string{
		"clearinghouseState":     clearing,
		"spotClearinghouseState": spot,
		"frontendOpenOrders":     `[]`,
		"allMids":                `{"BTC":"60000"}`,
	})
}

// margin_ratio = maintenance / TRUE unified equity. BTC 40k notional at mmf
// 1/(2·40) = 0.0125 → maintenance 500; equity = spot USDC 10000 (uPnL 0) →
// ratio 0.05. The old equityOf basis gave 500/max(8000,2000) = 0.0625.
func TestMarginRatioUsesUnifiedEquityBasis(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, unifiedMarginResp())
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pf.MaintenanceMargin != "500" {
		t.Fatalf("maintenance margin = %q, want 500", pf.MaintenanceMargin)
	}
	if pf.MarginRatio != "0.05" {
		t.Fatalf("margin_ratio = %q, want 0.05 (maintenance / unified equity — the gates' basis)", pf.MarginRatio)
	}
}

// preview.resulting_account_leverage = gross book / the same unified equity:
// (40000 + 6000) / 10000 = 4.6. The old basis gave 46000/8000 = 5.75.
func TestPreviewAccountLeverageUsesUnifiedEquityBasis(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, unifiedMarginResp())
	res, err := c.Preview(ctx, "BTC", Buy, "0.1", "60000", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultingAccountLeverage != "4.6" {
		t.Fatalf("resulting_account_leverage = %q, want 4.6 (gate-basis equity)", res.ResultingAccountLeverage)
	}
}

// unifiedOutcomeMarginResp models a UNIFIED account that also holds HIP-4
// outcome shares: spot USDC $10k backing a BTC perp of $40k notional (uPnL 0,
// maintenance $500), plus 20,000 Yes shares (cost $4k) marked at 0.25 — outcome
// market value $5,000. Outcome shares are spot tokens: they count in the gates'
// total-wealth equity (accountEquity) but can NOT meet a perp margin call, so
// they must never back the liquidation-proximity denominators (fix-up).
func unifiedOutcomeMarginResp() respFn {
	clearing := `{"assetPositions":[{"position":{"coin":"BTC","szi":"0.66667","entryPx":"60000","positionValue":"40000","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"8000","leverage":{"type":"cross","value":5}},"type":"oneWay"}],"marginSummary":{"accountValue":"8000","totalMarginUsed":"8000","totalNtlPos":"40000","totalRawUsd":"8000"},"crossMarginSummary":{"accountValue":"8000","totalMarginUsed":"8000","totalNtlPos":"40000","totalRawUsd":"8000"},"withdrawable":"0"}`
	spot := `{"balances":[{"coin":"USDC","token":0,"total":"10000","hold":"0","entryNtl":"0"},{"coin":"+6410","total":"20000","hold":"0","entryNtl":"4000"}],"tokenToAvailableAfterMaintenance":[[0,"2000"]]}`
	return infoMap(map[string]string{
		"clearinghouseState":     clearing,
		"spotClearinghouseState": spot,
		"frontendOpenOrders":     `[]`,
		"allMids":                `{"BTC":"60000","#6410":"0.25"}`,
	})
}

// On a unified account, held outcome tokens must NOT dilute margin_ratio:
// maintenance 500 / (spot USDC 10000 + perp uPnL 0) = 0.05 — not 500/15000 ≈
// 0.033, which counts the $5k of outcome shares as margin-backing equity and
// understates liquidation proximity (an agent's "reduce if margin_ratio > X"
// rule would never fire while the perp book liquidates). The gates' equity
// (accountEquity: total wealth, the drawdown/daily-loss basis) still counts
// the outcome value — pinned here so the fix cannot leak into the gate basis.
func TestMarginRatioUnifiedExcludesOutcomeValue(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, unifiedOutcomeMarginResp())
	c.Meta().AddOutcomes(outcome641)
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pf.CollateralShared {
		t.Fatal("fixture must model a unified account")
	}
	hasOutcome := false
	for _, p := range pf.Positions {
		if p.Class == "outcome" && p.PositionValue == "5000" {
			hasOutcome = true
		}
	}
	if !hasOutcome {
		t.Fatalf("fixture must surface the $5000 outcome holding, got %+v", pf.Positions)
	}
	if pf.MaintenanceMargin != "500" {
		t.Fatalf("maintenance margin = %q, want 500 (outcome rows carry no maintenance)", pf.MaintenanceMargin)
	}
	if pf.MarginRatio != "0.05" {
		t.Fatalf("margin_ratio = %q, want 0.05 (outcome shares cannot back perp margin)", pf.MarginRatio)
	}
	if got := accountEquity(pf); got != 15000 {
		t.Fatalf("accountEquity (gate basis) = %v, want 15000 — the gates' total-wealth equity must keep counting outcome value", got)
	}
	if got := perpMarginEquity(pf); got != 10000 {
		t.Fatalf("perpMarginEquity = %v, want 10000 (spot USDC + perp uPnL, outcome value excluded)", got)
	}
}

// preview.resulting_account_leverage on the same unified+outcome account must
// divide by the outcome-free margin equity: gross (40000 existing + 6000
// proposed + 5000 outcome at-mark) / 10000 = 5.1 — not 51000/15000 = 3.4.
func TestPreviewAccountLeverageUnifiedExcludesOutcomeValue(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, unifiedOutcomeMarginResp())
	c.Meta().AddOutcomes(outcome641)
	res, err := c.Preview(ctx, "BTC", Buy, "0.1", "60000", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultingAccountLeverage != "5.1" {
		t.Fatalf("resulting_account_leverage = %q, want 5.1 (outcome value must not back perp margin)", res.ResultingAccountLeverage)
	}
}

// nonUnifiedMarginResp models a NON-unified account (no shared collateral: the
// spot state carries no tokenToAvailableAfterMaintenance): the perp wallet holds
// account_value $1,000 backing a BTC long of $8,000 notional, while $9,000 of
// USDC sits idle in the SPOT wallet — where it CANNOT meet a perp margin call.
func nonUnifiedMarginResp() respFn {
	clearing := `{"assetPositions":[{"position":{"coin":"BTC","szi":"0.13333","entryPx":"60000","positionValue":"8000","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"800","leverage":{"type":"cross","value":10}},"type":"oneWay"}],"marginSummary":{"accountValue":"1000","totalMarginUsed":"800","totalNtlPos":"8000","totalRawUsd":"1000"},"crossMarginSummary":{"accountValue":"1000","totalMarginUsed":"800","totalNtlPos":"8000","totalRawUsd":"1000"},"withdrawable":"200"}`
	spot := `{"balances":[{"coin":"USDC","token":0,"total":"9000","hold":"0","entryNtl":"0"}]}`
	return infoMap(map[string]string{
		"clearinghouseState":     clearing,
		"spotClearinghouseState": spot,
		"frontendOpenOrders":     `[]`,
		"allMids":                `{"BTC":"60000"}`,
	})
}

// On a NON-unified account, liquidation depends on the perp wallet's equity
// ALONE — idle spot USDC cannot be pulled in to meet a margin call. margin_ratio
// must divide by Σ perp account_value (here $1,000): maintenance = 8000 ×
// 1/(2·40) = 100 → ratio 0.1. Diluting the denominator with the $9,000 idle
// spot USDC (ratio 0.01) UNDERSTATES liquidation proximity — an agent's
// "reduce if margin_ratio > X" rule never fires and the perp wallet liquidates.
func TestMarginRatioNonUnifiedUsesPerpWalletEquity(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, nonUnifiedMarginResp())
	pf, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pf.CollateralShared {
		t.Fatal("fixture must model a NON-unified account")
	}
	if pf.MaintenanceMargin != "100" {
		t.Fatalf("maintenance margin = %q, want 100", pf.MaintenanceMargin)
	}
	if pf.MarginRatio != "0.1" {
		t.Fatalf("margin_ratio = %q, want 0.1 (maintenance / perp-wallet equity — spot USDC cannot back perp margin on a non-unified account)", pf.MarginRatio)
	}
}

// preview.resulting_account_leverage on the same non-unified account: gross book
// (8000 existing + 6000 proposed) / perp-wallet equity 1000 = 14 — not the
// spot-diluted 14000/10000 = 1.4, which would let an agent oversize massively.
func TestPreviewAccountLeverageNonUnifiedUsesPerpWalletEquity(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, nonUnifiedMarginResp())
	res, err := c.Preview(ctx, "BTC", Buy, "0.1", "60000", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultingAccountLeverage != "14" {
		t.Fatalf("resulting_account_leverage = %q, want 14 (perp-wallet equity denominator)", res.ResultingAccountLeverage)
	}
}
