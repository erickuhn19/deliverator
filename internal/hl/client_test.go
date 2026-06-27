package hl

import (
	"encoding/json"
	"testing"
)

func TestAPIResponseUnmarshal(t *testing.T) {
	t.Run("ok_resting", func(t *testing.T) {
		var r APIResponse[OrderResponse]
		if err := json.Unmarshal([]byte(`{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":123}}]}}}`), &r); err != nil {
			t.Fatal(err)
		}
		if !r.Ok || r.Type != "order" || len(r.Data.Statuses) != 1 {
			t.Fatalf("unexpected: %+v", r)
		}
		if r.Data.Statuses[0].Resting == nil || r.Data.Statuses[0].Resting.Oid != 123 {
			t.Fatalf("resting oid: %+v", r.Data.Statuses[0])
		}
	})
	t.Run("ok_filled", func(t *testing.T) {
		var r APIResponse[OrderResponse]
		if err := json.Unmarshal([]byte(`{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"1.5","avgPx":"100.2","oid":7}}]}}}`), &r); err != nil {
			t.Fatal(err)
		}
		f := r.Data.Statuses[0].Filled
		if f == nil || f.Oid != 7 || f.TotalSz != "1.5" || f.AvgPx != "100.2" {
			t.Fatalf("filled: %+v", f)
		}
	})
	t.Run("ok_per_order_error", func(t *testing.T) {
		var r APIResponse[OrderResponse]
		if err := json.Unmarshal([]byte(`{"status":"ok","response":{"type":"order","data":{"statuses":[{"error":"insufficient margin"}]}}}`), &r); err != nil {
			t.Fatal(err)
		}
		if !r.Ok || r.Data.Statuses[0].Error == nil || *r.Data.Statuses[0].Error != "insufficient margin" {
			t.Fatalf("per-order error not parsed: %+v", r)
		}
	})
	t.Run("not_ok_string", func(t *testing.T) {
		var r APIResponse[OrderResponse]
		if err := json.Unmarshal([]byte(`{"status":"err","response":"Must deposit before trading"}`), &r); err != nil {
			t.Fatal(err)
		}
		if r.Ok || r.Err != "Must deposit before trading" {
			t.Fatalf("expected not-ok with err string, got %+v", r)
		}
	})
	t.Run("missing_data", func(t *testing.T) {
		var r APIResponse[OrderResponse]
		err := json.Unmarshal([]byte(`{"status":"ok","response":{"type":"order"}}`), &r)
		if err == nil {
			t.Fatal("expected error for missing response.data")
		}
	})
}

func TestMixedArrayFirstError(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string // "" => no error
	}{
		{"all_success", `["success","success"]`, ""},
		{"empty", `[]`, ""},
		{"string_error", `["success","Order was never placed"]`, "Order was never placed"},
		{"object_error", `[{"error":"bad oid"}]`, "bad oid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ma MixedArray
			if err := json.Unmarshal([]byte(c.json), &ma); err != nil {
				t.Fatal(err)
			}
			got := ma.FirstError()
			if c.wantErr == "" {
				if got != nil {
					t.Fatalf("want no error, got %v", got)
				}
				return
			}
			if got == nil || got.Error() != c.wantErr {
				t.Fatalf("want %q, got %v", c.wantErr, got)
			}
		})
	}
}

func TestSpotUserStateUnifiedCollateral(t *testing.T) {
	raw := `{"balances":[{"coin":"USDC","token":0,"total":"122.26","hold":"0.0","entryNtl":"0.0"}],"tokenToAvailableAfterMaintenance":[[0,"122.258893"],[235,"0.0"]]}`
	var s SpotUserState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.TokenToAvailableAfterMaintenance) != 2 {
		t.Fatalf("entries: %+v", s.TokenToAvailableAfterMaintenance)
	}
	ta := s.TokenToAvailableAfterMaintenance[0]
	if ta.Token != 0 || ta.Available != "122.258893" {
		t.Fatalf("USDC collateral entry = %+v, want {0, 122.258893}", ta)
	}
}

func TestMixedValue(t *testing.T) {
	var mv MixedValue
	if err := json.Unmarshal([]byte(`"hello"`), &mv); err != nil {
		t.Fatal(err)
	}
	if s, ok := mv.String(); !ok || s != "hello" {
		t.Fatalf("String(): %q ok=%v", s, ok)
	}
	var obj MixedValue
	_ = json.Unmarshal([]byte(`{"a":1}`), &obj)
	m, ok := obj.Object()
	if !ok || m["a"].(float64) != 1 {
		t.Fatalf("Object(): %+v ok=%v", m, ok)
	}
	var num MixedValue
	_ = json.Unmarshal([]byte(`42`), &num)
	var got int
	if err := num.Parse(&got); err != nil || got != 42 {
		t.Fatalf("Parse(): %d err=%v", got, err)
	}
}
