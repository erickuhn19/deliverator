package core

import (
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// posTpslResp serves mids + a clearinghouse state (caller-supplied) + a grouped
// order response, capturing the signed action's grouping and its wire orders so
// tests can assert positionTpsl grouping, the closing side ("b"), and that the
// cloid ("c") rides exactly one leg.
func posTpslResp(state, exResp string, grouping *string, orders *[]any) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, state
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if g, ok := action["grouping"].(string); ok && grouping != nil {
				*grouping = g
			}
			if os, ok := action["orders"].([]any); ok && orders != nil {
				*orders = os
			}
		}
		return 200, exResp
	}
}

func orderMap(t *testing.T, orders []any, i int) map[string]any {
	t.Helper()
	if i >= len(orders) {
		t.Fatalf("want >= %d wire orders, got %d", i+1, len(orders))
	}
	m, ok := orders[i].(map[string]any)
	if !ok {
		t.Fatalf("wire order %d not an object: %T", i, orders[i])
	}
	return m
}

// A long position protected by tp+sl: one positionTpsl group of two reduce-only
// SELL triggers that rest, the cloid on exactly the first leg, + a position_tpsl
// audit row.
func TestPlacePositionTpslLong(t *testing.T) {
	var grouping string
	var orders []any
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":["waitingForTrigger","waitingForTrigger"]}}}`
	state := clearingWith("100000", posWith("BTC", "0.05", "3200")) // long 0.05 BTC
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, exResp, &grouping, &orders))

	res, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", TP: "72000", SL: "58000"})
	if err != nil {
		t.Fatal(err)
	}
	if grouping != "positionTpsl" {
		t.Fatalf("must be sent as a positionTpsl group, got %q", grouping)
	}
	if len(orders) != 2 {
		t.Fatalf("want 2 wire orders, got %d", len(orders))
	}
	// A long is protected by SELL triggers (b=false) on both legs.
	for i := range orders {
		if b, _ := orderMap(t, orders, i)["b"].(bool); b {
			t.Errorf("leg %d must be a SELL for a long position (b=false), got b=true", i)
		}
	}
	// The cloid rides exactly one leg — HL rejects a duplicate cloid in one action.
	if _, ok := orderMap(t, orders, 0)["c"]; !ok {
		t.Errorf("first leg must carry the cloid")
	}
	if _, ok := orderMap(t, orders, 1)["c"]; ok {
		t.Errorf("second leg must NOT carry a cloid (duplicate would be rejected)")
	}
	if len(res) != 2 {
		t.Fatalf("want tp+sl legs, got %d: %+v", len(res), res)
	}
	for i, name := range []string{"tp", "sl"} {
		if res[i].Side != name || !res[i].ReduceOnly || res[i].Status != "waitingForTrigger" {
			t.Errorf("%s leg wrong: %+v", name, res[i])
		}
	}
	if a := readAudit(t); len(a) == 0 || a[len(a)-1]["action"] != "position_tpsl" {
		t.Errorf("expected a position_tpsl audit row, got %v", a)
	}
}

// A short position's protective stop-loss is a BUY trigger, side derived from szi.
func TestPlacePositionTpslShortDerivesBuySide(t *testing.T) {
	var orders []any
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":["waitingForTrigger"]}}}`
	state := clearingWith("100000", posWith("BTC", "-0.05", "3200")) // short
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, exResp, nil, &orders))

	// short stop-loss sits ABOVE the mark, protected by a buy-to-cover.
	if _, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", SL: "70000"}); err != nil {
		t.Fatal(err)
	}
	if b, ok := orderMap(t, orders, 0)["b"].(bool); !ok || !b {
		t.Fatalf("a short's protective trigger must be a BUY (b=true), got b=%v", orders)
	}
}

// A take-profit-only order: one reduce-only leg, side derived from the position.
func TestPlacePositionTpslTpOnly(t *testing.T) {
	var orders []any
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":["waitingForTrigger"]}}}`
	state := clearingWith("100000", posWith("BTC", "0.05", "3200")) // long
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, exResp, nil, &orders))

	res, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", TP: "72000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Side != "tp" || !res[0].ReduceOnly {
		t.Fatalf("want one reduce-only tp leg, got %+v", res)
	}
	if len(orders) != 1 {
		t.Fatalf("want 1 wire order, got %d", len(orders))
	}
}

// No live position => a clear exchange error, before signing.
func TestPlacePositionTpslNoPosition(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(emptyState, okOrder(`{"resting":{"oid":1}}`), nil, nil))
	_, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", SL: "58000"})
	assertErr(t, err, output.CatExchange, output.ExitExchange)
}

// Neither tp nor sl is a validation error before any read/sign.
func TestPlacePositionTpslNoLegs(t *testing.T) {
	state := clearingWith("100000", posWith("BTC", "0.05", "3200"))
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, "", nil, nil))
	_, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// --size larger than the open position is rejected (you can't "protect" more than you hold).
func TestPlacePositionTpslSizeExceedsPosition(t *testing.T) {
	state := clearingWith("100000", posWith("BTC", "0.05", "3200"))
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, "", nil, nil))
	_, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", SL: "58000", Size: "0.5"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// A dust position whose size rounds to 0 at the coin's precision is rejected
// before signing (RoundSize guards the degenerate 0-size order).
func TestPlacePositionTpslDustPositionRejected(t *testing.T) {
	state := clearingWith("100000", posWith("BTC", "0.000001", "0.06")) // < 1e-5 lot, BTC SzDecimals=5
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, "", nil, nil))
	_, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", SL: "58000"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// Dry-run never signs: legs come back labeled with status dry_run, reduce-only.
func TestPlacePositionTpslDryRun(t *testing.T) {
	state := clearingWith("100000", posWith("BTC", "0.05", "3200"))
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, posTpslResp(state, "", nil, nil))
	res, _, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", TP: "72000", SL: "58000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 dry-run legs, got %d", len(res))
	}
	for _, r := range res {
		if r.Status != "dry_run" || !r.DryRun || !r.ReduceOnly {
			t.Errorf("leg not a reduce-only dry-run: %+v", r)
		}
	}
}

// A wrong-side level (sl ABOVE mark for a long) warns rather than rejecting.
func TestPlacePositionTpslWrongSideWarns(t *testing.T) {
	state := clearingWith("100000", posWith("BTC", "0.05", "3200")) // long, mark 64000
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":["waitingForTrigger"]}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, posTpslResp(state, exResp, nil, nil))
	_, warnings, err := c.PlacePositionTpsl(ctx, PositionTpslReq{Coin: "BTC", SL: "70000"}) // sl above mark for a long
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "wrong side") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a wrong-side warning, got %v", warnings)
	}
}
