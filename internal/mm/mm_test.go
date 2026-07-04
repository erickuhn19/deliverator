package mm

import (
	"math"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
)

func TestClampProbAndProbString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.5, "0.5"},
		{0.123456, "0.12346"}, // rounds to 5 dp
		{-1, "0.00001"},       // clamped to floor
		{2, "0.99999"},        // clamped to ceil
		{0.000001, "0.00001"}, // below floor → floor (never rounds to 0)
		{0.999999, "0.99999"}, // above ceil → ceil (never rounds to ≥1)
	}
	for _, c := range cases {
		if got := ProbString(c.in); got != c.want {
			t.Errorf("ProbString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// The output must always survive core's limit-price rounder (open interval, ≤5dp).
	for _, in := range []float64{-5, 0, 0.00001, 0.5, 0.99999, 1, 5, 0.017283} {
		if _, _, err := core.RoundOutcomePrice(ProbString(in)); err != nil {
			t.Errorf("ProbString(%v) = %q rejected by RoundOutcomePrice: %v", in, ProbString(in), err)
		}
	}
}

func TestParseExpiryAndTau(t *testing.T) {
	exp, ok := ParseExpiry("2026-07-02 14:36Z")
	if !ok {
		t.Fatal("failed to parse a well-formed expiry")
	}
	if exp.Year() != 2026 || exp.Month() != time.July || exp.Day() != 2 || exp.Hour() != 14 || exp.Minute() != 36 {
		t.Fatalf("parsed expiry wrong: %v", exp)
	}
	if exp.Location() != time.UTC {
		t.Fatalf("expiry must be UTC, got %v", exp.Location())
	}
	if _, ok := ParseExpiry("garbage"); ok {
		t.Fatal("garbage expiry should not parse")
	}

	m := core.Market{Expiry: "2026-07-02 14:36Z"}
	// 1 year before expiry → τ ≈ 1.0
	now := time.Date(2025, 7, 2, 14, 36, 0, 0, time.UTC)
	tau, ok := Tau(m, now)
	if !ok || math.Abs(tau-1.0) > 0.01 {
		t.Fatalf("Tau one year out = %v (ok=%v), want ≈1.0", tau, ok)
	}
	// After expiry → not priceable.
	if _, ok := Tau(m, exp.Add(time.Minute)); ok {
		t.Fatal("Tau past expiry should be ok=false")
	}
}

func TestEncodingHelpers(t *testing.T) {
	yes := core.Market{IsOutcome: true, Outcome: 641, Side: "Yes"}
	no := core.Market{IsOutcome: true, Outcome: 641, Side: "No"}
	if YesCoin(641) != "#6410" || NoCoin(641) != "#6411" {
		t.Fatalf("coin encoding wrong: yes=%s no=%s", YesCoin(641), NoCoin(641))
	}
	if !IsYes(yes) || IsYes(no) {
		t.Fatal("IsYes classification wrong")
	}
	if SiblingCoin(yes) != "#6411" || SiblingCoin(no) != "#6410" {
		t.Fatalf("sibling wrong: yes→%s no→%s", SiblingCoin(yes), SiblingCoin(no))
	}
}

func TestPriceable(t *testing.T) {
	set := map[string]bool{"BTC": true}
	open := core.Market{IsOutcome: true, ResolutionStatus: "open", Underlying: "btc", TargetPrice: "76000", Expiry: "2026-07-02 14:36Z"}
	if !Priceable(open, set) {
		t.Fatal("open BTC priceBinary should be priceable")
	}
	settled := open
	settled.ResolutionStatus = "settled"
	if Priceable(settled, set) {
		t.Fatal("settled market should not be priceable")
	}
	event := core.Market{IsOutcome: true, ResolutionStatus: "open", Underlying: "", TargetPrice: "", Expiry: ""}
	if Priceable(event, set) {
		t.Fatal("event market (no underlying) should not be priceable")
	}
	unknown := open
	unknown.Underlying = "DOGE"
	if Priceable(unknown, set) {
		t.Fatal("underlying not in priceable set should be excluded")
	}
}

func TestInventoryAndBookTop(t *testing.T) {
	inv := Inventory{Yes: 30, No: 12}
	if inv.Net() != 18 {
		t.Fatalf("Net = %d, want 18", inv.Net())
	}
	b := BookTop{Bid: 0.40, Ask: 0.44, HasBid: true, HasAsk: true}
	if mid, ok := b.Mid(); !ok || math.Abs(mid-0.42) > 1e-9 {
		t.Fatalf("Mid = %v ok=%v, want 0.42", mid, ok)
	}
	if _, ok := (BookTop{HasBid: true}).Mid(); ok {
		t.Fatal("one-sided book should have no mid")
	}
}

func TestFairValid(t *testing.T) {
	now := time.Now()
	good := Fair{P: 0.5, Conf: 0.8, ValidUntil: now.Add(time.Minute)}
	if !good.Valid(now) {
		t.Fatal("fresh in-band estimate should be valid")
	}
	for _, bad := range []Fair{
		{P: 0, Conf: 0.8},
		{P: 1, Conf: 0.8},
		{P: 0.5, Conf: 0},
		{P: 0.5, Conf: 0.8, ValidUntil: now.Add(-time.Minute)},
	} {
		if bad.Valid(now) {
			t.Fatalf("expected invalid: %+v", bad)
		}
	}
}
