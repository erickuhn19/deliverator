package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// A filled order's audit row must carry filled_sz + avg_px so a partial fill is
// distinguishable from a full one in the persisted trail (status "filled" alone
// can't tell them apart).
func TestAuditRecordsOrderFillTelemetry(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"filled":{"totalSz":"0.005","avgPx":"64000","oid":43}}`)))
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "64000"}); err != nil {
		t.Fatal(err)
	}
	a := readAudit(t)
	last := a[len(a)-1]
	if last["action"] != "order" || last["filled_sz"] != "0.005" || last["avg_px"] != "64000" {
		t.Fatalf("order audit must record fill telemetry: %v", last)
	}
}

// A close's audit row must carry the size AND the fill telemetry (both were
// dropped before).
func TestAuditRecordsCloseFillTelemetry(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp(btcShort, "", "", okOrder(`{"filled":{"totalSz":"0.01","avgPx":"64000","oid":50}}`)))
	if _, _, err := c.Close(ctx, "BTC", "", true, "", ""); err != nil {
		t.Fatal(err)
	}
	a := readAudit(t)
	last := a[len(a)-1]
	if last["action"] != "close" || last["filled_sz"] != "0.01" || last["avg_px"] != "64000" {
		t.Fatalf("close audit must record fill telemetry: %v", last)
	}
	if _, ok := last["size"]; !ok {
		t.Errorf("close audit must record the size: %v", last)
	}
}

// A batch audit row must carry per-leg telemetry (coin/side/type/status/oid),
// not just an opaque order count — parity with the single-order and bracket rows,
// so a multi-leg action is reconstructable from the trail alone.
func TestAuditRecordsBatchLegTelemetry(t *testing.T) {
	resp := okOrders(`{"resting":{"oid":11}}`, `{"error":"Order must have minimum value of $10."}`)
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", resp))
	reduce := limitOrder("BTC", Sell, "0.001", "70000")
	reduce.ReduceOnly = true
	if _, _, err := c.PlaceBatch(ctx, []OrderReq{limitOrder("BTC", Buy, "0.001", "60000"), reduce}); err != nil {
		t.Fatal(err)
	}
	last := readAudit(t)
	row := last[len(last)-1]
	if row["action"] != "batch" {
		t.Fatalf("want batch row, got %v", row)
	}
	legs, ok := row["legs"].([]any)
	if !ok || len(legs) != 2 {
		t.Fatalf("batch row must carry 2 per-leg records, got %v", row["legs"])
	}
	l0 := legs[0].(map[string]any)
	if l0["coin"] != "BTC" || l0["side"] != "buy" || l0["type"] != "limit" || l0["status"] != "resting" || l0["oid"].(float64) != 11 {
		t.Errorf("leg 0 telemetry wrong: %v", l0)
	}
	l1 := legs[1].(map[string]any)
	if l1["reduce_only"] != true || l1["status"] != "rejected" || l1["error"] == "" {
		t.Errorf("leg 1 must record reduce_only + reject reason: %v", l1)
	}
}

// A cancel-all audit row must name the canceled oids, not just a count — so the
// trail says WHICH orders were torn down.
func TestAuditRecordsCancelAllOids(t *testing.T) {
	open := `[{"coin":"BTC","oid":71,"side":"B","limitPx":"60000","origSz":"0.001","sz":"0.001","orderType":"Limit","tif":"Gtc","timestamp":0},` +
		`{"coin":"BTC","oid":72,"side":"A","limitPx":"70000","origSz":"0.001","sz":"0.001","orderType":"Limit","tif":"Gtc","timestamp":0}]`
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			if typ == "frontendOpenOrders" {
				return 200, open
			}
			return engineResp("", "", "", "")(path, typ, body)
		}
		return 200, `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success","success"]}}}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	if _, err := c.Cancel(ctx, CancelReq{All: true}); err != nil {
		t.Fatal(err)
	}
	last := readAudit(t)
	row := last[len(last)-1]
	if row["action"] != "cancel_all" {
		t.Fatalf("want cancel_all row, got %v", row)
	}
	oids, ok := row["oids"].([]any)
	if !ok || len(oids) != 2 || oids[0].(float64) != 71 || oids[1].(float64) != 72 {
		t.Fatalf("cancel_all row must name the canceled oids, got %v", row["oids"])
	}
}

// A partial bracket entry must be flagged IsPartial so the bracket command can
// surface exit 60 (the cmd layer keys on results[0].IsPartial()).
func TestPlaceBracketEntryPartialFlagged(t *testing.T) {
	var grouping string
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":[` +
		`{"filled":{"totalSz":"0.0001","avgPx":"64010","oid":45}},"waitingForTrigger","waitingForTrigger"]}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, bracketResp(exResp, &grouping))
	res, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.0002", TP: "70000", SL: "60000"})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].IsPartial() {
		t.Fatalf("a partially-filled bracket entry must be IsPartial(): %+v", res[0])
	}
}
