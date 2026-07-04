package strategy

import (
	"math"
	"testing"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// approx compares floats at half-a-tick tolerance — finer than a tick would be
// meaningless because everything ultimately snaps to the 5-dp outcome grid.
func approx(a, b float64) bool { return math.Abs(a-b) < 5e-6 }

// splitSides partitions a QuoteSet into (bids, asks) for assertions.
func splitSides(qs mm.QuoteSet) (bids, asks []mm.Quote) {
	for _, q := range qs.Quotes {
		if q.Side == core.Buy {
			bids = append(bids, q)
		} else {
			asks = append(asks, q)
		}
	}
	return
}

// baseParams is a generous, non-dropping config: no min-notional, no min-edge, so a
// test can observe the raw ladder geometry before layering constraints on top. HeldShares
// is large so the no-naked-short ask cap never trims the geometry — the skew (netInv) and
// the ask cap (HeldShares) are independent inputs; dedicated tests exercise the cap.
func baseParams() Params {
	return Params{
		BaseSpread:         0.02,
		LevelStep:          0.01,
		Levels:             3,
		BaseSizeShares:     10,
		SizeStepShares:     5,
		InventorySkew:      1.0,
		MaxInventoryShares: 100,
		MinEdge:            0,
		MinNotionalUSD:     0,
		SpreadMult:         0,    // ⇒ effective 1.0
		HeldShares:         1000, // plenty of inventory: asks are never cap-trimmed here
	}
}

// When the fair sits well below the market, the raw asks (fair+spread) would cross the
// market bid and be post-only-rejected. The book clamp must pull them to just inside
// the touch so every quote rests, and the ladder must not self-cross.
func TestBookClampMakerValid(t *testing.T) {
	p := Params{BaseSpread: 0.008, Levels: 3, LevelStep: 0.005, BaseSizeShares: 10, MaxInventoryShares: 500, SpreadMult: 1, HeldShares: 500}
	book := mm.BookTop{Bid: 0.73, Ask: 0.74, HasBid: true, HasAsk: true}
	qs := BuildQuoteSet("#10", 0.70, book, 0, p) // fair 0.70 ≪ market, holds inventory so asks exist

	sawTouchAsk := false
	for _, q := range qs.Quotes {
		if q.Side == core.Sell {
			if q.Px <= book.Bid {
				t.Fatalf("ask %.5f crosses/at the bid %.5f — would Alo-reject", q.Px, book.Bid)
			}
			if math.Abs(q.Px-(book.Bid+dedupeEpsilon)) < 1e-9 {
				sawTouchAsk = true
			}
		}
		if q.Side == core.Buy && q.Px >= book.Ask {
			t.Fatalf("bid %.5f crosses the ask %.5f", q.Px, book.Ask)
		}
	}
	if !sawTouchAsk {
		t.Fatal("a fair below the market should clamp an ask to just above the bid (sell lean)")
	}
	// Empty book ⇒ no clamp: the ladder is the deterministic fair±spread shape.
	if raw := BuildQuoteSet("#10", 0.70, mm.BookTop{}, 0, p); len(raw.Quotes) == 0 {
		t.Fatal("empty-book quote set should still build a ladder")
	}
}

func TestSymmetricLadderNoInventory(t *testing.T) {
	p := baseParams()
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	bids, asks := splitSides(qs)

	if len(bids) != 3 || len(asks) != 3 {
		t.Fatalf("want 3+3 rungs, got %d bids %d asks", len(bids), len(asks))
	}
	// r == fair (no inventory), half == 0.02, step 0.01.
	wantBid := []float64{0.48, 0.47, 0.46}
	wantAsk := []float64{0.52, 0.53, 0.54}
	wantSz := []int64{10, 15, 20}
	for i := range wantBid {
		if !approx(bids[i].Px, wantBid[i]) {
			t.Errorf("bid[%d] px = %v want %v", i, bids[i].Px, wantBid[i])
		}
		if !approx(asks[i].Px, wantAsk[i]) {
			t.Errorf("ask[%d] px = %v want %v", i, asks[i].Px, wantAsk[i])
		}
		if bids[i].Sz != wantSz[i] || asks[i].Sz != wantSz[i] {
			t.Errorf("rung %d sizes = %d/%d want %d", i, bids[i].Sz, asks[i].Sz, wantSz[i])
		}
		if bids[i].Level != i || asks[i].Level != i {
			t.Errorf("rung %d level tags = %d/%d", i, bids[i].Level, asks[i].Level)
		}
	}
	// Ladder is symmetric about fair: touch bid and touch ask equidistant.
	if !approx(0.5-bids[0].Px, asks[0].Px-0.5) {
		t.Errorf("ladder not symmetric about fair")
	}
	// Never self-cross.
	assertNoCross(t, qs)
}

func TestLongInventorySkewsReservationDown(t *testing.T) {
	p := baseParams()
	// Half the cap long: fill = 0.5, r = 0.5 - 1.0*0.5*0.02 = 0.49.
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 50, p)
	bids, asks := splitSides(qs)
	if len(bids) == 0 || len(asks) == 0 {
		t.Fatalf("expected two-sided quotes below the cap")
	}
	midTouch := (bids[0].Px + asks[0].Px) / 2
	if !approx(midTouch, 0.49) {
		t.Errorf("reservation mid = %v, want 0.49 (leaned below fair)", midTouch)
	}
	if midTouch >= 0.5 {
		t.Errorf("long inventory should push reservation BELOW fair, got %v", midTouch)
	}
}

