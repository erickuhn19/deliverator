package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
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

// FeeTokenTotal is the per-token breakdown of fees NOT charged in USDC (e.g. a
// spot buy's fee, charged in the base token). Amount is the raw token quantity.
// Converted=true means the USD value (amount × the live mid) IS included in
// trading_fees/net; false means no mid was available and the fee is EXCLUDED
// from the USD sums (reported here + warned) — a base-token quantity must never
// be summed as dollars at face value (NEXT-2 item 5).
type FeeTokenTotal struct {
	Token     string `json:"token"`
	Amount    string `json:"amount"`
	UsdValue  string `json:"usd_value,omitempty"`
	Mid       string `json:"mid,omitempty"`
	Converted bool   `json:"converted"`
}

// PnlAttributionView is the per-coin breakdown plus a totals row. One window
// (window_start_ms → now) governs ALL components — fills, fees, funding — so
// the composite is meaningful (NEXT-2 item 4); Window documents which window
// applied. FeeTokens itemizes non-USDC fees (see FeeTokenTotal); Truncated
// mirrors the envelope flag when the underlying paged reads hit the safety cap.
type PnlAttributionView struct {
	SinceMs       int64           `json:"since_ms,omitempty"` // set when an explicit --since was passed
	WindowStartMs int64           `json:"window_start_ms"`    // effective start applied to fills, fees, AND funding
	Window        string          `json:"window"`             // e.g. "utc-day (default)" or "explicit --since"
	ByCoin        []PnlRow        `json:"by_coin"`
	Totals        PnlRow          `json:"totals"`
	FeeTokens     []FeeTokenTotal `json:"fee_tokens,omitempty"`
	Truncated     bool            `json:"truncated,omitempty"`
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

// isUSDFeeToken reports whether a fill's fee is already USD(C)-denominated. A
// blank feeToken is the perp default (quote = USDC).
func isUSDFeeToken(tok string) bool {
	return tok == "" || strings.EqualFold(tok, "USDC")
}

// spotUSDMid resolves token's USD price from an allMids payload via its
// <token>/USDC spot pair (mids key either the pair name or the "@<index>"
// non-canonical form). The pair is resolved through SpotPairForToken — on
// mainnet nearly every pair except PURR/USDC has an "@<index>" universe name,
// so a plain "<TOKEN>/USDC" name lookup would miss a mid that IS available and
// wrongly exclude the fee. false when the token has no readable USDC mid.
func (c *Client) spotUSDMid(mids map[string]string, token string) (float64, bool) {
	if len(mids) == 0 {
		return 0, false
	}
	mk, ok := c.meta.SpotPairForToken(token)
	if !ok || !mk.IsSpot {
		return 0, false
	}
	if s, ok := mids[mk.Coin]; ok {
		if v := parseFloatSafe(s); v > 0 {
			return v, true
		}
	}
	if s, ok := mids["@"+strconv.Itoa(mk.AssetIndex-10000)]; ok {
		if v := parseFloatSafe(s); v > 0 {
			return v, true
		}
	}
	return 0, false
}

// PnlAttribution aggregates fills + funding into a per-coin/by-source P&L view.
// ONE window governs realized PnL, fees, AND funding identically (item 4):
// since=nil defaults to the current UTC day — a "session", matching the
// daily-loss gate's UTC-day anchor — never one window for fills and another for
// funding. coin: "" = all. Non-USDC fill fees (spot buys pay in the BASE token)
// are converted to USD via the live mid, or excluded + reported when no mid is
// available (item 5) — a token quantity is never summed as dollars.
func (c *Client) PnlAttribution(ctx context.Context, since *int64, coin string) (*PnlAttributionView, ReadMeta, error) {
	var rm ReadMeta
	if err := c.requireQueryAddr(); err != nil {
		return nil, rm, err
	}
	windowStart, window := utcMidnightMs(), "utc-day (default: since 00:00 UTC)"
	if since != nil {
		windowStart, window = *since, "explicit --since"
	}
	fills, frm, err := c.Fills(ctx, &windowStart, 0) // 0 = no local cap
	if err != nil {
		return nil, rm, err
	}
	rm.merge(frm)
	funding, urm, err := c.Funding(ctx, &windowStart)
	if err != nil {
		return nil, rm, err
	}
	rm.merge(urm)

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
	// Non-USDC fee tokens need a live mid to be valued; fetch allMids ONCE, and
	// only when such a fee exists in the window.
	var mids map[string]string
	for _, f := range fills {
		if !isUSDFeeToken(f.FeeToken) {
			if m, merr := c.info.AllMids(ctx); merr == nil {
				mids = m
			}
			break
		}
	}
	type feeTokAcc struct {
		amount, usd float64
		mid         float64
		converted   bool
	}
	feeToks := map[string]*feeTokAcc{}
	for _, f := range fills {
		if coin != "" && !matchesCoinFilter(f.Coin, coin) {
			continue
		}
		a := acc(f.Coin)
		a.realized += parseFloatSafe(f.ClosedPnl)
		// The trading fee AND the builder fee are both denominated in the fill's
		// single feeToken (there is no separate builder-fee token on the wire).
		fee := parseFloatSafe(f.Fee)
		builderFee := parseFloatSafe(f.BuilderFee)
		if isUSDFeeToken(f.FeeToken) {
			a.fees -= fee           // a cost; a maker rebate (negative fee) becomes a credit
			a.builder -= builderFee // builder cost, already USDC
			continue
		}
		// Fees charged in a non-USDC token (e.g. a spot buy's base-token fee):
		// value them at the live mid when readable, otherwise EXCLUDE from the
		// USD sums — face-value summing a token quantity as dollars is a money
		// bug (it hit builder_fees before this fix). Either way the per-token
		// breakdown reports the full trading+builder quantity.
		ft := feeToks[f.FeeToken]
		if ft == nil {
			ft = &feeTokAcc{}
			if mid, ok := c.spotUSDMid(mids, f.FeeToken); ok {
				ft.mid, ft.converted = mid, true
			}
			feeToks[f.FeeToken] = ft
		}
		ft.amount += fee + builderFee
		if ft.converted {
			feeUSD, builderUSD := fee*ft.mid, builderFee*ft.mid
			ft.usd += feeUSD + builderUSD
			a.fees -= feeUSD
			a.builder -= builderUSD
		}
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

	view := &PnlAttributionView{ByCoin: []PnlRow{}, WindowStartMs: windowStart, Window: window}
	if since != nil {
		view.SinceMs = *since
	}
	toks := make([]string, 0, len(feeToks))
	for tok := range feeToks {
		toks = append(toks, tok)
	}
	sort.Strings(toks)
	for _, tok := range toks {
		ft := feeToks[tok]
		row := FeeTokenTotal{Token: tok, Amount: usd(ft.amount), Converted: ft.converted}
		if ft.converted {
			row.UsdValue = usd(ft.usd)
			row.Mid = f2s(ft.mid)
		} else {
			rm.Notes = append(rm.Notes, fmt.Sprintf(
				"fees of %s %s could not be valued in USD (no %s/USDC mid) — EXCLUDED from trading_fees and net_session_pnl; see fee_tokens",
				usd(ft.amount), tok, tok))
		}
		view.FeeTokens = append(view.FeeTokens, row)
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
	view.Truncated = rm.Truncated
	return view, rm, nil
}
