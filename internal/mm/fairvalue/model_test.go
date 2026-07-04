package fairvalue

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// stubFeed is a controllable UnderlyingFeed: each field carries a value and an "ok" so a
// test can independently simulate a missing mark, a missing vol, or a bad (≤0) value.
type stubFeed struct {
	mark, vol     float64
	markOK, volOK bool
}

func (s stubFeed) Mark(string) (float64, bool) { return s.mark, s.markOK }
func (s stubFeed) Vol(string) (float64, bool)  { return s.vol, s.volOK }

// fixedNow returns a clock stuck at t so ValidUntil is exact and the model deterministic.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// mkt builds an outcome market one year before expiry relative to the given now, with the
// supplied strike. τ ≈ 1.0 makes σ√τ ≈ σ, keeping the hand-checked d2 math simple.
func oneYearMarket(now time.Time, target string) core.Market {
	exp := now.Add(365 * 24 * time.Hour)
	return core.Market{
		IsOutcome:        true,
		Underlying:       "BTC",
		TargetPrice:      target,
		Expiry:           exp.Format(mm.ExpiryLayout),
		ResolutionStatus: "open",
		Side:             "Yes",
		Coin:             "#100",
	}
}

func TestStandardNormalCDF(t *testing.T) {
	cases := []struct {
		x, want, tol float64
	}{
		{0, 0.5, 1e-12},
		{1, 0.8413447460685429, 1e-9},
		{-1, 0.15865525393145705, 1e-9},
		{1.96, 0.9750021048517795, 1e-9},
		{-1.96, 0.024997895148220435, 1e-9},
		{8, 1.0, 1e-9},  // deep positive saturates to 1
		{-8, 0.0, 1e-9}, // deep negative saturates to 0
	}
	for _, c := range cases {
		if got := StandardNormalCDF(c.x); math.Abs(got-c.want) > c.tol {
			t.Errorf("StandardNormalCDF(%v) = %v, want %v", c.x, got, c.want)
		}
	}
	// Symmetry: Φ(x) + Φ(−x) = 1.
	for _, x := range []float64{0.3, 1.1, 2.7, 5.0} {
		if s := StandardNormalCDF(x) + StandardNormalCDF(-x); math.Abs(s-1) > 1e-12 {
			t.Errorf("Φ(%v)+Φ(-%v) = %v, want 1", x, x, s)
		}
	}
}

