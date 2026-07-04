// Package settle holds the end-of-life policy for an outcome market: when to stop
// quoting and hold the book flat as expiry approaches (the blackout), the hard stop
// once the chain reports the market settled, and the estimator that INFERS which leg
// won from the mark tape.
//
// Why a dedicated policy: quoting into the final minutes before a binary expires is
// pure adverse selection — the fair probability is snapping toward 0 or 1 and any
// resting quote is a free option for anyone closer to the underlying. So the engine
// pulls all quotes inside a blackout window and simply holds to settlement, where
// 1 YES + 1 NO is guaranteed $1 (HIP-4). This package decides WHEN that switch flips.
//
// This system exposes NO settled-price or settled-fraction endpoint. The ONLY
// settlement signal core surfaces is Market.ResolutionStatus flipping "open"->
// "settled". The winning side is therefore not reported and must be reconstructed
// from the marks we recorded around expiry — that is what EstimateWinner does, so the
// oms can label held inventory as won/lost for reporting even though the chain never
// tells us directly.
//
// PURE: every entry point takes an explicit `now` / sample timestamps. Nothing here
// reads the wall clock, touches the network, or spawns goroutines — tests drive time.
package settle

import (
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// State is the market's lifecycle phase from the market maker's point of view. It is
// deliberately coarser than core's resolution_status: it folds "how close to expiry"
// and "can we even parse the clock" into the same enum the engine switches on to
// decide quote/cancel.
type State int

const (
	// Quoting: normal operation — outside the blackout window, open, clock parses.
	Quoting State = iota
	// Blackout: inside the pre-expiry no-quote window (or already past expiry but not
	// yet marked settled). Cancel resting quotes and hold; do not open new ones.
	Blackout
	// Settled: the chain has resolved the market. Hard stop — the book is gone, only
	// held inventory remains to be swept.
	Settled
	// Unknown: we could not determine the phase (missing/unparseable expiry). Treated
	// as fail-closed — the caller must behave as if in blackout (cancel, don't quote)
	// rather than assume it is safe to keep quoting into an unknown deadline.
	Unknown
)

// String renders the state for logs/console. Kept exhaustive so an out-of-range value
// is visible rather than silently blank.
func (s State) String() string {
	switch s {
	case Quoting:
		return "quoting"
	case Blackout:
		return "blackout"
	case Settled:
		return "settled"
	case Unknown:
		return "unknown"
	default:
		return "State(" + itoa(int(s)) + ")"
	}
}

// itoa is a tiny local int->string so String() needn't pull in strconv for the
// unreachable default branch (keeps the package's import surface minimal).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Policy is the blackout configuration. BlackoutMins is the number of minutes before
// expiry at which the MM stops quoting and holds to settlement. It is a plain int
// (not a config type) so this leaf package stays free of the config import — the
// engine passes the operator's configured value in.
type Policy struct {
	BlackoutMins int
}

// Status classifies a market at instant `now`. Precedence is deliberate and
// fail-closed:
//
//  1. Settled dominates everything — once the chain resolves, phase is Settled
//     regardless of what the (now-frozen) expiry clock says.
//  2. If the expiry clock is missing/unparseable we return Unknown, NOT Quoting: we
//     never keep quoting into a deadline we cannot see.
//  3. Otherwise TTL <= blackout window (INCLUDING zero and negative/past-expiry) is
//     Blackout; strictly more than the window is Quoting.
func (p Policy) Status(m core.Market, now time.Time) State {
	// (1) Hard stop first: a settled market has no book to quote and its expiry clock
	// is meaningless, so short-circuit before touching TTL.
	if isSettled(m) {
		return Settled
	}

	// (2) Fail-closed on an unreadable clock. mm.TTL returns ok=false only when expiry
	// is empty/unparseable; a valid-but-past expiry returns ok=true with a negative
	// duration, which we WANT to fall through to the blackout test below.
	ttl, ok := mm.TTL(m, now)
	if !ok {
		return Unknown
	}

	// (3) Inside the window (or already past expiry) ⇒ blackout. Compare against the
	// window as a Duration. Note "<=" so the exact boundary and past-expiry (negative
	// ttl) both blackout — the safe side of the fence.
	if ttl <= time.Duration(p.BlackoutMins)*time.Minute {
		return Blackout
	}
	return Quoting
}

// InBlackout reports whether the MM must refrain from opening new quotes for this
// market: true for Blackout, Unknown (fail-closed), and Settled — i.e. every phase
// except Quoting. This is the single predicate the engine gates new placements on.
func (p Policy) InBlackout(m core.Market, now time.Time) bool {
	return p.Status(m, now) != Quoting
}

// isSettled centralizes the resolution-status test (case-insensitive, trimmed) so the
// exact spelling core emits ("settled") is matched in one place.
func isSettled(m core.Market) bool {
	return strings.EqualFold(strings.TrimSpace(m.ResolutionStatus), "settled")
}

// MarkSample is one recorded underlying-mark observation with its timestamp. The oms
// accumulates these off the mids stream around each market's expiry so the winner can
// be inferred after the chain settles.
type MarkSample struct {
	T  time.Time
	Px float64
}

// EstimateWinner infers whether the YES leg won by reconstructing the underlying mark
// AT expiry and comparing it to the market's target price.
//
// HIP-4 priceBinary settlement rule modeled here: YES resolves iff the underlying's
// mark at the expiry instant is >= targetPrice. Because marks arrive discretely, the
// value exactly at expiry is estimated by LINEAR interpolation between the last mark
// at/before expiry and the first mark at/after expiry (the two samples that flank the
// deadline). This mirrors how an oracle would read a price between two updates.
//
// Behavior:
//   - Needs a parseable target and expiry, and at least one sample; otherwise ok=false.
//   - When samples flank expiry on both sides, interpolate between them.
//   - When all samples fall on one side of expiry (only before, or only after),
//     fall back to the single nearest sample (no extrapolation — the closest known
//     mark is our best estimate).
//   - winnerYes = estimatedMark >= target.
//
// PURE: the "current time" is never consulted; only the passed samples and the
// market's own frozen expiry matter, so the result is reproducible from the tape.
func EstimateWinner(m core.Market, samples []MarkSample) (winnerYes bool, ok bool) {
	target, tok := mm.ParseFloat(m.TargetPrice)
	expiry, eok := mm.ParseExpiry(m.Expiry)
	if !tok || !eok || len(samples) == 0 {
		return false, false
	}

	// Find the flanking samples: `before` = latest sample at/before expiry, `after` =
	// earliest sample at/after expiry. A sample landing exactly on expiry satisfies
	// both tests; that is fine — it drives the interpolation to that exact mark.
	var (
		before, after    *MarkSample
		haveBef, haveAft bool
	)
	for i := range samples {
		s := &samples[i]
		if !s.T.After(expiry) { // s.T <= expiry
			if !haveBef || s.T.After(before.T) {
				before, haveBef = s, true
			}
		}
		if !s.T.Before(expiry) { // s.T >= expiry
			if !haveAft || s.T.Before(after.T) {
				after, haveAft = s, true
			}
		}
	}

	var mark float64
	switch {
	case haveBef && haveAft:
		// Both sides present. If they are the same instant (or coincide at expiry),
		// dt is zero — avoid dividing by it and just take that mark.
		dt := after.T.Sub(before.T).Seconds()
		if dt <= 0 {
			mark = before.Px
		} else {
			// Fraction of the before->after gap that expiry sits at, in [0,1].
			frac := expiry.Sub(before.T).Seconds() / dt
			mark = before.Px + frac*(after.Px-before.Px)
		}
	case haveBef:
		// Only history before expiry — no post-expiry mark to interpolate to. Best
		// estimate is the last known mark (nearest sample). No extrapolation.
		mark = before.Px
	case haveAft:
		// Only marks after expiry (e.g. we started sampling late). Nearest is the
		// first post-expiry mark.
		mark = after.Px
	default:
		// Unreachable given len(samples) > 0, but keep it explicit and fail-closed.
		return false, false
	}

	return mark >= target, true
}
