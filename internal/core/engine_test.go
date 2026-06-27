package core

import (
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// builderAllCfg returns a config with a builder auto-attached on every order.
func builderAllCfg() *config.Config {
	cfg := config.Default()
	cfg.Builder.Address = "0xabcdef0123456789abcdef0123456789abcdef01"
	cfg.Builder.FeeTenthsBps = 50
	cfg.Builder.AttachMode = config.AttachAll
	return cfg
}

func warningsContain(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// A market close charges the builder fee; the result must echo the builder
// (like buy/sell) so the operator can see closes earn revenue.
func TestCloseEchoesBuilder(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp(btcShort, "", "", okOrder(`{"filled":{"totalSz":"0.01","avgPx":"64000","oid":50}}`))))
	res, warnings, err := c.Close(ctx, "BTC", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder == nil || res.Builder.FeeTenthsBps != 50 || res.Builder.Address != "0xabcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("close should echo the builder, got %+v", res.Builder)
	}
	if !warningsContain(warnings, "builder fee") {
		t.Fatalf("close should warn the builder fee was applied: %v", warnings)
	}
}

// HL doesn't support a builder on TWAP — warn so the operator knows it's unmonetized.
func TestTwapWarnsBuilderNotApplied(t *testing.T) {
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, engineResp("", "", "", twapRunning))
	_, warnings, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Buy, Size: "0.01", Minutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !warningsContain(warnings, "TWAP") {
		t.Fatalf("twap should warn the builder fee is not applied: %v", warnings)
	}
}

// HL doesn't support a builder on modify — warn that the replacement loses the fee.
func TestModifyWarnsBuilderDropped(t *testing.T) {
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Gtc","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", front, okOrder(`{"resting":{"oid":99}}`))))
	oid := int64(42)
	_, warnings, err := c.Modify(ctx, &oid, "", "", "63000")
	if err != nil {
		t.Fatal(err)
	}
	if !warningsContain(warnings, "modify") {
		t.Fatalf("modify should warn the builder fee is dropped: %v", warnings)
	}
}

// canned responses
const (
	emptyState  = `{"assetPositions":[],"marginSummary":{"accountValue":"100000","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"100000"},"crossMarginSummary":{"accountValue":"100000","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"100000"},"withdrawable":"100000"}`
	btcShort    = `{"assetPositions":[{"position":{"coin":"BTC","szi":"-0.01","positionValue":"640","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"100","leverage":{"type":"isolated","value":5}},"type":"oneWay"}],"marginSummary":{"accountValue":"100000","totalMarginUsed":"100","totalNtlPos":"640","totalRawUsd":"100000"},"crossMarginSummary":{"accountValue":"100000","totalMarginUsed":"100","totalNtlPos":"640","totalRawUsd":"100000"},"withdrawable":"99000"}`
	defaultEx   = `{"status":"ok","response":{"type":"default"}}`
	cancelOk    = `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`
	twapRunning = `{"status":"ok","response":{"type":"twapOrder","data":{"status":{"running":{"twapId":99}}}}}`
)

// engineResp serves the common info reads + a per-test exchange response.
func engineResp(chState, mids, frontOrders, exResp string) respFn {
	if chState == "" {
		chState = emptyState
	}
	if mids == "" {
		mids = `{"BTC":"64000","ETH":"3000","@0":"0.1"}`
	}
	if frontOrders == "" {
		frontOrders = `[]`
	}
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, mids
			case "clearinghouseState":
				return 200, chState
			case "frontendOpenOrders":
				return 200, frontOrders
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		return 200, exResp
	}
}

func limitBuy() OrderReq {
	return OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "64000", Tif: "Gtc"}
}

func TestPlaceLimitResting(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":42}}`)))
	res, _, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resting" || res.Oid == nil || *res.Oid != 42 || res.Type != "limit" {
		t.Fatalf("place result: %+v", res)
	}
	if a := readAudit(t); len(a) == 0 || a[len(a)-1]["status"] != "resting" || a[len(a)-1]["action"] != "order" {
		t.Errorf("audit not written correctly: %+v", a)
	}
}

