package hl

import "testing"

func TestTwapStatesAndSliceFills(t *testing.T) {
	responses := map[string]string{
		// webData2.twapStates is an array of [id, state] tuples.
		"webData2":           `{"twapStates":[[42,{"coin":"BTC","side":"B","sz":"1.0","executedSz":"0.4","executedNtl":"26000","minutes":30,"reduceOnly":false,"randomize":true,"timestamp":1750000000000}],[43,{"coin":"ETH","side":"A","sz":"5.0","executedSz":"5.0","executedNtl":"16000","minutes":10,"timestamp":1750000001000}]]}`,
		"userTwapSliceFills": `[{"fill":{"coin":"BTC","px":"65000","sz":"0.2","side":"B","time":1,"oid":7,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":1},"twapId":42}]`,
	}
	fn := func(typ string, _ map[string]any) (int, string) {
		if r, ok := responses[typ]; ok {
			return 200, r
		}
		return 200, `{}`
	}
	info, ctx := testInfo(t, fn)

	states, err := info.TwapStates(ctx, "0xabc")
	if err != nil {
		t.Fatalf("TwapStates err: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("want 2 states, got %d", len(states))
	}
	if states[0].ID != 42 || states[0].Coin != "BTC" || states[0].ExecutedSz != "0.4" || states[0].Minutes != 30 || !states[0].Randomize {
		t.Fatalf("bad first state: %+v", states[0])
	}
	if states[1].ID != 43 || states[1].Side != "A" {
		t.Fatalf("bad second state: %+v", states[1])
	}

	fills, err := info.UserTwapSliceFills(ctx, "0xabc")
	if err != nil {
		t.Fatalf("UserTwapSliceFills err: %v", err)
	}
	if len(fills) != 1 || fills[0].TwapID != 42 || fills[0].Fill.Coin != "BTC" || fills[0].Fill.Price != "65000" {
		t.Fatalf("bad slice fills: %+v", fills)
	}
}
