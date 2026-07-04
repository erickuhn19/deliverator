package oms

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// mmCloidMarker is a fixed 4-hex tag in the leading bytes of every client order id
// the MM generates. It lets the OMS recognize its OWN resting orders (RestingForCoin)
// so the quote-diff never modifies or cancels an order the MM did not place — e.g. an
// operator's manual limit or a second strategy sharing the account. A HIP-4 cloid is
// "0x" + 32 hex (16 bytes); we spend the first 2 bytes on the marker and 14 on
// randomness (112 bits — collision-safe).
const mmCloidMarker = "de11"

// NewMMCloid returns a fresh MM-owned client order id ("0x" + marker + 28 random hex).
func NewMMCloid() string {
	var b [14]byte
	_, _ = rand.Read(b[:])
	return "0x" + mmCloidMarker + hex.EncodeToString(b[:])
}

// IsMMCloid reports whether a client order id was minted by NewMMCloid (carries the
// marker). Empty / foreign cloids return false.
func IsMMCloid(cloid string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(cloid)), "0x"+mmCloidMarker)
}