func TestPlaceFilledPartial(t *testing.T) {
	// filled 0.005 of 0.01 => partial.
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"filled":{"totalSz":"0.005","avgPx":"64000","oid":43}}`)))
	res, _, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "filled" || !res.IsPartial() {
		t.Fatalf("expected partial fill: %+v", res)
	}
}

func TestPlaceDryRun(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, engineResp("", "", "", `SHOULD_NOT_BE_CALLED`))
	res, _, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Status != "dry_run" {
		t.Fatalf("dry-run result: %+v", res)
	}
}

func TestPlaceRoundingWarning(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":44}}`)))
	// size 0.0123456 -> rounds to szDecimals 5 = 0.01235
	res, warnings, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.0123456", Limit: "64000"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rounded == nil || len(warnings) == 0 {
		t.Fatalf("expected rounding warning: rounded=%+v warnings=%v", res.Rounded, warnings)
	}
}

func TestPlaceStrictPrecisionReject(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{Strict: true}, engineResp("", "", "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.0123456", Limit: "64000"})
	assertErr(t, err, output.CatPrecision, output.ExitPrecision)
}

func TestPlaceNotionalCapReject(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 100
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", `{}`))
	// 1 BTC @ 64000 = $64000 notional >> $100 cap.
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "1", Limit: "64000"})
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

func TestPlaceMarketFailClosedNoMid(t *testing.T) {
	// caps on, but allMids returns empty => no reference price => refuse.
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", `{}`, "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01"})
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

func TestPlaceMarketWithMid(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"filled":{"totalSz":"0.01","avgPx":"64010","oid":45}}`)))
	res, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != "market" || res.Status != "filled" {
		t.Fatalf("market result: %+v", res)
	}
}

func TestPlaceRejectMapsToExchange(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"error":"Order price not divisible by tick size"}`)))
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatExchange, output.ExitExchange)
}

func TestCloseMarket(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp(btcShort, "", "", okOrder(`{"filled":{"totalSz":"0.01","avgPx":"64000","oid":50}}`)))
	res, _, err := c.Close(ctx, "BTC", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Side != "close" || res.Status != "filled" {
		t.Fatalf("close result: %+v", res)
	}
}

func TestSetLeverage(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", defaultEx))
	res, err := c.SetLeverage(ctx, "BTC", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Leverage != 10 || res.Mode != "cross" {
		t.Fatalf("leverage result: %+v", res)
	}
}

func TestSetLeverageConfigCap(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxLeverage = 10
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", defaultEx))
	_, err := c.SetLeverage(ctx, "BTC", 20, true)
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

func TestSetLeverageMarketMax(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxLeverage = 0 // no config cap; ETH market max is 25
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", defaultEx))
	_, err := c.SetLeverage(ctx, "ETH", 30, false)
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

func TestAdjustMargin(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", defaultEx))
	res, err := c.AdjustMargin(ctx, "BTC", 50)
	if err != nil || res.USD != 50 {
		t.Fatalf("margin result: %+v err=%v", res, err)
	}
}

func TestCancelByOid(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", cancelOk))
	oid := int64(42)
	res, err := c.Cancel(ctx, CancelReq{Oid: &oid, Coin: "BTC"})
	if err != nil || res.Canceled != 1 {
		t.Fatalf("cancel result: %+v err=%v", res, err)
	}
}

func TestScheduleCancel(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", defaultEx))
	deadline := int64(1750000000000)
	if err := c.ScheduleCancel(ctx, &deadline); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}
}

// A scheduleCancel rejection (e.g. HL's volume requirement) must map to a proper
// exchange category/exit, not leak as exit 1 (unknown).
func TestScheduleCancelRejectedMapsExchange(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", `{"status":"err","response":"Cannot set scheduled cancel time until enough volume traded"}`))
	deadline := int64(1750000000000)
	assertErr(t, c.ScheduleCancel(ctx, &deadline), output.CatExchange, output.ExitExchange)
}

func TestTwapRunning(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", twapRunning))
	res, _, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Buy, Size: "0.01", Minutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "running" || res.TwapID == nil || *res.TwapID != 99 {
		t.Fatalf("twap result: %+v", res)
	}
}

func TestTwapMinutesInvalid(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", twapRunning))
	_, _, err := c.Twap(ctx, TwapReq{Coin: "BTC", Side: Buy, Size: "0.01", Minutes: 0})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
	_, _, err = c.Twap(ctx, TwapReq{Coin: "BTC", Side: Buy, Size: "0.01", Minutes: 9999})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

func TestTwapCancel(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", `{"status":"ok","response":{"type":"twapCancel","data":{"status":"success"}}}`))
	res, err := c.TwapCancel(ctx, "BTC", 99)
	if err != nil || !res.Canceled {
		t.Fatalf("twap cancel: %+v err=%v", res, err)
	}
}

func TestModifyByOid(t *testing.T) {
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, okOrder(`{"resting":{"oid":42}}`)))
	oid := int64(42)
	res, _, err := c.Modify(ctx, &oid, "", "0.02", "63000")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resting" || res.Size != "0.02" || res.LimitPx != "63000" {
		t.Fatalf("modify result: %+v", res)
	}
}

