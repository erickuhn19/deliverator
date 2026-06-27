package cmd

import (
	"encoding/json"
	"testing"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// A batch file may write sizes/prices as strings OR bare numbers; both must
// round-trip to the exact literal (no float precision loss) and a trigger maps.
func TestBatchOrderJSONParsing(t *testing.T) {
	const body = `[
		{"coin":"BTC","side":"buy","size":"0.001","limit":"60000.5","tif":"Alo"},
		{"coin":"ETH","side":"sell","size":0.25,"limit":3000,"reduce_only":true,
		 "trigger":{"trigger_px":2900,"is_market":true,"tpsl":"sl"}}
	]`
	var rows []batchOrderJSON
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}

	a, err := rows[0].toOrderReq()
	if err != nil {
		t.Fatal(err)
	}
	if a.Coin != "BTC" || a.Side != core.Buy || a.Size != "0.001" || a.Limit != "60000.5" || a.Tif != "Alo" {
		t.Errorf("row 0 mapped wrong: %+v", a)
	}

	b, err := rows[1].toOrderReq()
	if err != nil {
		t.Fatal(err)
	}
	// numbers preserved as their literal tokens
	if b.Size != "0.25" || b.Limit != "3000" || !b.ReduceOnly || b.Side != core.Sell {
		t.Errorf("row 1 mapped wrong: %+v", b)
	}
	if b.Trigger == nil || b.Trigger.TriggerPx != "2900" || !b.Trigger.IsMarket || b.Trigger.Tpsl != "sl" {
		t.Errorf("row 1 trigger mapped wrong: %+v", b.Trigger)
	}
}

func TestBatchOrderJSONBadSide(t *testing.T) {
	var rows []batchOrderJSON
	if err := json.Unmarshal([]byte(`[{"coin":"BTC","side":"hodl","size":"1"}]`), &rows); err != nil {
		t.Fatal(err)
	}
	if _, err := rows[0].toOrderReq(); err == nil {
		t.Error("an invalid side should error")
	}
}

func TestBatchExit(t *testing.T) {
	exitOf := func(err error) int {
		if err == nil {
			return 0
		}
		if ce, ok := err.(*output.CmdError); ok {
			return ce.Code
		}
		return -1
	}
	rest := &core.PlaceResult{Status: "resting"}
	full := &core.PlaceResult{Status: "filled", FilledSz: "0.001", Size: "0.001"}
	partial := &core.PlaceResult{Status: "filled", FilledSz: "0.0005", Size: "0.001"}
	rejected := &core.PlaceResult{Status: "rejected"}
	unknown := &core.PlaceResult{Status: "unknown"}

	cases := []struct {
		name string
		in   []*core.PlaceResult
		want int
	}{
		{"empty", nil, 0},
		{"all resting", []*core.PlaceResult{rest, rest}, 0},
		{"full fill", []*core.PlaceResult{full, rest}, 0},
		{"some rejected", []*core.PlaceResult{rest, rejected}, output.ExitPartial},
		{"all rejected", []*core.PlaceResult{rejected, rejected}, output.ExitExchange},
		{"partial fill", []*core.PlaceResult{rest, partial}, output.ExitPartial},
		{"unknown leg", []*core.PlaceResult{rest, unknown}, output.ExitPartial},
	}
	for _, tc := range cases {
		if got := exitOf(batchExit(tc.in)); got != tc.want {
			t.Errorf("%s: exit = %d, want %d", tc.name, got, tc.want)
		}
	}
}
