package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestPegPrice(t *testing.T) {
	cases := []struct {
		name     string
		side     Side
		bid, ask float64
		offset   float64
		want     float64
		ok       bool
	}{
		{"buy joins the bid", Buy, 100, 101, 0, 100, true},
		{"buy sits behind the bid", Buy, 100, 101, 0.5, 99.5, true},
		{"buy improves into the spread", Buy, 100, 101, -0.4, 100.4, true},
		{"sell joins the ask", Sell, 100, 101, 0, 101, true},
		{"sell sits behind the ask", Sell, 100, 101, 0.5, 101.5, true},
		{"buy with empty bid", Buy, 0, 101, 0, 0, false},
		{"sell with empty ask", Sell, 100, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pegPrice(tc.side, tc.bid, tc.ask, tc.offset)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("pegPrice = %v,%v; want %v,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPegStringRoundsAndGuardsEmptyBook(t *testing.T) {
	c := newCfgClient(t, config.Default())
	mk, _ := c.meta.Lookup("BTC")
	if s, ok := pegString(Buy, mk, &BboView{Bid: "63000.4", Ask: "63010"}, 0); !ok || s != "63000" {
		t.Fatalf("buy peg = %q,%v; want 63000 rounded", s, ok)
	}
	if _, ok := pegString(Buy, mk, &BboView{Ask: "63010"}, 0); ok {
		t.Fatal("an empty bid must yield no peg for a buy")
	}
}

// chaseResp serves the reads/writes a chase step touches: l2Book (bbo),
// frontendOpenOrders (the resting order), orderStatus (historical), and the
// /exchange modify action.
func chaseResp(book, openOrders, orderStatus string) respFn {
	return func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "l2Book":
				return 200, book
			case "frontendOpenOrders":
				return 200, openOrders
			case "orderStatus":
				return 200, orderStatus
			}
			return 200, `{}`
		}
		// /exchange: the modify action returns an order response.
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
}

const chaseCloid = "0x000000000000000000000000000000ab"

func chaseRestingOrder(px, sz string) string {
	return `[{"coin":"BTC","oid":71,"side":"B","limitPx":"` + px + `","origSz":"` + sz +
		`","sz":"` + sz + `","orderType":"Limit","tif":"Alo","timestamp":0,"cloid":"` + chaseCloid + `"}]`
}

func newChaseState() *chaseState {
	return &chaseState{cloid: chaseCloid, side: Buy, offset: 0, placedSz: 0.0002, tif: "Alo"}
}

// Book moved above the resting price → chaseStep reprices (modify) and reports it.
func TestChaseStepReprices(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(book, chaseRestingOrder("60000", "0.0002"), ""))
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if done {
		t.Fatal("a reprice is not terminal")
	}
	if reprices != 1 {
		t.Fatalf("want 1 reprice, got %d", reprices)
	}
	last := events[len(events)-1]
	if last.Event != "repriced" || last.Px != "63000" {
		t.Fatalf("want a repriced@63000 event, got %+v", last)
	}
}

// Already pegged at the touch → no reprice, not terminal, no event.
func TestChaseStepNoRepriceWhenPegged(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(book, chaseRestingOrder("63000", "0.0002"), ""))
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if done || reprices != 0 || len(events) != 0 {
		t.Fatalf("pegged order must do nothing: done=%v reprices=%d events=%+v", done, reprices, events)
	}
}

// A reprice cap stops the chase before modifying further.
func TestChaseStepRepriceCap(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(book, chaseRestingOrder("60000", "0.0002"), ""))
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 3
	done := c.chaseStep(ctx, st, ChaseParams{MaxReprices: 3, LeaveResting: true}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if !done {
		t.Fatal("hitting the reprice cap must end the chase")
	}
	if events[len(events)-1].Event != "max_reprices" {
		t.Fatalf("want a max_reprices terminal event, got %+v", events)
	}
}

// The order left the book and history says filled → terminal "filled".
func TestChaseStepGoneFilled(t *testing.T) {
	status := `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"60000","sz":"0","oid":71,"origSz":"0.0002","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Alo","children":[]},"status":"filled","statusTimestamp":1}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(`{}`, `[]`, status))
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 2
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if !done || events[len(events)-1].Event != "filled" {
		t.Fatalf("a gone+filled order must end as filled, got done=%v %+v", done, events)
	}
}

// The order left the book and history says canceled → terminal "canceled".
func TestChaseStepGoneCanceled(t *testing.T) {
	status := `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"60000","sz":"0.0002","oid":71,"origSz":"0.0002","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Alo","children":[]},"status":"canceled","statusTimestamp":1}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(`{}`, `[]`, status))
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if !done || events[len(events)-1].Event != "canceled" {
		t.Fatalf("a gone+canceled order must end as canceled, got done=%v %+v", done, events)
	}
}

