package hl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func i64(v int64) *int64 { return &v }

func TestModifyOrderByOid(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":5}}]}}}`
	})
	st, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Oid: i64(5), Order: limitOrder()})
	if err != nil || st.Resting == nil || st.Resting.Oid != 5 {
		t.Fatalf("modify by oid: %+v err=%v", st, err)
	}
}

func TestModifyOrderByCloid(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":6}}]}}}`
	})
	o := limitOrder()
	st, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Cloid: &Cloid{Value: "0x0000000000000000000000000000abcd"}, Order: o})
	if err != nil || st.Resting == nil || st.Resting.Oid != 6 {
		t.Fatalf("modify by cloid: %+v err=%v", st, err)
	}
}

func TestModifyOrderRejectsBothIDs(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) { return 200, `{}` })
	_, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Oid: i64(1), Cloid: &Cloid{Value: "0x0000000000000000000000000000abcd"}, Order: limitOrder()})
	if err == nil {
		t.Fatal("expected error when both Oid and Cloid set")
	}
}

// A successful modify returns {"status":"ok","response":{"type":"default"}} with
// no data — this must be treated as success, not a parse error.
func TestModifyOrderNoData(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"default"}}`
	})
	if _, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Oid: i64(5), Order: limitOrder()}); err != nil {
		t.Fatalf("no-data modify success must not error, got %v", err)
	}
}

func TestModifyOrderRejected(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"err","response":"Order was never placed"}`
	})
	if _, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Oid: i64(5), Order: limitOrder()}); err == nil {
		t.Fatal("rejected modify must return an error")
	}
}

func TestCancelByCloidRoundTrips(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`
	})
	if _, err := ex.CancelByCloid(ctx, "BTC", "0x0000000000000000000000000000abcd"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateIsolatedMargin(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"default"}}`
	})
	if _, err := ex.UpdateIsolatedMargin(ctx, 50, "BTC"); err != nil {
		t.Fatalf("add margin: %v", err)
	}
	if _, err := ex.UpdateIsolatedMargin(ctx, -25, "BTC"); err != nil {
		t.Fatalf("remove margin: %v", err)
	}
}

// Locks the live-verified encoding: isBuy is ALWAYS true and ntli is a SIGNED
// integer in USD*1e6 (+ add, - remove). The old Go-SDK form (isBuy=amount>0,
// abs float ntli) failed signature recovery and added on a remove.
func TestUpdateIsolatedMarginEncoding(t *testing.T) {
	cases := []struct {
		amount float64
		ntli   float64 // JSON numbers decode to float64
	}{
		{50, 50000000},
		{-50, -50000000},
		{1, 1000000},
	}
	for _, c := range cases {
		var last map[string]any
		ex, ctx := captureExchange(t, "", &last)
		if _, err := ex.UpdateIsolatedMargin(ctx, c.amount, "BTC"); err != nil {
			t.Fatalf("amount %v: %v", c.amount, err)
		}
		action, _ := last["action"].(map[string]any)
		if action["isBuy"] != true {
			t.Errorf("amount %v: isBuy=%v, want true (always)", c.amount, action["isBuy"])
		}
		if n, _ := action["ntli"].(float64); n != c.ntli {
			t.Errorf("amount %v: ntli=%v, want %v", c.amount, n, c.ntli)
		}
	}
}

func TestScheduleCancel(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"default"}}`
	})
	if _, err := ex.ScheduleCancel(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := ex.ScheduleCancel(ctx, i64(goldenNonce+60000)); err != nil {
		t.Fatalf("arm: %v", err)
	}
}

func TestTwapCancelRoundTrips(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"twapCancel","data":{"status":"success"}}}`
	})
	if _, err := ex.TwapCancel(ctx, "BTC", 99); err != nil {
		t.Fatal(err)
	}
}

