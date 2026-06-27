package hl

// Float wire-encoding helpers, ported from the Hyperliquid Python SDK
// (and the reference Go port) verbatim. These feed the signed action bytes, so
// the rounding behavior must match exactly — do not "improve" them.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// floatToWire formats a price/size as the exchange expects: 8-decimal string
// with trailing zeros trimmed. It errors if rounding to 8 decimals would lose
// precision (the caller should have pre-rounded to the asset's tick/lot size).
func floatToWire(x float64) (string, error) {
	// Reject non-finite values BEFORE they reach the signed action bytes. A NaN/Inf
	// (e.g. from a poisoned mid — strconv.ParseFloat accepts "NaN"/"Inf") would
	// otherwise serialize as the literal "NaN"/"+Inf" and be signed; this is the
	// last-line guard that aborts the build before a nonce is consumed (audit S2).
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return "", fmt.Errorf("refusing to encode non-finite value: %v", x)
	}
	rounded := fmt.Sprintf("%.8f", x)
	parsed, err := strconv.ParseFloat(rounded, 64)
	if err != nil {
		return "", err
	}
	if math.Abs(parsed-x) >= 1e-12 {
		return "", fmt.Errorf("float_to_wire causes rounding: %f", x)
	}
	if rounded == "-0.00000000" {
		rounded = "0.00000000"
	}
	result := strings.TrimRight(rounded, "0")
	result = strings.TrimRight(result, ".")
	return result, nil
}

// roundToDecimals rounds value to the given number of decimals (round-half-away).
func roundToDecimals(value float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(value*pow) / pow
}

// roundToSignificantFigures rounds price to sigFigs significant figures using
// Hyperliquid's exact float64 algorithm (used to derive market-order slippage
// prices). If the integer part already has >= sigFigs digits it is returned
// whole (more sig figs than requested), matching the exchange.
func roundToSignificantFigures(price float64, sigFigs int) float64 {
	if price == 0 {
		return 0
	}
	absPrice := math.Abs(price)
	integerPart := math.Floor(absPrice)

	if integerPart > 0 {
		numIntegerDigits := 0
		temp := int(integerPart)
		for temp > 0 {
			temp /= 10
			numIntegerDigits++
		}
		if numIntegerDigits >= sigFigs {
			return math.Copysign(integerPart, price)
		}
		sigFigsLeft := sigFigs - numIntegerDigits
		rounded := roundToDecimals(absPrice, sigFigsLeft)
		return math.Copysign(rounded, price)
	}

	multiplications := 0
	for absPrice < 1 {
		absPrice *= 10
		multiplications++
	}
	rounded := roundToDecimals(absPrice, sigFigs-1)
	return math.Copysign(rounded/math.Pow(10, float64(multiplications)), price)
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0.0
	}
	return f
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
