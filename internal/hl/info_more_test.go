package hl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveTypes builds an httptest server dispatching on the request "type".
func serveTypes(t *testing.T, responses map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		typ, _ := body["type"].(string)
		if out, ok := responses[typ]; ok {
			_, _ = io.WriteString(w, out)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// NewInfo with nil meta/spotMeta fetches them at construction (covers Meta/SpotMeta).
func TestNewInfoFetchesMeta(t *testing.T) {
	url := serveTypes(t, map[string]string{
		"meta":     `{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}],"marginTables":[],"collateralToken":0}`,
		"spotMeta": `{"universe":[{"name":"PURR/USDC","index":0,"tokens":[1]}],"tokens":[{"index":1,"szDecimals":2}]}`,
	})
	info := NewInfo(context.Background(), url, true, nil, nil, nil,
		InfoOptClientOptions(ClientOptHTTPClient(&http.Client{})))
	if a, ok := info.CoinToAsset("BTC"); !ok || a != 0 {
		t.Fatalf("BTC asset = %d ok=%v", a, ok)
	}
	if a, ok := info.CoinToAsset("PURR/USDC"); !ok || a != spotAssetIndexOffset {
		t.Fatalf("spot asset = %d ok=%v", a, ok)
	}
}

func TestInfoTimeRangeAndCloid(t *testing.T) {
	url := serveTypes(t, map[string]string{
		"meta":                        `{"universe":[{"name":"BTC","szDecimals":5}],"marginTables":[],"collateralToken":0}`,
		"spotMeta":                    `{"universe":[],"tokens":[]}`,
		"userFillsByTime":             `[{"coin":"BTC","px":"65000","sz":"0.1","side":"B","time":2,"oid":8,"hash":"0x","fee":"0","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open","crossed":true,"tid":2}]`,
		"userNonFundingLedgerUpdates": `[{"delta":{"type":"deposit","usdc":"100"},"hash":"0x","time":3}]`,
		"orderStatus":                 `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"64000","sz":"1","oid":9,"origSz":"1","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Gtc","children":[]},"status":"open","statusTimestamp":1}}`,
	})
	info := NewInfo(context.Background(), url, true, nil, nil, nil)
	ctx := context.Background()

	fills, err := info.UserFillsByTime(ctx, "0xabc", 0, i64(100), nil)
	if err != nil || len(fills) != 1 || fills[0].Oid != 8 {
		t.Fatalf("UserFillsByTime: %+v err=%v", fills, err)
	}
	led, err := info.UserNonFundingLedgerUpdates(ctx, "0xabc", 0, nil)
	if err != nil || len(led) != 1 || led[0].Delta.Type != "deposit" {
		t.Fatalf("ledger: %+v err=%v", led, err)
	}
	oq, err := info.QueryOrderByCloid(ctx, "0xabc", "0x0000000000000000000000000000abcd")
	if err != nil || oq.Status != OrderQueryStatusSuccess || oq.Order.Order.Oid != 9 {
		t.Fatalf("QueryOrderByCloid: %+v err=%v", oq, err)
	}
}
