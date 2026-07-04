// Package arb is the cross-book YES/NO near-riskless arbitrage scanner for HIP-4
// outcome markets. It is a PURE leaf: deterministic, no I/O, no goroutines, no
// clock — every input arrives as an argument so the engine (and tests) fully
// control it. It depends only on internal/mm (shared vocabulary) and internal/core.
//
// The economics it exploits: exactly one of {YES, NO} on a given outcome resolves
// to $1 and the other to $0. So one YES share + one NO share held to settlement is
// worth EXACTLY $1, no matter which way the market resolves. If we can BUY (lift
// the ask on) both legs for a combined price strictly below $1 — with enough margin
// to cover the settle fee — the pair is a locked-in profit that carries no
// directional/underlying risk. The only residual risks are execution (both legs
// must actually fill) and settlement mechanics; hence "near-riskless", not
// "riskless". This scanner sizes and gates the BUY-both-under-1 capture; the
// symmetric SELL-both-over-1 arb is deferred (see ScanCrossBook doc).
package arb

import (
	"math"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// defaultMinNotionalUSD is the fallback per-leg notional floor when Params leaves
// MinNotionalUSD at its zero value. HL rejects dust orders, and a leg worth a few
// cents is not worth the settle-fee + execution risk, so we default to $10 to match
// the venue's practical minimum rather than silently emitting an un-fillable leg.
const defaultMinNotionalUSD = 10.0

// Params configures the arb scan. All fields are in probability units unless the
// name says USD; probability units and $1-payout fractions are interchangeable here
// because a share pays exactly $1, so an edge of 0.005 in probability == $0.005 of
// locked profit per pair of shares.
type Params struct {
	// MinEdge is the required profit per pair AFTER the settle-fee estimate, in
	// probability units (e.g. 0.005 = half a cent per $1 pair). Captures whose edge
	// does not strictly clear this bar are rejected — this is the knob that keeps us
	// from chasing sub-tick "arbs" that execution slippage would erase.
	MinEdge float64
	// MaxSizeShares caps shares per capture so one signal can't drain the whole book
	// or blow past position limits; the engine owns the broader inventory budget.
	MaxSizeShares int64
	// SettleFeeFrac is the estimated fee charged on settle/burn, as a fraction of the
	// $1 payout (0 = none). Fees on HIP-4 are charged only on CLOSE/settle, never on
	// open, so this is the ONLY cost that erodes the buy-both edge and it must be
	// subtracted before we compare against MinEdge.
	SettleFeeFrac float64
	// MinNotionalUSD is the minimum px*shares each leg must clear (default $10 via
	// defaultMinNotionalUSD). Both legs must independently satisfy it — a cheap leg
	// (tiny p) needs more shares to clear the floor than an expensive one.
	MinNotionalUSD float64
}

// minNotional returns the effective per-leg USD floor, applying the default when the
// caller left MinNotionalUSD unset (<=0). Kept as a helper so the gate reads clearly.
func (p Params) minNotional() float64 {
	if p.MinNotionalUSD <= 0 {
		return defaultMinNotionalUSD
	}
	return p.MinNotionalUSD
}

// Opportunity is a sized, gated buy-both-under-1 capture ready for the OMS to lift.
// YesPx/NoPx are the ASK prices we would pay on each leg; Shares is the integer
// count to buy on BOTH legs (equal, since one YES + one NO = the $1 pair).
type Opportunity struct {
	YesCoin, NoCoin string
	YesPx, NoPx     float64 // the ask prices we would lift on each leg
	Shares          int64
	// EdgePerShare is the locked profit per pair after costs: 1 - YesPx - NoPx -
	// SettleFeeFrac. Total expected capture is EdgePerShare * Shares (USD).
	EdgePerShare float64
}

// ScanCrossBook evaluates the BUY-both-under-1 arbitrage on one outcome market given
// the top-of-book for its YES and NO coins, and returns a sized Opportunity plus
// true when a capture clears every gate. It returns (zero, false) otherwise.
//
// It is PURE: the market, both book tops, and the params are all inputs; there is no
// clock, randomness, or I/O, so the same arguments always yield the same result.
//
// Why only buy-both here: buying one YES + one NO for a combined ask below $1 needs
// no pre-existing position — we can always open both legs — so the capture is
// self-funding and available to a flat book. The mirror trade (SELL one YES + one
// NO when their combined BID exceeds $1) is equally riskless in theory but requires
// ALREADY HOLDING both shares to deliver (there is no naked-short on outcome coins),
// so it is inventory-dependent and belongs to the engine's inventory-unwind path.
// That case is intentionally DEFERRED; v1 scans the always-available side only.
func ScanCrossBook(m core.Market, yes mm.BookTop, no mm.BookTop, p Params) (Opportunity, bool) {
	// Both legs must have a real ask to lift; without an offer on either side there
	// is nothing to buy and no arb to size. HasAsk (not Ask>0) is the correct guard
	// because a genuine 0-side is distinguished from "no level" by the flag.
	if !yes.HasAsk || !no.HasAsk {
		return Opportunity{}, false
	}

	// Edge per pair = $1 payout minus what we pay for the two legs minus the settle
	// fee. This is the whole thesis: if 1 - yesAsk - noAsk exceeds the fee by at
	// least MinEdge, the pair locks in profit regardless of resolution.
	edge := 1 - yes.Ask - no.Ask - p.SettleFeeFrac
	if edge < p.MinEdge {
		return Opportunity{}, false
	}

	// Size to the shares actually available at the touch on the THINNER leg, floored
	// to integers (outcome szDecimals is 0 — you cannot buy a fractional share), then
	// capped by MaxSizeShares. Flooring before the min keeps the pair balanced: we
	// buy the SAME integer count on both legs so every share is a matched $1 pair.
	shares := int64(math.Floor(yes.AskSz))
	if n := int64(math.Floor(no.AskSz)); n < shares {
		shares = n
	}
	if p.MaxSizeShares > 0 && p.MaxSizeShares < shares {
		shares = p.MaxSizeShares
	}
	if shares <= 0 {
		// No liftable size on at least one leg (or a non-positive cap) ⇒ nothing to do.
		return Opportunity{}, false
	}

	// Each leg must independently clear the notional floor. Notional at risk for a
	// leg is price*shares (the USDC we actually pay). The cheaper leg has the smaller
	// notional at a given share count, so BOTH must pass — we don't want to open a
	// dust leg the venue rejects, which would leave us un-hedged on the filled leg.
	floor := p.minNotional()
	if yes.Ask*float64(shares) < floor || no.Ask*float64(shares) < floor {
		return Opportunity{}, false
	}

	return Opportunity{
		YesCoin:      mm.YesCoin(m.Outcome),
		NoCoin:       mm.NoCoin(m.Outcome),
		YesPx:        yes.Ask,
		NoPx:         no.Ask,
		Shares:       shares,
		EdgePerShare: edge,
	}, true
}
