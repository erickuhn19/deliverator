package core

import (
	"bytes"
	"math"
	"os"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
)

func approxEq(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestComputePortfolioMetrics(t *testing.T) {
	m := computePortfolioMetrics(map[string]float64{"BTC": 1000, "ETH": -600, "DUST": 0.001})
	if m.openPositions != 2 {
		t.Errorf("openPositions=%d want 2 (DUST below the 0.005 dust filter)", m.openPositions)
	}
	if m.maxCoin != "BTC" || !approxEq(m.maxCoinNotional, 1000) {
		t.Errorf("maxCoin=%s/%v want BTC/1000", m.maxCoin, m.maxCoinNotional)
	}
	if !approxEq(m.gross, 1600.001) || !approxEq(m.net, 400.001) {
		t.Errorf("gross/net=%v/%v want ~1600.001/~400.001", m.gross, m.net)
	}
}

func TestReadRiskStateNoMutationAndCompute(t *testing.T) {
	testHome(t)
	_ = os.MkdirAll(config.Dir(), 0o700)
	today := time.Now().UTC().Format("2006-01-02")
	content := []byte(`{"peak_equity":1000,"day":"` + today + `","day_anchor_equity":900}`)
	if err := os.WriteFile(riskStatePath(), content, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(riskStatePath())

	st, dd, dlUSD, dlPct, found := ReadRiskState(800)
	if !found {
		t.Fatal("want found=true")
	}
	if st.PeakEquity != 1000 {
		t.Errorf("peak=%v want 1000", st.PeakEquity)
	}
	if !approxEq(dd, 20) { // (1000-800)/1000*100
		t.Errorf("drawdown=%v want 20", dd)
	}
	if !approxEq(dlUSD, 100) || !approxEq(dlPct, 100.0/900*100) { // 900-800
		t.Errorf("dailyLoss=%v/%v want 100 / ~11.1", dlUSD, dlPct)
	}
	after, _ := os.ReadFile(riskStatePath())
	if !bytes.Equal(before, after) {
		t.Fatal("ReadRiskState MUTATED risk_state.json — it must be strictly read-only")
	}
}

func TestReadRiskStateFreshAndCorrupt(t *testing.T) {
	testHome(t)
	if _, _, _, _, found := ReadRiskState(1000); found {
		t.Error("missing file should yield found=false")
	}
	_ = os.MkdirAll(config.Dir(), 0o700)
	_ = os.WriteFile(riskStatePath(), []byte("{not json"), 0o600)
	if _, _, _, _, found := ReadRiskState(1000); found {
		t.Error("corrupt file should yield found=false")
	}
}

func TestReadRiskStateStaleDayNoDailyLoss(t *testing.T) {
	testHome(t)
	_ = os.MkdirAll(config.Dir(), 0o700)
	// Anchor from a different UTC day: a new day re-anchors on the next write, so a
	// read-only view reports no daily loss; drawdown (from the stored peak) still computes.
	content := []byte(`{"peak_equity":1000,"day":"2000-01-01","day_anchor_equity":900}`)
	_ = os.WriteFile(riskStatePath(), content, 0o600)
	_, dd, dlUSD, _, found := ReadRiskState(800)
	if !found || !approxEq(dd, 20) {
		t.Errorf("drawdown should still compute: dd=%v found=%v", dd, found)
	}
	if dlUSD != 0 {
		t.Errorf("stale-day daily loss should be 0, got %v", dlUSD)
	}
}

func capByKey(caps []RiskCap, key string) (RiskCap, bool) {
	for _, c := range caps {
		if c.Key == key {
			return c, true
		}
	}
	return RiskCap{}, false
}

func TestRiskStatusUtilization(t *testing.T) {
	testHome(t)
	cfg := config.Default()
	cfg.Risk.MaxNetExposureUSD = 100000
	cfg.Risk.MaxAccountLeverage = 5
	c, ctx := newTestClient(t, cfg, Options{}, infoMap(map[string]string{
		"clearinghouseState":     btcShort, // BTC short, positionValue 640, accountValue 100000
		"frontendOpenOrders":     `[]`,
		"spotClearinghouseState": `{"balances":[{"coin":"USDC","token":0,"total":"500","hold":"0","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"480"]]}`,
	}))
	rv, err := c.RiskStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	net, ok := capByKey(rv.Caps, "risk.max_net_exposure_usd")
	if !ok || !net.Active || net.Current == nil || net.UtilPct == nil {
		t.Fatalf("net-exposure cap should be active + show utilization: %+v", net)
	}
	if !approxEq(*net.Current, 640) {
		t.Errorf("net current=%v want ~640 (btcShort notional)", *net.Current)
	}
	lev, _ := capByKey(rv.Caps, "risk.max_account_leverage")
	if lev.Current == nil || lev.UtilPct == nil {
		t.Errorf("account-leverage cap should show utilization: %+v", lev)
	}
	if rv.Halted {
		t.Error("no halt file in a fresh temp home => not halted")
	}
	dd, _ := capByKey(rv.Caps, "risk.max_drawdown_pct")
	if dd.Active || dd.UtilPct != nil {
		t.Errorf("drawdown cap is off by default (0): %+v", dd)
	}
}
