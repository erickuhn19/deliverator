package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestPnlAttribution(t *testing.T) {
	// BTC: realized +50, fee 0.10, builderFee 0.05; ETH: realized −20, fee 0.20 (no builder).
	fills := `[` +
		`{"coin":"BTC","px":"60000","sz":"0.01","side":"A","time":3,"oid":3,"hash":"0x","fee":"0.10","feeToken":"USDC","builderFee":"0.05","closedPnl":"50","startPosition":"0.01","dir":"Close","crossed":true,"tid":3},` +
		`{"coin":"ETH","px":"2000","sz":"0.1","side":"A","time":2,"oid":2,"hash":"0x","fee":"0.20","feeToken":"USDC","closedPnl":"-20","startPosition":"0.1","dir":"Close","crossed":true,"tid":2}` +
		`]`
	// funding: BTC +1.00 received, ETH −0.50 paid.
	funding := `[` +
		`{"delta":{"coin":"BTC","fundingRate":"0.0001","size":"0.01","type":"funding","usdc":"1.00"},"hash":"0x","time":3},` +
		`{"delta":{"coin":"ETH","fundingRate":"0.0001","size":"0.1","type":"funding","usdc":"-0.50"},"hash":"0x","time":2}` +
		`]`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"userFills":   fills,
		"userFunding": funding,
	}))
	v, err := c.PnlAttribution(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.ByCoin) != 2 || v.ByCoin[0].Coin != "BTC" || v.ByCoin[1].Coin != "ETH" {
		t.Fatalf("by_coin: %+v", v.ByCoin)
	}
	// BTC net = 50 − 0.10 − 0.05 + 1.00 = 50.85
	if v.ByCoin[0].NetSessionPnl != "50.85" || v.ByCoin[0].BuilderFees != "-0.05" {
		t.Fatalf("BTC row wrong: %+v", v.ByCoin[0])
	}
	// ETH net = −20 − 0.20 + 0 − 0.50 = −20.7
	if v.ByCoin[1].NetSessionPnl != "-20.7" {
		t.Fatalf("ETH net wrong: %+v", v.ByCoin[1])
	}
	// totals net = 50.85 − 20.7 = 30.15
	if v.Totals.Coin != "*TOTAL*" || v.Totals.NetSessionPnl != "30.15" {
		t.Fatalf("totals wrong: %+v", v.Totals)
	}
}

func TestPnlAttributionCoinFilter(t *testing.T) {
	fills := `[{"coin":"BTC","px":"60000","sz":"0.01","side":"A","time":1,"oid":1,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"10","startPosition":"0","dir":"Close","crossed":true,"tid":1},` +
		`{"coin":"ETH","px":"2000","sz":"0.1","side":"A","time":1,"oid":2,"hash":"0x","fee":"0.1","feeToken":"USDC","closedPnl":"5","startPosition":"0","dir":"Close","crossed":true,"tid":2}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"userFills":   fills,
		"userFunding": `[]`,
	}))
	v, err := c.PnlAttribution(ctx, nil, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.ByCoin) != 1 || v.ByCoin[0].Coin != "BTC" {
		t.Fatalf("coin filter should keep only BTC, got %+v", v.ByCoin)
	}
}
