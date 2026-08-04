package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FORWARD COMPATIBILITY. Load is strict about unknown keys (deliberately — a
// typo'd risk cap must never be silently ignored), and Save re-encodes the whole
// struct. So a NEW field without `omitempty` is written at its zero value by the
// first `config set` from a build that has it, and every OLDER binary sharing that
// config then fails to load — including an un-restarted `serve`.
//
// Observed live on 2026-08-04: one `config set` left a running server unable to
// parse its own config. It failed closed and warned (so nothing traded against a
// wrong cap), but the operator's new cap did not take effect until the stray line
// was removed by hand.
//
// This pins the rule: a DEFAULT config must not emit keys an older binary would
// reject. When this fails, the fix is `omitempty` on the field you just added.
func TestSaveOfADefaultConfigEmitsNoNewKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")

	var c Config
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// Keys introduced after the initial config surface. Each must be omitempty so
	// a zero value is not written into a config an older binary may read.
	for _, k := range []string{"drawdown_window_days"} {
		if strings.Contains(got, k) {
			t.Errorf("a default config wrote %q — add `omitempty` to that field, or every older "+
				"binary reading this config will fail Load's strict unknown-key check", k)
		}
	}
}

// And the round-trip must still work: a config that DOES set the key parses back.
func TestNewKeyStillRoundTrips(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	c := Config{Network: NetworkMainnet}
	c.Builder.AttachMode = AttachManual
	w := 30
	c.Risk.DrawdownWindowDays = &w
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatalf("a config that sets the key must load: %v", err)
	}
	if back.Risk.DrawdownWindow() != 30 {
		t.Errorf("round-trip lost the value: %d", back.Risk.DrawdownWindow())
	}
}
