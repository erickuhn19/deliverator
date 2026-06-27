package hl

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// normalizeCloid normalizes a client order id to the wire form: hex WITH a 0x
// prefix, exactly 16 bytes (34 chars including 0x). A nil/empty cloid returns
// (nil, nil) so the order wire omits the field. Case is preserved.
func normalizeCloid(cloid *string) (*string, error) {
	if cloid == nil || *cloid == "" {
		return nil, nil
	}
	v := *cloid
	if !strings.HasPrefix(v, "0x") {
		v = "0x" + v
	}
	if len(v) != 34 {
		return nil, fmt.Errorf("cloid must be exactly 32 hex characters (got %d excluding 0x prefix): %s", len(v)-2, v)
	}
	if _, err := hex.DecodeString(v[2:]); err != nil {
		return nil, fmt.Errorf("cloid must be valid hex string: %w", err)
	}
	return &v, nil
}
