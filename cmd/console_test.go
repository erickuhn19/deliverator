package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
)

// fakeRiskClient stubs just RiskStatus (unstubbed methods panic, which is correct).
type fakeRiskClient struct{ core.ClientAPI }

func (fakeRiskClient) RiskStatus(ctx context.Context) (*core.RiskView, error) {
	return &core.RiskView{
		Equity: "1000",
		Caps: []core.RiskCap{
			{Key: "risk.max_net_exposure_usd", Label: "Net exposure", Unit: "usd", Value: "5000", Active: true},
		},
	}, nil
}

func TestRiskCmdRunE(t *testing.T) {
	withFakeClient(t, fakeRiskClient{})
	env, err := runCmd(t, riskCmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Cmd != "risk" {
		t.Fatalf("risk envelope: %+v", env)
	}
	if !bytes.Contains(env.Data, []byte("max_net_exposure_usd")) {
		t.Errorf("risk output should include the caps: %s", env.Data)
	}
}

func TestRiskCmdBuildClientError(t *testing.T) {
	withClientErr(t, errors.New("no agent key"))
	_, err := runCmd(t, riskCmd, nil)
	if err == nil {
		t.Fatal("risk must surface a build-client error")
	}
}

func TestSetConfigKeyPriorityKeys(t *testing.T) {
	cfg := config.Default()
	if err := setConfigKey(cfg, "risk.max_priority_bps", "5"); err != nil {
		t.Fatalf("set risk.max_priority_bps: %v", err)
	}
	if cfg.Risk.MaxPriorityBps != 5 {
		t.Errorf("max_priority_bps = %d want 5", cfg.Risk.MaxPriorityBps)
	}
	if err := setConfigKey(cfg, "automation.priority_bps", "3"); err != nil {
		t.Fatalf("set automation.priority_bps: %v", err)
	}
	if cfg.Automation.PriorityBps != 3 {
		t.Errorf("automation.priority_bps = %d want 3", cfg.Automation.PriorityBps)
	}
	// Out of range is rejected by Validate (HL hard max is 8).
	if err := setConfigKey(config.Default(), "risk.max_priority_bps", "99"); err == nil {
		t.Error("max_priority_bps > 8 must be rejected")
	}
}

func TestSetCapGuarded(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := config.Default().Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	saved := flagConfig
	flagConfig = cfgPath
	t.Cleanup(func() { flagConfig = saved })

	old, isRisk, err := setCapGuarded("risk.max_leverage", "7")
	if err != nil {
		t.Fatalf("setCapGuarded: %v", err)
	}
	if !isRisk {
		t.Error("max_leverage is a risk cap")
	}
	if old != "10" { // the default
		t.Errorf("old value = %q want 10", old)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Risk.MaxLeverage != 7 {
		t.Errorf("persisted max_leverage = %d want 7", reloaded.Risk.MaxLeverage)
	}

	// An invalid value is rejected by Validate and not persisted.
	if _, _, err := setCapGuarded("risk.max_leverage", "-1"); err == nil {
		t.Error("negative leverage must be rejected")
	}
	reloaded2, _ := config.Load(cfgPath)
	if reloaded2.Risk.MaxLeverage != 7 {
		t.Errorf("rejected edit must not persist; got %d", reloaded2.Risk.MaxLeverage)
	}
}

func TestSetConfigKeyOutcomes(t *testing.T) {
	cfg := config.Default()
	if err := setConfigKey(cfg, "outcomes", "true"); err != nil {
		t.Fatalf("set outcomes: %v", err)
	}
	if !cfg.Outcomes {
		t.Error("outcomes should be true after set")
	}
	if err := setConfigKey(cfg, "outcomes", "false"); err != nil || cfg.Outcomes {
		t.Errorf("outcomes=false: err=%v val=%v", err, cfg.Outcomes)
	}
	if err := setConfigKey(config.Default(), "outcomes", "notabool"); err == nil {
		t.Error("a non-bool must be rejected")
	}
}
