package core

import (
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
)

// pv builds a PositionView with just the distance field set (the only input
// evalLiqDistance reads).
func pv(coin, dist string) PositionView {
	return PositionView{Coin: coin, DistanceToLiqPct: dist}
}

func TestEvalLiqDistance(t *testing.T) {
	cfg := WatchConfig{Metric: WatchLiqDistancePct, Below: 5}

	t.Run("no positions => no value, no breach", func(t *testing.T) {
		ev := cfg.evalLiqDistance(nil)
		if ev.HasValue || ev.Breached || ev.Positions != 0 {
			t.Fatalf("got %+v, want empty/no-value", ev)
		}
		if ev.Threshold != "5" {
			t.Errorf("threshold = %q, want 5", ev.Threshold)
		}
	})

	t.Run("flat positions (no liq px) are ignored", func(t *testing.T) {
		ev := cfg.evalLiqDistance([]PositionView{pv("BTC", ""), pv("ETH", "")})
		if ev.HasValue || ev.Positions != 0 {
			t.Fatalf("got %+v, want no value", ev)
		}
	})

	t.Run("takes the minimum distance across positions", func(t *testing.T) {
		ev := cfg.evalLiqDistance([]PositionView{pv("BTC", "12.5"), pv("ETH", "3.2"), pv("SOL", "40")})
		if !ev.HasValue {
			t.Fatal("want HasValue")
		}
		if ev.Value != "3.2" {
			t.Errorf("value = %q, want 3.2", ev.Value)
		}
		if ev.WorstCoin != "ETH" {
			t.Errorf("worst_coin = %q, want ETH", ev.WorstCoin)
		}
		if ev.Positions != 3 {
			t.Errorf("positions = %d, want 3", ev.Positions)
		}
		if !ev.Breached {
			t.Error("3.2 < 5 should breach")
		}
	})

	t.Run("above threshold does not breach", func(t *testing.T) {
		ev := cfg.evalLiqDistance([]PositionView{pv("BTC", "8")})
		if !ev.HasValue || ev.Breached {
			t.Fatalf("got %+v, want value, no breach", ev)
		}
	})

	t.Run("exactly at threshold does not breach (strict <)", func(t *testing.T) {
		ev := cfg.evalLiqDistance([]PositionView{pv("BTC", "5")})
		if ev.Breached {
			t.Error("5 is not < 5; should not breach")
		}
	})
}

func TestCooldownGate(t *testing.T) {
	g := cooldownGate{cooldown: time.Minute}
	t0 := time.Unix(1_750_000_000, 0)

	if !g.allow(t0) {
		t.Fatal("first trigger must be allowed")
	}
	if g.allow(t0.Add(30 * time.Second)) {
		t.Error("within cooldown must be blocked")
	}
	if g.allow(t0.Add(59 * time.Second)) {
		t.Error("still within cooldown must be blocked")
	}
	if !g.allow(t0.Add(time.Minute)) {
		t.Error("at the cooldown boundary must be allowed again")
	}
	// After re-arming, the window restarts from the last allowed time.
	if g.allow(t0.Add(time.Minute + 10*time.Second)) {
		t.Error("re-armed window should block again")
	}
}

// A sub-dex clearinghouse state with a liquidationPx so DistanceToLiqPct computes:
// mark = 41/0.01 = 4100; liq 4305 => |4100-4305|/4100*100 = 5.0%.
const goldSubDexWithLiq = `{"assetPositions":[{"position":{"coin":"GOLD","szi":"-0.01","positionValue":"41","unrealizedPnl":"0","returnOnEquity":"0","marginUsed":"8","leverage":{"type":"isolated","value":5},"liquidationPx":"4305"},"type":"oneWay"}],"marginSummary":{"accountValue":"50","totalMarginUsed":"8","totalNtlPos":"41","totalRawUsd":"50"},"crossMarginSummary":{"accountValue":"50","totalMarginUsed":"8","totalNtlPos":"41","totalRawUsd":"50"},"withdrawable":"42"}`

// Finding 1 regression: a HIP-3 sub-dex position must contribute to the watch
// metric. Watch merges subDexPositions into the per-frame eval; this exercises
// that path (sub-dex clearinghouse -> evalLiqDistance) without a live socket.
func TestWatchMergesSubDexPositions(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "clearinghouseState" && body["dex"] == "xyz" {
			return 200, goldSubDexWithLiq
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)

	positions := c.subDexPositions(ctx, "")
	ev := WatchConfig{Metric: WatchLiqDistancePct, Below: 6}.evalLiqDistance(positions)
	if !ev.HasValue {
		t.Fatal("sub-dex position must contribute a metric value")
	}
	if ev.WorstCoin != "xyz:GOLD" {
		t.Fatalf("worst_coin = %q, want xyz:GOLD", ev.WorstCoin)
	}
	if ev.Value != "5" || !ev.Breached {
		t.Fatalf("distance %q should be 5 and breach below 6", ev.Value)
	}
}

// A sub-dex whose per-frame read fails must be reported in stale_dexs (so the
// failsafe's degraded coverage is observable) while a readable dex still
// contributes its position to the metric.
func TestWatchSubDexStaleReporting(t *testing.T) {
	cfg := config.Default()
	cfg.PerpDexs = []string{"xyz", "down"}
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "clearinghouseState" {
			switch body["dex"] {
			case "xyz":
				return 200, goldSubDexWithLiq
			case "down":
				return 500, `{"error":"sub-dex unavailable"}`
			}
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, cfg, Options{}, resp)

	pvs, stale := c.subDexPositionsTimeout(ctx, "", subDexWatchTimeout)
	if len(pvs) != 1 || pvs[0].Coin != "xyz:GOLD" {
		t.Fatalf("readable dex must still contribute, got %+v", pvs)
	}
	if len(stale) != 1 || stale[0] != "down" {
		t.Fatalf("failed dex must be reported stale, got %v", stale)
	}

	// And the stale set propagates onto the eval the monitor emits.
	ev := WatchConfig{Metric: WatchLiqDistancePct, Below: 6}.evalLiqDistance(pvs)
	ev.StaleDexs = stale
	if len(ev.StaleDexs) != 1 || ev.StaleDexs[0] != "down" {
		t.Fatalf("eval should carry stale_dexs, got %+v", ev.StaleDexs)
	}
}

func TestCooldownGateZeroFiresEveryTime(t *testing.T) {
	g := cooldownGate{cooldown: 0}
	t0 := time.Unix(1_750_000_000, 0)
	for i := 0; i < 3; i++ {
		if !g.allow(t0.Add(time.Duration(i) * time.Millisecond)) {
			t.Fatalf("zero cooldown should always allow (i=%d)", i)
		}
	}
}
