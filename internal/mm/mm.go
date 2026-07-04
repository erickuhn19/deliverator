// Package mm holds the shared contract types for the HIP-4 outcome market maker
// (the outcome-mm binary). Every leaf package (fairvalue, selector, strategy, arb,
// settle, oms, engine) depends on THIS package for its value types and interfaces;
// this package depends only on internal/core. Keeping the shared vocabulary here —
// not in engine — is what prevents import cycles: leaves import mm, mm imports core,
// engine imports leaves.
package mm

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
)

// Outcome price band + tick, mirrored from the authoritative core/hl constants
// (hl.outcomeMinPrice/outcomeMaxPrice, core.MaxDecimalsOutcome). The LIMIT order
// path (core.RoundOutcomePrice) REJECTS out-of-band prices rather than clamping, so
// the MM must clamp its own quotes into this band before it signs — see ProbString.
const (
	OutcomeMinPrice = 0.00001
	OutcomeMaxPrice = 0.99999
	OutcomeDecimals = 5 // core.MaxDecimalsOutcome — fixed 5-dp tick for every outcome market

	// ExpiryLayout is the format core emits for Market.Expiry ("YYYY-MM-DD HH:MMZ",
	// UTC, minute resolution — see core.formatOutcomeExpiry).
	ExpiryLayout = "2006-01-02 15:04Z"

	// yearSeconds is the seconds in a 365-day year, for annualizing time-to-expiry
	// τ so it composes with an annualized vol σ in the Black-Scholes-digital model.
	yearSeconds = 365.0 * 24.0 * 60.0 * 60.0
)

// Fair is a fair-value estimate for one outcome market's Yes probability.
type Fair struct {
	P          float64   // fair Yes probability in (0,1)
	Conf       float64   // confidence in [0,1]; stale/low-conf ⇒ widen or pull
	ValidUntil time.Time // the estimate must not be used past this
}

// Valid reports whether f is usable at now: in-band probability, positive
// confidence, and not past ValidUntil (a zero ValidUntil means "no expiry set").
func (f Fair) Valid(now time.Time) bool {
	if f.P <= 0 || f.P >= 1 || f.Conf <= 0 {
		return false
	}
	if !f.ValidUntil.IsZero() && now.After(f.ValidUntil) {
		return false
	}
	return true
}

// FairValuer estimates the fair Yes probability of an outcome market. v1 is the
// deterministic PriceBinaryModel (Black-Scholes-digital); v2 adds an LLMAnalyst
// behind this same interface, so the quoting engine is model-agnostic.
type FairValuer interface {
	Estimate(ctx context.Context, m core.Market) (Fair, error)
}

// UnderlyingFeed supplies the live inputs the PriceBinaryModel needs that the
// Market itself does not carry: the underlying's mark price and its annualized
// realized volatility. The engine/oms maintains these off the mids/candles streams
// and implements this interface; the model reads from it.
type UnderlyingFeed interface {
	// Mark returns the current mark/mid price of the underlying (e.g. "BTC"), and
	// false when no fresh mark is available (⇒ the model must not price).
	Mark(underlying string) (px float64, ok bool)
	// Vol returns the annualized realized volatility (e.g. 0.6 = 60%/yr) of the
	// underlying, and false when there aren't enough samples yet.
	Vol(underlying string) (sigmaAnnual float64, ok bool)
}

// Quote is one desired resting limit order on an outcome coin. Px is a probability
// already clamped into the tradable band; Sz is integer shares (outcome szDecimals
// is 0). Level is the ladder rung (0 = touch) for diagnostics/rendering.
type Quote struct {
	Coin  string
	Side  core.Side
	Px    float64
	Sz    int64
	Level int
}

// QuoteSet is the full desired quote set for one market this tick — typically a
// two-sided ladder across the Yes book (bids buy Yes, asks sell Yes) or a Yes/No
// pair. The engine diffs this against resting orders to compute place/modify/cancel.
type QuoteSet struct {
	Coin   string
	Quotes []Quote
}

// Inventory is the current holding of a market's two legs, in integer shares. Both
// are non-negative (an outcome holding is a token balance; you cannot be short a
// share — a synthetic short of Yes is a long of No).
type Inventory struct {
	Yes int64
	No  int64
}

// Net returns the signed Yes-equivalent inventory (Yes − No shares), the directional
// exposure the inventory-skew strategy leans against.
func (inv Inventory) Net() int64 { return inv.Yes - inv.No }

// BookTop is the top-of-book for one coin, in probability units. HasBid/HasAsk
// distinguish a real 0-side from "no level".
type BookTop struct {
	Bid, Ask       float64
	BidSz, AskSz   float64
	HasBid, HasAsk bool
}

// Mid returns the book mid when both sides are present, else false.
func (b BookTop) Mid() (float64, bool) {
	if b.HasBid && b.HasAsk {
		return (b.Bid + b.Ask) / 2, true
	}
	return 0, false
}

// ---- pure helpers shared across packages ----

// Chunk splits xs into consecutive slices of at most n elements (n>0). Used to keep
// a batch of orders/modifies under the exchange's per-action limit.
func Chunk[T any](xs []T, n int) [][]T {
	if n <= 0 {
		return [][]T{xs}
	}
	var out [][]T
	for len(xs) > 0 {
		k := n
		if k > len(xs) {
			k = len(xs)
		}
		out = append(out, xs[:k])
		xs = xs[k:]
	}
	return out
}

