package hl

import (
	"encoding/json"
	"testing"
)

// A grouped (normalTpsl) bracket response interleaves object statuses (the
// entry that fills/rests) with bare-string statuses for the trigger legs
// ("waitingForTrigger"). OrderStatus must tolerate both in one array.
func TestOrderStatusUnmarshalMixed(t *testing.T) {
	const body = `{"statuses":[{"filled":{"oid":42,"totalSz":"0.25","avgPx":"70.5"}},"waitingForTrigger","waitingForTrigger"]}`
	var resp OrderResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal grouped bracket response: %v", err)
	}
	if len(resp.Statuses) != 3 {
		t.Fatalf("got %d statuses, want 3", len(resp.Statuses))
	}
	if resp.Statuses[0].Filled == nil || resp.Statuses[0].Filled.Oid != 42 {
		t.Errorf("status[0] should be filled oid=42, got %+v", resp.Statuses[0])
	}
	for i := 1; i < 3; i++ {
		if resp.Statuses[i].Status != "waitingForTrigger" {
			t.Errorf("status[%d] = %q, want waitingForTrigger", i, resp.Statuses[i].Status)
		}
		if resp.Statuses[i].Filled != nil || resp.Statuses[i].Resting != nil || resp.Statuses[i].Error != nil {
			t.Errorf("status[%d] string form should leave object fields nil, got %+v", i, resp.Statuses[i])
		}
	}
}

func TestOrderStatusUnmarshalObjectForms(t *testing.T) {
	cases := map[string]func(OrderStatus) bool{
		`{"resting":{"oid":7}}`:                func(s OrderStatus) bool { return s.Resting != nil && s.Resting.Oid == 7 },
		`{"error":"order has invalid price"}`:  func(s OrderStatus) bool { return s.Error != nil && *s.Error == "order has invalid price" },
		`{"filled":{"oid":1,"totalSz":"1.0"}}`: func(s OrderStatus) bool { return s.Filled != nil && s.Filled.Oid == 1 },
		`"success"`:                            func(s OrderStatus) bool { return s.Status == "success" },
	}
	for in, ok := range cases {
		var s OrderStatus
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("unmarshal %s: %v", in, err)
		}
		if !ok(s) {
			t.Errorf("unexpected parse of %s: %+v", in, s)
		}
	}
}
