package fairvalue

import (
	"math"
	"time"
)

// secondsPerYear mirrors mm's annualization constant (365-day year). Realized variance
// is accumulated per SECOND and annualized by this factor so the resulting σ composes
// with the model's τ (also in years) — the two must share the same year convention or
// the digital is priced with a mismatched vol/time scale.
const secondsPerYear = 365.0 * 24.0 * 60.0 * 60.0

// volMinSamples is the warm-up: how many valid return samples must accumulate before
// Sigma reports ok. With λ≈0.94 the EWMA's effective memory is ~1/(1−λ)≈16 samples, so
// we require a comparable count to let the estimate settle before the model trusts it.
// Below this Sigma returns ok=false and the model fails closed (no quote).
const volMinSamples = 20

// EWMAVol is an exponentially-weighted realized-volatility estimator (RiskMetrics style)
// that the engine feeds off the underlying's mark/mid stream to implement
// UnderlyingFeed.Vol. It tracks an EWMA of the PER-SECOND variance of log returns, which
// makes it robust to irregular sampling: each return's variance contribution is scaled by
// its own elapsed time (r²/Δt) before smoothing, so a burst of ticks and a slow minute
// both contribute the right amount of variance-per-unit-time.
//
// PURITY / determinism: EWMAVol never calls the wall clock. Sample timestamps are passed
// in by the caller (Update(price, t)), so tests replay a synthetic tick sequence with
// controlled time and get exactly reproducible σ. It is not safe for concurrent use; the
// engine owns one per underlying and updates it from a single goroutine.
type EWMAVol struct {
	lambda      float64 // decay: variance = λ·variance + (1−λ)·newSample; higher ⇒ slower/smoother
	hasLast     bool
	lastPx      float64
	lastT       time.Time
	variance    float64 // EWMA of per-second variance of log returns
	initialized bool    // seeded by the first valid return sample
	samples     int     // count of valid return samples folded in (for the warm-up gate)
}

// NewEWMAVol builds an estimator with the given decay λ. λ must lie in (0,1); anything
// outside falls back to 0.94, the RiskMetrics daily default — a sensible middle ground
// between responsiveness and noise for the sub-minute marks the MM sees.
func NewEWMAVol(lambda float64) *EWMAVol {
	if lambda <= 0 || lambda >= 1 {
		lambda = 0.94
	}
	return &EWMAVol{lambda: lambda}
}

// Update folds one (price, timestamp) mark into the estimator. It computes the log return
// against the previous mark, converts it to a per-second variance contribution r²/Δt, and
// smooths that into the running EWMA.
//
// Bad samples are ignored (state untouched, so the NEXT good tick still measures against
// the last good price, not a skipped one): a non-positive price has no log return, and a
// non-positive Δt (out-of-order or duplicate timestamp) would blow up r²/Δt. Silently
// dropping them keeps a noisy feed from poisoning the vol with Inf/NaN.
func (e *EWMAVol) Update(price float64, t time.Time) {
	if price <= 0 {
		return // no valid log return; do not even reseed on a garbage price
	}
	if !e.hasLast {
		e.lastPx, e.lastT, e.hasLast = price, t, true
		return
	}
	dt := t.Sub(e.lastT).Seconds()
	if dt <= 0 {
		return // out-of-order / duplicate timestamp; ignore to avoid dividing by ≤0
	}

	r := math.Log(price / e.lastPx)
	// Variance scales linearly with time under GBM, so the per-second variance carried by
	// a return of size r observed over dt seconds is r²/dt. Smoothing THIS (not r² itself)
	// is what makes the estimator sampling-rate independent.
	perSecVar := (r * r) / dt

	if !e.initialized {
		e.variance = perSecVar // seed on the first sample rather than decay from 0
		e.initialized = true
	} else {
		e.variance = e.lambda*e.variance + (1-e.lambda)*perSecVar
	}
	e.samples++
	e.lastPx, e.lastT = price, t
}

// Sigma returns the annualized volatility and ok=true once enough samples have warmed up
// the EWMA. Annualization is √(perSecondVariance · secondsPerYear): variance is additive
// in time, so per-second variance times seconds-per-year is the annual variance, and its
// square root is the annual σ the model expects. Before the warm-up gate it returns
// (0,false) so the model fails closed instead of pricing off a half-formed estimate.
func (e *EWMAVol) Sigma() (float64, bool) {
	if !e.initialized || e.samples < volMinSamples {
		return 0, false
	}
	return math.Sqrt(e.variance * secondsPerYear), true
}
