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

// Regression tests for the review-confirmed bugs: exchange status:"err"
// responses on no-data actions must surface as errors, not false successes.

const errEnvelope = `{"status":"err","response":"Insufficient margin"}`

func TestScheduleCancelRejection(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"err","response":"Cannot set scheduled cancel time until enough volume traded"}`
	})
	if _, err := ex.ScheduleCancel(ctx, i64(goldenNonce+60000)); err == nil {
		t.Fatal("rejected dead-man's switch arm must return an error, not nil")
	}
}

func TestUpdateLeverageRejection(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, errEnvelope
	})
	if _, err := ex.UpdateLeverage(ctx, 50, "BTC", true); err == nil {
		t.Fatal("rejected leverage change must return an error")
	}
}

func TestUpdateIsolatedMarginRejection(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, errEnvelope
	})
	if _, err := ex.UpdateIsolatedMargin(ctx, 50, "BTC"); err == nil {
		t.Fatal("rejected margin change must return an error")
	}
}

func TestTwapCancelInnerError(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		// envelope ok, but the cancel logically failed inside data.
		return 200, `{"status":"ok","response":{"type":"twapCancel","data":{"status":{"error":"twap not found"}}}}`
	})
	if _, err := ex.TwapCancel(ctx, "BTC", 7); err == nil {
		t.Fatal("logically-failed twap cancel must return an error")
	}
}

// #41: only the recognized success marker ({"status":"success"}) counts as a
// successful cancel; every other shape — including unfamiliar ones — must fail
// closed, so a still-running TWAP is never reported as cancelled.
func TestTwapCancelInnerErrorFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		success bool
	}{
		{"success", `{"status":"success"}`, true},
		{"nested error", `{"status":{"error":"twap not found"}}`, false},
		{"other status string", `{"status":"notFound"}`, false},
		{"empty status string", `{"status":""}`, false},
		{"wrong status type", `{"status":42}`, false},
		{"missing status", `{"foo":"bar"}`, false},
		{"empty object", `{}`, false},
		{"not an object", `[1,2,3]`, false},
		{"bare string", `"success"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := twapCancelInnerError(MixedValue(c.data))
			if c.success && err != nil {
				t.Fatalf("expected success for %s, got %v", c.data, err)
			}
			if !c.success && err == nil {
				t.Fatalf("expected fail-closed error for %s, got nil", c.data)
			}
		})
	}
}

// testExchangeSpot builds an Exchange whose Info knows a perp (BTC) and a
// non-canonical spot pair (PURR/USDC -> asset 10000).
func testExchangeSpot(t *testing.T, infoFn respond) (*Exchange, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		typ, _ := body["type"].(string)
		code, out := 200, `{}`
		if r.URL.Path == "/info" {
			code, out = infoFn(typ, body)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	key, _ := crypto.HexToECDSA(goldenKeyHex)
	meta := &Meta{Universe: []AssetInfo{{Name: "BTC", SzDecimals: 5}}}
	spot := &SpotMeta{
		Universe: []SpotAssetInfo{{Name: "PURR/USDC", Index: 0, Tokens: []int{1}}},
		Tokens:   []SpotTokenInfo{{Index: 1, SzDecimals: 2}},
	}
	ex := NewExchange(context.Background(), key, srv.URL, meta, "", "0xabc", spot, nil)
	return ex, context.Background()
}

// A spot market order's mid is keyed by "@<index>", not the pair name.
func TestSlippagePriceSpotAtIndex(t *testing.T) {
	ex, ctx := testExchangeSpot(t, func(typ string, _ map[string]any) (int, string) {
		if typ == "allMids" {
			return 200, `{"@0":"1.5","BTC":"65000"}` // no "PURR/USDC" key
		}
		return 200, `{}`
	})
	price, err := ex.SlippagePrice(ctx, "PURR/USDC", true, 0.05, nil)
	if err != nil {
		t.Fatalf("spot @index mid should resolve: %v", err)
	}
	if price != 1.575 { // 1.5 * 1.05, spot decimals 8-2=6
		t.Fatalf("spot slippage price = %v, want 1.575", price)
	}
}

// Pin the perp-vs-spot decimals branch and 5-sig-fig rounding with explicit px
// (no network).
func TestSlippagePriceDecimalsBranch(t *testing.T) {
	ex, ctx := testExchangeSpot(t, noInfo)
	px := 65000.0
	if got, _ := ex.SlippagePrice(ctx, "BTC", true, 0.05, &px); got != 68250 {
		t.Errorf("perp buy: got %v want 68250", got)
	}
	if got, _ := ex.SlippagePrice(ctx, "BTC", false, 0.05, &px); got != 61750 {
		t.Errorf("perp sell: got %v want 61750", got)
	}
	spx := 1.5
	if got, _ := ex.SlippagePrice(ctx, "PURR/USDC", true, 0.05, &spx); got != 1.575 {
		t.Errorf("spot buy: got %v want 1.575", got)
	}
	// missing mid (no px) errors
	if _, err := ex.SlippagePrice(ctx, "BTC", true, 0.05, nil); err == nil {
		t.Error("missing mid should error")
	}
}
