package hl

import "testing"

func TestPredictedFundingsParse(t *testing.T) {
	body := `[["BTC",[["BinPerp",{"fundingRate":"0.0001","nextFundingTime":1750000000000}],["HlPerp",{"fundingRate":"0.0000125","nextFundingTime":1750000000000,"fundingIntervalHours":1}]]],["ETH",[["HlPerp",{"fundingRate":"-0.00002","nextFundingTime":1750000000000}]]]]`
	fn := func(typ string, _ map[string]any) (int, string) {
		if typ == "predictedFundings" {
			return 200, body
		}
		return 200, `{}`
	}
	info, ctx := testInfo(t, fn)

	pf, err := info.PredictedFundings(ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(pf) != 2 {
		t.Fatalf("want 2 coins, got %d", len(pf))
	}
	if pf[0].Coin != "BTC" || len(pf[0].Venues) != 2 {
		t.Fatalf("bad BTC entry: %+v", pf[0])
	}
	if pf[0].Venues[1].Venue != "HlPerp" || pf[0].Venues[1].FundingRate != "0.0000125" || pf[0].Venues[1].FundingIntervalHours != 1 {
		t.Fatalf("bad HlPerp venue: %+v", pf[0].Venues[1])
	}
	if pf[1].Coin != "ETH" || pf[1].Venues[0].FundingRate != "-0.00002" {
		t.Fatalf("bad ETH entry: %+v", pf[1])
	}
}

func TestHistoricalOrdersParse(t *testing.T) {
	body := `[{"order":{"coin":"BTC","side":"B","limitPx":"64000","sz":"0","oid":7,"origSz":"1.0","timestamp":1,"triggerCondition":"N/A","isTrigger":false,"triggerPx":"0","children":[],"isPositionTpsl":false,"reduceOnly":false,"orderType":"Limit","tif":"Gtc","cloid":"0xabc"},"status":"filled","statusTimestamp":1750000000000},{"order":{"coin":"ETH","side":"A","limitPx":"3200","sz":"2.0","oid":8,"origSz":"2.0","timestamp":2,"triggerCondition":"N/A","isTrigger":false,"triggerPx":"0","children":[],"isPositionTpsl":false,"reduceOnly":false,"orderType":"Limit","tif":"Alo"},"status":"canceled","statusTimestamp":1750000001000}]`
	fn := func(typ string, _ map[string]any) (int, string) {
		if typ == "historicalOrders" {
			return 200, body
		}
		return 200, `{}`
	}
	info, ctx := testInfo(t, fn)

	hos, err := info.HistoricalOrders(ctx, "0xabc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hos) != 2 {
		t.Fatalf("want 2 orders, got %d", len(hos))
	}
	if hos[0].Order.Coin != "BTC" || hos[0].Status != "filled" || hos[0].Order.Oid != 7 {
		t.Fatalf("bad first order: %+v", hos[0])
	}
	if hos[1].Status != "canceled" || hos[1].Order.Coin != "ETH" {
		t.Fatalf("bad second order: %+v", hos[1])
	}
}
