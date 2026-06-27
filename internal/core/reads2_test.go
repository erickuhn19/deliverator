package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestFundingLedgerCandles(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(map[string]string{
		"userFunding":                 `[{"delta":{"coin":"BTC","fundingRate":"0.0001","size":"0.01","type":"funding","usdc":"0.05"},"hash":"0x","time":1}]`,
		"userNonFundingLedgerUpdates": `[{"delta":{"type":"deposit","usdc":"100"},"hash":"0x","time":2}]`,
		"candleSnapshot":              `[{"t":1,"T":2,"s":"BTC","i":"1m","o":"1","h":"2","l":"1","c":"2","v":"10","n":3}]`,
	}))
	if f, err := c.Funding(ctx, nil); err != nil || len(f) != 1 || f[0].Delta.Coin != "BTC" {
		t.Fatalf("funding: %+v err=%v", f, err)
	}
	if l, err := c.Ledger(ctx, nil); err != nil || len(l) != 1 || l[0].Delta.Type != "deposit" {
		t.Fatalf("ledger: %+v err=%v", l, err)
	}
	if cd, err := c.Candles(ctx, "BTC", "1m", nil); err != nil || len(cd) != 1 || cd[0].Close != "2" {
		t.Fatalf("candles: %+v err=%v", cd, err)
	}
}

func TestBuilderStatus(t *testing.T) {
	cfg := config.Default()
	cfg.Builder.Address = "0xBuilderEOA"
	cfg.Builder.FeeTenthsBps = 40
	cfg.Builder.AttachMode = config.AttachAll
	c, ctx := newTestClient(t, cfg, Options{}, infoMap(map[string]string{"maxBuilderFee": `50`}))
	bs, err := c.BuilderStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bs.FeeTenthsBps != 40 || bs.AttachMode != config.AttachAll || bs.ApprovedMaxTenths == nil || *bs.ApprovedMaxTenths != 50 {
		t.Fatalf("builder status: %+v", bs)
	}
}

func TestMeasureSkew(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, infoMap(nil))
	// httptest responses carry a Date header, so skew resolves (≈0).
	if _, err := c.MeasureSkew(ctx); err != nil {
		t.Fatalf("measure skew: %v", err)
	}
}

func TestInfoPostRateLimitError(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, func(path, typ string, body map[string]any) (int, string) {
		return 429, `{"code":429,"msg":"rate limited"}`
	})
	var out any
	err := c.InfoPost(ctx, map[string]any{"type": "userRateLimit", "user": testMaster}, &out)
	if err == nil {
		t.Fatal("expected rate-limit error on 429")
	}
}
