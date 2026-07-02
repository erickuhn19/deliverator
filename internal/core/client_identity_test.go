package core

// Identity safety for New/exchange (audit 2026-07-01, P0-D): an explicitly
// passed --account that does not resolve must HARD-FAIL (exit 10) — a silently
// empty queryAddr routes env-key writes to the MASTER account and blinds the
// per-coin position/flip gates — and the documented master synonyms
// (main/master/default, any case) must key the keychain on the canonical
// "main" alias onboard stores under.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
	"github.com/erickuhn19/deliverator/internal/wallet"
)

const testSubAccount = "0x00000000000000000000000000000000000000ab"

// seedMetaCache writes a valid testnet meta cache under the temp HOME so New
// constructs without any network fetch.
func seedMetaCache(t *testing.T) {
	t.Helper()
	if err := testMeta().Save(filepath.Join(config.Dir(), "meta.json")); err != nil {
		t.Fatal(err)
	}
}

func identityCfg() *config.Config {
	cfg := config.Default()
	cfg.Network = config.NetworkTestnet
	cfg.Wallet.MasterAddress = testMaster
	cfg.Accounts = map[string]string{"treasury": testSubAccount}
	return cfg
}

// An unknown (typo'd or case-mismatched) --account must be a hard validation
// error (exit 10) naming the configured aliases — never a client with an empty
// query address.
func TestNewRejectsUnknownAccountAlias(t *testing.T) {
	testHome(t)
	seedMetaCache(t)
	cfg := identityCfg()

	for _, account := range []string{"tresury", "Treasury"} { // typo + case slip
		c, err := New(context.Background(), cfg, Options{Account: account})
		if err == nil {
			t.Fatalf("New(--account %q) must fail, got client with queryAddr=%q", account, c.QueryAddr())
		}
		oe, ok := err.(*output.Error)
		if !ok {
			t.Fatalf("New(--account %q) error should be *output.Error, got %T: %v", account, err, err)
		}
		if oe.ExitCode() != output.ExitValidation {
			t.Errorf("New(--account %q) exit = %d, want %d (validation)", account, oe.ExitCode(), output.ExitValidation)
		}
		if !strings.Contains(oe.Message, "unknown account") {
			t.Errorf("New(--account %q) message should name the unknown account, got %q", account, oe.Message)
		}
		if !strings.Contains(oe.Hint, "treasury") {
			t.Errorf("New(--account %q) hint should list the configured aliases, got %q", account, oe.Hint)
		}
	}
}

// Regression: a valid alias resolves to its sub-account address (query AND
// vault), and the master synonyms resolve to the master with no vault.
func TestNewResolvesValidAlias(t *testing.T) {
	testHome(t)
	seedMetaCache(t)
	cfg := identityCfg()

	c, err := New(context.Background(), cfg, Options{Account: "treasury"})
	if err != nil {
		t.Fatalf("New(--account treasury) should resolve: %v", err)
	}
	if c.QueryAddr() != testSubAccount {
		t.Errorf("queryAddr = %q, want %q", c.QueryAddr(), testSubAccount)
	}
	if c.vaultAddr != testSubAccount {
		t.Errorf("vaultAddr = %q, want %q (sub-account writes must carry the vault)", c.vaultAddr, testSubAccount)
	}

	for _, syn := range []string{"", "main", "MASTER", "default"} {
		c, err := New(context.Background(), cfg, Options{Account: syn})
		if err != nil {
			t.Fatalf("New(--account %q) should resolve to the master: %v", syn, err)
		}
		if c.QueryAddr() != testMaster || c.vaultAddr != "" {
			t.Errorf("--account %q: queryAddr=%q vaultAddr=%q, want master/no-vault", syn, c.QueryAddr(), c.vaultAddr)
		}
	}
}

// The tolerant path survives: no --account on a box with no master configured
// still constructs (requireQueryAddr guards the reads with exit 30).
func TestNewToleratesUnsetMasterWithoutAccount(t *testing.T) {
	testHome(t)
	seedMetaCache(t)
	cfg := identityCfg()
	cfg.Wallet.MasterAddress = ""
	cfg.Accounts = nil

	c, err := New(context.Background(), cfg, Options{Account: ""})
	if err != nil {
		t.Fatalf("New with no --account and unset master must stay tolerant: %v", err)
	}
	if c.QueryAddr() != "" {
		t.Errorf("queryAddr = %q, want \"\"", c.QueryAddr())
	}
}

// The master synonyms (and their case variants) must load the keychain key
// onboard stores under "agent:main" — not miss on "agent:master"/"agent:default"
// and fail writes exit 30 on a correctly onboarded box.
func TestExchangeMasterSynonymsResolveKeychainKey(t *testing.T) {
	testHome(t)
	t.Setenv(wallet.EnvKeyVar, "") // keychain mode, never the env override
	keyring.MockInit()
	if _, err := wallet.Store("", testAgentKeyHex); err != nil { // how onboard stores the default key
		t.Fatal(err)
	}
	cfg := identityCfg()

	for _, syn := range []string{"", "main", "master", "default", "MAIN"} {
		c := &Client{
			cfg:       cfg,
			opts:      Options{Account: syn},
			network:   config.NetworkTestnet,
			signURL:   "http://127.0.0.1:0", // never dialed — meta is pre-seeded
			httpc:     &http.Client{},
			meta:      testMeta(),
			queryAddr: testMaster,
			audit:     state.NewAudit(filepath.Join(config.Dir(), "audit.jsonl"), false),
		}
		ex, err := c.exchange(context.Background())
		if err != nil {
			t.Errorf("exchange(--account %q) must find the onboarded key: %v", syn, err)
			continue
		}
		if ex == nil {
			t.Errorf("exchange(--account %q) returned nil exchange", syn)
		}
	}
}
