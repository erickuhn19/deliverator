package core

import (
	"context"

	"github.com/erickuhn19/deliverator/internal/output"
)

// Preview (#46) is a no-sign what-if: given a proposed order it projects the
// resulting position, the resulting ACCOUNT leverage (exact, same basis as the
// risk gates), the margin it needs, and an estimated liquidation price — so an
// agent can size risk-aware BEFORE committing, instead of placing then polling.
// It reads only; it never signs.

// PreviewResult is the projection. Prices/sizes are strings.
type PreviewResult struct {
	Coin           string `json:"coin"`
	Side           string `json:"side"`
	Size           string `json:"size"`
	EntryPx        string `json:"entry_px"`           // price used: --limit, else the live mark/mid
	Leverage       int    `json:"leverage,omitempty"` // L used (given, else the position's, else asset max); N/A for outcomes
	OrderNotional  string `json:"order_notional"`     // size * entry_px
	MarginRequired string `json:"margin_required"`    // order_notional / L (= at-stake for an outcome)

	// HIP-4 outcome-only: fully collateralized (no leverage/liquidation). The max
	// loss is the at-stake (size × price → 0 if it resolves against you); the max
	// gain is size × (1 − price) (it resolves to 1 per share).
	IsOutcome  bool   `json:"is_outcome,omitempty"`
	AtStakeUSD string `json:"at_stake_usd,omitempty"`
	MaxGainUSD string `json:"max_gain_usd,omitempty"`

	// EstLiquidationPx is a single-position ISOLATED estimate at the order price
	// (mmf = 1/(2 × the coin's tier max leverage)). A CROSS position's actual liq
	// depends on the whole account's shared margin, so treat this as an estimate;
	// for an EXISTING position the exact figure is `positions.liquidation_px`.
	// Both are omitted for outcomes (no liquidation).
	EstLiquidationPx    string `json:"est_liquidation_px,omitempty"`
	EstDistanceToLiqPct string `json:"est_distance_to_liq_pct,omitempty"`

	ResultingPositionSzi      string `json:"resulting_position_szi,omitempty"` // signed size after the order fills
	ResultingPositionNotional string `json:"resulting_position_notional,omitempty"`
	ResultingAccountLeverage  string `json:"resulting_account_leverage,omitempty"` // gross book / equity

	Model string `json:"model"` // caveat on the estimate
}

// outcomePreview projects a HIP-4 outcome bet. It is fully collateralized — no
// leverage, no liquidation — so the meaningful figures are the at-stake (max loss)
// and the max gain, not a liquidation price or account leverage.
func outcomePreview(mk Market, side Side, szF, px float64) *PreviewResult {
	// Buy a share at price p: pay p, resolve to 1 (gain 1−p) or 0 (lose p). A
	// sell (exit/short the side) inverts the payoff — receive p, resolve to 1
	// (owe the 1, net loss 1−p) or 0 (keep p). So at-stake and max-gain SWAP by
	// side; reporting buy semantics for a sell understates the risk (audit #91 / T3-preview).
	atStake := szF * px
	maxGain := szF * (1 - px)
	if side == Sell {
		atStake, maxGain = szF*(1-px), szF*px
	}
	if atStake < 0 {
		atStake = 0
	}
	if maxGain < 0 {
		maxGain = 0
	}
	return &PreviewResult{
		Coin:           mk.Coin,
		Side:           side.String(),
		Size:           f2s(szF),
		EntryPx:        f2s(px),
		IsOutcome:      true,
		OrderNotional:  f2s(atStake),
		MarginRequired: f2s(atStake), // 100% collateralized (= max loss)
		AtStakeUSD:     f2s(atStake),
		MaxGainUSD:     f2s(maxGain),
		Model:          "HIP-4 outcome: fully collateralized — no leverage or liquidation. A BUY stakes size×price (max loss; resolves to 0 against you) to gain size×(1−price) (resolves to 1); a SELL/exit inverts these. Settles automatically at expiry; reduce-only/close is a sell of the held side.",
	}
}