func TestShortInventorySkewsReservationUp(t *testing.T) {
	p := baseParams()
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, -50, p)
	bids, asks := splitSides(qs)
	midTouch := (bids[0].Px + asks[0].Px) / 2
	if !approx(midTouch, 0.51) {
		t.Errorf("reservation mid = %v, want 0.51 (leaned above fair)", midTouch)
	}
}

func TestLongCapSuppressesBids(t *testing.T) {
	p := baseParams()
	// netInv >= MaxInventoryShares ⇒ reduce only ⇒ sells, no buys.
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 100, p)
	bids, asks := splitSides(qs)
	if len(bids) != 0 {
		t.Errorf("at long cap expected no bids, got %d", len(bids))
	}
	if len(asks) == 0 {
		t.Errorf("at long cap expected reducing asks, got none")
	}
}

func TestShortCapSuppressesAsks(t *testing.T) {
	p := baseParams()
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, -100, p)
	bids, asks := splitSides(qs)
	if len(asks) != 0 {
		t.Errorf("at short cap expected no asks, got %d", len(asks))
	}
	if len(bids) == 0 {
		t.Errorf("at short cap expected reducing bids, got none")
	}
}

// No naked short: with zero holdings of this coin the ask ladder must be empty even
// though the reservation would otherwise place asks (the venue rejects selling a coin
// you don't own; the engine expresses a short by buying the sibling NO coin instead).
func TestNoNakedShortSuppressesAsksWhenFlat(t *testing.T) {
	p := baseParams()
	p.HeldShares = 0
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	bids, asks := splitSides(qs)
	if len(asks) != 0 {
		t.Errorf("flat (0 held) must place no asks, got %d", len(asks))
	}
	if len(bids) == 0 {
		t.Error("flat should still bid (buying is always allowed)")
	}
}

// The ask ladder's cumulative size can never exceed the held shares of that coin.
func TestAsksCappedToHeldShares(t *testing.T) {
	p := baseParams()
	p.MinNotionalUSD = 10 // touch ask 0.52 ⇒ ceil(10/0.52)=20 shares at rung 0
	p.HeldShares = 25     // room for the 20-share touch rung but not the next
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	_, asks := splitSides(qs)
	var total int64
	for _, a := range asks {
		total += a.Sz
	}
	if total > p.HeldShares {
		t.Errorf("ask size %d exceeds held %d (naked short)", total, p.HeldShares)
	}
	if total == 0 {
		t.Error("holding 25 shares with a 20-share min rung should still quote one ask")
	}
}

