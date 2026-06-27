package hl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testInfo builds an Info wired to an httptest server. meta/spot are provided so
// construction does no network I/O; read methods hit the server.
func testInfo(t *testing.T, fn respond) (*Info, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		typ, _ := body["type"].(string)
		code, out := fn(typ, body)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	meta := &Meta{Universe: []AssetInfo{{Name: "BTC", SzDecimals: 5}}}
	info := NewInfo(context.Background(), srv.URL, true, meta, &SpotMeta{}, nil)
	return info, context.Background()
}

func TestInfoReads(t *testing.T) {
	responses := map[string]string{
		"clearinghouseState":     `{"assetPositions":[{"position":{"coin":"BTC","szi":"0.5","positionValue":"32500","unrealizedPnl":"10","returnOnEquity":"0.1","marginUsed":"100","leverage":{"type":"cross","value":10},"entryPx":"65000","liquidationPx":"30000"},"type":"oneWay"}],"marginSummary":{"accountValue":"1000","totalMarginUsed":"100","totalNtlPos":"32500","totalRawUsd":"1000"},"crossMarginSummary":{"accountValue":"1000","totalMarginUsed":"100","totalNtlPos":"32500","totalRawUsd":"1000"},"withdrawable":"900"}`,
		"spotClearinghouseState": `{"balances":[{"coin":"USDC","token":0,"hold":"0","total":"500","entryNtl":"0"}]}`,
		"allMids":                `{"BTC":"65000","ETH":"3200"}`,
		"l2Book":                 `{"coin":"BTC","time":1,"levels":[[{"px":"64999","sz":"1.0","n":2}],[{"px":"65001","sz":"2.0","n":3}]]}`,
		"frontendOpenOrders":     `[{"coin":"BTC","oid":7,"limitPx":"64000","origSz":"1.0","sz":"1.0","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0.0"}]`,
		"orderStatus":            `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"64000","sz":"1.0","oid":7,"origSz":"1.0","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Gtc","children":[]},"status":"open","statusTimestamp":1}}`,
		"candleSnapshot":         `[{"t":1,"T":2,"s":"BTC","i":"1m","o":"1","h":"2","l":"1","c":"2","v":"10","n":3}]`,
		"userFills":              `[{"coin":"BTC","px":"65000","sz":"0.1","side":"B","time":1,"oid":7,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":1}]`,
		"userFunding":            `[{"delta":{"coin":"BTC","fundingRate":"0.0001","size":"0.1","type":"funding","usdc":"0.5"},"hash":"0x","time":1}]`,
		"portfolio":              `[["day",{"accountValueHistory":[[1,"1000"]],"pnlHistory":[[1,"0"]],"vlm":"100"}]]`,
		"metaAndAssetCtxs":       `[{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}],"marginTables":[],"collateralToken":0},[{"funding":"0.0001","openInterest":"100","prevDayPx":"64000","dayNtlVlm":"1000","premium":"0.001","oraclePx":"65000","markPx":"65010","midPx":"65005","impactPxs":["64999","65001"]}]]`,
	}
	fn := func(typ string, _ map[string]any) (int, string) {
		if r, ok := responses[typ]; ok {
			return 200, r
		}
		return 200, `{}`
	}
	info, ctx := testInfo(t, fn)

	us, err := info.UserState(ctx, "0xabc")
	if err != nil || us.MarginSummary.AccountValue != "1000" || len(us.AssetPositions) != 1 || us.AssetPositions[0].Position.Szi != "0.5" {
		t.Fatalf("UserState: %+v err=%v", us, err)
	}
	sus, err := info.SpotUserState(ctx, "0xabc")
	if err != nil || len(sus.Balances) != 1 || sus.Balances[0].Total != "500" {
		t.Fatalf("SpotUserState: %+v err=%v", sus, err)
	}
	mids, err := info.AllMids(ctx)
	if err != nil || mids["BTC"] != "65000" {
		t.Fatalf("AllMids: %+v err=%v", mids, err)
	}
	book, err := info.L2Snapshot(ctx, "BTC")
	if err != nil || book.Coin != "BTC" || len(book.Levels) != 2 || book.Levels[0][0].Px != 64999 {
		t.Fatalf("L2Snapshot: %+v err=%v", book, err)
	}
	orders, err := info.FrontendOpenOrders(ctx, "0xabc")
	if err != nil || len(orders) != 1 || orders[0].Oid != 7 || orders[0].Side != OrderSideBid {
		t.Fatalf("FrontendOpenOrders: %+v err=%v", orders, err)
	}
	oq, err := info.QueryOrderByOid(ctx, "0xabc", 7)
	if err != nil || oq.Status != OrderQueryStatusSuccess || oq.Order.Order.Coin != "BTC" {
		t.Fatalf("QueryOrderByOid: %+v err=%v", oq, err)
	}
	candles, err := info.CandlesSnapshot(ctx, "BTC", "1m", 0, 10)
	if err != nil || len(candles) != 1 || candles[0].Close != "2" {
		t.Fatalf("CandlesSnapshot: %+v err=%v", candles, err)
	}
	fills, err := info.UserFills(ctx, UserFillsParams{Address: "0xabc"})
	if err != nil || len(fills) != 1 || fills[0].Oid != 7 {
		t.Fatalf("UserFills: %+v err=%v", fills, err)
	}
	fund, err := info.UserFundingHistory(ctx, "0xabc", 0, nil)
	if err != nil || len(fund) != 1 || fund[0].Delta.Coin != "BTC" {
		t.Fatalf("UserFundingHistory: %+v err=%v", fund, err)
	}
	pf, err := info.Portfolio(ctx, "0xabc")
	if err != nil || len(pf) != 1 {
		t.Fatalf("Portfolio: %+v err=%v", pf, err)
	}
	mac, err := info.MetaAndAssetCtxs(ctx, MetaAndAssetCtxsParams{})
	if err != nil || len(mac.Meta.Universe) != 1 || len(mac.Ctxs) != 1 || mac.Ctxs[0].MarkPx != "65010" {
		t.Fatalf("MetaAndAssetCtxs: %+v err=%v", mac, err)
	}
}

// Info read methods surface transport/HTTP errors.
func TestInfoReadError(t *testing.T) {
	info, ctx := testInfo(t, func(string, map[string]any) (int, string) {
		return 500, `{"code":500,"msg":"boom"}`
	})
	if _, err := info.UserState(ctx, "0xabc"); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}
