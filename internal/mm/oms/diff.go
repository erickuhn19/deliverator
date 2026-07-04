// Package oms is the order-management layer for the outcome market maker: it turns a
// desired quote set into the minimal place/modify/cancel actions against the resting
// book (diff.go, pure), adapts core reads into typed live state (state.go), streams
// underlying marks + realized vol for the fair-value model (feed.go), and accounts
// fill/settlement PnL (pnl.go). It sits between the strategy (what to quote) and
// core.Client (how to sign) — reusing every core guardrail, re-implementing none.
package oms

import (
	"sort"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// RestingOrder is one live resting outcome order, as the diff needs it. RemainingSz
// is the UNFILLED share count (hl.FrontendOpenOrder.Sz), never OrigSz — re-pricing
// must target the remaining size so a partially-filled quote is not silently
// re-grown (the ModifyReq empty-Size=OrigSz footgun).
type RestingOrder struct {
	Oid         int64
	Cloid       string
	Coin        string
	Side        core.Side
	Px          float64
	RemainingSz int64
}

// ModifyAction re-prices/re-sizes one resting order in place (one leg of a
// modify-batch). NewSz is always set explicitly, so ModifyReq.Size is never empty.
type ModifyAction struct {
	Oid   int64
	Cloid string
	Coin  string
	Side  core.Side
	NewPx float64
	NewSz int64
}

// DiffResult is the minimal set of writes to move the resting book to the desired
// quotes: new places, in-place re-prices, and cancels of now-unwanted rungs.
type DiffResult struct {
	Place  []mm.Quote
	Modify []ModifyAction
	Cancel []RestingOrder
}

// Empty reports whether the diff requires no writes at all — the engine skips the
// signed action entirely (saving a rate-cap charge) when the book already matches.
func (d DiffResult) Empty() bool {
	return len(d.Place) == 0 && len(d.Modify) == 0 && len(d.Cancel) == 0
}

// Diff computes the actions to reconcile resting orders on ONE coin to the desired
// quote set. It matches by side and price rank (the i-th best desired bid pairs with
// the i-th best resting bid), so a routine re-price stays in place as a modify rather
// than a churny cancel+replace. pxTol is the price tolerance under which a rung is
// left untouched (avoid re-quoting on sub-tick noise) — typically half a 5-dp tick.
//
// It is PURE: no I/O, no clock, deterministic in its inputs. The engine turns the
// result into one PlaceBatch + one ModifyBatch + one Cancel (each a single signed
// action) to stay inside the rate budget.
func Diff(desired mm.QuoteSet, resting []RestingOrder, pxTol float64) DiffResult {
	var res DiffResult

	dBuy, dSell := splitQuotes(desired.Quotes)
	rBuy, rSell := splitResting(resting)

	res.append(matchSide(dBuy, rBuy, pxTol))
	res.append(matchSide(dSell, rSell, pxTol))
	return res
}

func (d *DiffResult) append(o DiffResult) {
	d.Place = append(d.Place, o.Place...)
	d.Modify = append(d.Modify, o.Modify...)
	d.Cancel = append(d.Cancel, o.Cancel...)
}

// matchSide zips one side's desired rungs (best-first) against its resting orders
// (best-first): pair i↔i, place the desired tail, cancel the resting tail.
func matchSide(desired []mm.Quote, resting []RestingOrder, pxTol float64) DiffResult {
	var res DiffResult
	n := len(desired)
	if len(resting) < n {
		n = len(resting)
	}
	for i := 0; i < n; i++ {
		d, r := desired[i], resting[i]
		if absf(d.Px-r.Px) <= pxTol && d.Sz == r.RemainingSz {
			continue // already where we want it — leave it resting (no rate charge)
		}
		res.Modify = append(res.Modify, ModifyAction{
			Oid: r.Oid, Cloid: r.Cloid, Coin: r.Coin, Side: r.Side, NewPx: d.Px, NewSz: d.Sz,
		})
	}
	for i := n; i < len(desired); i++ {
		res.Place = append(res.Place, desired[i])
	}
	for i := n; i < len(resting); i++ {
		res.Cancel = append(res.Cancel, resting[i])
	}
	return res
}

// splitQuotes partitions a quote set into buys (best = highest px first) and sells
// (best = lowest px first).
func splitQuotes(qs []mm.Quote) (buys, sells []mm.Quote) {
	for _, q := range qs {
		if q.Side == core.Buy {
			buys = append(buys, q)
		} else {
			sells = append(sells, q)
		}
	}
	sort.SliceStable(buys, func(i, j int) bool { return buys[i].Px > buys[j].Px })
	sort.SliceStable(sells, func(i, j int) bool { return sells[i].Px < sells[j].Px })
	return buys, sells
}

func splitResting(rs []RestingOrder) (buys, sells []RestingOrder) {
	for _, r := range rs {
		if r.Side == core.Buy {
			buys = append(buys, r)
		} else {
			sells = append(sells, r)
		}
	}
	sort.SliceStable(buys, func(i, j int) bool { return buys[i].Px > buys[j].Px })
	sort.SliceStable(sells, func(i, j int) bool { return sells[i].Px < sells[j].Px })
	return buys, sells
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
