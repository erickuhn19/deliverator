package hl

import (
	"errors"
	"strings"
	"testing"
)

// Exercises newOrderTypeWire's trigger branch + newCreateOrderAction.
func TestOrderTrigger(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":71}}]}}}`
	})
	st, err := ex.Order(ctx, CreateOrderRequest{
		Coin: "BTC", IsBuy: false, Price: 0, Size: 0.1,
		OrderType: OrderType{Trigger: &TriggerOrderType{TriggerPx: 61000, IsMarket: true, Tpsl: StopLoss}},
	}, nil, 0)
	if err != nil || st.Resting == nil || st.Resting.Oid != 71 {
		t.Fatalf("trigger order: %+v err=%v", st, err)
	}
}

// #40: a trigger px that can't be wire-encoded (more precision than 8 decimals)
// must abort the action build with an error — not silently sign a trigger px of
// "0" that the exchange rejects after a nonce is consumed.
func TestOrderTriggerPxWireError(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":72}}]}}}`
	})
	_, err := ex.Order(ctx, CreateOrderRequest{
		Coin: "BTC", IsBuy: false, Price: 0, Size: 0.1,
		OrderType: OrderType{Trigger: &TriggerOrderType{TriggerPx: 0.123456789, IsMarket: true, Tpsl: StopLoss}},
	}, nil, 0)
	if err == nil {
		t.Fatal("un-wireable trigger px must error before signing, not coerce to 0")
	}
}

func TestModifyNeitherID(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) { return 200, `{}` })
	if _, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Order: limitOrder()}); err == nil {
		t.Fatal("modify with neither Oid nor Cloid must error")
	}
}

func TestTransportAPIError(t *testing.T) {
	info, ctx := testInfo(t, func(string, map[string]any) (int, string) {
		return 400, `{"code":429,"msg":"rate limited"}`
	})
	_, err := info.UserState(ctx, "0xabc")
	var apiErr APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.Code != 429 || apiErr.Error() != "API error 429: rate limited" {
		t.Fatalf("APIError: %+v -> %q", apiErr, apiErr.Error())
	}
}

func TestTransportNonJSONError(t *testing.T) {
	info, ctx := testInfo(t, func(string, map[string]any) (int, string) {
		return 500, `upstream down`
	})
	_, err := info.UserState(ctx, "0xabc")
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("expected raw status error, got %v", err)
	}
}

// Valid JSON that isn't an APIError shape falls through to the raw status string.
func TestTransportJSONNonAPIError(t *testing.T) {
	info, ctx := testInfo(t, func(string, map[string]any) (int, string) {
		return 503, `[1,2,3]`
	})
	_, err := info.UserState(ctx, "0xabc")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected 503 error, got %v", err)
	}
}
