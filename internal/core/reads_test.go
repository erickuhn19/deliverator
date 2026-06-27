package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// Mids merges main + each configured HIP-3 sub-dex's mids (keyed by "<dex>:<coin>").
func TestMidsAggregatesSubDex(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "allMids" {
			if body["dex"] == "xyz" {
				return 200, `{"xyz:BRENTOIL":"77.2","xyz:AAPL":"299.6"}`
			}
			return 200, `{"BTC":"64000"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	m, err := c.Mids(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m["BTC"] != "64000" || m["xyz:BRENTOIL"] != "77.2" || m["xyz:AAPL"] != "299.6" {
		t.Fatalf("mids should merge main + sub-dex, got %v", m)
	}
}

// bbo's derived mid/spread must be clean tick-aligned strings, not float-noise.
// The whole-number TestBookAndBbo can't catch this (63999/64001 is exactly
// float-representable); these prices reproduce the binary-float error the decimal
// path now avoids: float64 gives mid 3500.2200000000003 / spread
// 0.020000000004074536, decimal gives the exact 3500.22 / 0.02.
func TestBboMidSpreadNoFloatNoise(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"3500.21","sz":"1","n":1}],[{"px":"3500.23","sz":"1","n":1}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{"l2Book": book}))
	bbo, err := c.Bbo(ctx, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if bbo.Mid != "3500.22" {
		t.Errorf("mid = %q, want clean 3500.22 (no float noise)", bbo.Mid)
	}
	if bbo.Spread != "0.02" {
		t.Errorf("spread = %q, want clean 0.02 (no float noise)", bbo.Spread)
	}

	// A half-tick mid must keep full precision (not round to px_decimals).
	book2 := `{"coin":"BTC","time":1,"levels":[[{"px":"100.1","sz":"1","n":1}],[{"px":"100.2","sz":"1","n":1}]]}`
	c2, ctx2 := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{"l2Book": book2}))
	bbo2, _ := c2.Bbo(ctx2, "BTC")
	if bbo2.Mid != "100.15" {
		t.Errorf("half-tick mid = %q, want 100.15 (full precision preserved)", bbo2.Mid)
	}
}

// infoMap serves /info responses keyed by request type; everything else => {}.
func infoMap(m map[string]string) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			if out, ok := m[typ]; ok {
				return 200, out
			}
		}
		return 200, `{}`
	}
}

func TestPortfolioRead(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"clearinghouseState":     btcShort,
		"frontendOpenOrders":     `[{"coin":"BTC","oid":7,"limitPx":"60000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`,
		"spotClearinghouseState": `{"balances":[{"coin":"USDC","token":0,"total":"500","hold":"0","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"480"]]}`,
	}))
	pv, err := c.Portfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pv.AccountValue != "100000" || len(pv.Positions) != 1 || len(pv.OpenOrders) != 1 || pv.AvailableCollateral != "480" {
		t.Fatalf("portfolio: %+v", pv)
	}
}

func TestPositionsAndBalance(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"clearinghouseState":     btcShort,
		"spotClearinghouseState": `{"balances":[{"coin":"USDC","token":0,"total":"500","hold":"0","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"480"]]}`,
	}))
	pos, err := c.Positions(ctx, "")
	if err != nil || len(pos) != 1 || pos[0].Side != "short" {
		t.Fatalf("positions: %+v err=%v", pos, err)
	}
	bal, err := c.Balance(ctx)
	if err != nil || bal.Perp.AccountValue != "100000" || bal.AvailableCollateral != "480" {
		t.Fatalf("balance: %+v err=%v", bal, err)
	}
}

