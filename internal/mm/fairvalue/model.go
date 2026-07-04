// Package fairvalue holds the deterministic v1 fair-value model for HIP-4 outcome
// markets and its realized-vol companion. It implements mm.FairValuer so the quoting
// engine stays model-agnostic (a v2 LLMAnalyst can slot in behind the same interface).
//
// The model prices an "underlying ABOVE target at expiry" binary as a Black-Scholes
// digital: under geometric Brownian motion the risk-neutral probability that the
// terminal price S_T exceeds the strike K is Φ(d2). A YES share on such a market pays
// $1 iff that event happens, so its fair price in USDC IS that probability — the model
// output plugs straight into the outcome book as a limit price.
//
// PURITY: every function here is deterministic and free of I/O, goroutines and wall
// clock. The only "now" the model needs is injected (Now func() time.Time), so tests
// control time and two calls with identical inputs always agree. The live inputs the
// Market does not carry (spot mark, realized vol) arrive through the injected
// mm.UnderlyingFeed; the model never reaches for them itself.
package fairvalue

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// StandardNormalCDF returns Φ(x), the cumulative distribution function of the standard
// normal, computed from the complementary error function: Φ(x) = ½·erfc(−x/√2). We go
// through math.Erfc (rather than 0.5*(1+erf)) because erfc is the accurate branch in
// the tails — for large |x| erf saturates at ±1 and 1+erf loses all precision, whereas
// erfc keeps the small tail mass, which is exactly the deep ITM/OTM regime a digital
// lives in.
func StandardNormalCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// PriceBinaryModel is the v1 FairValuer: a Black-Scholes digital for a
// "underlying ABOVE target at expiry" binary. It reads the underlying's spot mark and
// annualized realized vol from an injected UnderlyingFeed and prices the YES leg as the
// risk-neutral probability of finishing in the money.
type PriceBinaryModel struct {
	feed     mm.UnderlyingFeed
	mu       float64       // drift μ used in d2; 0 = driftless (risk-neutral-ish) default
	plateauK float64       // confidence curvature; higher ⇒ confidence falls off faster off 0.5
	validFor time.Duration // how long an estimate stays fresh (sets Fair.ValidUntil)
	now      func() time.Time
}

// Option configures a PriceBinaryModel at construction. Functional options keep the
// constructor stable while the tunables (drift, confidence shape, freshness, clock) grow.
type Option func(*PriceBinaryModel)

// WithMu sets the drift μ (per year) used in the d2 numerator. Default 0: we do not try
// to forecast the underlying's direction — a driftless digital reduces to "is spot above
// the vol-scaled strike?", which is the honest prior when we have no edge on direction.
func WithMu(mu float64) Option { return func(m *PriceBinaryModel) { m.mu = mu } }

// WithPlateauK sets the confidence curvature exponent (default 2.0). Confidence is a
// plateau centred on P=0.5; k controls how sharply it drops toward the 0/1 extremes.
func WithPlateauK(k float64) Option {
	return func(m *PriceBinaryModel) {
		if k > 0 {
			m.plateauK = k
		}
	}
}

// WithValidFor sets how long each estimate stays fresh (default 5s). The engine must
// re-price at least this often; a stale Fair fails mm.Fair.Valid and pulls quotes.
func WithValidFor(d time.Duration) Option {
	return func(m *PriceBinaryModel) {
		if d > 0 {
			m.validFor = d
		}
	}
}

// WithNow injects the clock used to stamp ValidUntil. Defaults to time.Now; tests pass a
// fixed clock so ValidUntil is exact and the model stays deterministic.
func WithNow(now func() time.Time) Option {
	return func(m *PriceBinaryModel) {
		if now != nil {
			m.now = now
		}
	}
}

