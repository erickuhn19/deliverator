package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// frontendOpenOrders is per-dex: Orders() must sweep every configured HIP-3
// sub-dex (sending the dex field) and merge, or sub-dex resting orders are
// invisible to orders / cancel --all / modify / panic.
func TestOrdersSweepsSubDex(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	mainOrders := "[" + openOrderJSON("BTC", 1, "") + "]"
	subOrders := `[{"coin":"xyz:GOLD","oid":2,"cloid":null,"limitPx":"3000","origSz":"0.004","sz":"0.004","side":"B","orderType":"Limit","tif":"Gtc","reduceOnly":false,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0","timestamp":1}]`
	sawDex := false
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "frontendOpenOrders" {
			if body["dex"] == "xyz" {
				sawDex = true
				return 200, subOrders
			}
			return 200, mainOrders
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)
	orders, err := c.Orders(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sawDex {
		t.Error("Orders must query the sub-dex with the dex field")
	}
	coins := map[string]bool{}
	for _, o := range orders {
		coins[o.Coin] = true
	}
	if !coins["BTC"] || !coins["xyz:GOLD"] {
		t.Fatalf("Orders must include BOTH main-dex and sub-dex orders, got %v", coins)
	}
}

// With no sub-dex configured, Orders() does a single main-dex read (no sweep).
func TestOrdersNoSubDexSingleRead(t *testing.T) {
	calls := 0
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "frontendOpenOrders" {
			calls++
			return 200, "[]"
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp) // PerpDexs empty
	if _, err := c.Orders(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("no sub-dex configured -> exactly one open-orders read, got %d", calls)
	}
}
