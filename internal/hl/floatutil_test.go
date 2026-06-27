package hl

import (
	"math"
	"testing"
)

// floatToWire must refuse non-finite values BEFORE they reach the signed bytes — a
// NaN/Inf (e.g. from a poisoned mid) would otherwise serialize as "NaN"/"+Inf" and
// be signed and a nonce consumed (audit S2).
func TestFloatToWireRejectsNonFinite(t *testing.T) {
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if s, err := floatToWire(x); err == nil {
			t.Errorf("floatToWire(%v) should error, got %q", x, s)
		}
	}
	// Finite values encode exactly as before.
	for in, want := range map[float64]string{64000: "64000", 0.25: "0.25", 0: "0"} {
		if s, err := floatToWire(in); err != nil || s != want {
			t.Errorf("floatToWire(%v) = %q, %v; want %q", in, s, err, want)
		}
	}
}
