package oms

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
)

func q(side core.Side, px float64, sz int64) mm.Quote { return mm.Quote{Side: side, Px: px, Sz: sz} }

func TestDiffPlaceModifyCancel(t *testing.T) {
	desired := mm.QuoteSet{Coin: "#10", Quotes: []mm.Quote{
		q(core.Buy, 0.40, 10),
		q(core.Buy, 0.38, 10),
		q(core.Sell, 0.60, 10),
	}}
	resting := []RestingOrder{
		{Oid: 1, Coin: "#10", Side: core.Buy, Px: 0.40, RemainingSz: 10},  // matches best bid → no-op
		{Oid: 2, Coin: "#10", Side: core.Buy, Px: 0.35, RemainingSz: 10},  // 2nd bid, wrong px → modify to 0.38
		{Oid: 3, Coin: "#10", Side: core.Sell, Px: 0.65, RemainingSz: 10}, // ask wrong px → modify to 0.60
		{Oid: 4, Coin: "#10", Side: core.Sell, Px: 0.70, RemainingSz: 10}, // extra ask → cancel
	}
	d := Diff(desired, resting, 5e-6)
	if len(d.Place) != 0 {
		t.Fatalf("no places expected, got %+v", d.Place)
	}
	if len(d.Modify) != 2 {
		t.Fatalf("want 2 modifies, got %d: %+v", len(d.Modify), d.Modify)
	}
	if len(d.Cancel) != 1 || d.Cancel[0].Oid != 4 {
		t.Fatalf("want cancel oid 4, got %+v", d.Cancel)
	}
	// The 2nd-bid modify must carry the NEW size explicitly (never empty/OrigSz).
	for _, m := range d.Modify {
		if m.NewSz != 10 {
			t.Fatalf("modify must set explicit size, got %+v", m)
		}
	}
}

func TestDiffNoOpWhenMatched(t *testing.T) {
	desired := mm.QuoteSet{Coin: "#10", Quotes: []mm.Quote{q(core.Buy, 0.40, 10), q(core.Sell, 0.60, 5)}}
	resting := []RestingOrder{
		{Oid: 1, Side: core.Buy, Px: 0.40, RemainingSz: 10},
		{Oid: 2, Side: core.Sell, Px: 0.60, RemainingSz: 5},
	}
	if d := Diff(desired, resting, 5e-6); !d.Empty() {
		t.Fatalf("matched book should diff empty, got %+v", d)
	}
}

func TestDiffPlacesWhenNoResting(t *testing.T) {
	desired := mm.QuoteSet{Coin: "#10", Quotes: []mm.Quote{q(core.Buy, 0.40, 10), q(core.Sell, 0.60, 10)}}
	d := Diff(desired, nil, 5e-6)
	if len(d.Place) != 2 || len(d.Modify) != 0 || len(d.Cancel) != 0 {
		t.Fatalf("want 2 places on empty book, got %+v", d)
	}
}

func TestDiffCancelsAllWhenNoDesired(t *testing.T) {
	resting := []RestingOrder{{Oid: 1, Side: core.Buy, Px: 0.4, RemainingSz: 10}, {Oid: 2, Side: core.Sell, Px: 0.6, RemainingSz: 10}}
	d := Diff(mm.QuoteSet{Coin: "#10"}, resting, 5e-6)
	if len(d.Cancel) != 2 {
		t.Fatalf("want all cancelled, got %+v", d)
	}
}