func TestNearOneFairClampsWithoutDupesOrCross(t *testing.T) {
	p := baseParams()
	p.Levels = 5
	// Fair pinned at the top of the band: every ask clamps to OutcomeMaxPrice.
	qs := BuildQuoteSet("#0", mm.OutcomeMaxPrice, mm.BookTop{}, 0, p)
	assertNoDupes(t, qs)
	assertNoCross(t, qs)
	_, asks := splitSides(qs)
	// All asks collapse to the single max-price tick ⇒ dedupe leaves at most one.
	if len(asks) > 1 {
		t.Errorf("near-1 fair should collapse asks to <=1 rung, got %d", len(asks))
	}
	for _, a := range asks {
		if a.Px > mm.OutcomeMaxPrice+1e-9 {
			t.Errorf("ask px %v exceeds band max", a.Px)
		}
	}
}

func TestNearZeroFairClampsWithoutDupesOrCross(t *testing.T) {
	p := baseParams()
	p.Levels = 5
	qs := BuildQuoteSet("#0", mm.OutcomeMinPrice, mm.BookTop{}, 0, p)
	assertNoDupes(t, qs)
	assertNoCross(t, qs)
	bids, _ := splitSides(qs)
	if len(bids) > 1 {
		t.Errorf("near-0 fair should collapse bids to <=1 rung, got %d", len(bids))
	}
	for _, b := range bids {
		if b.Px < mm.OutcomeMinPrice-1e-9 {
			t.Errorf("bid px %v below band min", b.Px)
		}
	}
}

func TestMinNotionalBumpsSmallSizes(t *testing.T) {
	p := baseParams()
	p.Levels = 1
	p.BaseSizeShares = 1
	p.MinNotionalUSD = 10
	// fair 0.5, half 0.02 ⇒ touch bid 0.48, ask 0.52.
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	bids, asks := splitSides(qs)
	// bid: ceil(10/0.48)=21 ; ask: ceil(10/0.52)=20. Both exceed base=1.
	if bids[0].Sz != 21 {
		t.Errorf("bid size = %d want 21 (ceil 10/0.48)", bids[0].Sz)
	}
	if asks[0].Sz != 20 {
		t.Errorf("ask size = %d want 20 (ceil 10/0.52)", asks[0].Sz)
	}
	// And the bumped legs actually clear the minimum notional.
	if bids[0].Px*float64(bids[0].Sz) < 10 {
		t.Errorf("bid notional below minimum after bump")
	}
}

func TestMinNotionalDoesNotShrinkLargeSizes(t *testing.T) {
	p := baseParams()
	p.Levels = 1
	p.BaseSizeShares = 1000
	p.MinNotionalUSD = 10
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	bids, _ := splitSides(qs)
	if bids[0].Sz != 1000 {
		t.Errorf("size shrank below base: got %d want 1000", bids[0].Sz)
	}
}

func TestMinEdgeDropsInsideRungs(t *testing.T) {
	p := baseParams()
	p.Levels = 3
	p.BaseSpread = 0.01 // touch at ±0.01 from fair
	p.LevelStep = 0.01
	p.MinEdge = 0.015 // drops the touch rung (edge 0.01) but keeps L1 (0.02), L2 (0.03)
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	bids, asks := splitSides(qs)
	if len(bids) != 2 || len(asks) != 2 {
		t.Fatalf("MinEdge should drop only the touch rung: got %d bids %d asks", len(bids), len(asks))
	}
	// Every surviving rung sits at least MinEdge from fair.
	for _, q := range qs.Quotes {
		if math.Abs(q.Px-0.5) < p.MinEdge {
			t.Errorf("rung px %v is inside MinEdge of fair", q.Px)
		}
	}
}

