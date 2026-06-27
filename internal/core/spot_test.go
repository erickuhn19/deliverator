package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

func TestSpotBaseToken(t *testing.T) {
	m := testMeta()
	if tok, ok := m.SpotBaseToken("PURR/USDC"); !ok || tok != 1 {
		t.Fatalf("PURR/USDC base token = %d,%v; want 1,true", tok, ok)
	}
	if tok, ok := m.SpotBaseToken("purr/usdc"); !ok || tok != 1 {
		t.Errorf("lookup should be case-insensitive, got %d,%v", tok, ok)
	}
	if _, ok := m.SpotBaseToken("NOTAPAIR"); ok {
		t.Error("unknown pair should not resolve")
	}
}

// Ctx on a spot pair returns the spot context, indexed by the pair's universe
// INDEX (3 here), NOT its array position (0). The ctxs slice carries decoys at
// positions 0-2 — indexing by position would return the wrong pair's data.
func TestSpotCtx(t *testing.T) {
	spotMeta := `{"tokens":[{"name":"USDC","index":0,"szDecimals":8},{"name":"PURR","index":1,"szDecimals":0}],` +
		`"universe":[{"name":"PURR/USDC","tokens":[1,0],"index":3,"isCanonical":true}]}`
	decoy := `{"markPx":"9.99","midPx":"9.99","prevDayPx":"9.99","dayNtlVlm":"1","circulatingSupply":"1","totalSupply":"1","coin":"@0"}`
	real := `{"prevDayPx":"0.098","dayNtlVlm":"1000","markPx":"0.10","midPx":"0.099","circulatingSupply":"500","totalSupply":"600","coin":"PURR/USDC"}`
	ctxs := "[" + decoy + "," + decoy + "," + decoy + "," + real + "]" // real at index 3
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "spotMetaAndAssetCtxs" {
			return 200, "[" + spotMeta + "," + ctxs + "]"
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	v, err := c.Ctx(ctx, "PURR/USDC")
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsSpot || v.MarkPx != "0.10" || v.MidPx != "0.099" || v.PrevDayPx != "0.098" {
		t.Fatalf("spot ctx must read index 3 (got %+v) — alignment bug if mark is 9.99", v)
	}
	if v.CirculatingSupply != "500" || v.TotalSupply != "600" {
		t.Errorf("spot ctx supply wrong: %+v", v)
	}
	if v.Funding != "" || v.OpenInterest != "" || v.OraclePx != "" {
		t.Errorf("spot ctx must omit perp-only fields: %+v", v)
	}
}

// A spot close exits a large holding even above max_order_notional and outside
// the allowlist — like a perp close — but still respects the $10 floor.
func TestCloseSpotBypassesCapsAndAllowlist(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 100            // 6000 PURR @ 0.10 = $600 >> this cap
	cfg.Automation.AllowedCoins = []string{"BTC"} // PURR/USDC not allowlisted
	bal := `[{"coin":"PURR","token":1,"hold":"0.0","total":"6000","entryNtl":"0"}]`
	c, ctx := newTestClient(t, cfg, Options{}, spotCloseResp(bal, okOrder(`{"filled":{"totalSz":"6000","avgPx":"0.10","oid":7}}`)))
	// 6000 PURR @ 0.10 = $600 >> $100 order cap, and not allowlisted.
	res, _, err := c.Close(ctx, "PURR/USDC", "", false, "0.10", "")
	if err != nil {
		t.Fatalf("a spot close must exit despite caps/allowlist, got %v", err)
	}
	if res.Side != "sell" {
		t.Fatalf("spot close: %+v", res)
	}
}

// spotCloseResp serves spot balances + an @0 mid + a fixed order response.
func spotCloseResp(balancesJSON, exResp string) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "spotClearinghouseState":
				return 200, `{"balances":` + balancesJSON + `}`
			case "allMids":
				return 200, `{"@0":"0.10"}`
			}
			return 200, `{}`
		}
		return 200, exResp
	}
}

// Closing a spot pair sells the base token (Total − Hold), not a reduce-only perp.
func TestCloseSpotLimit(t *testing.T) {
	bal := `[{"coin":"PURR","token":1,"hold":"0.0","total":"150","entryNtl":"0"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, spotCloseResp(bal, okOrder(`{"filled":{"totalSz":"150","avgPx":"0.10","oid":1}}`)))
	res, _, err := c.Close(ctx, "PURR/USDC", "", false, "0.10", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Side != "sell" || res.Size != "150" || res.ReduceOnly {
		t.Fatalf("spot close should sell the balance, not reduce-only: %+v", res)
	}
}

func TestCloseSpotMarket(t *testing.T) {
	bal := `[{"coin":"PURR","token":1,"hold":"0.0","total":"150","entryNtl":"0"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, spotCloseResp(bal, okOrder(`{"filled":{"totalSz":"150","avgPx":"0.10","oid":2}}`)))
	res, _, err := c.Close(ctx, "PURR/USDC", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Side != "sell" {
		t.Fatalf("spot market close should sell: %+v", res)
	}
}

// The sellable balance is FLOORED to the lot size (szDecimals 0 here), never
// rounded up — rounding 150.6 up to 151 would request more than the balance and
// be rejected by HL.
func TestCloseSpotFloorsNotRoundUp(t *testing.T) {
	bal := `[{"coin":"PURR","token":1,"hold":"0.0","total":"150.6","entryNtl":"0"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, spotCloseResp(bal, okOrder(`{"filled":{"totalSz":"150","avgPx":"0.10","oid":4}}`)))
	res, _, err := c.Close(ctx, "PURR/USDC", "", false, "0.10", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != "150" {
		t.Fatalf("sellable must FLOOR to 150 (not round to 151), got %s", res.Size)
	}
}

// Hold (reserved by open orders) is excluded: Total==Hold => nothing sellable.
func TestCloseSpotHoldExcluded(t *testing.T) {
	bal := `[{"coin":"PURR","token":1,"hold":"150","total":"150","entryNtl":"0"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, spotCloseResp(bal, "{}"))
	_, _, err := c.Close(ctx, "PURR/USDC", "", false, "0.10", "")
	assertErr(t, err, output.CatExchange, output.ExitExchange)
}

func TestCloseSpotNoBalance(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, spotCloseResp(`[]`, "{}"))
	_, _, err := c.Close(ctx, "PURR/USDC", "", false, "0.10", "")
	assertErr(t, err, output.CatExchange, output.ExitExchange)
}

// An explicit size overrides the balance lookup (partial spot exit). 150 @ 0.10
// = $15 clears the $10 floor that (correctly) applies to non-reduce-only spot sells.
func TestCloseSpotExplicitSize(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, spotCloseResp(`[]`, okOrder(`{"filled":{"totalSz":"150","avgPx":"0.10","oid":3}}`)))
	res, _, err := c.Close(ctx, "PURR/USDC", "150", false, "0.10", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Side != "sell" || res.Size != "150" {
		t.Fatalf("explicit-size spot close wrong: %+v", res)
	}
}
