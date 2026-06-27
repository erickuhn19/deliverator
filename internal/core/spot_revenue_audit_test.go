package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// A spot BUY earns no builder fee (HL charges the taker fee in the base token),
// so the result must not claim one and must warn.
func TestSpotBuyBuilderNotEarned(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`))))
	res, w, err := c.Place(ctx, OrderReq{Coin: "PURR/USDC", Side: Buy, Size: "150", Limit: "0.1", Tif: "Gtc"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder != nil {
		t.Errorf("spot buy must not claim a builder fee, got %+v", res.Builder)
	}
	if !warningsContain(w, "NOT earned") {
		t.Errorf("spot buy must warn the builder fee is not earned: %v", w)
	}
}

// A spot SELL does earn the builder fee (fee in USDC), like perps.
func TestSpotSellBuilderEarned(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`))))
	res, w, err := c.Place(ctx, OrderReq{Coin: "PURR/USDC", Side: Sell, Size: "150", Limit: "0.2", Tif: "Gtc"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder == nil {
		t.Errorf("spot sell SHOULD earn the builder fee")
	}
	if !warningsContain(w, "builder fee 0.050% applied") {
		t.Errorf("spot sell should warn the fee was applied: %v", w)
	}
}

// A modify audit row must record the cloid the REPLACEMENT carries (preservedCloid)
// + the new size/limit, not the empty input cloid.
func TestModifyAuditRecordsPreservedCloid(t *testing.T) {
	cl := "0x00000000000000000000000000000099"
	front := "[" + openOrderJSON("BTC", 42, cl) + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, defaultEx))
	oid := int64(42)
	if _, _, err := c.Modify(ctx, &oid, "", "", "63000"); err != nil {
		t.Fatal(err)
	}
	last := readAudit(t)[len(readAudit(t))-1]
	if last["action"] != "modify" || last["cloid"] != cl {
		t.Fatalf("modify audit must log the preserved cloid %s, got %v", cl, last)
	}
	if last["limit_px"] == nil || last["size"] == nil {
		t.Errorf("modify audit must record the new size + limit_px: %v", last)
	}
}

// A bracket audit row must record per-leg status + the entry's fill telemetry.
func TestBracketAuditRecordsLegStatusAndFill(t *testing.T) {
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":[` +
		`{"filled":{"totalSz":"0.0002","avgPx":"64000","oid":1}},"waitingForTrigger","waitingForTrigger"]}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, bracketResp(exResp, nil))
	if _, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.0002", TP: "70000", SL: "60000"}); err != nil {
		t.Fatal(err)
	}
	last := readAudit(t)[len(readAudit(t))-1]
	if last["action"] != "bracket" {
		t.Fatalf("expected bracket audit, got %v", last)
	}
	ls, ok := last["leg_status"].([]any)
	if !ok || len(ls) != 3 || ls[0] != "filled" {
		t.Fatalf("bracket audit must record per-leg status [filled,...]: %v", last["leg_status"])
	}
	if last["filled_sz"] != "0.0002" {
		t.Errorf("bracket audit must record the entry fill telemetry: %v", last)
	}
}
