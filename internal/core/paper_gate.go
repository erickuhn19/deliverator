package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// GlobalHaltError is the halt gate shared with Hyperliquid writes. A stat
// failure fails closed (Halted treats it as halted) so a damaged state dir
// cannot open a second venue.
func GlobalHaltError() error {
	if (&Client{}).Halted() {
		return output.Halt("halted", "global halt is active — new orders rejected").
			WithHint("deliverator halt off  (to resume)")
	}
	return nil
}

// PortfolioGatesConfigured reports whether a Papertrade open must also pass the
// Hyperliquid account-wide gates. Those gates read the Hyperliquid book, so the
// caller skips that network round-trip when this is false.
func PortfolioGatesConfigured(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return (&Client{cfg: cfg}).portfolioGuardsActive()
}

// PaperOpenGate enforces the per-order caps on a Papertrade isolated open.
// notionalUSD and coinNotionalUSD are margin × leverage in dollars; the coin
// figure includes existing Papertrade slugs in that market. The order is
// treated as a market order, so automation.limit_only rejects it. The shared
// per-minute rate cap is charged, same as a Hyperliquid order.
func PaperOpenGate(cfg *config.Config, coin string, leverage int, notionalUSD, coinNotionalUSD float64) error {
	if cfg == nil {
		return output.Validation("config_invalid", "config is not loaded")
	}
	c := &Client{cfg: cfg}
	if err := c.checkLeverage(leverage); err != nil {
		return err
	}
	min := cfg.Risk.MinOrderNotionalUSD
	return c.preTradeChecks(riskCheck{
		Coin:                coin,
		IsMarket:            true,
		NotionalUSD:         notionalUSD,
		PositionNotionalUSD: coinNotionalUSD,
		MinNotionalUSD:      min,
	})
}

// GatePaperExposure applies the account-wide gates (net exposure, leverage,
// concentration, drawdown, daily loss, open-position count) to a new Papertrade
// slug so those caps cannot be bypassed by leaving the Hyperliquid order path.
// paperOpenCount is the number of Papertrade slugs already open; the new slug
// is added. Hyperliquid positions are counted as well because each Papertrade
// slug is its own position even when the coin is already held on Hyperliquid.
func (c *Client) GatePaperExposure(ctx context.Context, coin string, signedNotional float64, paperOpenCount int) error {
	if !c.portfolioGuardsActive() {
		return nil
	}
	if strings.TrimSpace(c.queryAddr) == "" && (c.riskConfig().MaxAccountLeverage > 0 ||
		c.riskConfig().MaxConcentrationPctPerCoin > 0 || c.riskConfig().MaxDrawdownPct > 0 ||
		c.riskConfig().MaxDailyLossUSD > 0 || c.riskConfig().MaxDailyLossPct > 0 ||
		c.riskConfig().MaxOpenPositions > 0) {
		return output.Validation("master_address",
			"wallet.master_address is required to enforce portfolio risk gates on a Papertrade open").
			WithHint("set wallet.master_address to the account those gates measure")
	}
	if _, err := c.checkPortfolioGates(ctx, []exposureDelta{{
		coin: coin, signedNotional: signedNotional,
	}}); err != nil {
		return err
	}
	capN := c.riskConfig().MaxOpenPositions
	if capN <= 0 {
		return nil
	}
	pos, meta, err := c.Positions(ctx, "")
	if err != nil {
		return output.Network("portfolio_state",
			"cannot count Hyperliquid positions to enforce max_open_positions: "+err.Error()).Retry().
			WithHint("the open-position cap fails closed when the book cannot be read")
	}
	if len(meta.Degraded) > 0 || len(meta.DegradedDexs) > 0 {
		return output.Network("portfolio_state",
			"Hyperliquid position read is partial — refusing a Papertrade open while max_open_positions is set").Retry()
	}
	total := paperOpenCount + len(pos) + 1
	if total > capN {
		return output.Risk("max_open_positions",
			fmt.Sprintf("this would make %d open positions (%d on Papertrade, %d on Hyperliquid), over the max of %d",
				total, paperOpenCount+1, len(pos), capN)).
			WithHint(fmt.Sprintf("close a position so the combined count stays <= %d", capN))
	}
	return nil
}
