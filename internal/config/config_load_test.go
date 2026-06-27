package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A mistyped key must be rejected (naming it), not silently dropped — else the
// operator believes a risk cap is set when it isn't, and `config set` would re-Save
// and erase the typo'd line for good (audit S1).
func TestLoadRejectsUnknownKey(t *testing.T) {
	p := writeTempConfig(t, "network = \"testnet\"\n\n[risk]\nmax_order_notinal_usd = 5\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "max_order_notinal_usd") {
		t.Fatalf("expected an error naming the stray key, got %v", err)
	}
}

func TestLoadRejectsUnknownTopLevelKey(t *testing.T) {
	p := writeTempConfig(t, "network = \"testnet\"\nnetwrok = \"mainnet\"\n")
	if _, err := Load(p); err == nil {
		t.Fatal("an unknown top-level key must be rejected")
	}
}

// Keys removed in past versions are tolerated so an older config still loads.
func TestLoadToleratesDeprecatedKeys(t *testing.T) {
	p := writeTempConfig(t, "network = \"testnet\"\n\n[wallet]\nmaster_address = \"\"\nagent_key_source = \"keychain\"\nagent_key_file = \"/x/agent.age\"\n")
	if _, err := Load(p); err != nil {
		t.Fatalf("deprecated wallet.agent_key_source/file should be tolerated, got %v", err)
	}
}

// A clean, fully-known config still loads and applies its values.
func TestLoadAcceptsKnownConfig(t *testing.T) {
	p := writeTempConfig(t, "network = \"mainnet\"\n\n[risk]\nmax_order_notional_usd = 5000\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("a known config should load: %v", err)
	}
	if cfg.Risk.MaxOrderNotionalUSD != 5000 {
		t.Errorf("risk cap not applied: %v", cfg.Risk.MaxOrderNotionalUSD)
	}
}
