package core

import (
	"context"
	"math"
	"sort"
)

// usd rounds a summed float to 8 decimals, clearing float-accumulation noise
// (e.g. -0.07320900000000001 -> -0.073209) while keeping full USDC precision.
func usd(f float64) string { return f2s(math.Round(f*1e8) / 1e8) }

// PnlAttribution (#47) nets a trustworthy session P&L from data that already
// exists but is never aggregated — realized PnL on closing fills, trading fees,
// builder fees, and funding payments — broken down per coin and by source, so an
// agent/operator can answer "did this make money, and where did it go".
//
// net_session_pnl = realized_pnl + trading_fees + builder_fees + funding_delta
// where trading_fees / builder_fees are stored SIGNED as a cost (negative; a maker
// rebate shows positive) and funding_delta is the signed USDC delta (+ received,
// − paid). So net is a simple sum.
//
// Note on builder_fees: this is the fee the trading account paid to the builder on
// each fill. If you are your own builder (self-builder), it is a wash overall — a
// cost to this account, revenue to your builder EOA — but from THIS account's P&L
// it is a cost, so it is netted out here.

// PnlRow is one coin's (or the total) attribution. Values are strings (USDC).
type PnlRow struct {
	Coin          string `json:"coin"`
	RealizedPnl   string `json:"realized_pnl"`    // Σ closedPnl
	TradingFees   string `json:"trading_fees"`    // −Σ fee (cost; + for a net maker rebate)
	BuilderFees   string `json:"builder_fees"`    // −Σ builderFee (cost)
	FundingDelta  string `json:"funding_delta"`   // Σ funding usdc (signed)
	NetSessionPnl string `json:"net_session_pnl"` // sum of the four
}

// PnlAttributionView is the per-coin breakdown plus a totals row.
type PnlAttributionView struct {
	SinceMs int64    `json:"since_ms,omitempty"`
	ByCoin  []PnlRow `json:"by_coin"`
	Totals  PnlRow   `json:"totals"`
}

// pnlAcc accumulates the four signed components for one coin.
type pnlAcc struct{ realized, fees, builder, funding float64 }

func (a pnlAcc) row(coin string) PnlRow {
	net := a.realized + a.fees + a.builder + a.funding
	return PnlRow{
		Coin: coin, RealizedPnl: usd(a.realized), TradingFees: usd(a.fees),
		BuilderFees: usd(a.builder), FundingDelta: usd(a.funding), NetSessionPnl: usd(net),
	}
}

// PnlAttribution aggregates fills + funding into a per-coin/by-source P&L view.
// since: nil/0 = the full available window (fills are HL-capped); coin: "" = all.
func (c *Client) PnlAttribution(ctx context.Context, since *int64, coin string) (*PnlAttributionView, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	fills, err := c.Fills(ctx, since, 0) // 0 = no local cap
	if err != nil {
		return nil, err
	}
	funding, err := c.Funding(ctx, since)
	if err != nil {
		return nil, err
	}

	per := map[string]*pnlAcc{}
	acc := func(coin string) *pnlAcc {
		key := bareCoin(coin)
		a := per[key]
		if a == nil {
			a = &pnlAcc{}
			per[key] = a
		}
		return a
	}
	for _, f := range fills {
		if coin != "" && !matchesCoinFilter(f.Coin, coin) {
			continue
		}
		a := acc(f.Coin)
		a.realized += parseFloatSafe(f.ClosedPnl)
		a.fees -= parseFloatSafe(f.Fee) // a cost; a maker rebate (negative fee) becomes a credit
		a.builder -= parseFloatSafe(f.BuilderFee)
	}
	for _, fh := range funding {
		if coin != "" && !matchesCoinFilter(fh.Delta.Coin, coin) {
			continue
		}
		acc(fh.Delta.Coin).funding += parseFloatSafe(fh.Delta.USDC)
	}

	coins := make([]string, 0, len(per))
	for k := range per {
		coins = append(coins, k)
	}
	sort.Strings(coins)

	view := &PnlAttributionView{ByCoin: []PnlRow{}}
	if since != nil {
		view.SinceMs = *since
	}
	var total pnlAcc
	for _, k := range coins {
		a := per[k]
		view.ByCoin = append(view.ByCoin, a.row(k))
		total.realized += a.realized
		total.fees += a.fees
		total.builder += a.builder
		total.funding += a.funding
	}
	view.Totals = total.row("*TOTAL*")
	return view, nil
}