// A modify cancels the resting order and places a replacement. The replacement
// must carry the original cloid, or status/cancel-by-cloid and the retry
// protocol silently break (the order becomes unaddressable by its client id).
func TestModifyPreservesCloid(t *testing.T) {
	cl := "0x0000000000000000000000000000c111"
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0","cloid":"` + cl + `"}]`
	var postedCloid string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "frontendOpenOrders":
				return 200, front
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if order, ok := action["order"].(map[string]any); ok {
				postedCloid, _ = order["c"].(string)
			}
		}
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	oid := int64(42)
	res, _, err := c.Modify(ctx, &oid, "", "", "63000")
	if err != nil {
		t.Fatal(err)
	}
	if postedCloid != cl {
		t.Fatalf("modify dropped the cloid: posted new-order cloid = %q, want %q", postedCloid, cl)
	}
	if res.Cloid != cl {
		t.Fatalf("modify result cloid = %q, want %q", res.Cloid, cl)
	}
}

// After a modify, a cloid maps to both the canceled predecessor and the live
// replacement. OrderStatus must report the live (open) order, not the stale
// canceled one HL's orderStatus-by-cloid may return — else the retry protocol
// thinks a live order is gone and double-places.
func TestOrderStatusPrefersLiveOrder(t *testing.T) {
	cl := "0x0000000000000000000000000000c222"
	front := `[{"coin":"BTC","oid":99,"limitPx":"63000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":5,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0","cloid":"` + cl + `"}]`
	stale := `{"status":"order","order":{"status":"canceled","statusTimestamp":1,"order":{"coin":"BTC","side":"B","limitPx":"64000","sz":"0.01","oid":42,"origSz":"0.01","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Gtc","children":[],"cloid":"` + cl + `"}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"frontendOpenOrders": front,
		"orderStatus":        stale,
	}))
	oq, err := c.OrderStatus(ctx, nil, cl)
	if err != nil {
		t.Fatal(err)
	}
	if oq.Order.Status != "open" || oq.Order.Order.Oid != 99 || oq.Order.Order.LimitPx != "63000" {
		t.Fatalf("order status should prefer the live open order; got status=%q oid=%d px=%q",
			oq.Order.Status, oq.Order.Order.Oid, oq.Order.Order.LimitPx)
	}
}

// When no order is resting, OrderStatus falls back to the historical query.
func TestOrderStatusFallsBackToHistorical(t *testing.T) {
	cl := "0x0000000000000000000000000000c333"
	stale := `{"status":"order","order":{"status":"canceled","statusTimestamp":1,"order":{"coin":"BTC","side":"B","limitPx":"64000","sz":"0.01","oid":42,"origSz":"0.01","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Gtc","children":[],"cloid":"` + cl + `"}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"frontendOpenOrders": `[]`,
		"orderStatus":        stale,
	}))
	oq, err := c.OrderStatus(ctx, nil, cl)
	if err != nil {
		t.Fatal(err)
	}
	if oq.Order.Status != "canceled" || oq.Order.Order.Oid != 42 {
		t.Fatalf("expected historical canceled order; got status=%q oid=%d", oq.Order.Status, oq.Order.Order.Oid)
	}
}