// A partial fill (less resting than placed) is reported, and the chase continues.
func TestChaseStepPartialFill(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	// placed 0.0002, only 0.00012 still resting at the current peg (63000).
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(book, chaseRestingOrder("63000", "0.00012"), ""))
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if len(events) == 0 || events[0].Event != "partial_fill" {
		t.Fatalf("want a partial_fill event, got %+v", events)
	}
	if events[0].FilledSz != "0.00008" {
		t.Errorf("filled_sz = %q, want 0.00008", events[0].FilledSz)
	}
}

// Regression (review HIGH): after a partial fill, a reprice must carry the
// REMAINING size, not re-grow the order back to its original size via OrigSz.
func TestChaseStepRepriceUsesRemainingSize(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	var modSize string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "l2Book":
				return 200, book
			case "frontendOpenOrders":
				return 200, chaseRestingOrder("60000", "0.0006") // placed 0.001, 0.0006 left
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if o, ok := action["order"].(map[string]any); ok {
				if s, ok := o["s"].(string); ok {
					modSize = s
				}
			}
		}
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	st := newChaseState()
	st.placedSz = 0.001 // placed 0.001 (~$63); 0.0006 left (~$38, above the $10 floor)
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if events[0].Event != "partial_fill" || events[len(events)-1].Event != "repriced" {
		t.Fatalf("want partial_fill then repriced, got %+v", events)
	}
	if modSize != "0.0006" {
		t.Fatalf("reprice must carry the REMAINING size 0.0006, got %q (OrigSz-regrow bug)", modSize)
	}
}

// A remainder that has fallen below the $10 floor can't be re-placed, so chase
// leaves it resting (no reprice, no per-tick reject) — only the partial_fill note.
func TestChaseStepDustRemainderNotRepriced(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(book, chaseRestingOrder("60000", "0.00012"), "")) // ~$7.56 left
	st := newChaseState()                                                                                               // placedSz 0.0002
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if done || reprices != 0 {
		t.Fatalf("dust remainder must not reprice or end the chase: done=%v reprices=%d", done, reprices)
	}
	for _, e := range events {
		if e.Event == "repriced" || e.Event == "error" {
			t.Fatalf("dust remainder must not reprice or error, got %+v", events)
		}
	}
}

// offset > 0 must peg BEHIND the touch end-to-end (not just in the pure helper).
func TestChaseStepOffsetPegsBehindTouch(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, chaseResp(book, chaseRestingOrder("63000", "0.0002"), ""))
	st := newChaseState()
	st.offset = 5 // buy pegs at bid-5 = 62995
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if len(events) == 0 || events[len(events)-1].Event != "repriced" || events[len(events)-1].Px != "62995" {
		t.Fatalf("offset 5 must peg at 62995, got %+v", events)
	}
}

// A transient BBO read error keeps the chase alive (error event, not terminal).
func TestChaseStepTransientBookErrorRetries(t *testing.T) {
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "frontendOpenOrders":
				return 200, chaseRestingOrder("63000", "0.0002")
			case "l2Book":
				return 500, `{"error":"upstream"}` // book read blips
			}
			return 200, `{}`
		}
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if done {
		t.Fatal("a transient book error must NOT end the chase")
	}
	if events[len(events)-1].Event != "error" {
		t.Fatalf("want an error event, got %+v", events)
	}
}

// Regression (review MED): a transient open-orders read error must retry, not be
// misclassified as "order gone" (which would prematurely end the chase).
func TestChaseStepTransientOpenOrdersErrorRetries(t *testing.T) {
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/info" && typ == "frontendOpenOrders" {
			return 500, `{"error":"upstream"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	st := newChaseState()
	st.mk, _ = c.meta.Lookup("BTC")

	var events []ChaseEvent
	reprices := 0
	done := c.chaseStep(ctx, st, ChaseParams{}, &reprices, func(e ChaseEvent) { events = append(events, e) })
	if done {
		t.Fatal("a transient open-orders error must NOT end the chase as gone")
	}
	if len(events) != 1 || events[0].Event != "error" {
		t.Fatalf("want a single error event, got %+v", events)
	}
}
