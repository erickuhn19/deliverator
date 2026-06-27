package core

import (
	"path/filepath"
	"testing"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

func TestDexOfAndCoinMatches(t *testing.T) {
	if dexOf("xyz:BRENTOIL") != "xyz" || dexOf("BTC") != "" || dexOf("") != "" {
		t.Error("dexOf wrong")
	}
	if !coinMatches("xyz:BRENTOIL", "xyz:BRENTOIL") || !coinMatches("BRENTOIL", "xyz:BRENTOIL") {
		t.Error("coinMatches should match exact + prefix-stripped")
	}
	if coinMatches("BTC", "xyz:BRENTOIL") {
		t.Error("coinMatches false positive")
	}
}

func TestPerpDexAssetID(t *testing.T) {
	// HIP-3: base 100000 + dexIndex*10000 + indexInUniverse. xyz (dex 1):BRENTOIL
	// at universe index 49 -> 110049.
	if got := hl.PerpDexAsset(0, 0); got != 100000 {
		t.Errorf("PerpDexAsset(0,0) = %d, want 100000", got)
	}
	if got := hl.PerpDexAsset(1, 49); got != 110049 {
		t.Errorf("PerpDexAsset(1,49) = %d, want 110049", got)
	}
}

func TestMetaStoreAddPerpDex(t *testing.T) {
	ms := testMeta()
	ms.AddPerpDex(1, &hl.Meta{Universe: []hl.AssetInfo{
		{Name: "xyz:BRENTOIL", SzDecimals: 2, MaxLeverage: 20},
	}})
	mk, ok := ms.Lookup("xyz:BRENTOIL")
	if !ok {
		t.Fatal("xyz:BRENTOIL should resolve after AddPerpDex")
	}
	if mk.AssetIndex != 110000 || mk.SzDecimals != 2 || mk.PxDecimals != 4 || mk.IsSpot {
		t.Fatalf("sub-dex market wrong: %+v", mk)
	}
	if mk.MaxLeverage != 20 {
		t.Errorf("max leverage = %d, want 20", mk.MaxLeverage)
	}
	if len(ms.PerpDexEntries()) != 1 || ms.PerpDexEntries()[0].Index != 1 {
		t.Errorf("PerpDexEntries should report the loaded dex: %+v", ms.PerpDexEntries())
	}
}

func TestMetaStoreBuildAndLookup(t *testing.T) {
	ms := testMeta()

	btc, ok := ms.Lookup("BTC")
	if !ok || btc.AssetIndex != 0 || btc.SzDecimals != 5 || btc.PxDecimals != 1 || btc.MaxLeverage != 40 || btc.IsSpot {
		t.Fatalf("BTC market wrong: %+v", btc)
	}
	eth, ok := ms.Lookup("eth") // case-insensitive
	if !ok || eth.AssetIndex != 1 || eth.PxDecimals != 2 || !eth.OnlyIsolated {
		t.Fatalf("ETH market wrong: %+v", eth)
	}
	purr, ok := ms.Lookup("PURR/USDC")
	if !ok || !purr.IsSpot || purr.AssetIndex != 10000 || purr.PxDecimals != 8 {
		t.Fatalf("spot market wrong: %+v", purr)
	}
	if _, ok := ms.Lookup("NOPE"); ok {
		t.Error("unknown coin should not resolve")
	}
	if n := len(ms.Markets()); n != 3 {
		t.Errorf("Markets() = %d, want 3 (2 perp + 1 spot)", n)
	}
}

func TestMetaCacheRoundTrip(t *testing.T) {
	testHome(t)
	ms := testMeta()
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := ms.Save(path); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadMetaCache(path, "testnet")
	if !ok {
		t.Fatal("expected cache to load")
	}
	if len(got.Markets()) != len(ms.Markets()) {
		t.Errorf("loaded %d markets, want %d", len(got.Markets()), len(ms.Markets()))
	}
	if _, ok := got.Lookup("BTC"); !ok {
		t.Error("loaded cache should resolve BTC")
	}
	// Wrong network must not load.
	if _, ok := LoadMetaCache(path, "mainnet"); ok {
		t.Error("cache for testnet must not load as mainnet")
	}
	// Missing file must not load.
	if _, ok := LoadMetaCache(filepath.Join(t.TempDir(), "nope.json"), "testnet"); ok {
		t.Error("missing cache file must not load")
	}
}

func TestMetaStoreAge(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	ms := NewMetaStore("testnet", &hl.Meta{Universe: []hl.AssetInfo{{Name: "BTC", SzDecimals: 5}}}, &hl.SpotMeta{}, past)
	if ms.Age() < time.Hour {
		t.Errorf("Age() = %v, want >= 1h", ms.Age())
	}
	if !ms.FetchedAt().Equal(past) {
		t.Errorf("FetchedAt() = %v, want %v", ms.FetchedAt(), past)
	}
}
