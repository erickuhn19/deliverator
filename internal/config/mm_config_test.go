package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg writes body to a temp config file and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	// A valid baseline so Validate passes; tests append their own [mm].
	full := "network = \"testnet\"\n" + body
	if err := os.WriteFile(p, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMM_AbsentIsNil(t *testing.T) {
	cfg, err := Load(writeCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MM != nil {
		t.Fatalf("no [mm] in file, want cfg.MM == nil, got %+v", cfg.MM)
	}
	if got := cfg.MMOrDefault(); got == nil || got.QuoteIntervalMs != DefaultMM().QuoteIntervalMs {
		t.Fatalf("MMOrDefault should return DefaultMM, got %+v", got)
	}
}

func TestMM_AbsentOmittedOnSave(t *testing.T) {
	cfg, err := Load(writeCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.toml")
	if err := cfg.Save(out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "[mm]") {
		t.Fatalf("a config with no MM must not grow an [mm] block on Save:\n%s", b)
	}
}

// A PARTIAL [mm] table must inherit DefaultMM for every absent sub-key — the crux of
// the pointer-with-merge design (a plain overlay would zero the absent weights).
func TestMM_PartialTableInheritsDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, "[mm.selection]\nmax_active_markets = 9\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MM == nil {
		t.Fatal("[mm] present, want non-nil MM")
	}
	if cfg.MM.Selection.MaxActiveMarkets != 9 {
		t.Fatalf("explicit key not applied: got %d want 9", cfg.MM.Selection.MaxActiveMarkets)
	}
	d := DefaultMM()
	if cfg.MM.Selection.WLiquidity != d.Selection.WLiquidity {
		t.Fatalf("absent w_liquidity should inherit default %v, got %v", d.Selection.WLiquidity, cfg.MM.Selection.WLiquidity)
	}
	if cfg.MM.Strategy.BaseSpread != d.Strategy.BaseSpread {
		t.Fatalf("absent base_spread should inherit default %v, got %v", d.Strategy.BaseSpread, cfg.MM.Strategy.BaseSpread)
	}
	if !cfg.MM.DryRun {
		t.Fatal("absent dry_run should inherit default true (safe baseline)")
	}
}

func TestMM_SetMMKeySeedsDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetMMKey("mm.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if cfg.MM == nil || !cfg.MM.Enabled {
		t.Fatal("SetMMKey should allocate MM and set enabled")
	}
	// Seeded from DefaultMM, not zero — a lone enable must not blank the weights.
	if cfg.MM.Selection.WLiquidity != DefaultMM().Selection.WLiquidity {
		t.Fatalf("SetMMKey on nil MM must seed DefaultMM, got zeroed selection: %+v", cfg.MM.Selection)
	}
}

func TestMM_SetMMKeyTypesAndErrors(t *testing.T) {
	cfg := Default()
	cases := []struct {
		key, val string
		wantErr  bool
	}{
		{"mm.quote_interval_ms", "250", false},
		{"mm.strategy.base_size_shares", "42", false},
		{"mm.selection.pins", "#6410, #6411", false},
		{"mm.arb.enabled", "false", false},
		{"mm.fees.close_fee_bps", "12.5", false},
		{"mm.enabled", "notabool", true},
		{"mm.quote_interval_ms", "1.5", true},
		{"mm.nope", "x", true},
	}
	for _, c := range cases {
		err := cfg.SetMMKey(c.key, c.val)
		if (err != nil) != c.wantErr {
			t.Fatalf("SetMMKey(%q,%q) err=%v wantErr=%v", c.key, c.val, err, c.wantErr)
		}
	}
	if len(cfg.MM.Selection.Pins) != 2 || cfg.MM.Selection.Pins[0] != "#6410" {
		t.Fatalf("pins list parse wrong: %+v", cfg.MM.Selection.Pins)
	}
}

func TestMM_ValidateRejectsBad(t *testing.T) {
	if _, err := Load(writeCfg(t, "[mm.strategy]\nbase_spread = 1.5\n")); err == nil {
		t.Fatal("base_spread >= 1 should fail validation")
	}
	if _, err := Load(writeCfg(t, "[mm.selection]\nmin_ttl_mins = 100\nmax_ttl_mins = 10\n")); err == nil {
		t.Fatal("min_ttl > max_ttl should fail validation")
	}
	if _, err := Load(writeCfg(t, "[mm.selection]\nw_liquidity = -1\n")); err == nil {
		t.Fatal("negative weight should fail validation")
	}
}

func TestMM_UnknownKeyUnderMMRejected(t *testing.T) {
	// Strict decode must still catch a typo'd key inside [mm].
	if _, err := Load(writeCfg(t, "[mm]\nenabld = true\n")); err == nil {
		t.Fatal("typo'd key under [mm] should be rejected by strict decode")
	}
}

func TestMM_RoundTrip(t *testing.T) {
	cfg, err := Load(writeCfg(t, "[mm]\nenabled = true\ndry_run = false\n[mm.selection]\nmax_active_markets = 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "rt.toml")
	if err := cfg.Save(out); err != nil {
		t.Fatal(err)
	}
	re, err := Load(out)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if re.MM == nil || !re.MM.Enabled || re.MM.DryRun || re.MM.Selection.MaxActiveMarkets != 4 {
		t.Fatalf("round-trip lost MM values: %+v", re.MM)
	}
}
