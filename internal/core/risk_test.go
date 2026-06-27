package core

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// assertErr checks that err is an *output.Error of the expected category/exit.
func assertErr(t *testing.T, err error, wantCat output.Category, wantExit int) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error category %s, got nil", wantCat)
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %T: %v", err, err)
	}
	if oe.Category != wantCat {
		t.Errorf("category = %s, want %s (msg: %s)", oe.Category, wantCat, oe.Message)
	}
	if oe.ExitCode() != wantExit {
		t.Errorf("exit = %d, want %d", oe.ExitCode(), wantExit)
	}
}

func passCheck() riskCheck {
	return riskCheck{Coin: "BTC", IsMarket: false, NotionalUSD: 100, PositionNotionalUSD: 100}
}

func TestPreTradeChecksPass(t *testing.T) {
	cfg := config.Default()
	c := newCfgClient(t, cfg)
	if err := c.preTradeChecks(passCheck()); err != nil {
		t.Fatalf("clean check should pass, got %v", err)
	}
}

func TestPreTradeChecksHalt(t *testing.T) {
	c := newCfgClient(t, config.Default())
	if err := SetHalt(true); err != nil {
		t.Fatal(err)
	}
	assertErr(t, c.preTradeChecks(passCheck()), output.CatHalt, output.ExitHalt)
}

func TestPreTradeChecksAllowlist(t *testing.T) {
	cfg := config.Default()
	cfg.Automation.AllowedCoins = []string{"ETH", "SOL"}
	c := newCfgClient(t, cfg)
	// BTC not in the allowlist -> risk-rejected.
	assertErr(t, c.preTradeChecks(passCheck()), output.CatRisk, output.ExitRisk)
	// An allowed coin passes (case-insensitive).
	if err := c.preTradeChecks(riskCheck{Coin: "eth", NotionalUSD: 100}); err != nil {
		t.Errorf("allowed coin should pass, got %v", err)
	}
}

func TestPreTradeChecksLimitOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Automation.LimitOnly = true
	c := newCfgClient(t, cfg)
	// Market order blocked.
	assertErr(t, c.preTradeChecks(riskCheck{Coin: "BTC", IsMarket: true, NotionalUSD: 100}), output.CatRisk, output.ExitRisk)
	// Limit order (IsMarket false) still allowed.
	if err := c.preTradeChecks(riskCheck{Coin: "BTC", IsMarket: false, NotionalUSD: 100}); err != nil {
		t.Errorf("limit order should pass under limit_only, got %v", err)
	}
}

func TestPreTradeChecksMaxOrderNotional(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 1000
	c := newCfgClient(t, cfg)
	assertErr(t, c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 1500}), output.CatRisk, output.ExitRisk)
	if err := c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 900}); err != nil {
		t.Errorf("under cap should pass, got %v", err)
	}
}

func TestPreTradeChecksMaxPositionNotional(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxPositionNotionalUSD = 5000
	c := newCfgClient(t, cfg)
	assertErr(t, c.preTradeChecks(riskCheck{Coin: "BTC", NotionalUSD: 100, PositionNotionalUSD: 6000}), output.CatRisk, output.ExitRisk)
}

// Under concurrency the local rate cap must still admit at most max_orders_per_min:
// the flock serializes the read-modify-write so overlapping processes can't all
// pass the gate and clobber rate.log (a TOCTOU cap bypass).
func TestCheckRateCapConcurrent(t *testing.T) {
	cfg := config.Default()
	cfg.Automation.MaxOrdersPerMin = 5
	c := newCfgClient(t, cfg)

	const n = 40
	var passed int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.checkRateCap() == nil {
				atomic.AddInt64(&passed, 1)
			}
		}()
	}
	wg.Wait()
	if passed != 5 {
		t.Fatalf("concurrent rate cap admitted %d of %d, want exactly 5 (TOCTOU bypass)", passed, n)
	}
}

func TestPreTradeChecksRateCap(t *testing.T) {
	cfg := config.Default()
	cfg.Automation.MaxOrdersPerMin = 1
	c := newCfgClient(t, cfg)
	// First passes (records one), second trips the local cap.
	if err := c.preTradeChecks(passCheck()); err != nil {
		t.Fatalf("first within cap should pass, got %v", err)
	}
	assertErr(t, c.preTradeChecks(passCheck()), output.CatRateLimit, output.ExitRateLimit)
}

// A reduce-only order can't increase exposure, so it must bypass BOTH notional
// caps — otherwise a legitimate close/bracket gets blocked by the cap.
func TestPreTradeChecksReduceOnlyBypassesCaps(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxOrderNotionalUSD = 100
	cfg.Risk.MaxPositionNotionalUSD = 100
	c := newCfgClient(t, cfg)
	rc := riskCheck{Coin: "BTC", NotionalUSD: 5000, PositionNotionalUSD: 5000, ReduceOnly: true}
	if err := c.preTradeChecks(rc); err != nil {
		t.Fatalf("reduce-only must bypass notional caps, got %v", err)
	}
	// Same numbers but NOT reduce-only -> blocked.
	rc.ReduceOnly = false
	assertErr(t, c.preTradeChecks(rc), output.CatRisk, output.ExitRisk)
	// Reduce-only is still subject to halt.
	rc.ReduceOnly = true
	if err := SetHalt(true); err != nil {
		t.Fatal(err)
	}
	assertErr(t, c.preTradeChecks(rc), output.CatHalt, output.ExitHalt)
}

func TestCoinAllowed(t *testing.T) {
	c := newCfgClient(t, config.Default()) // empty allowlist => allow all
	if !c.coinAllowed("ANYTHING") {
		t.Error("empty allowlist should allow all")
	}
	c.cfg.Automation.AllowedCoins = []string{"BTC"}
	if !c.coinAllowed("btc") {
		t.Error("allowlist should be case-insensitive")
	}
	if c.coinAllowed("ETH") {
		t.Error("ETH not in allowlist should be rejected")
	}
}

func TestCheckLeverage(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxLeverage = 10
	c := newCfgClient(t, cfg)
	if err := c.checkLeverage(10); err != nil {
		t.Errorf("10x at cap should pass, got %v", err)
	}
	assertErr(t, c.checkLeverage(20), output.CatRisk, output.ExitRisk)
	// No cap configured => any leverage passes.
	c.cfg.Risk.MaxLeverage = 0
	if err := c.checkLeverage(100); err != nil {
		t.Errorf("no cap should pass, got %v", err)
	}
}

func TestHaltToggle(t *testing.T) {
	c := newCfgClient(t, config.Default())
	if c.Halted() {
		t.Fatal("should start unhalted")
	}
	if err := SetHalt(true); err != nil {
		t.Fatal(err)
	}
	if !c.Halted() {
		t.Error("should be halted after SetHalt(true)")
	}
	if err := SetHalt(false); err != nil {
		t.Fatal(err)
	}
	if c.Halted() {
		t.Error("should be unhalted after SetHalt(false)")
	}
}

func TestCheckRateCapDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Automation.MaxOrdersPerMin = 0 // disabled
	c := newCfgClient(t, cfg)
	for i := 0; i < 50; i++ {
		if err := c.checkRateCap(); err != nil {
			t.Fatalf("rate cap disabled should never trip, got %v at %d", err, i)
		}
	}
}
