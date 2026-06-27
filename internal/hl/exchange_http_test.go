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

// respond is a per-path canned response: it returns (httpStatus, jsonBody).
type respond func(reqType string, body map[string]any) (int, string)

// testExchange spins up an httptest server and an Exchange wired to it. meta is
// provided so construction does no network I/O. The signer runs for real.
func testExchange(t *testing.T, infoFn, exchangeFn respond) (*Exchange, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fn := exchangeFn
		if r.URL.Path == "/info" {
			fn = infoFn
		}
		typ, _ := body["type"].(string)
		code, out := fn(typ, body)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)

	key, err := crypto.HexToECDSA(goldenKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	meta := &Meta{Universe: []AssetInfo{{Name: "BTC", SzDecimals: 5}}}
	ex := NewExchange(context.Background(), key, srv.URL, meta, "", "0x1234567890abcdef1234567890abcdef12345678", &SpotMeta{}, nil)
	return ex, context.Background()
}

func noInfo(string, map[string]any) (int, string) { return 200, `{}` }

func limitOrder() CreateOrderRequest {
	return CreateOrderRequest{Coin: "BTC", IsBuy: true, Price: 65000, Size: 0.1, OrderType: OrderType{Limit: &LimitOrderType{Tif: TifGtc}}}
}

func TestOrderResting(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":42}}]}}}`
	})
	st, err := ex.Order(ctx, limitOrder(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Resting == nil || st.Resting.Oid != 42 {
		t.Fatalf("resting: %+v", st)
	}
}

func TestOrderFilled(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"0.1","avgPx":"65000","oid":43}}]}}}`
	})
	st, err := ex.Order(ctx, limitOrder(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Filled == nil || st.Filled.Oid != 43 || st.Filled.TotalSz != "0.1" {
		t.Fatalf("filled: %+v", st)
	}
}

// The key dual-return contract: a per-order rejection must set BOTH st.Error and
// a non-nil error, so the engine routes to mapOrderReject (not mapExchangeErr).
func TestOrderPerOrderError(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"error":"Insufficient margin"}]}}}`
	})
	st, err := ex.Order(ctx, limitOrder(), nil, 0)
	if err == nil {
		t.Fatal("expected error for per-order rejection")
	}
	if st.Error == nil || *st.Error != "Insufficient margin" {
		t.Fatalf("st.Error not populated on rejection: %+v", st)
	}
}

// An envelope-level failure returns an error with st.Error nil (engine routes to
// mapExchangeErr).
func TestOrderEnvelopeError(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"err","response":"Must deposit before trading"}`
	})
	st, err := ex.Order(ctx, limitOrder(), nil, 0)
	if err == nil {
		t.Fatal("expected envelope error")
	}
	if st.Error != nil {
		t.Fatalf("st.Error should be nil on envelope failure: %+v", st)
	}
}

func TestCancelSuccess(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`
	})
	if _, err := ex.Cancel(ctx, "BTC", 42); err != nil {
		t.Fatal(err)
	}
}

func TestCancelError(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"cancel","data":{"statuses":[{"error":"Order was never placed"}]}}}`
	})
	if _, err := ex.Cancel(ctx, "BTC", 42); err == nil {
		t.Fatal("expected cancel error from statuses")
	}
}

func TestMarketOpenUsesMids(t *testing.T) {
	info := func(typ string, _ map[string]any) (int, string) {
		if typ == "allMids" {
			return 200, `{"BTC":"65000"}`
		}
		return 200, `{}`
	}
	ex, ctx := testExchange(t, info, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"0.1","avgPx":"68250","oid":50}}]}}}`
	})
	st, err := ex.MarketOpen(ctx, "BTC", true, 0.1, nil, 0.05, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Filled == nil || st.Filled.Oid != 50 {
		t.Fatalf("market open: %+v", st)
	}
}

func TestTwapOrderRunning(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"twapOrder","data":{"status":{"running":{"twapId":99}}}}}`
	})
	st, err := ex.TwapOrder(ctx, "BTC", true, 2, false, 30, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running == nil || st.Running.TwapID != 99 {
		t.Fatalf("twap running: %+v", st)
	}
}

func TestTwapOrderError(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"twapOrder","data":{"status":{"error":"size too small"}}}}`
	})
	st, err := ex.TwapOrder(ctx, "BTC", true, 2, false, 30, false)
	if err == nil {
		t.Fatal("expected twap error")
	}
	if st.Error == nil || *st.Error != "size too small" {
		t.Fatalf("twap st.Error: %+v", st)
	}
}

func TestUpdateLeverageRoundTrips(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"default"}}`
	})
	if _, err := ex.UpdateLeverage(ctx, 10, "BTC", true); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 429, `{"code":429,"msg":"rate limited"}`
	})
	if _, err := ex.Order(ctx, limitOrder(), nil, 0); err == nil {
		t.Fatal("expected error on HTTP 429")
	}
}
