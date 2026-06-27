package config

// Coverage for the #91 config-layer hardening: secure-scheme endpoints (S7),
// audit-path confinement (T3-path), the absolute Dir() fallback (T3-cwd), and the
// atomic+0600 Save (S12 / T3-file-mode).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateAcceptsSecureEndpoints(t *testing.T) {
	c := Default()
	c.Endpoints.InfoURL = "https://api.hyperliquid.xyz"
	c.Endpoints.WSURL = "wss://api.hyperliquid.xyz/ws"
	if err := c.Validate(); err != nil {
		t.Fatalf("https/wss overrides should validate: %v", err)
	}
	// Empty (the default — derive from the signing URL) stays valid.
	c.Endpoints.InfoURL, c.Endpoints.WSURL = "", ""
	if err := c.Validate(); err != nil {
		t.Fatalf("empty endpoint overrides should validate: %v", err)
	}
}

func TestAuditPathConfinement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)

	bad := []struct {
		name, path string
	}{
		{"absolute escape", "/etc/passwd"},
		{"relative escape", "../../../etc/passwd"},
		{"clean escape", filepath.Join(home, "..", "evil.jsonl")},
	}
	for _, b := range bad {
		c := Default()
		c.State.AuditPath = b.path
		if err := c.Validate(); err == nil {
			t.Errorf("%s: audit_path %q should be rejected", b.name, b.path)
		}
	}

	ok := []struct {
		name, path string
	}{
		{"default", filepath.Join(home, "audit.jsonl")},
		{"relative within", "logs/audit.jsonl"},
		{"empty (use default)", ""},
	}
	for _, o := range ok {
		c := Default()
		c.State.AuditPath = o.path
		if err := c.Validate(); err != nil {
			t.Errorf("%s: audit_path %q should be allowed, got %v", o.name, o.path, err)
		}
	}
}

// With $HOME unknown and DELIVERATOR_HOME unset, Dir() must return an ABSOLUTE
// path so two processes in different working dirs share one nonce file (T3-cwd).
func TestDirFallbackIsAbsolute(t *testing.T) {
	t.Setenv("DELIVERATOR_HOME", "")
	t.Setenv("HOME", "")        // unix
	t.Setenv("USERPROFILE", "") // windows
	d := Dir()
	if !filepath.IsAbs(d) {
		t.Fatalf("Dir() fallback %q is not absolute — defeats the cross-process nonce lock", d)
	}
}

func TestDirHonorsDeliveratorHome(t *testing.T) {
	t.Setenv("DELIVERATOR_HOME", "/tmp/dlv-test-home")
	if d := Dir(); d != "/tmp/dlv-test-home" {
		t.Fatalf("Dir() = %q, want the DELIVERATOR_HOME override", d)
	}
}

// Save writes atomically at 0600, even over a pre-existing world-readable file,
// and the result re-loads cleanly (S12 / T3-file-mode).
func TestSaveIsAtomicAnd0600(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	p := Path()
	if err := os.WriteFile(p, []byte("network = \"mainnet\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Network = "testnet"
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("saved config mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != "testnet" {
		t.Fatalf("round-trip lost the network: %q", got.Network)
	}
}
