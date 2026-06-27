package hl

import "testing"

func TestNormalizeCloid(t *testing.T) {
	full := "0x0000000000000000000000000000abcd"
	t.Run("nil", func(t *testing.T) {
		got, err := normalizeCloid(nil)
		if err != nil || got != nil {
			t.Fatalf("nil cloid: got=%v err=%v, want (nil,nil)", got, err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		empty := ""
		got, err := normalizeCloid(&empty)
		if err != nil || got != nil {
			t.Fatalf("empty cloid: got=%v err=%v, want (nil,nil)", got, err)
		}
	})
	t.Run("with_prefix", func(t *testing.T) {
		got, err := normalizeCloid(&full)
		if err != nil {
			t.Fatal(err)
		}
		if *got != full {
			t.Fatalf("got %q, want %q", *got, full)
		}
	})
	t.Run("adds_prefix", func(t *testing.T) {
		bare := "0000000000000000000000000000abcd"
		got, err := normalizeCloid(&bare)
		if err != nil {
			t.Fatal(err)
		}
		if *got != full {
			t.Fatalf("got %q, want %q", *got, full)
		}
	})
	t.Run("preserves_case", func(t *testing.T) {
		mixed := "0x0000000000000000000000000000ABCD"
		got, err := normalizeCloid(&mixed)
		if err != nil {
			t.Fatal(err)
		}
		if *got != mixed {
			t.Fatalf("case not preserved: got %q, want %q", *got, mixed)
		}
	})
	t.Run("wrong_length", func(t *testing.T) {
		short := "0x1234"
		if _, err := normalizeCloid(&short); err == nil {
			t.Fatal("expected error for short cloid")
		}
	})
	t.Run("bad_hex", func(t *testing.T) {
		bad := "0xzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
		if _, err := normalizeCloid(&bad); err == nil {
			t.Fatal("expected error for non-hex cloid")
		}
	})
}
