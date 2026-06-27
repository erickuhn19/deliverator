package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// Panic must cancel every RUNNING TWAP (they live in webData2.twapStates, not in
// open orders) — else a live TWAP keeps slicing and rebuilds a position after the
// flatten. It also writes a first-class "panic" audit row.
func TestPanicCancelsRunningTwaps(t *testing.T) {
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "frontendOpenOrders":
				return 200, "[]"
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			case "webData2":
				return 200, `{"twapStates":[[99,{"coin":"BTC","side":"B","sz":"1.0"}]]}`
			}
			return 200, "{}"
		}
		return 200, `{"status":"ok","response":{"type":"twapCancel","data":{"status":"success"}}}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	res, err := c.Panic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TwapsCanceled) != 1 || res.TwapsCanceled[0] != 99 {
		t.Fatalf("panic must cancel the running TWAP, got %+v", res)
	}
	if !res.Complete {
		t.Errorf("a clean teardown should verify Complete: %+v", res)
	}
	a := readAudit(t)
	if a[len(a)-1]["action"] != "panic" {
		t.Errorf("panic must write a first-class audit row: %v", a[len(a)-1])
	}
}

// A sub-dex read failure must NOT be silently swallowed: panic surfaces the dex
// in Degraded and reports Complete=false, so an incomplete emergency-flatten is
// never read as success.
func TestPanicDegradedOnSubDexReadFailure(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "frontendOpenOrders":
				if body["dex"] == "xyz" {
					return 200, `{"not":"an array"}` // unmarshal error => read fails
				}
				return 200, "[]"
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `{"balances":[]}`
			case "webData2":
				return 200, `{"twapStates":[]}`
			}
			return 200, "{}"
		}
		return 200, defaultEx
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	res, err := c.Panic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatalf("a sub-dex read failure must mark panic NOT complete: %+v", res)
	}
	found := false
	for _, d := range res.Degraded {
		if d == "xyz" {
			found = true
		}
	}
	if !found {
		t.Errorf("Degraded must name the failed sub-dex, got %v", res.Degraded)
	}
}
