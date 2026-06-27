package core

import (
	"strings"
	"testing"
)

func TestNormalizeCloid(t *testing.T) {
	// generated when empty
	g, err := normalizeCloid("")
	if err != nil || !strings.HasPrefix(g, "0x") || len(g) != 34 {
		t.Fatalf("empty cloid should generate 0x+32hex, got %q err %v", g, err)
	}
	// accepts with/without 0x, lowercases
	for _, in := range []string{
		"0x00000000000000000000000000000001",
		"00000000000000000000000000000001",
		"0xABCDEF00000000000000000000000001",
	} {
		out, err := normalizeCloid(in)
		if err != nil || len(out) != 34 || out != strings.ToLower(out) {
			t.Errorf("normalizeCloid(%q) = (%q,%v); want lowercase 34-char", in, out, err)
		}
	}
	// rejects wrong length / non-hex
	for _, bad := range []string{"0x123", "0xZZ000000000000000000000000000001", "0x000000000000000000000000000000011"} {
		if _, err := normalizeCloid(bad); err == nil {
			t.Errorf("normalizeCloid(%q) should error", bad)
		}
	}
}

func TestIsPartial(t *testing.T) {
	cases := []struct {
		status, filled, size string
		want                 bool
	}{
		{"filled", "0.5", "1.0", true},
		{"filled", "1.0", "1.0", false},
		{"resting", "", "1.0", false},
		{"filled", "", "1.0", false},
	}
	for _, c := range cases {
		r := &PlaceResult{Status: c.status, FilledSz: c.filled, Size: c.size}
		if r.IsPartial() != c.want {
			t.Errorf("IsPartial(%+v) = %v, want %v", c, r.IsPartial(), c.want)
		}
	}
}
