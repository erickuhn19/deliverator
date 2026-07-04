package oms

import (
	"context"
	"math"
	"strconv"
	"testing"

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

func candle(t int64, close float64) hl.Candle {
	return hl.Candle{TimeOpen: t, Close: strconv.FormatFloat(close, 'f', -1, 64)}
}

func TestMarkAndVol(t *testing.T) {
	// Too few candles → not usable.
	if _, _, ok := markAndVol([]hl.Candle{candle(0, 100), candle(1, 101)}); ok {
		t.Fatal("fewer than 5 candles must be rejected")
	}
	// Flat price → mark set, vol 0.
	var flat []hl.Candle
	for i := 0; i < 10; i++ {
		flat = append(flat, candle(int64(i), 100))
	}
	if m, v, ok := markAndVol(flat); !ok || m != 100 || v != 0 {
		t.Fatalf("flat: mark=%v vol=%v ok=%v (want 100,0,true)", m, v, ok)
	}
	// Alternating 100/101 → non-zero vol; mark = latest close.
	var alt []hl.Candle
	for i := 0; i < 20; i++ {
		px := 100.0
		if i%2 == 1 {
			px = 101
		}
		alt = append(alt, candle(int64(i), px))
	}
	mark, vol, ok := markAndVol(alt)
	if !ok || vol <= 0 || mark != 101 {
		t.Fatalf("alt: mark=%v vol=%v ok=%v (want 101, >0, true)", mark, vol, ok)
	}
	exp := math.Log(101.0/100.0) * math.Sqrt(minutesPerYear) // ≈ per-step |return| annualized
	if math.Abs(vol-exp) > 0.25*exp {
		t.Fatalf("annualized vol %v not near expected %v", vol, exp)
	}
	// Unordered candles must be sorted by TimeOpen → mark is the latest.
	shuf := []hl.Candle{candle(2, 102), candle(0, 100), candle(4, 104), candle(1, 101), candle(3, 103)}
	if m, _, ok := markAndVol(shuf); !ok || m != 104 {
		t.Fatalf("sorted mark should be 104 (TimeOpen=4), got %v ok=%v", m, ok)
	}
}

type fakeCandles struct{ data map[string][]hl.Candle }

func (f *fakeCandles) Candles(_ context.Context, coin, _ string, _ *int64) ([]hl.Candle, error) {
	return f.data[coin], nil
}

func TestCandleFeedRefresh(t *testing.T) {
	r := &fakeCandles{data: map[string][]hl.Candle{}}
	for i := 0; i < 30; i++ {
		r.data["BTC"] = append(r.data["BTC"], candle(int64(i), 60000+float64(i)*10))
	}
	f := NewCandleFeed(0)                                             // 0 ⇒ staleness guard off (this test exercises mark/vol math)
	f.Refresh(context.Background(), r, []string{"BTC", "btc", "ETH"}) // dedup + case-insensitive; ETH has no data

	if m, ok := f.Mark("BTC"); !ok || m != 60290 {
		t.Fatalf("BTC mark: got %v ok=%v (want 60290 = last close)", m, ok)
	}
	if _, ok := f.Vol("btc"); !ok { // case-insensitive lookup
		t.Fatal("BTC vol should be available after refresh")
	}
	if _, ok := f.Mark("ETH"); ok {
		t.Fatal("ETH had no candle data → no mark")
	}
	if _, ok := f.Mark("SOL"); ok {
		t.Fatal("SOL was never refreshed → no mark")
	}
}