func TestRestingForCoin(t *testing.T) {
	bid, ask := NewMMCloid(), NewMMCloid()
	trig, other, red := NewMMCloid(), NewMMCloid(), NewMMCloid()
	foreign := "0x" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // a manual/foreign order — must be ignored
	orders := []hl.FrontendOpenOrder{
		{Coin: "#10", Oid: 1, Side: hl.OrderSideBid, LimitPx: 0.40, Sz: 10, Cloid: &bid},
		{Coin: "#10", Oid: 2, Side: hl.OrderSideAsk, LimitPx: 0.60, Sz: 7, Cloid: &ask},
		{Coin: "#10", Oid: 3, Side: hl.OrderSideBid, LimitPx: 0.30, Sz: 5, IsTrigger: true, Cloid: &trig}, // dropped: trigger
		{Coin: "#11", Oid: 4, Side: hl.OrderSideBid, LimitPx: 0.20, Sz: 5, Cloid: &other},                 // other coin
		{Coin: "#10", Oid: 5, Side: hl.OrderSideAsk, LimitPx: 0.55, Sz: 5, ReduceOnly: true, Cloid: &red}, // dropped: reduce-only
		{Coin: "#10", Oid: 6, Side: hl.OrderSideBid, LimitPx: 0.38, Sz: 4, Cloid: &foreign},               // dropped: NOT MM-owned
	}
	got := RestingForCoin(orders, "#10")
	if len(got) != 2 {
		t.Fatalf("want 2 MM-owned resting on #10, got %d: %+v", len(got), got)
	}
	if got[0].Side != core.Buy || got[0].RemainingSz != 10 || !IsMMCloid(got[0].Cloid) {
		t.Fatalf("bid row wrong: %+v", got[0])
	}
	if got[1].Side != core.Sell || got[1].RemainingSz != 7 {
		t.Fatalf("ask row wrong: %+v", got[1])
	}
	// The foreign order (oid 6) must never be adopted — the MM must not touch it.
	for _, r := range got {
		if r.Oid == 6 {
			t.Fatal("adopted a non-MM order — the ownership filter failed")
		}
	}
}

func TestInventoryHelpers(t *testing.T) {
	positions := []core.PositionView{
		{Coin: "#6410", Class: "outcome", Szi: "30", Side: "long"},
		{Coin: "#6411", Class: "outcome", Szi: "12", Side: "long"},
		{Coin: "BTC", Class: "", Szi: "-1.5"}, // perp ignored
	}
	byCoin := InventoryByCoin(positions)
	if byCoin["#6410"] != 30 || byCoin["#6411"] != 12 {
		t.Fatalf("inventory wrong: %+v", byCoin)
	}
	if _, ok := byCoin["BTC"]; ok {
		t.Fatal("perp position should be excluded")
	}
	inv := PairInventory(core.Market{Outcome: 641, Side: "Yes"}, byCoin)
	if inv.Yes != 30 || inv.No != 12 || inv.Net() != 18 {
		t.Fatalf("pair inventory wrong: %+v", inv)
	}
}

func TestPnLAccountant(t *testing.T) {
	a := NewPnLAccountant()
	// Buy 10 Yes @ 0.40 (cost 4.00), fee 0.01.
	a.IngestFills([]hl.Fill{{Tid: 1, Coin: "#10", Side: "B", Price: "0.40", Size: "10", Fee: "0.01", ClosedPnl: "0"}})
	// Re-ingest same Tid — must be ignored (dedup).
	a.IngestFills([]hl.Fill{{Tid: 1, Coin: "#10", Side: "B", Price: "0.40", Size: "10", Fee: "0.01", ClosedPnl: "0"}})

	v := a.View(map[string]float64{"#10": 0.50})
	if v.Fees != 0.01 {
		t.Fatalf("fees deduped wrong: %v", v.Fees)
	}
	// Open = 10*0.50 - 4.00 = 1.00.
	if math.Abs(v.Open-1.0) > 1e-9 {
		t.Fatalf("open PnL: got %v want 1.0", v.Open)
	}
	if math.Abs(v.Net-(0-0.01+1.0)) > 1e-9 {
		t.Fatalf("net PnL: got %v", v.Net)
	}

	// Sell 4 @ 0.55 with exchange-booked closedPnl 0.60, fee 0.02.
	a.IngestFills([]hl.Fill{{Tid: 2, Coin: "#10", Side: "A", Price: "0.55", Size: "4", Fee: "0.02", ClosedPnl: "0.60"}})
	v = a.View(map[string]float64{"#10": 0.50})
	if math.Abs(v.Realized-0.60) > 1e-9 {
		t.Fatalf("realized from closedPnl: got %v want 0.60", v.Realized)
	}
	// Remaining 6 shares, cost 6*0.40=2.40; open at 0.50 = 3.00-2.40 = 0.60.
	if math.Abs(v.Open-0.60) > 1e-9 {
		t.Fatalf("open after partial sell: got %v want 0.60", v.Open)
	}

	// Settle the winning side: 6 shares pay $1 each, cost 2.40 released.
	a.RealizeSettlement("#10", 1.0)
	v = a.View(nil)
	if math.Abs(v.Realized-(0.60+(6*1.0-2.40))) > 1e-9 {
		t.Fatalf("realized after settlement: got %v", v.Realized)
	}
	if v.Open != 0 {
		t.Fatalf("open should be 0 after settlement, got %v", v.Open)
	}
}