// NewPriceBinaryModel builds a PriceBinaryModel over feed with the given options applied
// over the documented defaults (μ=0, k=2.0, validFor=5s, clock=time.Now).
func NewPriceBinaryModel(feed mm.UnderlyingFeed, opts ...Option) *PriceBinaryModel {
	m := &PriceBinaryModel{
		feed:     feed,
		mu:       0,
		plateauK: 2.0,
		validFor: 5 * time.Second,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Estimate prices the YES leg of an outcome market as a Black-Scholes digital.
//
// It ALWAYS returns the YES probability in Fair.P regardless of m.Side: both legs of a
// market share one model, and the NO fair is simply 1−P, derived by the caller. This
// keeps a single source of truth and guarantees the two legs stay coherent (YES+NO≈1).
//
// FAIL-CLOSED: any missing or nonsensical input (bad strike, no time to expiry, no mark,
// no vol, non-positive vol/spot) returns a descriptive error and a zero Fair. The caller
// treats an error as "do not quote this market" — we would rather show no market than a
// market priced off a stale or absent input.
//
// As τ→0 no special estimator is needed: with vanishing time the vol term σ√τ in the d2
// denominator shrinks, so d2→±∞ according to sign(ln(S/K)); Φ then collapses to the 0/1
// step function 1[S>K]. The digital degenerates into "is spot above the strike right now"
// exactly as expiry demands, which is why there is no separate near-expiry branch here.
func (m *PriceBinaryModel) Estimate(ctx context.Context, mk core.Market) (mm.Fair, error) {
	now := m.now()

	K, okK := mm.ParseFloat(mk.TargetPrice)
	if !okK || K <= 0 {
		return mm.Fair{}, fmt.Errorf("fairvalue: unusable target price %q for %s", mk.TargetPrice, mk.Coin)
	}
	tau, okTau := mm.Tau(mk, now)
	if !okTau {
		return mm.Fair{}, fmt.Errorf("fairvalue: no time-to-expiry (missing/past) for %s", mk.Coin)
	}
	S, okS := m.feed.Mark(mk.Underlying)
	if !okS || S <= 0 {
		return mm.Fair{}, fmt.Errorf("fairvalue: no usable mark for underlying %q", mk.Underlying)
	}
	sigma, okSig := m.feed.Vol(mk.Underlying)
	if !okSig || sigma <= 0 {
		return mm.Fair{}, fmt.Errorf("fairvalue: no usable vol for underlying %q", mk.Underlying)
	}

	// d2 = [ ln(S/K) + (μ − ½σ²)·τ ] / (σ·√τ). The −½σ² is the Itô/GBM convexity term:
	// the median of a lognormal sits below its mean, so the probability of finishing
	// above K is Φ(d2), not Φ(d1). We price on d2 because a digital pays on the terminal
	// event, not on a delta-hedged forward.
	sqrtTau := math.Sqrt(tau)
	d2 := (math.Log(S/K) + (m.mu-0.5*sigma*sigma)*tau) / (sigma * sqrtTau)
	pYes := StandardNormalCDF(d2)

	// Clamp into the tradable outcome band. A raw Φ can round to exactly 0 or 1 for a
	// deep ITM/OTM strike; core's limit path REJECTS 0/≥1, and mm.Fair.Valid rejects the
	// open interval's endpoints, so we keep P strictly inside (min,max).
	pYes = mm.ClampProb(pYes)

	return mm.Fair{
		P:          pYes,
		Conf:       m.plateau(pYes),
		ValidUntil: now.Add(m.validFor),
	}, nil
}

// plateau maps a probability to a confidence in [0,1]: 1 − |2P−1|^k, clamped. It peaks at
// 1.0 when P=0.5 (the model is most useful — and quotes tightest — where the outcome is a
// genuine coin flip) and decays toward 0 as P approaches either extreme (near-certain
// markets carry little edge and huge asymmetric tail risk, so the engine widens or pulls).
// k shapes the fall-off: k=2 is a smooth quadratic dome; larger k keeps confidence high
// longer near 0.5 then drops sharply. Because P is pre-clamped strictly inside (0,1),
// |2P−1|<1 and the result is strictly >0, so a valid estimate always has positive Conf.
func (m *PriceBinaryModel) plateau(p float64) float64 {
	c := 1 - math.Pow(math.Abs(2*p-1), m.plateauK)
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

// Compile-time proof the model satisfies the shared FairValuer contract.
var _ mm.FairValuer = (*PriceBinaryModel)(nil)
