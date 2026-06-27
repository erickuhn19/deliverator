package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// bracketResp serves mids + clearinghouse + a fixed grouped order response, and
// captures the grouping field of the signed action so the test can assert the
// bracket is sent as a linked normalTpsl group (not independent "na" triggers).
func bracketResp(exResp string, grouping *string) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000","ETH":"3000"}`
			case "clearinghouseState":
				return 200, emptyState
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if g, ok := action["grouping"].(string); ok && grouping != nil {
				*grouping = g
			}
		}
		return 200, exResp
	}
}

// A market-entry bracket: entry fills, tp+sl rest as waitingForTrigger. The
// legs must be a linked OCO group (grouping=normalTpsl), labeled, and the tp/sl
// reduce-only. An audit row is written.
func TestPlaceBracketMarketEntry(t *testing.T) {
	var grouping string
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":[` +
		`{"filled":{"totalSz":"0.01","avgPx":"64010","oid":45}},"waitingForTrigger","waitingForTrigger"]}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, bracketResp(exResp, &grouping))

	res, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.01", TP: "70000", SL: "60000"})
	if err != nil {
		t.Fatal(err)
	}
	if grouping != "normalTpsl" {
		t.Fatalf("bracket must be sent as a linked group, grouping=%q", grouping)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 legs, got %d: %+v", len(res), res)
	}
	if res[0].Side != "entry" || res[0].Status != "filled" || res[0].Oid == nil || *res[0].Oid != 45 {
		t.Errorf("entry leg wrong: %+v", res[0])
	}
	for i, name := range []string{"tp", "sl"} {
		leg := res[i+1]
		if leg.Side != name || leg.Status != "waitingForTrigger" || !leg.ReduceOnly {
			t.Errorf("%s leg wrong: %+v", name, leg)
		}
	}
	if a := readAudit(t); len(a) == 0 || a[len(a)-1]["action"] != "bracket" {
		t.Errorf("expected a bracket audit row, got %v", a)
	}
}

// A limit entry with only a take-profit: entry rests, tp waits. Two legs.
func TestPlaceBracketLimitEntryTPOnly(t *testing.T) {
	var grouping string
	exResp := `{"status":"ok","response":{"type":"order","data":{"statuses":[` +
		`{"resting":{"oid":42}},"waitingForTrigger"]}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, bracketResp(exResp, &grouping))

	res, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "64000", Tif: "Gtc", TP: "70000"})
	if err != nil {
		t.Fatal(err)
	}
	if grouping != "normalTpsl" {
		t.Fatalf("grouping=%q", grouping)
	}
	if len(res) != 2 || res[0].Side != "entry" || res[0].Status != "resting" || res[1].Side != "tp" {
		t.Fatalf("unexpected legs: %+v", res)
	}
}

// A bracket with neither tp nor sl is a validation error, before any signing.
func TestPlaceBracketNoLegs(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, bracketResp(okOrder(`{"resting":{"oid":1}}`), nil))
	_, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.01"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// Dry-run never signs: legs come back labeled with status dry_run, tp/sl reduce-only.
func TestPlaceBracketDryRun(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, bracketResp("", nil))
	res, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "64000", TP: "70000", SL: "60000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 dry-run legs, got %d", len(res))
	}
	for _, r := range res {
		if r.Status != "dry_run" || !r.DryRun {
			t.Errorf("leg not dry-run: %+v", r)
		}
	}
	if res[1].ReduceOnly == false || res[2].ReduceOnly == false {
		t.Errorf("tp/sl legs must be reduce-only")
	}
}

// The risk gauntlet runs on the entry notional: a too-large bracket is rejected
// before signing with the order-notional cap category.
func TestPlaceBracketNotionalCapReject(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 100 // entry ~ 64000 * 0.01 = $640
	c, ctx := newTestClient(t, cfg, Options{}, bracketResp(okOrder(`{"resting":{"oid":1}}`), nil))
	_, _, err := c.PlaceBracket(ctx, BracketReq{Coin: "BTC", Side: Buy, Size: "0.01", TP: "70000", SL: "60000"})
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}
