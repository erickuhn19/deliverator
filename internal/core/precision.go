package core

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Hyperliquid tick/lot rules (verified against docs, spec §5.5):
//   - size: rounded to the asset's szDecimals.
//   - price: ≤ MaxSigFigs significant figures AND ≤ (MaxDecimals − szDecimals)
//     decimal places; integer prices are ALWAYS allowed regardless of sig figs.
//   - MaxDecimals = 6 for perps, 8 for spot.
const (
	MaxDecimalsPerp = 6
	MaxDecimalsSpot = 8
	MaxSigFigs      = 5
	// MaxDecimalsOutcome bounds HIP-4 outcome (probability) price decimals. Live
	// mainnet order books show prices with ≤5 decimals (and ≤5 sig figs), from
	// 0.00003 up to ~0.999 — never 6 dp. 5 is the conservative choice: if the true
	// cap were 6 we lose a digit of precision but never get rejected for too many.
	MaxDecimalsOutcome = 5
)

// FromTo records an auto-round of a single field (§5.5).
type FromTo struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Rounding is the auto-round report attached to a result as a warning (§5.5).
type Rounding struct {
	Px        *FromTo `json:"px,omitempty"`
	Sz        *FromTo `json:"sz,omitempty"`
	TriggerPx *FromTo `json:"trigger_px,omitempty"`
}

// Empty reports whether nothing was rounded.
func (r *Rounding) Empty() bool {
	return r == nil || (r.Px == nil && r.Sz == nil && r.TriggerPx == nil)
}

// RoundSize rounds a size string to szDecimals. It returns the canonical rounded
// string and whether the value changed. Size must be a positive number.
func RoundSize(sz string, szDecimals int) (out string, changed bool, err error) {
	d, err := decimal.NewFromString(sz)
	if err != nil {
		return "", false, fmt.Errorf("size %q is not a number", sz)
	}
	if !d.IsPositive() {
		return "", false, fmt.Errorf("size %q must be > 0", sz)
	}
	if szDecimals < 0 {
		szDecimals = 0
	}
	r := d.Round(int32(szDecimals))
	// A positive size that rounds DOWN to 0 is a degenerate order: it would be
	// emitted with size "0" and notional 0, silently bypassing the notional caps.
	// Reject it like an explicit 0 rather than letting it through (§5.5).
	if !r.IsPositive() {
		return "", false, fmt.Errorf("size %q rounds to 0 at %d decimals (below the minimum lot)", sz, szDecimals)
	}
	return clean(r), !r.Equal(d), nil
}

// RoundPrice rounds a price string to the Hyperliquid tick rule for the asset.
// Integer prices pass through untouched. Returns the canonical rounded string
// and whether the value changed. Price must be a positive number.
func RoundPrice(px string, szDecimals int, isSpot bool) (out string, changed bool, err error) {
	d, err := decimal.NewFromString(px)
	if err != nil {
		return "", false, fmt.Errorf("price %q is not a number", px)
	}
	if !d.IsPositive() {
		return "", false, fmt.Errorf("price %q must be > 0", px)
	}
	// Integer prices are always allowed, regardless of significant figures.
	if d.Equal(d.Truncate(0)) {
		return clean(d), false, nil
	}
	maxDec := MaxDecimalsPerp - szDecimals
	if isSpot {
		maxDec = MaxDecimalsSpot - szDecimals
	}
	if maxDec < 0 {
		maxDec = 0
	}
	r := roundSigFigs(d, MaxSigFigs).Round(int32(maxDec))
	// A positive price that rounds DOWN to 0 (sub-tick on a high-szDecimals coin)
	// must not be emitted as a zero-priced order. Reject like an explicit 0.
	if !r.IsPositive() {
		return "", false, fmt.Errorf("price %q rounds to 0 at %d decimals (below the minimum tick)", px, maxDec)
	}
	return clean(r), !r.Equal(d), nil
}

// RoundOutcomePrice rounds a HIP-4 outcome price — a probability in the OPEN
// interval (0,1) — to the exchange's tick: ≤ MaxSigFigs significant figures AND
// ≤ MaxDecimalsOutcome decimal places. Unlike RoundPrice there is NO
// integer-passthrough: 0 and 1 are both invalid probabilities (Yes+No sum to 1).
// Bounds are empirical — live mainnet orders rest from ~0.00003 up to ~0.999, so
// there is no [0.001,0.999] clamp, only the open-interval guard.
func RoundOutcomePrice(px string) (out string, changed bool, err error) {
	d, err := decimal.NewFromString(px)
	if err != nil {
		return "", false, fmt.Errorf("price %q is not a number", px)
	}
	one := decimal.NewFromInt(1)
	if !d.IsPositive() || d.GreaterThanOrEqual(one) {
		return "", false, fmt.Errorf("outcome price %q must be a probability in (0,1)", px)
	}
	r := roundSigFigs(d, MaxSigFigs).Round(int32(MaxDecimalsOutcome))
	if !r.IsPositive() {
		return "", false, fmt.Errorf("price %q rounds to 0 at %d decimals (below the minimum tick)", px, MaxDecimalsOutcome)
	}
	if r.GreaterThanOrEqual(one) {
		return "", false, fmt.Errorf("price %q rounds up to >= 1 — not a valid probability", px)
	}
	return clean(r), !r.Equal(d), nil
}

// roundSigFigs rounds d to figs significant figures using exact decimal math
// (no float64 — the coefficient/exponent give the leading digit's power of ten).
func roundSigFigs(d decimal.Decimal, figs int) decimal.Decimal {
	if d.IsZero() || figs <= 0 {
		return d
	}
	coeff := d.Coefficient().String()
	if coeff[0] == '-' {
		coeff = coeff[1:]
	}
	// value = coefficient × 10^exponent ⇒ leading digit's power = exponent + (len-1).
	leadingPow := int(d.Exponent()) + (len(coeff) - 1)
	places := figs - 1 - leadingPow // decimal places to round to (may be negative)
	return d.Round(int32(places))
}

// clean renders a decimal as a minimal canonical string (no trailing zeros).
func clean(d decimal.Decimal) string {
	s := d.String()
	if !containsDot(s) {
		return s
	}
	// trim trailing zeros, then a trailing dot
	i := len(s)
	for i > 0 && s[i-1] == '0' {
		i--
	}
	if i > 0 && s[i-1] == '.' {
		i--
	}
	return s[:i]
}

func containsDot(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}
