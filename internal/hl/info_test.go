package hl

import (
	"context"
	"testing"
)

func TestNewInfoAssetMapping(t *testing.T) {
	meta := &Meta{Universe: []AssetInfo{
		{Name: "BTC", SzDecimals: 5},
		{Name: "ETH", SzDecimals: 4},
	}}
	spot := &SpotMeta{
		Universe: []SpotAssetInfo{{Name: "PURR/USDC", Index: 0, Tokens: []int{1}}},
		Tokens:   []SpotTokenInfo{{Index: 1, SzDecimals: 2}},
	}
	// meta + spot provided => no network fetch.
	info := NewInfo(context.Background(), "", true, meta, spot, nil)

	for coin, want := range map[string]int{"BTC": 0, "ETH": 1, "PURR/USDC": spotAssetIndexOffset} {
		got, ok := info.CoinToAsset(coin)
		if !ok || got != want {
			t.Errorf("CoinToAsset(%q) = %d ok=%v, want %d", coin, got, ok, want)
		}
	}
	if got := info.assetToDecimal[0]; got != 5 {
		t.Errorf("BTC szDecimals = %d, want 5", got)
	}
	if got := info.assetToDecimal[spotAssetIndexOffset]; got != 2 {
		t.Errorf("spot szDecimals = %d, want 2 (from base token)", got)
	}
	if _, ok := info.CoinToAsset("NOPE"); ok {
		t.Errorf("CoinToAsset(NOPE) should be absent")
	}
}

func TestParseMetaResponse(t *testing.T) {
	raw := []byte(`{
		"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50,"onlyIsolated":false,"isDelisted":false}],
		"marginTables":[[50,{"description":"tier","marginTiers":[{"lowerBound":"0","maxLeverage":50}]}]],
		"collateralToken":0
	}`)
	m, err := parseMetaResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Universe) != 1 || m.Universe[0].Name != "BTC" || m.Universe[0].MaxLeverage != 50 {
		t.Fatalf("universe: %+v", m.Universe)
	}
	if len(m.MarginTables) != 1 || m.MarginTables[0].ID != 50 || m.MarginTables[0].Description != "tier" {
		t.Fatalf("margin tables: %+v", m.MarginTables)
	}
	if len(m.MarginTables[0].MarginTiers) != 1 || m.MarginTables[0].MarginTiers[0].MaxLeverage != 50 {
		t.Fatalf("margin tiers: %+v", m.MarginTables[0].MarginTiers)
	}
}

// A meta payload without marginTables/collateralToken must not panic.
func TestParseMetaResponseMinimal(t *testing.T) {
	m, err := parseMetaResponse([]byte(`{"universe":[{"name":"BTC","szDecimals":5}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Universe) != 1 || m.Universe[0].Name != "BTC" {
		t.Fatalf("universe: %+v", m.Universe)
	}
	if len(m.MarginTables) != 0 {
		t.Fatalf("expected no margin tables, got %+v", m.MarginTables)
	}
}
