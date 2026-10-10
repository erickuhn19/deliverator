package papertrade

import (
	"math/big"
	"strings"
	"unicode"
)

// ParseMargin parses a base-10 USDC amount into the venue's 18-decimal raw
// integer. The accepted form is the venue client's: /^(?:0|[1-9]\d*)(?:\.\d{1,18})?$/
// Scientific notation and leading zeros are rejected so a float round-trip
// cannot change the signed margin.
func ParseMargin(s string) (*big.Int, error) { return parseDecimal18(s) }

func parseDecimal18(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" || !decimal18Re(s) {
		return nil, errAmount
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac = frac + strings.Repeat("0", 18-len(frac))
	raw := strings.TrimLeft(whole+frac, "0")
	if raw == "" {
		raw = "0"
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, errAmount
	}
	return n, nil
}

func decimal18Re(s string) bool {
	dot := strings.IndexByte(s, '.')
	whole, frac := s, ""
	if dot >= 0 {
		if strings.Count(s, ".") != 1 {
			return false
		}
		whole, frac = s[:dot], s[dot+1:]
		if len(frac) < 1 || len(frac) > 18 {
			return false
		}
	}
	if whole == "" {
		return false
	}
	for _, r := range whole + frac {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	if len(whole) > 1 && whole[0] == '0' {
		return false
	}
	return true
}

// FormatRaw18 renders a raw 18-decimal integer as a fixed decimal string
// without scientific notation and without trailing zeros after the point.
func FormatRaw18(n *big.Int) string { return formatRaw18(n) }

// formatRaw18 renders a raw 18-decimal integer as a fixed decimal string
// without scientific notation and without trailing zeros after the point.
func formatRaw18(n *big.Int) string {
	if n == nil || n.Sign() == 0 {
		return "0"
	}
	s := n.String()
	if len(s) <= 18 {
		s = strings.Repeat("0", 18-len(s)+1) + s
	}
	whole, frac := s[:len(s)-18], s[len(s)-18:]
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// notionalUSD is marginRaw * leverage / 1e18 as a float for the shared risk
// caps. Values up to the venue's $10M position cap are exactly representable
// enough for a cap comparison (the cap itself is a float in config).
func notionalUSD(marginRaw *big.Int, leverage int) float64 {
	if marginRaw == nil || leverage <= 0 {
		return 0
	}
	r := new(big.Rat).SetInt(marginRaw)
	r.Mul(r, big.NewRat(int64(leverage), 1))
	r.Quo(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	f, _ := r.Float64()
	return f
}

func cmpRaw(a *big.Int, decimal string) (int, bool) {
	b, ok := new(big.Int).SetString(strings.TrimSpace(decimal), 10)
	if !ok {
		return 0, false
	}
	return a.Cmp(b), true
}

var (
	oneRaw128 = new(big.Int).Lsh(big.NewInt(1), 128)
	errAmount = errString("margin must be a base-10 amount with at most 18 decimal places (example: 1000 or 1000.5)")
)

type errString string

func (e errString) Error() string { return string(e) }
