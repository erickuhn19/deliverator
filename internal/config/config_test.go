package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"bad network", func(c *Config) { c.Network = "devnet" }},
		{"bad master addr", func(c *Config) { c.Wallet.MasterAddress = "0x123" }},
		{"attach all without builder", func(c *Config) { c.Builder.AttachMode = AttachAll; c.Builder.Address = "" }},
		{"fee out of range", func(c *Config) { c.Builder.FeeTenthsBps = 5000 }},
		{"negative order cap", func(c *Config) { c.Risk.MaxOrderNotionalUSD = -1 }},
		{"negative position cap", func(c *Config) { c.Risk.MaxPositionNotionalUSD = -1 }},
		{"negative dms", func(c *Config) { c.Risk.DeadManSwitchSecs = -1 }},
		{"negative leverage", func(c *Config) { c.Risk.MaxLeverage = -1 }},
		{"negative min notional", func(c *Config) { c.Risk.MinOrderNotionalUSD = -1 }},
		{"min above max order notional", func(c *Config) {
			c.Risk.MinOrderNotionalUSD = 20000 // > default max 10000 => unsatisfiable
		}},
		{"min above max position notional", func(c *Config) {
			c.Risk.MaxPositionNotionalUSD = 5 // < default min 10 => no fresh open possible
		}},
		{"negative account leverage", func(c *Config) { c.Risk.MaxAccountLeverage = -1 }},
		{"negative net exposure", func(c *Config) { c.Risk.MaxNetExposureUSD = -1 }},
		{"negative concentration", func(c *Config) { c.Risk.MaxConcentrationPctPerCoin = -1 }},
		{"negative daily loss", func(c *Config) { c.Risk.MaxDailyLossUSD = -1 }},
		{"drawdown over 100", func(c *Config) { c.Risk.MaxDrawdownPct = 150 }},
		{"daily loss pct over 100", func(c *Config) { c.Risk.MaxDailyLossPct = 150 }},
		{"negative open positions", func(c *Config) { c.Risk.MaxOpenPositions = -1 }},
		{"bad copy scale mode", func(c *Config) { c.Copy.DefaultScaleMode = "yolo" }},
		{"negative copy scale", func(c *Config) { c.Copy.DefaultScale = -1 }},
		{"non-http leaderboard url", func(c *Config) { c.Endpoints.LeaderboardURL = "ftp://evil/x" }},
		{"scheme-less leaderboard url", func(c *Config) { c.Endpoints.LeaderboardURL = "stats-data.hyperliquid.xyz/x" }},
		{"plaintext info url", func(c *Config) { c.Endpoints.InfoURL = "http://evil/info" }},
		{"scheme-less info url", func(c *Config) { c.Endpoints.InfoURL = "api.hyperliquid.xyz" }},
		{"plaintext ws url", func(c *Config) { c.Endpoints.WSURL = "ws://evil/ws" }},
		{"http(s) ws url", func(c *Config) { c.Endpoints.WSURL = "https://evil/ws" }},
		{"priority over HL cap", func(c *Config) { c.Automation.PriorityBps = 9 }},
		{"negative priority", func(c *Config) { c.Automation.PriorityBps = -1 }},
		{"max priority over HL cap", func(c *Config) { c.Risk.MaxPriorityBps = 9 }},
		{"negative max priority", func(c *Config) { c.Risk.MaxPriorityBps = -1 }},
	}
	for _, tc := range cases {
		c := Default()
		tc.mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}

func TestValidateAcceptsHTTPSLeaderboardURL(t *testing.T) {
	c := Default()
	c.Endpoints.LeaderboardURL = "https://stats-data.hyperliquid.xyz/Mainnet/leaderboard"
	if err := c.Validate(); err != nil {
		t.Fatalf("https leaderboard_url should validate: %v", err)
	}
}

func TestResolveAddress(t *testing.T) {
	c := Default()
	c.Wallet.MasterAddress = "0x1111111111111111111111111111111111111111"
	c.Accounts = map[string]string{"vault1": "0x2222222222222222222222222222222222222222"}

	if a, _ := c.ResolveAddress(""); a != c.Wallet.MasterAddress {
		t.Errorf("empty account should resolve to master")
	}
	if a, _ := c.ResolveAddress("main"); a != c.Wallet.MasterAddress {
		t.Errorf("main should resolve to master")
	}
	if a, _ := c.ResolveAddress("vault1"); a != c.Accounts["vault1"] {
		t.Errorf("vault1 should resolve to its address")
	}
	if _, err := c.ResolveAddress("ghost"); err == nil {
		t.Errorf("unknown account should error")
	}
}

// The master synonyms canonicalize to "main" — the alias onboard/init store the
// default agent key under — so a keychain lookup keyed on the raw flag can't
// miss (e.g. --account master → agent:main). Real aliases pass through.
func TestCanonicalAccount(t *testing.T) {
	for _, syn := range []string{"", "main", "MAIN", "master", "Default"} {
		if got := CanonicalAccount(syn); got != "main" {
			t.Errorf("CanonicalAccount(%q) = %q, want main", syn, got)
		}
	}
	if got := CanonicalAccount("treasury"); got != "treasury" {
		t.Errorf("CanonicalAccount(treasury) = %q, want treasury", got)
	}
}

// The synonyms are reserved alias names: an [accounts] entry named
// main/master/default would silently resolve to the MASTER address.
func TestIsReservedAlias(t *testing.T) {
	for _, name := range []string{"main", "MASTER", "Default", ""} {
		if !IsReservedAlias(name) {
			t.Errorf("IsReservedAlias(%q) should be true", name)
		}
	}
	if IsReservedAlias("vault1") {
		t.Error("IsReservedAlias(vault1) should be false")
	}
}

// Removing the outcome-mm daemon must not brick configs that still carry its
// [mm] table. The strict unknown-key check is there to catch a mistyped risk cap
// silently failing to apply; an obsolete table from a removed feature is not a
// typo, and treating it as one made EVERY command fail to load — including the
// read-only ones a running recorder depends on.
func TestLegacyMMTableIsToleratedNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
network = "mainnet"
outcomes = true

[wallet]
master_address = "0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e"

[risk]
max_outcome_question_notional_usd = 50.0

[mm]
enabled = false
dry_run = true
quote_interval_ms = 3000

[mm.strategy]
base_spread = 0.02
levels = 2
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a legacy [mm] table must be ignored, not fatal: %v", err)
	}
	// The gate that actually protects the account must survive the removal.
	if cfg.Risk.MaxOutcomeQuestionNotionalUSD != 50 {
		t.Errorf("outcome risk gate lost: got %v", cfg.Risk.MaxOutcomeQuestionNotionalUSD)
	}
	if !cfg.Outcomes {
		t.Error("HIP-4 outcome support must survive removing the MM daemon")
	}
}

// The tolerance is scoped to the removed table only — a genuine typo anywhere
// else must still be rejected loudly.
func TestAGenuineTypoIsStillFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
network = "mainnet"

[risk]
max_ordr_notional_usd = 50.0
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a mistyped risk cap must still fail the strict unknown-key check")
	}
}
