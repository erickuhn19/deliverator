package core

import "testing"

func TestRoundSize(t *testing.T) {
	cases := []struct {
		sz         string
		szDecimals int
		want       string
		changed    bool
	}{
		{"0.1234", 3, "0.123", true},
		{"1.001", 3, "1.001", false},
		{"1.0001", 3, "1", true},   // rounds to 1.000 → "1"
		{"0.10", 3, "0.1", false},  // trailing zeros are not a change
		{"1000", 0, "1000", false}, // integer size, 0 decimals
		{"0.5", 0, "1", true},      // 0 decimals rounds half-up
	}
	for _, c := range cases {
		got, changed, err := RoundSize(c.sz, c.szDecimals)
		if err != nil {
			t.Fatalf("RoundSize(%q,%d) error: %v", c.sz, c.szDecimals, err)
		}
		if got != c.want || changed != c.changed {
			t.Errorf("RoundSize(%q,%d) = (%q,%v); want (%q,%v)", c.sz, c.szDecimals, got, changed, c.want, c.changed)
		}
	}
}

func TestRoundSizeRejectsNonPositive(t *testing.T) {
	for _, s := range []string{"0", "-1", "abc"} {
		if _, _, err := RoundSize(s, 3); err == nil {
			t.Errorf("RoundSize(%q) expected error, got nil", s)
		}
	}
}

// A positive size that rounds down to 0 must be rejected, not emitted as "0"
// (which would carry notional 0 and bypass the caps).
func TestRoundSizeRejectsRoundsToZero(t *testing.T) {
	for _, s := range []string{"0.004", "0.00000001", "1e-3"} {
		if out, _, err := RoundSize(s, 2); err == nil {
			t.Errorf("RoundSize(%q,2) should reject (rounds to 0), got out=%q", s, out)
		}
	}
	// A small value that rounds to a non-zero lot is still fine.
	if out, _, err := RoundSize("0.006", 2); err != nil || out != "0.01" {
		t.Errorf("RoundSize(0.006,2) = (%q,%v); want (0.01,nil)", out, err)
	}
}

func TestRoundPricePerp(t *testing.T) {
	// BTC: szDecimals=5 → maxDec = 6-5 = 1 decimal, AND ≤5 sig figs.
	cases := []struct {
		px         string
		szDecimals int
		isSpot     bool
		want       string
		changed    bool
	}{
		{"64000", 5, false, "64000", false},    // integer always allowed
		{"64000.1", 5, false, "64000", true},   // 6 sig figs → 5 sig figs = 64000
		{"64000.123", 5, false, "64000", true}, // 6+ sig figs → 64000
		{"1234.5", 0, false, "1234.5", false},  // 5 sig figs, maxDec=6 ok
		{"1234.56", 0, false, "1234.6", true},  // 6 sig figs → 1234.6 (half-up)
		{"0.001234", 0, false, "0.001234", false},
		{"0.0012345", 0, false, "0.001235", true}, // 5 sig figs → 6 decimals, half-up
	}
	for _, c := range cases {
		got, changed, err := RoundPrice(c.px, c.szDecimals, c.isSpot)
		if err != nil {
			t.Fatalf("RoundPrice(%q,%d,%v) error: %v", c.px, c.szDecimals, c.isSpot, err)
		}
		if got != c.want || changed != c.changed {
			t.Errorf("RoundPrice(%q,%d,%v) = (%q,%v); want (%q,%v)", c.px, c.szDecimals, c.isSpot, got, changed, c.want, c.changed)
		}
	}
}

// A positive sub-tick price that rounds down to 0 must be rejected, not emitted
// as a zero-priced order.
func TestRoundPriceRejectsRoundsToZero(t *testing.T) {
	// BTC szDecimals=5 → maxDec=1, so 0.04 rounds to 0.0.
	if out, _, err := RoundPrice("0.04", 5, false); err == nil {
		t.Errorf("RoundPrice(0.04,5) should reject (rounds to 0), got out=%q", out)
	}
	// On a coin allowing more decimals (szDecimals=1 → maxDec=5), 0.04 is valid.
	if out, _, err := RoundPrice("0.04", 1, false); err != nil || out != "0.04" {
		t.Errorf("RoundPrice(0.04,1) = (%q,%v); want (0.04,nil)", out, err)
	}
}

func TestRoundPriceIntegerAlwaysAllowed(t *testing.T) {
	// 123456 has 6 sig figs but is an integer → always allowed, unchanged.
	got, changed, err := RoundPrice("123456", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "123456" || changed {
		t.Errorf("integer price 123456 should pass through: got %q changed=%v", got, changed)
	}
}

// HIP-4 outcome prices are probabilities in the OPEN interval (0,1): ≤5 sig figs
// AND ≤5 decimals, no integer-passthrough (0 and 1 are invalid).
func TestRoundOutcomePrice(t *testing.T) {
	ok := []struct {
		px      string
		want    string
		changed bool
	}{
		{"0.5", "0.5", false},
		{"0.039", "0.039", false},
		{"0.00003", "0.00003", false},  // 5 dp, valid (live books rest this low)
		{"0.0123456", "0.01235", true}, // ≤5 sig figs / ≤5 dp
		{"0.998885", "0.99889", true},  // near-certain Yes, still < 1
	}
	for _, c := range ok {
		got, changed, err := RoundOutcomePrice(c.px)
		if err != nil {
			t.Fatalf("RoundOutcomePrice(%q) error: %v", c.px, err)
		}
		if got != c.want || changed != c.changed {
			t.Errorf("RoundOutcomePrice(%q) = (%q,%v); want (%q,%v)", c.px, got, changed, c.want, c.changed)
		}
	}
	// Out-of-band probabilities are rejected, not clamped silently.
	for _, bad := range []string{"0", "-0.1", "1", "1.5", "0.9999996"} { // last rounds up to >= 1
		if out, _, err := RoundOutcomePrice(bad); err == nil {
			t.Errorf("RoundOutcomePrice(%q) should reject, got %q", bad, out)
		}
	}
}
