package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
)

// Regression tests for #41: a long-lived `serve` captured risk config at startup
// and enforced it from memory forever. The operator raised max_drawdown_pct on
// disk, `deliverator risk` (a fresh fork) reported the new value, and the socket
// rejected 3,000+ placements against the old one for two days.

// writeConfig writes a config.toml with the given risk body and returns its path.
// Each write is stamped forward in time: the freshness check keys on (mtime,
// size), and a same-millisecond rewrite of the same length would be invisible.
func writeConfig(t *testing.T, dir, body string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(age)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	return p
}

func clientWithGuards(t *testing.T, path string) *Client {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return &Client{cfg: cfg, guards: guardConfigFrom(cfg), guardGen: currentGuardGeneration(cfg)}
}

// THE OUTAGE. A cap raised on disk must reach the gates, and the change must be
// reported rather than applied silently.
func TestReloadGuardsPicksUpAConfigSet(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -2*time.Hour)
	c := clientWithGuards(t, p)

	if got := c.riskConfig().MaxDrawdownPct; got != 70 {
		t.Fatalf("startup cap = %v, want 70", got)
	}

	// The operator runs `deliverator config set risk.max_drawdown_pct 100`.
	writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 100.0\n", 0)

	warns := c.ReloadGuardsIfChanged(p)
	if got := c.riskConfig().MaxDrawdownPct; got != 100 {
		t.Errorf("after reload cap = %v, want 100 — the socket is still enforcing the startup snapshot", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "max_drawdown_pct") || !strings.Contains(warns[0], "WIDENED") {
		t.Errorf("a widened cap must be reported, got %q", warns)
	}
}

// An unchanged file must not re-parse or re-warn: this runs on every request.
func TestReloadGuardsIsQuietWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -time.Hour)
	c := clientWithGuards(t, p)

	for i := 0; i < 3; i++ {
		if w := c.ReloadGuardsIfChanged(p); len(w) != 0 {
			t.Fatalf("unchanged config produced warnings on call %d: %q", i, w)
		}
	}
	if got := c.riskConfig().MaxDrawdownPct; got != 70 {
		t.Errorf("cap drifted to %v with no config change", got)
	}
}

// FAIL CLOSED. A truncated config parses as valid TOML into an all-zero Config,
// and every cap is `0 = off` — honouring it would silently disable the account's
// entire risk envelope. The previous caps must survive.
func TestReloadGuardsRefusesAZeroLengthConfig(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -time.Hour)
	c := clientWithGuards(t, p)

	writeConfig(t, dir, "", 0) // truncated by a crash mid-write, a bad edit, a full disk

	warns := c.ReloadGuardsIfChanged(p)
	if got := c.riskConfig().MaxDrawdownPct; got != 70 {
		t.Fatalf("cap = %v after a zero-length config; the gate was silently DISABLED", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "zero-length") {
		t.Errorf("a zero-length config must be reported loudly, got %q", warns)
	}
}

// Same rule for a file that vanished.
func TestReloadGuardsRefusesAMissingConfig(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -time.Hour)
	c := clientWithGuards(t, p)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	warns := c.ReloadGuardsIfChanged(p)
	if got := c.riskConfig().MaxDrawdownPct; got != 70 {
		t.Fatalf("cap = %v after the config vanished; the gate was silently DISABLED", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "unreadable") {
		t.Errorf("a missing config must be reported, got %q", warns)
	}
}

// And for one that no longer parses.
func TestReloadGuardsRefusesAnUnparseableConfig(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -time.Hour)
	c := clientWithGuards(t, p)

	writeConfig(t, dir, "[risk\nthis is not = = toml\n", 0)

	warns := c.ReloadGuardsIfChanged(p)
	if got := c.riskConfig().MaxDrawdownPct; got != 70 {
		t.Fatalf("cap = %v after an unparseable config; the gate was silently DISABLED", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "does not parse") {
		t.Errorf("an unparseable config must be reported, got %q", warns)
	}
}

// Turning a gate OFF is the most dangerous legal edit; it must say so in those
// words, not merely report a number change.
func TestReloadGuardsNamesADisabledGate(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -time.Hour)
	c := clientWithGuards(t, p)

	writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 0.0\n", 0)

	warns := c.ReloadGuardsIfChanged(p)
	if len(warns) != 1 || !strings.Contains(warns[0], "OFF (gate disabled)") {
		t.Errorf("disabling a gate must be called out explicitly, got %q", warns)
	}
}

// The rejection has to name the config it enforced. Without this, a stale server
// quotes a cap that no longer exists on disk and nothing distinguishes it from a
// correct rejection — which is what made the two-day outage invisible.
func TestGuardGenerationIdentifiesTheConfigInForce(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 70.0\n", -time.Hour)
	c := clientWithGuards(t, p)

	gen := c.GuardGeneration()
	if !strings.Contains(gen, "config generation") || !strings.Contains(gen, "mtime") {
		t.Fatalf("GuardGeneration() = %q, want a generation + mtime a human can compare against the file", gen)
	}

	writeConfig(t, dir, "[risk]\nmax_drawdown_pct = 100.0\n", 0)
	c.ReloadGuardsIfChanged(p)
	if next := c.GuardGeneration(); next == gen {
		t.Errorf("generation did not move after a reload: %q", next)
	}
}
