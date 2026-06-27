package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestTwapStatusFilterAndProgress(t *testing.T) {
	resp := func(path, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "webData2":
			return 200, `{"twapStates":[[42,{"coin":"BTC","side":"B","sz":"1.0","executedSz":"0.25","executedNtl":"16250","minutes":30,"timestamp":1750000000000}],[43,{"coin":"ETH","side":"A","sz":"5.0","executedSz":"1.0","executedNtl":"3200","minutes":10,"timestamp":1750000001000}]]}`
		case "userTwapSliceFills":
			return 200, `[{"fill":{"coin":"BTC","px":"65000","sz":"0.25","side":"B","time":1,"oid":7,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":1},"twapId":42},{"fill":{"coin":"ETH","px":"3200","sz":"1.0","side":"A","time":2,"oid":8,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open Short","crossed":true,"tid":2},"twapId":43}]`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)

	t.Run("all twaps with progress", func(t *testing.T) {
		v, err := c.TwapStatus(ctx, "", 0)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(v.Running) != 2 {
			t.Fatalf("want 2 running, got %d", len(v.Running))
		}
		if v.Running[0].TwapID != 42 || v.Running[0].ProgressPct != "25" || v.Running[0].Side != "buy" {
			t.Fatalf("BTC progress wrong: %+v", v.Running[0])
		}
		if len(v.SliceFills) != 2 {
			t.Fatalf("want 2 slice fills, got %d", len(v.SliceFills))
		}
	})

	t.Run("filter by id", func(t *testing.T) {
		v, err := c.TwapStatus(ctx, "", 43)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(v.Running) != 1 || v.Running[0].Coin != "ETH" || v.Running[0].Side != "sell" {
			t.Fatalf("id filter wrong: %+v", v.Running)
		}
		if len(v.SliceFills) != 1 || v.SliceFills[0].TwapID != 43 {
			t.Fatalf("slice-fill id filter wrong: %+v", v.SliceFills)
		}
	})

	t.Run("filter by coin", func(t *testing.T) {
		v, err := c.TwapStatus(ctx, "BTC", 0)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(v.Running) != 1 || v.Running[0].Coin != "BTC" {
			t.Fatalf("coin filter wrong: %+v", v.Running)
		}
	})
}
