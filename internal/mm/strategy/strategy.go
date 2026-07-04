// Package strategy is the PURE quoting brain of the outcome market maker: given a
// fair Yes probability, the current top-of-book, and net inventory, it builds the
// desired two-sided ladder of resting limit orders (a mm.QuoteSet) for one coin.
//
// "Pure" is load-bearing here: no clock, no network, no goroutines, no globals —
// the same inputs always produce the same QuoteSet. That is what lets the engine
// unit-test its risk/quoting behavior deterministically and lets us reason about
// self-arb (never crossing our own quotes, never posting a Yes/No pair that sums
// past $1) as a property of a single function rather than of live market timing.
//
// It depends only on internal/mm (shared value types) which depends only on core;
// it does NOT import config — the engine translates config.MMStrategy into the
// local Params struct so this leaf stays a pure, side-effect-free computation.
package strategy

import (
	"math"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// Params is the strategy's tunable surface. It is a LOCAL mirror of the relevant
// config.MMStrategy / risk fields rather than an import of config: keeping the leaf
// config-free is what preserves the mm→core-only import graph and keeps this package
// unit-testable without wiring a whole config tree. The engine populates it.
type Params struct {
	BaseSpread         float64 // half-spread in probability units (e.g. 0.02 = ±2¢ around reservation)
	LevelStep          float64 // probability gap added per ladder rung as you step outward from the touch
	Levels             int     // rungs per side (0 ⇒ no quotes)
	BaseSizeShares     int64   // integer shares at rung 0 (the touch)
	SizeStepShares     int64   // shares added per rung outward (deeper rungs quote larger, classic PMM)
	InventorySkew      float64 // lean coefficient (≥0); skews the reservation price AGAINST net inventory
	MaxInventoryShares int64   // per-market soft cap; at/past it we quote ONLY the reducing side
	MinEdge            float64 // a rung must sit ≥ this far from fairP or it is dropped (don't quote on top of fair)
	MinNotionalUSD     float64 // risk.min_order_notional_usd (default 10); bump share count so px*sz ≥ this
	SpreadMult         float64 // engine widens spread under low confidence/staleness; ≤0 is treated as 1.0
	MidAnchorWeight    float64 // 0..1: blend the anchor toward the live book mid (0 ⇒ pure model fair, 1 ⇒ pure mid)
	HeldShares         int64   // shares of THIS coin currently held; caps total ask size (no naked short on HIP-4 outcomes)
}

// dedupeEpsilon is one 5-dp outcome tick. Two candidate prices that round to the
// same tick are the same order to the exchange, so we collapse them (this is what
// stops a near-0/near-1 clamp from emitting several identical rungs). It is also the
// safety margin CapDualBook leaves below $1 so a shaved Yes/No pair never rounds
// back up to a self-arb at the tick.
const dedupeEpsilon = 1.0 / 1e5 // == 10^-OutcomeDecimals

// effectiveMult resolves the confidence/staleness spread multiplier. The engine
// passes >1 to widen when the fair estimate is stale or low-confidence; a zero value
// (unset Params) or any non-positive value means "no widening" — never a zero or
// negative spread, which would invert the ladder.
func effectiveMult(m float64) float64 {
	if m <= 0 {
		return 1.0
	}
	return m
}

// BuildQuoteSet builds the desired resting ladder for one outcome coin.
//
// The core idea is Avellaneda-style inventory skew: instead of quoting symmetrically
// around the fair probability, we quote around a RESERVATION price r that leans away
// from our current net exposure. Long net-Yes ⇒ r sits below fair ⇒ our bids get
// cheaper and asks get more aggressive ⇒ we preferentially SELL and mean-revert the
// book toward flat. Short net-Yes ⇒ the mirror. At the hard inventory cap we stop
// quoting the accumulating side entirely and only post the reducing side.
//
// book is the live top-of-book: quotes are clamped to be maker-valid against it (a
// post-only order that crosses is venue-rejected). With an EMPTY book the clamp is a
// no-op, so the ladder stays a deterministic function of fairP + inventory (tests
// rely on this).
func BuildQuoteSet(coin string, fairP float64, book mm.BookTop, netInv int64, p Params) mm.QuoteSet {
	out := mm.QuoteSet{Coin: coin}
	if p.Levels <= 0 {
		return out
	}

	// Anchor point for quoting. On liquid outcome books the model fair can diverge from
	// the market (e.g. a noisy realized-vol estimate on a deep-ITM digital), which makes
	// the MM lean one-directional and accumulate inventory instead of earning spread.
	// Blend the fair toward the live book mid so quotes straddle the actual market
	// (symmetric spread capture); the model still tilts it by (1−weight). With no usable
	// two-sided book (wide/illiquid market) we fall back to the pure model fair, which is
	// the only signal there. This is the robust fix — it holds regardless of vol noise.
	anchor := fairP
	if p.MidAnchorWeight > 0 && book.Bid > 0 && book.Ask > 0 && book.Ask > book.Bid {
		w := p.MidAnchorWeight
		if w > 1 {
			w = 1
		}
		anchor = mm.ClampProb(w*((book.Bid+book.Ask)/2) + (1-w)*fairP)
	}

	// Reservation price. netInv/max is a signed fill fraction of the cap; scaling it
	// by InventorySkew·BaseSpread turns "how full am I" into "how many half-spreads to
	// lean". max(...,1) guards a zero/negative cap from dividing by zero. Long (net>0)
	// pushes r below the anchor; short pushes it above. Clamp so r itself stays tradable.
	capShares := p.MaxInventoryShares
	if capShares < 1 {
		capShares = 1
	}
	fill := float64(netInv) / float64(capShares)
	r := mm.ClampProb(anchor - p.InventorySkew*fill*p.BaseSpread)

	half := p.BaseSpread * effectiveMult(p.SpreadMult)

	// Inventory cap gates which sides we quote at all. At/over the long cap we must
	// only REDUCE, i.e. sell Yes, so we suppress bids; at/over the short cap we suppress
	// asks. Strictly < cap on both sides ⇒ quote both. A non-positive cap means "no
	// inventory cap" — quote both sides regardless (else max_inventory_shares=0 would
	// suppress everything at flat inventory and the MM would silently never quote).
	noCap := p.MaxInventoryShares <= 0
	quoteBids := noCap || netInv < p.MaxInventoryShares
	quoteAsks := noCap || netInv > -p.MaxInventoryShares

	// used tracks tick-rounded prices already emitted so a boundary clamp (many raw
	// rungs collapsing onto OutcomeMin/MaxPrice near a degenerate fair) yields at most
	// one order per price rather than a stack of duplicates.
	used := map[int64]bool{}

	var bids, asks []mm.Quote
	if quoteBids {
		bids = p.buildSide(coin, anchor, r, half, core.Buy, used)
	}
	if quoteAsks {
		asks = p.buildSide(coin, anchor, r, half, core.Sell, used)
	}

	// Book-aware maker clamp: a post-only (Alo) quote that would CROSS the live book is
	// rejected by the venue. Pull any bid at/above the best ask down to one tick below
	// it, and any ask at/below the best bid up to one tick above it. This keeps every
	// quote resting AND lets a fair that disagrees with the market express itself — a
	// fair below the market clamps our asks to the touch, so we lean to SELL there.
	bids = clampMaker(bids, book)
	asks = clampMaker(asks, book)

	// Re-bump size after the clamp: the clamp can shift a rung's price, so re-assert
	// px·sz ≥ MinNotionalUSD at the FINAL price, else a clamped rung can slip under the
	// venue/risk minimum and be rejected.
	bids = bumpNotional(bids, p.MinNotionalUSD)
	asks = bumpNotional(asks, p.MinNotionalUSD)

	// No naked short: on HIP-4 you can only SELL a coin you hold. Trim the ask ladder so
	// its total size never exceeds HeldShares (a rung that can't meet the min notional
	// with what's left is dropped). To go "short YES" while flat the engine instead quotes
	// the sibling NO coin — buying NO is the economically-equivalent, venue-legal move.
	asks = capAsksToHeld(asks, p.HeldShares, p.MinNotionalUSD)

	// Never cross our own book: drop any bid priced at/above the cheapest ask (and any
	// ask at/below the richest bid). In the normal regime bids live below r−half and
	// asks above r+half so nothing crosses; this only bites when clamping near a price
	// boundary folds a bid up onto an ask (or vice versa). We resolve bids-vs-asks first,
	// then re-derive the surviving richest bid to trim asks, so the result is coherent.
	minAsk := minPx(asks)
	bids = keepBelow(bids, minAsk)
	maxBid := maxPx(bids)
	asks = keepAbove(asks, maxBid)

	out.Quotes = append(out.Quotes, bids...)
	out.Quotes = append(out.Quotes, asks...)
	return out
}

// buildSide builds one side's rungs (bids or asks) around reservation price r.
// side selects direction: Buy steps prices DOWN from r−half, Sell steps UP from
// r+half. Each rung is clamped into the tradable band, deduped by tick, notional-
// bumped, and dropped if it sits inside MinEdge of fair.
func (p Params) buildSide(coin string, anchor, r, half float64, side core.Side, used map[int64]bool) []mm.Quote {
	quotes := make([]mm.Quote, 0, p.Levels)
	for L := 0; L < p.Levels; L++ {
		step := half + float64(L)*p.LevelStep
		var raw float64
		if side == core.Buy {
			raw = r - step
		} else {
			raw = r + step
		}
		px := mm.ClampProb(raw)

		// MinEdge: refuse to quote on top of the anchor. |px−anchor| < MinEdge means the
		// rung earns no edge (or negative after fees) so it is dropped, not clamped —
		// dropping is safer than nudging it to exactly MinEdge and mispricing.
		if math.Abs(px-anchor) < p.MinEdge {
			continue
		}

		// Dedupe by tick. Clamping can map several rungs to the same boundary price;
		// only the first survives so we don't stack identical resting orders.
		key := priceKey(px)
		if used[key] {
			continue
		}

		// Size grows outward (deeper = larger), then is bumped up so the leg clears the
		// venue/risk minimum notional px·sz ≥ MinNotionalUSD. Because px ≥ OutcomeMinPrice
		// after clamping, the ceil division can't divide by zero.
		sz := p.BaseSizeShares + int64(L)*p.SizeStepShares
		if p.MinNotionalUSD > 0 {
			need := int64(math.Ceil(p.MinNotionalUSD / px))
			if need > sz {
				sz = need
			}
		}
		if sz <= 0 {
			continue // a zero/negative base size yields no order rather than a bogus one
		}

		used[key] = true
		quotes = append(quotes, mm.Quote{
			Coin:  coin,
			Side:  side,
			Px:    px,
			Sz:    sz,
			Level: L,
		})
	}
	return quotes
}

// bumpNotional raises each quote's size so px·sz ≥ minNotionalUSD at its current
// price (a no-op when the floor is unset). ceil guarantees the product clears the
// floor; px is post-clamp so it is always ≥ OutcomeMinPrice (no divide-by-zero).
func bumpNotional(quotes []mm.Quote, minNotionalUSD float64) []mm.Quote {
	if minNotionalUSD <= 0 {
		return quotes
	}
	for i := range quotes {
		if px := quotes[i].Px; px > 0 && float64(quotes[i].Sz)*px < minNotionalUSD {
			quotes[i].Sz = int64(math.Ceil(minNotionalUSD / px))
		}
	}
	return quotes
}

// capAsksToHeld trims an ask ladder so its cumulative size ≤ held (a HIP-4 outcome sell
// can only deliver shares you actually own — there is no naked short). Rungs are kept
// touch-outward; the boundary rung is shrunk to the remainder, and any rung that would
// fall below the min notional after shrinking is dropped (a sub-min order is rejected by
// the venue). held ≤ 0 ⇒ no asks at all. A non-positive min notional skips the min check.
func capAsksToHeld(asks []mm.Quote, held int64, minNotionalUSD float64) []mm.Quote {
	if held <= 0 {
		return nil
	}
	out := asks[:0]
	remaining := held
	for _, q := range asks {
		if remaining <= 0 {
			break
		}
		if q.Sz > remaining {
			q.Sz = remaining
		}
		if minNotionalUSD > 0 && float64(q.Sz)*q.Px < minNotionalUSD {
			break // can't meet the venue minimum with the shares left to sell
		}
		out = append(out, q)
		remaining -= q.Sz
	}
	return out
}

// clampMaker pulls any quote that would cross the live top-of-book to one tick inside
// the touch (a post-only order that crosses is venue-rejected), then re-dedupes since
// the clamp can collapse several rungs onto the same price. An absent book side leaves
// that side's quotes untouched.
func clampMaker(quotes []mm.Quote, book mm.BookTop) []mm.Quote {
	seen := make(map[int64]bool, len(quotes))
	out := make([]mm.Quote, 0, len(quotes))
	for _, q := range quotes {
		px := q.Px
		switch {
		case q.Side == core.Buy && book.HasAsk && px >= book.Ask-dedupeEpsilon/2:
			px = mm.ClampProb(book.Ask - dedupeEpsilon)
		case q.Side == core.Sell && book.HasBid && px <= book.Bid+dedupeEpsilon/2:
			px = mm.ClampProb(book.Bid + dedupeEpsilon)
		}
		key := priceKey(px)
		if seen[key] {
			continue
		}
		seen[key] = true
		q.Px = px
		out = append(out, q)
	}
	return out
}

// priceKey maps a price to its 5-dp tick bucket for dedupe. Rounding (not truncation)
// matches how ProbString/core snap prices to the tick, so two prices that would sign
// as the same limit order share a key.
func priceKey(px float64) int64 { return int64(math.Round(px / dedupeEpsilon)) }

// minPx / maxPx return the extreme price across a rung slice; the sentinels (+Inf /
// −Inf) make an empty side impose no crossing constraint on the other.
func minPx(qs []mm.Quote) float64 {
	m := math.Inf(1)
	for _, q := range qs {
		if q.Px < m {
			m = q.Px
		}
	}
	return m
}

func maxPx(qs []mm.Quote) float64 {
	m := math.Inf(-1)
	for _, q := range qs {
		if q.Px > m {
			m = q.Px
		}
	}
	return m
}

// keepBelow drops bids priced at/above limit (the cheapest ask); keepAbove drops
// asks priced at/below limit (the richest bid). Strict crossing (== included) is
// treated as a cross because two orders at the same price on opposite sides would
// self-trade.
func keepBelow(qs []mm.Quote, limit float64) []mm.Quote {
	out := qs[:0:0]
	for _, q := range qs {
		if q.Px < limit {
			out = append(out, q)
		}
	}
	return out
}

func keepAbove(qs []mm.Quote, limit float64) []mm.Quote {
	out := qs[:0:0]
	for _, q := range qs {
		if q.Px > limit {
			out = append(out, q)
		}
	}
	return out
}

// ---- dual-book no-arb helpers ----
//
// A YES share pays $1 iff YES resolves; a NO share pays $1 iff NO resolves; exactly
// one happens. So anyone who can BUY a YES at yesBid and a NO at noBid for a combined
// yesBid+noBid < 1 locks in a riskless profit at settlement. If WE are the resting
// bids on both legs, a taker hitting both of ours arbitrages US. These helpers keep
// our own paired quotes coherent so we never post that free money.

// CoherentDualBook reports whether our own Yes-bid and No-bid are self-arb-free: the
// combined cost to lift both must not exceed the $1 guaranteed payout. Equality (==1)
// is allowed — it is a zero-edge round-trip, not a loss.
func CoherentDualBook(yesBid, noBid float64) bool {
	return yesBid+noBid <= 1.0
}

// CapDualBook returns a coherent (yesBid, noBid) pair. If the two already sum to ≤ 1
// they pass through unchanged; if they sum past $1 both are shaved PROPORTIONALLY so
// the pair sums to 1−epsilon (one tick under parity, so a subsequent tick-rounding
// can't push it back to a self-arb). Proportional shaving preserves the relative
// Yes/No lean instead of arbitrarily gutting one leg.
func CapDualBook(yesBid, noBid float64) (float64, float64) {
	sum := yesBid + noBid
	if sum <= 1.0 {
		return yesBid, noBid
	}
	if sum <= 0 { // degenerate guard; both non-positive can't sum past 1, but be safe
		return yesBid, noBid
	}
	scale := (1.0 - dedupeEpsilon) / sum
	return yesBid * scale, noBid * scale
}
