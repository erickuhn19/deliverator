package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

func TestPaperOpenGate(t *testing.T) {
	t.Setenv("DELIVERATOR_HOME", t.TempDir())
	cfg := config.Default()
	if err := PaperOpenGate(cfg, "BTC", 10, 10000, 10000); err != nil {
		t.Fatalf("margin 1000 at 10x is the default-cap boundary: %v", err)
	}
	assertRiskCode(t, PaperOpenGate(cfg, "BTC", 11, 10000, 10000), "max_leverage")
	assertRiskCode(t, PaperOpenGate(cfg, "BTC", 10, 10000.01, 10000.01), "max_order_notional")
	assertRiskCode(t, PaperOpenGate(cfg, "BTC", 10, 10000, 50000.01), "max_position_notional")

	limited := config.Default()
	limited.Automation.LimitOnly = true
	assertRiskCode(t, PaperOpenGate(limited, "BTC", 10, 10000, 10000), "limit_only")

	locked := config.Default()
	locked.Automation.AllowedCoins = []string{"ETH"}
	assertRiskCode(t, PaperOpenGate(locked, "BTC", 10, 10000, 10000), "coin_not_allowed")

	if err := os.WriteFile(filepath.Join(config.Dir(), "halt"), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var oe *output.Error
	if err := PaperOpenGate(cfg, "BTC", 10, 10000, 10000); !errors.As(err, &oe) || oe.Code != "halted" || oe.ExitCode() != output.ExitHalt {
		t.Fatalf("halt: %v", err)
	}
	if err := GlobalHaltError(); !errors.As(err, &oe) || oe.ExitCode() != output.ExitHalt {
		t.Fatalf("GlobalHaltError: %v", err)
	}
}

func TestPaperPortfolioGateCountsBothBooks(t *testing.T) {
	if PortfolioGatesConfigured(config.Default()) {
		t.Fatal("default config must not force a Hyperliquid read on a Papertrade open")
	}
	c := &Client{cfg: config.Default()}
	if err := c.GatePaperExposure(context.Background(), "BTC", 10000, 3); err != nil {
		t.Fatal(err)
	}

	clearing := emptyState
	live, ctx := newTestClient(t, riskCfg(config.Risk{MaxOpenPositions: 1}), Options{}, gateResp(&clearing, noSpot))
	if err := live.GatePaperExposure(ctx, "BTC", 1000, 0); err != nil {
		t.Fatalf("one new slug on an empty book fits a cap of 1: %v", err)
	}
	assertRiskCode(t, live.GatePaperExposure(ctx, "BTC", 1000, 1), "max_open_positions")

	held := clearingWith("100000", posWith("ETH", "1", "3000"))
	busy, ctx := newTestClient(t, riskCfg(config.Risk{MaxOpenPositions: 1}), Options{}, gateResp(&held, noSpot))
	assertRiskCode(t, busy.GatePaperExposure(ctx, "BTC", 1000, 0), "max_open_positions")

	down, ctx := newTestClient(t, riskCfg(config.Risk{MaxOpenPositions: 2}), Options{}, func(_, typ string, _ map[string]any) (int, string) {
		if typ == "clearinghouseState" || typ == "spotClearinghouseState" || typ == "frontendOpenOrders" {
			return 500, "nope"
		}
		return 200, `{}`
	})
	err := down.GatePaperExposure(ctx, "BTC", 1000, 0)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Category != output.CatNetwork || oe.ExitCode() != output.ExitNetwork {
		t.Fatalf("unreadable Hyperliquid book must fail closed, got %v", err)
	}
}