// isolatedLiqPrice estimates the liquidation price of a single isolated position:
//
//	liq = entry × (side − 1/L) / (side − mmf)     side = +1 long / −1 short
func isolatedLiqPrice(entry, side float64, leverage int, mmf float64) float64 {
	if entry <= 0 || leverage <= 0 {
		return 0
	}
	denom := side - mmf
	if denom == 0 {
		return 0
	}
	liq := entry * (side - 1/float64(leverage)) / denom
	if liq < 0 {
		return 0
	}
	return liq
}

// Preview projects the impact of a proposed perp order without signing.
func (c *Client) Preview(ctx context.Context, coin string, side Side, size, limit string, leverage int) (*PreviewResult, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	if mk.IsSpot {
		return nil, output.Validation("not_perp", "preview models perp positions; "+coin+" is spot")
	}
	szF := parseFloatSafe(size)
	if szF <= 0 {
		return nil, output.Validation("bad_size", "size must be > 0")
	}

	// Entry price: the limit if given, else the live mark/mid.
	px := parseFloatSafe(limit)
	if px <= 0 {
		m, hasMid := c.midPrice(ctx, mk.Coin)
		if !hasMid {
			return nil, output.Network("no_mid", "no mark/mid price available for "+mk.Coin+" — pass --limit").Retry()
		}
		px = m
	}

	// HIP-4 outcomes are fully collateralized: project at-stake/max-gain, not the
	// leverage/liquidation model below (no account snapshot needed).
	if mk.IsOutcome {
		return outcomePreview(mk, side, szF, px), nil
	}

	// One account snapshot for the existing position, leverage default, and the
	// resulting account-leverage math.
	pf, err := c.Portfolio(ctx)
	if err != nil {
		return nil, err
	}
	var curSzi float64
	var curLev int
	perCoin := map[string]float64{}
	for _, p := range pf.Positions {
		n := parseFloatSafe(p.PositionValue)
		if p.Side == "short" {
			n = -n
		}
		perCoin[p.Coin] += n
		if matchesCoinFilter(p.Coin, mk.Coin) {
			curSzi = parseFloatSafe(p.Szi)
			curLev = p.LeverageValue
		}
	}

	L := leverage
	if L <= 0 {
		if curLev > 0 {
			L = curLev // default to the existing position's leverage
		} else {
			L = mk.MaxLeverage
		}
	}
	if L <= 0 {
		L = 1
	}
	if mk.MaxLeverage > 0 && L > mk.MaxLeverage {
		L = mk.MaxLeverage // clamp to the asset cap
	}

	sideSign := 1.0
	if side == Sell {
		sideSign = -1.0
	}
	orderNotional := szF * px
	mmf := c.meta.MaintenanceMarginFraction(mk.Coin, orderNotional)
	liq := isolatedLiqPrice(px, sideSign, L, mmf)

	resultSzi := curSzi + sideSign*szF

	res := &PreviewResult{
		Coin:                      mk.Coin,
		Side:                      side.String(),
		Size:                      f2s(szF),
		EntryPx:                   f2s(px),
		Leverage:                  L,
		OrderNotional:             f2s(orderNotional),
		MarginRequired:            f2s(orderNotional / float64(L)),
		EstLiquidationPx:          f2s(liq),
		ResultingPositionSzi:      f2s(resultSzi),
		ResultingPositionNotional: f2s(absF(resultSzi) * px),
		Model:                     "est_liquidation_px is a single-position isolated estimate (mmf = 1/(2·tier max leverage)); a cross position's actual liq depends on the whole account — use positions.liquidation_px for an existing position",
	}
	if liq > 0 {
		res.EstDistanceToLiqPct = f2s(absF(px-liq) / px * 100)
	}
	// Resulting account leverage = gross book (incl. this order) / equity, the same
	// basis the risk gates use.
	if eq := equityOf(pf.AccountValue, pf.AvailableCollateral); eq > 0 {
		perCoin[mk.Coin] += sideSign * orderNotional
		gross := 0.0
		for _, v := range perCoin {
			gross += absF(v)
		}
		res.ResultingAccountLeverage = f2s(gross / eq)
	}
	return res, nil
}