func TestEstimateAtTheMoney(t *testing.T) {
	// S=K, τ=1 ⇒ d2 = (0 + (0 − ½σ²)·1)/(σ·1) = −½σ. So pYes = Φ(−½σ) < 0.5: even a
	// coin-flip spot is slightly odds-against finishing above the strike, by the GBM
	// convexity drift. This is the load-bearing sanity check on the −½σ² term.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sigma := 0.6
	feed := stubFeed{mark: 50000, markOK: true, vol: sigma, volOK: true}
	model := NewPriceBinaryModel(feed, WithNow(fixedNow(now)))

	f, err := model.Estimate(context.Background(), oneYearMarket(now, "50000"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := StandardNormalCDF(-0.5 * sigma) // τ≈1, S=K
	if math.Abs(f.P-want) > 5e-3 {
		t.Fatalf("ATM pYes = %v, want ≈%v", f.P, want)
	}
	if f.P >= 0.5 {
		t.Fatalf("ATM pYes = %v, want strictly below 0.5 (convexity drift)", f.P)
	}
	if !f.ValidUntil.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("ValidUntil = %v, want now+5s default", f.ValidUntil)
	}
}

func TestEstimateDeepITMandOTM(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feed := stubFeed{mark: 100000, markOK: true, vol: 0.5, volOK: true}
	model := NewPriceBinaryModel(feed, WithNow(fixedNow(now)))

	// Deep ITM: spot 100k vs strike 1k ⇒ almost sure YES ⇒ P near the upper band.
	itm, err := model.Estimate(context.Background(), oneYearMarket(now, "1000"))
	if err != nil {
		t.Fatal(err)
	}
	if itm.P < 0.999 {
		t.Fatalf("deep ITM pYes = %v, want ≈1", itm.P)
	}
	if itm.P > mm.OutcomeMaxPrice {
		t.Fatalf("deep ITM pYes = %v exceeds band ceil", itm.P)
	}

	// Deep OTM: spot 100k vs strike 10M ⇒ almost sure NO ⇒ P near the lower band.
	otm, err := model.Estimate(context.Background(), oneYearMarket(now, "10000000"))
	if err != nil {
		t.Fatal(err)
	}
	if otm.P > 0.001 {
		t.Fatalf("deep OTM pYes = %v, want ≈0", otm.P)
	}
	if otm.P < mm.OutcomeMinPrice {
		t.Fatalf("deep OTM pYes = %v below band floor", otm.P)
	}
}

func TestEstimateNearExpiryCollapsesToStep(t *testing.T) {
	// As τ→0 the digital must approach the step 1[S>K]: above strike ⇒ →1, below ⇒ →0.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Expiry has minute resolution (mm.ExpiryLayout), so the smallest positive τ we can
	// express is one minute out — τ = 60/yearSeconds ≈ 1.9e-6 yr, essentially 0.
	exp := now.Add(1 * time.Minute)
	base := core.Market{
		IsOutcome: true, Underlying: "BTC", ResolutionStatus: "open", Side: "Yes",
		Expiry: exp.Format(mm.ExpiryLayout), Coin: "#100",
	}
	feed := stubFeed{mark: 50000, markOK: true, vol: 0.6, volOK: true}
	model := NewPriceBinaryModel(feed, WithNow(fixedNow(now)))

	above := base
	above.TargetPrice = "49000" // spot > strike
	fa, err := model.Estimate(context.Background(), above)
	if err != nil {
		t.Fatal(err)
	}
	if fa.P < 0.99 {
		t.Fatalf("near-expiry, S>K pYes = %v, want →1", fa.P)
	}

	below := base
	below.TargetPrice = "51000" // spot < strike
	fb, err := model.Estimate(context.Background(), below)
	if err != nil {
		t.Fatal(err)
	}
	if fb.P > 0.01 {
		t.Fatalf("near-expiry, S<K pYes = %v, want →0", fb.P)
	}
}

func TestEstimateFailClosed(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	good := oneYearMarket(now, "50000")
	full := stubFeed{mark: 50000, markOK: true, vol: 0.6, volOK: true}

	cases := []struct {
		name string
		feed mm.UnderlyingFeed
		m    core.Market
	}{
		{"no mark", stubFeed{markOK: false, vol: 0.6, volOK: true}, good},
		{"zero mark", stubFeed{mark: 0, markOK: true, vol: 0.6, volOK: true}, good},
		{"negative mark", stubFeed{mark: -5, markOK: true, vol: 0.6, volOK: true}, good},
		{"no vol", stubFeed{mark: 50000, markOK: true, volOK: false}, good},
		{"zero vol", stubFeed{mark: 50000, markOK: true, vol: 0, volOK: true}, good},
		{"negative vol", stubFeed{mark: 50000, markOK: true, vol: -0.3, volOK: true}, good},
		{"bad target", full, func() core.Market { m := good; m.TargetPrice = "abc"; return m }()},
		{"zero target", full, func() core.Market { m := good; m.TargetPrice = "0"; return m }()},
		{"empty target", full, func() core.Market { m := good; m.TargetPrice = ""; return m }()},
		{"missing expiry", full, func() core.Market { m := good; m.Expiry = ""; return m }()},
		{"past expiry", full, func() core.Market {
			m := good
			m.Expiry = now.Add(-time.Hour).Format(mm.ExpiryLayout)
			return m
		}()},
	}
	for _, c := range cases {
		model := NewPriceBinaryModel(c.feed, WithNow(fixedNow(now)))
		f, err := model.Estimate(context.Background(), c.m)
		if err == nil {
			t.Errorf("%s: expected fail-closed error, got Fair %+v", c.name, f)
		}
		if (f != mm.Fair{}) {
			t.Errorf("%s: expected zero Fair on error, got %+v", c.name, f)
		}
	}
}

func TestPlateauConfidence(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	model := NewPriceBinaryModel(stubFeed{}, WithNow(fixedNow(now)))

	// Peaks at P=0.5.
	if c := model.plateau(0.5); math.Abs(c-1.0) > 1e-12 {
		t.Fatalf("plateau(0.5) = %v, want 1.0", c)
	}
	// Symmetric and decreasing away from 0.5.
	if math.Abs(model.plateau(0.3)-model.plateau(0.7)) > 1e-12 {
		t.Fatal("plateau must be symmetric about 0.5")
	}
	if !(model.plateau(0.5) > model.plateau(0.3) && model.plateau(0.3) > model.plateau(0.1)) {
		t.Fatal("plateau must strictly decrease from 0.5 toward the extremes")
	}
	// Strictly positive inside the band (so a valid estimate always has Conf>0) and →0
	// at the edges.
	if c := model.plateau(mm.OutcomeMinPrice); c <= 0 || c >= 0.01 {
		t.Fatalf("plateau at band floor = %v, want small but >0", c)
	}
	if c := model.plateau(mm.OutcomeMaxPrice); c <= 0 || c >= 0.01 {
		t.Fatalf("plateau at band ceil = %v, want small but >0", c)
	}
	// Confidence stays in [0,1] across the whole range.
	for p := 0.0; p <= 1.0; p += 0.05 {
		if c := model.plateau(p); c < 0 || c > 1 {
			t.Fatalf("plateau(%v) = %v out of [0,1]", p, c)
		}
	}
}

func TestPlateauKShape(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// A larger k keeps confidence higher near 0.5 (|2P-1|<1 raised to a bigger power is
	// smaller ⇒ 1−that is larger).
	soft := NewPriceBinaryModel(stubFeed{}, WithNow(fixedNow(now)), WithPlateauK(4))
	sharp := NewPriceBinaryModel(stubFeed{}, WithNow(fixedNow(now)), WithPlateauK(1))
	if !(soft.plateau(0.6) > sharp.plateau(0.6)) {
		t.Fatalf("higher k should give higher confidence near 0.5: soft=%v sharp=%v",
			soft.plateau(0.6), sharp.plateau(0.6))
	}
}

func TestEstimateHonorsOptions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feed := stubFeed{mark: 50000, markOK: true, vol: 0.6, volOK: true}
	model := NewPriceBinaryModel(feed,
		WithNow(fixedNow(now)),
		WithValidFor(30*time.Second),
		WithMu(0.2),
	)
	f, err := model.Estimate(context.Background(), oneYearMarket(now, "50000"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.ValidUntil.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("WithValidFor ignored: ValidUntil=%v", f.ValidUntil)
	}
	// A positive drift pushes the ATM YES probability up versus the driftless baseline.
	base := NewPriceBinaryModel(feed, WithNow(fixedNow(now)))
	bf, _ := base.Estimate(context.Background(), oneYearMarket(now, "50000"))
	if !(f.P > bf.P) {
		t.Fatalf("positive μ should raise pYes: withMu=%v baseline=%v", f.P, bf.P)
	}
}

// The estimate must remain usable by mm.Fair.Valid for the freshness window and drop out
// after ValidUntil — this ties the model to the contract the engine gates quotes on.
func TestEstimateFairValidWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feed := stubFeed{mark: 50000, markOK: true, vol: 0.6, volOK: true}
	model := NewPriceBinaryModel(feed, WithNow(fixedNow(now)), WithValidFor(5*time.Second))
	f, err := model.Estimate(context.Background(), oneYearMarket(now, "50000"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Valid(now) {
		t.Fatal("fresh estimate should be Valid at now")
	}
	if !f.Valid(now.Add(5 * time.Second)) {
		t.Fatal("estimate should be Valid at exactly ValidUntil")
	}
	if f.Valid(now.Add(6 * time.Second)) {
		t.Fatal("estimate should be stale after ValidUntil")
	}
}
