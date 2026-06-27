package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestApplyNotionalDerivesSize(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"allMids": `{"BTC":"50000"}`,
	}))
	mk, _ := c.meta.Lookup("BTC")

	// explicit limit => size = notional / limit
	if r, err := c.applyNotional(ctx, mk, OrderReq{Coin: "BTC", Notional: 1000, Limit: "50000"}); err != nil || r.Size != "0.02" {
		t.Fatalf("limit-priced notional: size=%q err=%v", r.Size, err)
	}
	// market => size = notional / live mid (50000)
	if r, err := c.applyNotional(ctx, mk, OrderReq{Coin: "BTC", Notional: 500}); err != nil || r.Size != "0.01" {
		t.Fatalf("mid-priced notional: size=%q err=%v", r.Size, err)
	}
	// trigger px is the reference when there's no limit
	if r, err := c.applyNotional(ctx, mk, OrderReq{Coin: "BTC", Notional: 1000, Trigger: &TriggerReq{TriggerPx: "40000"}}); err != nil || r.Size != "0.025" {
		t.Fatalf("trigger-priced notional: size=%q err=%v", r.Size, err)
	}
	// an explicit size is left untouched even if Notional is set
	if r, _ := c.applyNotional(ctx, mk, OrderReq{Coin: "BTC", Size: "0.5", Notional: 1000}); r.Size != "0.5" {
		t.Fatalf("explicit size must win, got %q", r.Size)
	}
}

func TestApplyNotionalNoMidFailsClosed(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{"allMids": `{}`}))
	mk, _ := c.meta.Lookup("BTC")
	if _, err := c.applyNotional(ctx, mk, OrderReq{Coin: "BTC", Notional: 100}); err == nil {
		t.Fatal("a market --notional with no mid must error, not size to 0")
	}
}
