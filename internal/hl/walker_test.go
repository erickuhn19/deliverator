package hl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// A str16 header (0xda) for a string <256 bytes must be rewritten to str8 (0xd9)
// to match Python's msgpack output, preserving the string value. (vmihailenco
// already emits str8 for 32..255 bytes, so we feed a hand-crafted str16 to
// exercise the conversion directly.)
func TestConvertStr16RoundTrip(t *testing.T) {
	long := strings.Repeat("a", 40) // 40 bytes
	in := append([]byte{0xda, 0x00, 0x28}, []byte(long)...)
	want := append([]byte{0xd9, 0x28}, []byte(long)...)

	out := convertStr16ToStr8(in)
	if !bytes.Equal(out, want) {
		t.Fatalf("str16->str8 conversion wrong:\n in:   %x\n out:  %x\n want: %x", in, out, want)
	}

	var back string
	if err := msgpack.Unmarshal(out, &back); err != nil {
		t.Fatalf("converted bytes do not decode: %v", err)
	}
	if back != long {
		t.Fatalf("value changed through conversion: %q", back)
	}
}

// The walker must copy every non-(str16<256) msgpack type byte-for-byte. These
// hand-crafted top-level values exercise the ext/bin/str32/array/map/int branches
// that real actions don't produce.
func TestWalkerPassthrough(t *testing.T) {
	cases := map[string][]byte{
		"ext8":          {0xc7, 0x03, 0x05, 0xaa, 0xbb, 0xcc},
		"ext16":         {0xc8, 0x00, 0x02, 0x05, 0xaa, 0xbb},
		"ext32":         {0xc9, 0x00, 0x00, 0x00, 0x01, 0x05, 0xaa},
		"fixext1":       {0xd4, 0x05, 0xaa},
		"fixext8":       {0xd7, 0x05, 1, 2, 3, 4, 5, 6, 7, 8},
		"bin8":          {0xc4, 0x02, 0x01, 0x02},
		"bin16":         {0xc5, 0x00, 0x02, 0x01, 0x02},
		"bin32":         {0xc6, 0x00, 0x00, 0x00, 0x02, 0x01, 0x02},
		"uint64":        {0xcf, 0, 0, 0, 0, 0, 0, 0, 1},
		"int64":         {0xd3, 0, 0, 0, 0, 0, 0, 0, 1},
		"float64":       {0xcb, 0, 0, 0, 0, 0, 0, 0, 0},
		"array16":       {0xdc, 0x00, 0x01, 0x05},
		"map16":         {0xde, 0x00, 0x01, 0xa1, 'k', 0x05},
		"nil_bool_fix":  {0xc0, 0xc2, 0xc3, 0x7f},
		"str8_preserve": {0xd9, 0x02, 'h', 'i'},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := convertStr16ToStr8(in)
			if !bytes.Equal(out, in) {
				t.Fatalf("walker altered %s:\n in:  %x\n out: %x", name, in, out)
			}
		})
	}
}

// A str16 with length >= 256 must NOT be converted (str8 can't hold it).
func TestWalkerKeepsLongStr16(t *testing.T) {
	body := bytes.Repeat([]byte{'x'}, 300)
	in := append([]byte{0xda, 0x01, 0x2c}, body...) // str16, len 300
	out := convertStr16ToStr8(in)
	if !bytes.Equal(out, in) {
		t.Fatalf("str16 with len>=256 must be left as-is")
	}
}