func TestBookAndBbo(t *testing.T) {
	book := `{"coin":"BTC","time":1,"levels":[[{"px":"63999","sz":"1","n":2},{"px":"63998","sz":"2","n":3}],[{"px":"64001","sz":"1","n":2},{"px":"64002","sz":"2","n":3}]]}`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{"l2Book": book}))
	bv, err := c.Book(ctx, "BTC", 1)
	if err != nil || len(bv.Bids) != 1 || bv.Bids[0].Px != "63999" {
		t.Fatalf("book: %+v err=%v", bv, err)
	}
	bbo, err := c.Bbo(ctx, "BTC")
	if err != nil || bbo.Bid != "63999" || bbo.Ask != "64001" || bbo.Mid != "64000" || bbo.Spread != "2" {
		t.Fatalf("bbo: %+v err=%v", bbo, err)
	}
}

func TestCtxAndMids(t *testing.T) {
	mac := `[{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":40}],"marginTables":[],"collateralToken":0},[{"funding":"0.0001","openInterest":"100","prevDayPx":"63000","dayNtlVlm":"1000","premium":"0.001","oraclePx":"64100","markPx":"64050","midPx":"64055","impactPxs":["64049","64051"]}]]`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"metaAndAssetCtxs": mac,
		"allMids":          `{"BTC":"64055"}`,
	}))
	cv, err := c.Ctx(ctx, "BTC")
	if err != nil || cv.MarkPx != "64050" || cv.Funding != "0.0001" {
		t.Fatalf("ctx: %+v err=%v", cv, err)
	}
	// impact_pxs must be surfaced (was previously fetched then dropped, #51).
	if len(cv.ImpactPxs) != 2 || cv.ImpactPxs[0] != "64049" || cv.ImpactPxs[1] != "64051" {
		t.Fatalf("ctx impact_pxs not surfaced: %+v", cv.ImpactPxs)
	}
	mids, err := c.Mids(ctx)
	if err != nil || mids["BTC"] != "64055" {
		t.Fatalf("mids: %+v err=%v", mids, err)
	}
}

func TestFillsAndPnl(t *testing.T) {
	fills := `[{"coin":"BTC","px":"64000","sz":"0.01","side":"B","time":2,"oid":7,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open","crossed":true,"tid":2},{"coin":"BTC","px":"64010","sz":"0.01","side":"B","time":1,"oid":6,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open","crossed":true,"tid":1}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"userFills": fills,
		"portfolio": `[["day",{"accountValueHistory":[[1,"100"],[2,"110"]],"pnlHistory":[[1,"0"],[2,"10"]],"vlm":"100"}]]`,
	}))
	f, err := c.Fills(ctx, nil, 1)
	if err != nil || len(f) != 1 || f[0].Time != 2 { // newest first, limited to 1
		t.Fatalf("fills: %+v err=%v", f, err)
	}
	pnl, err := c.Pnl(ctx)
	if err != nil || len(pnl) != 1 || pnl[0].Window != "day" || len(pnl[0].PnlHistory) != 2 {
		t.Fatalf("pnl: %+v err=%v", pnl, err)
	}
}

func TestOrderStatusAndLimits(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"orderStatus":   `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"60000","sz":"0.01","oid":7,"origSz":"0.01","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Gtc","children":[]},"status":"open","statusTimestamp":1}}`,
		"userRateLimit": `{"cumVlm":"1000","nRequestsUsed":50,"nRequestsCap":1000,"nRequestsSurplus":0}`,
	}))
	oid := int64(7)
	oq, err := c.OrderStatus(ctx, &oid, "")
	if err != nil || oq.Order.Order.Coin != "BTC" {
		t.Fatalf("order status: %+v err=%v", oq, err)
	}
	lim, err := c.Limits(ctx)
	if err != nil || lim.Cap != 1000 || lim.Used != 50 || lim.Remaining != 950 {
		t.Fatalf("limits: %+v err=%v", lim, err)
	}
}

func TestClientAccessors(t *testing.T) {
	c, _ := newTestClient(t, config.Default(), Options{}, infoMap(nil))
	if c.Network() != "testnet" || c.QueryAddr() != testMaster || c.Meta() == nil || c.Info() == nil {
		t.Error("accessors wrong")
	}
	if c.AgentAddress() != "" { // no agent loaded in harness
		t.Errorf("AgentAddress should be empty, got %q", c.AgentAddress())
	}
}