// A modify rebuilds the order; it must keep the resting order's time-in-force
// (e.g. Alo/post-only), not silently downgrade it to Gtc.
func TestModifyPreservesTif(t *testing.T) {
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Alo","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	var postedTif string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "frontendOpenOrders":
				return 200, front
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		postedTif = postedModifyTif(body)
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	oid := int64(42)
	if _, _, err := c.Modify(ctx, &oid, "", "", "63000"); err != nil {
		t.Fatal(err)
	}
	if postedTif != "Alo" {
		t.Fatalf("modify downgraded TIF: posted tif = %q, want %q", postedTif, "Alo")
	}
}

// The cloid path of findResting now sources from frontendOpenOrders (the only
// query that carries the TIF). Modifying by cloid must resolve the order there
// and preserve both its TIF and its cloid.
func TestModifyByCloidResolvesAndPreserves(t *testing.T) {
	cl := "0x0000000000000000000000000000c444"
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Alo","cloid":"` + cl + `","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	var postedTif, postedCloid string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "frontendOpenOrders":
				return 200, front
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		postedTif = postedModifyTif(body)
		if action, ok := body["action"].(map[string]any); ok {
			if order, ok := action["order"].(map[string]any); ok {
				postedCloid, _ = order["c"].(string)
			}
		}
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	res, _, err := c.Modify(ctx, nil, cl, "", "63000")
	if err != nil {
		t.Fatal(err)
	}
	if postedTif != "Alo" {
		t.Fatalf("modify-by-cloid dropped TIF: posted tif = %q, want Alo", postedTif)
	}
	if postedCloid != cl || res.Cloid != cl {
		t.Fatalf("modify-by-cloid dropped cloid: posted=%q result=%q, want %q", postedCloid, res.Cloid, cl)
	}
}

