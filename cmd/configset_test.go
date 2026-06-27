package cmd

// config set: honor --config (write the flagged file, not the default — #110) and
// surface a loud reminder when a risk cap changes (agent-in-the-loop: never block,
// keep the operator informed).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestConfigSetHonorsConfigFlag(t *testing.T) {
	home := t.TempDir() // the DEFAULT config path lives under here
	t.Setenv("DELIVERATOR_HOME", home)
	custom := filepath.Join(t.TempDir(), "custom.toml")
	if err := config.Default().Save(custom); err != nil {
		t.Fatal(err)
	}
	saveFlag := flagConfig
	flagConfig = custom
	t.Cleanup(func() { flagConfig = saveFlag })

	if _, err := runCmd(t, configSetCmd, []string{"risk.max_leverage", "7"}); err != nil {
		t.Fatalf("config set: %v", err)
	}

	// The flagged file is the one that changed.
	got, err := config.Load(custom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Risk.MaxLeverage != 7 {
		t.Fatalf("--config target not updated: max_leverage=%d, want 7", got.Risk.MaxLeverage)
	}
	// The DEFAULT config must NOT have been created/written (the #110 footgun).
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("config set --config must not touch the default config (#110); one exists at %s", filepath.Join(home, "config.toml"))
	}
}

func TestConfigSetRiskCapReminder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	if err := config.Default().Save(config.Path()); err != nil { // seeds max_leverage=10
		t.Fatal(err)
	}
	saveFlag := flagConfig
	flagConfig = ""
	t.Cleanup(func() { flagConfig = saveFlag })

	// A risk-cap change SUCCEEDS (never blocked) but surfaces a loud reminder.
	env, err := runCmd(t, configSetCmd, []string{"risk.max_leverage", "20"})
	if err != nil {
		t.Fatalf("config set must not be blocked: %v", err)
	}
	if !env.OK {
		t.Fatalf("config set should succeed, got %+v", env)
	}
	warned := false
	for _, w := range env.Warnings {
		if strings.Contains(w, "risk cap changed") && strings.Contains(w, "10 → 20") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a risk-cap change must surface a before→after reminder, got warnings: %v", env.Warnings)
	}

	// A non-risk key must NOT trigger the reminder.
	env, err = runCmd(t, configSetCmd, []string{"network", "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range env.Warnings {
		if strings.Contains(w, "risk cap changed") {
			t.Fatalf("a non-risk change must not warn about a cap, got: %v", env.Warnings)
		}
	}
}
