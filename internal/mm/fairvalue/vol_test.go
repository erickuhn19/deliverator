package fairvalue

import (
	"math"
	"testing"
	"time"
)

// feedConstantVol drives the estimator with a synthetic price path whose log returns all
// have magnitude |r| over a fixed Δt, so the per-second variance r²/Δt is constant and the
// EWMA converges to it exactly. The annualized σ that implies is |r|·√(secondsPerYear/Δt);
// we choose |r| to hit a target annual σ and assert the estimator recovers it. Returns
// alternate sign to keep the price from drifting off to 0 or ∞.
func TestEWMAVolConvergesToConstant(t *testing.T) {
	const targetSigma = 0.6
	const dt = 1.0 // seconds between marks
	// σ_annual = |r|·√(yr/dt) ⇒ |r| = σ_annual·√(dt/yr).
	r := targetSigma * math.Sqrt(dt/secondsPerYear)

	e := NewEWMAVol(0.94)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := 50000.0
	e.Update(px, start) // seed; no return yet

	sign := 1.0
	for i := 1; i <= 500; i++ {
		px *= math.Exp(sign * r) // log return = ±r exactly
		sign = -sign
		e.Update(px, start.Add(time.Duration(i)*time.Second))
	}
	got, ok := e.Sigma()
	if !ok {
		t.Fatal("Sigma should be ok after 500 samples")
	}
	if math.Abs(got-targetSigma) > 1e-6 {
		t.Fatalf("EWMAVol converged to σ=%v, want ≈%v", got, targetSigma)
	}
}

func TestEWMAVolWarmupGate(t *testing.T) {
	e := NewEWMAVol(0.94)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := 0.6 * math.Sqrt(1.0/secondsPerYear)
	px := 100.0
	e.Update(px, start)
	if _, ok := e.Sigma(); ok {
		t.Fatal("Sigma must not be ok with only a seed sample")
	}
	sign := 1.0
	for i := 1; i < volMinSamples; i++ { // one short of the gate
		px *= math.Exp(sign * r)
		sign = -sign
		e.Update(px, start.Add(time.Duration(i)*time.Second))
	}
	if _, ok := e.Sigma(); ok {
		t.Fatalf("Sigma must not be ok before %d samples", volMinSamples)
	}
	// One more crosses the gate.
	px *= math.Exp(r)
	e.Update(px, start.Add(time.Duration(volMinSamples)*time.Second))
	if _, ok := e.Sigma(); !ok {
		t.Fatalf("Sigma should be ok at %d samples", volMinSamples)
	}
}

func TestEWMAVolIgnoresBadSamples(t *testing.T) {
	e := NewEWMAVol(0.94)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := 0.6 * math.Sqrt(1.0/secondsPerYear)
	px := 100.0
	e.Update(px, start)

	sign := 1.0
	last := start
	added := 0
	for i := 1; added < 40; i++ {
		ts := start.Add(time.Duration(i) * time.Second)
		// Interleave garbage that must be ignored: a non-positive price and an
		// out-of-order timestamp. Neither may advance state or perturb the estimate.
		e.Update(-10, ts)  // non-positive price → ignored
		e.Update(px, last) // duplicate/backwards timestamp (dt≤0) → ignored
		px *= math.Exp(sign * r)
		sign = -sign
		e.Update(px, ts) // the one valid sample
		last = ts
		added++
	}
	got, ok := e.Sigma()
	if !ok {
		t.Fatal("Sigma should be ok after enough valid samples")
	}
	// Bad samples were dropped, so we still recover the clean constant σ.
	if math.Abs(got-0.6) > 1e-6 {
		t.Fatalf("bad samples leaked into σ: got %v, want ≈0.6", got)
	}
}

func TestNewEWMAVolLambdaFallback(t *testing.T) {
	for _, bad := range []float64{-1, 0, 1, 2} {
		if e := NewEWMAVol(bad); e.lambda != 0.94 {
			t.Fatalf("NewEWMAVol(%v) lambda = %v, want fallback 0.94", bad, e.lambda)
		}
	}
	if e := NewEWMAVol(0.9); e.lambda != 0.9 {
		t.Fatalf("valid lambda should be kept, got %v", e.lambda)
	}
}