func TestZeroLevelsEmitsNothing(t *testing.T) {
	p := baseParams()
	p.Levels = 0
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	if len(qs.Quotes) != 0 {
		t.Errorf("Levels=0 should emit no quotes, got %d", len(qs.Quotes))
	}
	if qs.Coin != "#0" {
		t.Errorf("coin not carried through")
	}
}

func TestSpreadMultWidens(t *testing.T) {
	p := baseParams()
	p.Levels = 1
	p.SpreadMult = 2.0 // half becomes 0.04
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	bids, asks := splitSides(qs)
	if !approx(bids[0].Px, 0.46) || !approx(asks[0].Px, 0.54) {
		t.Errorf("SpreadMult=2 should widen touch to 0.46/0.54, got %v/%v", bids[0].Px, asks[0].Px)
	}
}

func TestReservationClampGuardsZeroCap(t *testing.T) {
	p := baseParams()
	p.MaxInventoryShares = 0 // max(...,1) guard must avoid divide-by-zero
	// With cap floored to 1 and netInv 0, no skew; just assert it doesn't panic/NaN.
	qs := BuildQuoteSet("#0", 0.5, mm.BookTop{}, 0, p)
	for _, q := range qs.Quotes {
		if math.IsNaN(q.Px) || math.IsInf(q.Px, 0) {
			t.Fatalf("produced non-finite price with zero cap")
		}
	}
}

func TestCoherentDualBook(t *testing.T) {
	cases := []struct {
		yes, no float64
		want    bool
	}{
		{0.4, 0.5, true},  // sum 0.9 < 1
		{0.5, 0.5, true},  // sum exactly 1 — zero-edge, allowed
		{0.6, 0.5, false}, // sum 1.1 > 1 — self-arb
		{0.0, 0.0, true},  // degenerate empty
		{0.99, 0.02, false},
	}
	for i, c := range cases {
		if got := CoherentDualBook(c.yes, c.no); got != c.want {
			t.Errorf("case %d Coherent(%v,%v)=%v want %v", i, c.yes, c.no, got, c.want)
		}
	}
}

func TestCapDualBook(t *testing.T) {
	// Already coherent ⇒ untouched.
	y, n := CapDualBook(0.4, 0.5)
	if y != 0.4 || n != 0.5 {
		t.Errorf("coherent pair should pass through, got %v/%v", y, n)
	}

	// Over parity ⇒ proportional shave to 1-epsilon, ratio preserved.
	y, n = CapDualBook(0.8, 0.4) // sum 1.2
	sum := y + n
	if sum > 1.0 {
		t.Errorf("capped sum %v still exceeds 1", sum)
	}
	if !approx(sum, 1.0-dedupeEpsilon) {
		t.Errorf("capped sum = %v, want %v", sum, 1.0-dedupeEpsilon)
	}
	// Original ratio 0.8:0.4 == 2:1 preserved.
	if !approx(y/n, 2.0) {
		t.Errorf("proportional shave should preserve 2:1 ratio, got %v", y/n)
	}
	if !CoherentDualBook(y, n) {
		t.Errorf("capped pair must be coherent")
	}
}

// ---- shared invariant assertions ----

func assertNoCross(t *testing.T, qs mm.QuoteSet) {
	t.Helper()
	bids, asks := splitSides(qs)
	for _, b := range bids {
		for _, a := range asks {
			if b.Px >= a.Px {
				t.Errorf("self-cross: bid %v >= ask %v", b.Px, a.Px)
			}
		}
	}
}

func assertNoDupes(t *testing.T, qs mm.QuoteSet) {
	t.Helper()
	seen := map[[2]int64]bool{}
	for _, q := range qs.Quotes {
		k := [2]int64{int64(q.Side), priceKey(q.Px)}
		if seen[k] {
			t.Errorf("duplicate rung: side %v px %v", q.Side, q.Px)
		}
		seen[k] = true
	}
}
