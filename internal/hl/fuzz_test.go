package hl

import (
	"bytes"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func encodeAction(t testing.TB, action any) []byte {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	if err := enc.Encode(action); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// TestWalkerPreservesValue: on WELL-FORMED msgpack (the real action wire), the
// str16->str8 rewrite must not change the decoded value. (Kept out of the fuzz body
// because decoding arbitrary fuzz bytes can OOM the decoder on a hostile length
// field — see FuzzConvertStr16ToStr8.)
func TestWalkerPreservesValue(t *testing.T) {
	for _, c := range vectorActions() {
		raw := encodeAction(t, c.action)
		var v1, v2 any
		if err := msgpack.Unmarshal(raw, &v1); err != nil {
			t.Fatalf("%s: decode raw: %v", c.name, err)
		}
		if err := msgpack.Unmarshal(convertStr16ToStr8(raw), &v2); err != nil {
			t.Fatalf("%s: decode converted: %v", c.name, err)
		}
		if !reflect.DeepEqual(v1, v2) {
			t.Errorf("%s: walker changed the decoded value", c.name)
		}
	}
}

// FuzzConvertStr16ToStr8 fuzzes the hand-rolled msgpack str16->str8 walker — a
// recursive parser over bytes that become the signed action hash. On ARBITRARY
// input it must:
//   - never panic (the recursive map/array paths must not index past the buffer);
//   - never hang / amplify (a hostile length field must be bounded by the buffer,
//     and output must never grow — so it can't OOM);
//   - be idempotent — a second pass is a no-op (a malformed tail is emitted raw,
//     and re-walking it must reproduce it, not duplicate it).
//
// Value-preservation on well-formed input is covered by TestWalkerPreservesValue.
func FuzzConvertStr16ToStr8(f *testing.F) {
	for _, c := range vectorActions() {
		f.Add(encodeAction(f, c.action))
	}
	f.Add([]byte{})
	f.Add([]byte{0xda, 0x00, 0x03, 'a', 'b', 'c'}) // str16 "abc"
	f.Add([]byte{0x30, 0x8b})                      // fixint + truncated fixmap (regression: dup-on-salvage)
	f.Add([]byte{0xdf, 0xdc, 0x07, 0x61, 0x61})    // map32 with a ~3.7B declared count (length bomb)

	f.Fuzz(func(t *testing.T, data []byte) {
		out := convertStr16ToStr8(data) // must not panic or hang
		if len(out) > len(data) {
			t.Fatalf("walker amplified output: in %d -> out %d bytes", len(data), len(out))
		}
		if out2 := convertStr16ToStr8(out); !bytes.Equal(out2, out) {
			t.Fatalf("walker not idempotent")
		}
	})
}

// FuzzFloatToWire asserts floatToWire never panics, rejects non-finite, and on
// success emits a string that reparses to the input and carries no NaN/Inf.
func FuzzFloatToWire(f *testing.F) {
	for _, x := range []float64{0, 1, 0.5, 64000, 65000.5, 0.00000001, 123456789} {
		f.Add(x)
	}
	f.Fuzz(func(t *testing.T, x float64) {
		out, err := floatToWire(x)
		if math.IsNaN(x) || math.IsInf(x, 0) {
			if err == nil {
				t.Errorf("non-finite %v must error, got %q", x, out)
			}
			return
		}
		if err != nil {
			return // a rounding rejection is acceptable
		}
		if strings.ContainsAny(out, "nN") || strings.Contains(out, "Inf") {
			t.Fatalf("non-finite leaked into the wire string: %q", out)
		}
		p, perr := strconv.ParseFloat(out, 64)
		if perr != nil {
			t.Fatalf("wire %q not parseable: %v", out, perr)
		}
		if math.Abs(p-x) >= 1e-9 {
			t.Fatalf("floatToWire(%v) = %q reparses to %v", x, out, p)
		}
	})
}