// HL rejects modifying a trigger order; refuse early with a clear validation
// error rather than rebuilding it as a plain limit and burning a nonce.
func TestModifyTriggerRejected(t *testing.T) {
	front := `[{"coin":"BTC","oid":42,"limitPx":"80000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Stop Market","tif":null,"timestamp":1,"isTrigger":true,"isPositionTpsl":false,"triggerCondition":"Price above 80000","triggerPx":"80000"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, okOrder(`{"resting":{"oid":99}}`)))
	oid := int64(42)
	_, _, err := c.Modify(ctx, &oid, "", "", "63000")
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// postedModifyTif extracts order.t.limit.tif from a captured /exchange body.
func postedModifyTif(body map[string]any) string {
	action, _ := body["action"].(map[string]any)
	order, _ := action["order"].(map[string]any)
	ot, _ := order["t"].(map[string]any)
	lim, _ := ot["limit"].(map[string]any)
	tif, _ := lim["tif"].(string)
	return tif
}

// A modify can grow the order to any size, so it must be re-risk-gated. A grow
// past the order-notional cap is rejected and never reaches signing.
func TestModifyGrowNotionalCapReject(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 1000
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Alo","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", front, okOrder(`{"resting":{"oid":99}}`)))
	oid := int64(42)
	// grow to 0.05 @ 63000 = $3150 notional >> $1000 cap
	_, _, err := c.Modify(ctx, &oid, "", "0.05", "63000")
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

// A bare (no-0x) cloid is valid input (normalizeCloid accepts it); modify must
// normalize before matching against open orders, not return order_not_found.
func TestModifyBareCloidResolves(t *testing.T) {
	canonical := "0x0000000000000000000000000000c555"
	bare := "0000000000000000000000000000c555"
	front := `[{"coin":"BTC","oid":42,"limitPx":"64000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Alo","cloid":"` + canonical + `","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`
	var postedTif, postedCloid string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "frontendOpenOrders":
				return 200, front
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		postedTif = postedModifyTif(body)
		if action, ok := body["action"].(map[string]any); ok {
			if order, ok := action["order"].(map[string]any); ok {
				postedCloid, _ = order["c"].(string)
			}
		}
		return 200, okOrder(`{"resting":{"oid":99}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	res, _, err := c.Modify(ctx, nil, bare, "", "63000")
	if err != nil {
		t.Fatalf("bare cloid should resolve, got: %v", err)
	}
	if postedCloid != canonical || res.Cloid != canonical {
		t.Fatalf("bare cloid not normalized: posted=%q result=%q want %q", postedCloid, res.Cloid, canonical)
	}
	if postedTif != "Alo" {
		t.Fatalf("tif not preserved through bare-cloid modify: %q", postedTif)
	}
}

// A trigger order must wire the ROUNDED trigger price, not the raw user string —
// otherwise it fires at a tick-invalid price while resting at the rounded one.
func TestPlaceTriggerPxRounded(t *testing.T) {
	// BTC szDecimals=5 -> maxDec=1, so 67000.04 rounds to 67000.
	var postedTriggerPx string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"67000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if orders, ok := action["orders"].([]any); ok && len(orders) > 0 {
				if o, ok := orders[0].(map[string]any); ok {
					if tt, ok := o["t"].(map[string]any); ok {
						if tr, ok := tt["trigger"].(map[string]any); ok {
							postedTriggerPx, _ = tr["triggerPx"].(string)
						}
					}
				}
			}
		}
		return 200, okOrder(`{"resting":{"oid":7}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	// Trigger-LIMIT with a DISTINCT, sub-tick trigger price (the independent-round path).
	res, _, err := c.Place(ctx, OrderReq{
		Coin: "BTC", Side: Sell, Size: "0.001", Limit: "66000",
		Trigger: &TriggerReq{TriggerPx: "67000.04", IsMarket: false, Tpsl: "sl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if postedTriggerPx != "67000" {
		t.Fatalf("wired triggerPx not rounded: got %q, want 67000", postedTriggerPx)
	}
	if res.TriggerPx != "67000" {
		t.Fatalf("result trigger_px not surfaced/rounded: got %q, want 67000", res.TriggerPx)
	}
}

// Revenue-critical: an order must carry the builder {b,f} on the POSTed action,
// with the address LOWERCASED (so it matches the approved builder) and the fee
// passed through verbatim in tenths-of-bps. This is the path that earns money.
func TestPlaceAttachesBuilderLowercased(t *testing.T) {
	cfg := config.Default()
	cfg.Builder.Address = "0xABCDEF0123456789ABCDEF0123456789ABCDEF01" // checksummed/upper
	cfg.Builder.FeeTenthsBps = 25
	cfg.Builder.AttachMode = config.AttachAll
	var hadBuilder bool
	var bAddr string
	var bFee float64
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if b, ok := action["builder"].(map[string]any); ok {
				hadBuilder = true
				bAddr, _ = b["b"].(string)
				bFee, _ = b["f"].(float64)
			}
		}
		return 200, okOrder(`{"resting":{"oid":7}}`)
	}
	c, ctx := newTestClient(t, cfg, Options{}, approve(100, resp))
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "64000", Tif: "Gtc"}); err != nil {
		t.Fatal(err)
	}
	if !hadBuilder {
		t.Fatal("order posted with NO builder field — fee dropped, zero revenue")
	}
	if bAddr != "0xabcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("builder address not lowercased on the wire: %q (would mismatch the approved builder)", bAddr)
	}
	if int(bFee) != 25 {
		t.Fatalf("builder fee wrong on the wire: got %v want 25 (tenths-of-bps)", bFee)
	}
}

// In manual mode without a --builder-fee override, no builder must be attached
// (omitempty drops it), even with an address + fee configured.
func TestPlaceManualModeOmitsBuilder(t *testing.T) {
	cfg := config.Default()
	cfg.Builder.Address = "0xabcdef0123456789abcdef0123456789abcdef01"
	cfg.Builder.FeeTenthsBps = 25
	cfg.Builder.AttachMode = config.AttachManual // explicit: the shipped default is now attach_mode=all
	var hadBuilder bool
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			_, hadBuilder = action["builder"]
		}
		return 200, okOrder(`{"resting":{"oid":7}}`)
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	if _, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01", Limit: "64000", Tif: "Gtc"}); err != nil {
		t.Fatal(err)
	}
	if hadBuilder {
		t.Fatal("manual mode attached a builder without an explicit --builder-fee override")
	}
}

func TestUnknownCoinRejected(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", `{}`))
	_, _, err := c.Place(ctx, OrderReq{Coin: "DOGE", Side: Buy, Size: "1", Limit: "1"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}