// ClampProb clamps a probability into the tradable outcome band.
func ClampProb(p float64) float64 {
	if p < OutcomeMinPrice {
		return OutcomeMinPrice
	}
	if p > OutcomeMaxPrice {
		return OutcomeMaxPrice
	}
	return p
}

// ProbString formats a probability as a limit-price string the core write path will
// accept: clamped into the band and rounded to the fixed 5-dp outcome tick. Because
// core.RoundOutcomePrice rejects (not clamps) out-of-band or round-to-≥1 prices,
// clamping here is what keeps near-0/near-1 quotes from hard-rejecting at Place.
func ProbString(p float64) string {
	p = ClampProb(p)
	p = math.Round(p*1e5) / 1e5
	p = ClampProb(p) // rounding can nudge onto/over a boundary; keep it inside
	return strconv.FormatFloat(p, 'f', -1, 64)
}

// ParseFloat parses a decimal string (px/sz/target), returning ok=false on "" or
// a parse error rather than a sentinel — callers decide how to fail.
func ParseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// ParseExpiry parses a Market.Expiry string ("YYYY-MM-DD HH:MMZ") as a UTC instant.
func ParseExpiry(s string) (time.Time, bool) {
	t, err := time.Parse(ExpiryLayout, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// TTL returns the time from now until the market's expiry, and ok=false when the
// expiry is missing/unparseable. A negative duration (already expired) still returns
// ok=true so callers can treat it as "past expiry".
func TTL(m core.Market, now time.Time) (time.Duration, bool) {
	exp, ok := ParseExpiry(m.Expiry)
	if !ok {
		return 0, false
	}
	return exp.Sub(now), true
}

// Tau returns time-to-expiry in YEARS (for the annualized BS-digital model), and
// ok=false when expiry is missing or already passed (τ ≤ 0 is not priceable).
func Tau(m core.Market, now time.Time) (float64, bool) {
	d, ok := TTL(m, now)
	if !ok || d <= 0 {
		return 0, false
	}
	return d.Seconds() / yearSeconds, true
}

// IsYes reports whether the market is the Yes leg (side 0). Anything not explicitly
// "No" is treated as Yes (side label defaults to Yes).
func IsYes(m core.Market) bool { return !strings.EqualFold(strings.TrimSpace(m.Side), "No") }

// YesCoin / NoCoin return the "#<enc>" coin strings for a question outcome id, where
// enc = 10*outcome + side (side 0 = Yes, 1 = No) — mirroring hl.OutcomeEncoding.
func YesCoin(outcome int) string { return "#" + strconv.Itoa(10*outcome) }
func NoCoin(outcome int) string  { return "#" + strconv.Itoa(10*outcome+1) }

// SiblingCoin returns the OTHER leg's coin for a market (the No coin for a Yes
// market and vice versa) — the counterpart book for cross-book arb and dual-book
// coherence. In a coherent market YesMid + NoMid ≈ 1.
func SiblingCoin(m core.Market) string {
	if IsYes(m) {
		return NoCoin(m.Outcome)
	}
	return YesCoin(m.Outcome)
}

// QuestionKey groups outcome coins that are the SAME bet, mirroring core's
// per-question grouping: a real question (Question != 0) buckets all its outcomes;
// a standalone binary buckets its own Yes/No pair (keyed on Outcome). Shared by the
// selector's concentration penalty and the engine's notional tracking so they bucket
// identically to core's per-question gate.
func QuestionKey(m core.Market) string {
	if m.Question != 0 {
		return "q:" + strconv.Itoa(m.Question)
	}
	return "o:" + strconv.Itoa(m.Outcome)
}

// ParseBookTop projects a core.BboView into an mm.BookTop (probability units),
// tolerant of a nil view / missing side. Shared by the selector and the engine so
// the BboView→BookTop parse lives in one place.
func ParseBookTop(bbo *core.BboView) BookTop {
	var b BookTop
	if bbo == nil {
		return b
	}
	if v, ok := ParseFloat(bbo.Bid); ok {
		b.Bid, b.HasBid = v, true
	}
	if v, ok := ParseFloat(bbo.Ask); ok {
		b.Ask, b.HasAsk = v, true
	}
	if v, ok := ParseFloat(bbo.BidSz); ok {
		b.BidSz = v
	}
	if v, ok := ParseFloat(bbo.AskSz); ok {
		b.AskSz = v
	}
	return b
}

// Priceable reports whether v1 can price this market: an OPEN priceBinary whose
// underlying is in the operator's priceable set (a live mark + vol we can feed the
// model). Event/categorical markets are excluded until the v2 LLM layer.
func Priceable(m core.Market, priceableUnderlyings map[string]bool) bool {
	if !m.IsOutcome || strings.EqualFold(m.ResolutionStatus, "settled") {
		return false
	}
	if m.Underlying == "" || m.TargetPrice == "" || m.Expiry == "" {
		return false // not a priceBinary (no underlying/target/expiry parsed)
	}
	return priceableUnderlyings[strings.ToUpper(strings.TrimSpace(m.Underlying))]
}