// fakeStreamer replays canned allMids frames then blocks until ctx cancel.
type fakeStreamer struct{ frames []string }

func (s *fakeStreamer) Stream(ctx context.Context, _ []core.StreamSub, onEvent func(core.StreamEvent)) error {
	for _, fr := range s.frames {
		onEvent(core.StreamEvent{Channel: "allMids", Data: json.RawMessage(fr)})
	}
	<-ctx.Done()
	return nil
}

func TestFeedMarksAndVol(t *testing.T) {
	f := NewFeed([]string{"BTC"}, 0.94)
	// Feed a rising BTC series + an outcome mid via wrapped allMids frames.
	base := time.Unix(1_700_000_000, 0)
	i := 0
	f.now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Second) }
	frames := []string{
		`{"mids":{"BTC":"60000","#6410":"0.42"}}`,
		`{"mids":{"BTC":"60300","#6410":"0.43"}}`,
		`{"mids":{"BTC":"59900"}}`,
	}
	for _, fr := range frames {
		f.onEvent(core.StreamEvent{Channel: "allMids", Data: json.RawMessage(fr)})
	}
	if mk, ok := f.Mark("BTC"); !ok || mk != 59900 {
		t.Fatalf("BTC mark: got %v ok=%v want 59900", mk, ok)
	}
	if mid, ok := f.Mid("#6410"); !ok || mid != 0.43 {
		t.Fatalf("outcome mid: got %v ok=%v want 0.43", mid, ok)
	}
	// reconnect frame must be a no-op (not clobber marks).
	f.onEvent(core.StreamEvent{Channel: "reconnect"})
	if mk, _ := f.Mark("BTC"); mk != 59900 {
		t.Fatalf("reconnect frame changed marks: %v", mk)
	}
	// Untracked underlying has no vol; BTC may or may not be warmed up but must not panic.
	if _, ok := f.Vol("ETH"); ok {
		t.Fatal("untracked ETH should have no vol")
	}
}

func TestFeedMarkStale(t *testing.T) {
	f := NewFeed([]string{"BTC"}, 0.94)
	base := time.Unix(1_700_000_000, 0)
	f.now = func() time.Time { return base }
	f.onEvent(core.StreamEvent{Channel: "allMids", Data: json.RawMessage(`{"mids":{"BTC":"60000"}}`)})
	if _, ok := f.Mark("BTC"); !ok {
		t.Fatal("a just-updated mark must be fresh")
	}
	// Advance past maxMarkAge (15s): a frozen stream must report stale, not price on.
	f.now = func() time.Time { return base.Add(30 * time.Second) }
	if _, ok := f.Mark("BTC"); ok {
		t.Fatal("a 30s-old mark must report ok=false (stale)")
	}
}

func TestFeedRunReturnsOnCancel(t *testing.T) {
	f := NewFeed([]string{"BTC"}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, &fakeStreamer{frames: []string{`{"mids":{"BTC":"60000"}}`}}) }()
	time.Sleep(20 * time.Millisecond)
	if mk, ok := f.Mark("BTC"); !ok || mk != 60000 {
		t.Fatalf("feed did not ingest frame before cancel: %v", mk)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