func TestMarketCloseUsesPosition(t *testing.T) {
	info := func(typ string, _ map[string]any) (int, string) {
		switch typ {
		case "clearinghouseState":
			return 200, `{"assetPositions":[{"position":{"coin":"BTC","szi":"-0.5","positionValue":"32500","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"100","leverage":{"type":"isolated","value":5}},"type":"oneWay"}],"marginSummary":{"accountValue":"1000","totalMarginUsed":"100","totalNtlPos":"32500","totalRawUsd":"1000"},"crossMarginSummary":{"accountValue":"1000","totalMarginUsed":"100","totalNtlPos":"32500","totalRawUsd":"1000"},"withdrawable":"900"}`
		case "allMids":
			return 200, `{"BTC":"65000"}`
		}
		return 200, `{}`
	}
	ex, ctx := testExchange(t, info, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"0.5","avgPx":"65000","oid":60}}]}}}`
	})
	// short position (szi -0.5) => close is a buy.
	st, err := ex.MarketClose(ctx, "BTC", nil, nil, 0.05, nil, nil)
	if err != nil || st.Filled == nil || st.Filled.Oid != 60 {
		t.Fatalf("market close: %+v err=%v", st, err)
	}
}

func TestMarketCloseNoPosition(t *testing.T) {
	info := func(string, map[string]any) (int, string) {
		return 200, `{"assetPositions":[],"marginSummary":{"accountValue":"0","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"0"},"crossMarginSummary":{"accountValue":"0","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"0"},"withdrawable":"0"}`
	}
	ex, ctx := testExchange(t, info, func(string, map[string]any) (int, string) { return 200, `{}` })
	if _, err := ex.MarketClose(ctx, "BTC", nil, nil, 0.05, nil, nil); err == nil {
		t.Fatal("expected error closing a coin with no position")
	}
}

// captureExchange records the last posted /exchange body so tests can assert the
// signed envelope (vaultAddress, expiresAfter). Also exercises addressToBytes
// (vault set) and the ClientOptHTTPClient option.
func captureExchange(t *testing.T, vault string, last *map[string]any) (*Exchange, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/exchange" {
			*last = body
		}
		_, _ = io.WriteString(w, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":1}}]}}}`)
	}))
	t.Cleanup(srv.Close)
	key, _ := crypto.HexToECDSA(goldenKeyHex)
	meta := &Meta{Universe: []AssetInfo{{Name: "BTC", SzDecimals: 5}}}
	ex := NewExchange(context.Background(), key, srv.URL, meta, vault, "0x1234567890abcdef1234567890abcdef12345678", &SpotMeta{}, nil,
		ExchangeOptClientOptions(ClientOptHTTPClient(&http.Client{})))
	return ex, context.Background()
}

func TestPostEnvelopeVaultAndExpires(t *testing.T) {
	var last map[string]any
	vault := "0x1234567890abcdef1234567890abcdef12345678"
	ex, ctx := captureExchange(t, vault, &last)
	ex.SetExpiresAfter(i64(goldenNonce + 5000))
	if _, err := ex.Order(ctx, limitOrder(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if last["vaultAddress"] != vault {
		t.Errorf("vaultAddress not in payload: %v", last["vaultAddress"])
	}
	if _, ok := last["expiresAfter"]; !ok {
		t.Errorf("expiresAfter not in payload: %v", last)
	}
	sig, ok := last["signature"].(map[string]any)
	if !ok || sig["r"] == nil || sig["s"] == nil || sig["v"] == nil {
		t.Errorf("signature missing/malformed: %v", last["signature"])
	}
}

func TestNonceMonotonic(t *testing.T) {
	ex, _ := testExchange(t, noInfo, func(string, map[string]any) (int, string) { return 200, `{}` })
	ex.SetLastNonce(1000)
	a := ex.nextNonce()
	b := ex.nextNonce()
	if a <= 1000 || b <= a {
		t.Fatalf("nonce not strictly increasing: %d then %d (floor 1000)", a, b)
	}
	if ex.Info() == nil {
		t.Fatal("Info() should be non-nil")
	}
	if ex.isMainnet() {
		t.Fatal("test server URL should not be mainnet")
	}
}

func TestSmallHelpers(t *testing.T) {
	if (Cloid{Value: "0xabc"}).ToRaw() != "0xabc" {
		t.Error("ToRaw")
	}
	if (APIError{Code: 7, Message: "x"}).Error() != "API error 7: x" {
		t.Error("APIError.Error")
	}
	mv := MixedValue(`"hi"`)
	b, err := mv.MarshalJSON()
	if err != nil || string(b) != `"hi"` {
		t.Errorf("MixedValue.MarshalJSON: %s err=%v", b, err)
	}
	if absFloat(-3) != 3 || absFloat(3) != 3 {
		t.Error("absFloat")
	}
	if parseFloat("1.5") != 1.5 || parseFloat("nope") != 0 {
		t.Error("parseFloat")
	}
}
