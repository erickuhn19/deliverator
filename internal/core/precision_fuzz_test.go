package core

import (
	"testing"

	"github.com/shopspring/decimal"
)

// The precision rounders feed the price/size that gets signed. Fuzz their core
// invariants: never panic; a successful result is a positive decimal (outcome:
// strictly in (0,1)); and rounding is idempotent (re-rounding a rounded value is a
// no-op) — a violation silently changes the signed price/size. Validation uses
// shopspring/decimal (not float64) so absurd-magnitude inputs don't overflow the
// oracle itself.

func posDecimal(t *testing.T, what, out string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(out)
	if err != nil {
		t.Fatalf("%s = %q is not a parseable decimal: %v", what, out, err)
	}
	if !d.IsPositive() {
		t.Fatalf("%s = %q is not positive", what, out)
	}
	return d
}

func FuzzRoundPrice(f *testing.F) {
	for _, px := range []string{"64000", "0.001", "65000.5", "1.23456789", "0", "-5", "abc", "1e3"} {
		f.Add(px, 2, false)
	}
	f.Fuzz(func(t *testing.T, px string, szDec int, isSpot bool) {
		if szDec < 0 || szDec > 8 {
			return // szDecimals is the asset's lot precision; out-of-range isn't a real input
		}
		out, _, err := RoundPrice(px, szDec, isSpot)
		if err != nil {
			return
		}
		posDecimal(t, "RoundPrice", out)
		if out2, changed2, err2 := RoundPrice(out, szDec, isSpot); err2 != nil || out2 != out || changed2 {
			t.Fatalf("RoundPrice not idempotent: %q -> %q (changed=%v err=%v)", out, out2, changed2, err2)
		}
	})
}

func FuzzRoundSize(f *testing.F) {
	for _, sz := range []string{"0.001", "1", "0.5", "100", "0", "-1", "abc", "0.123456789"} {
		f.Add(sz, 2)
	}
	f.Fuzz(func(t *testing.T, sz string, szDec int) {
		if szDec < 0 || szDec > 8 {
			return
		}
		out, _, err := RoundSize(sz, szDec)
		if err != nil {
			return
		}
		posDecimal(t, "RoundSize", out)
		if out2, changed2, err2 := RoundSize(out, szDec); err2 != nil || out2 != out || changed2 {
			t.Fatalf("RoundSize not idempotent: %q -> %q (changed=%v err=%v)", out, out2, changed2, err2)
		}
	})
}

func FuzzRoundOutcomePrice(f *testing.F) {
	for _, px := range []string{"0.5", "0.97", "0.0001", "0.123456", "1", "0", "-0.1", "abc"} {
		f.Add(px)
	}
	f.Fuzz(func(t *testing.T, px string) {
		out, _, err := RoundOutcomePrice(px)
		if err != nil {
			return
		}
		d := posDecimal(t, "RoundOutcomePrice", out)
		if !d.LessThan(decimal.NewFromInt(1)) {
			t.Fatalf("RoundOutcomePrice(%q) = %q is not < 1", px, out)
		}
		if out2, changed2, err2 := RoundOutcomePrice(out); err2 != nil || out2 != out || changed2 {
			t.Fatalf("RoundOutcomePrice not idempotent: %q -> %q (changed=%v err=%v)", out, out2, changed2, err2)
		}
	})
}
