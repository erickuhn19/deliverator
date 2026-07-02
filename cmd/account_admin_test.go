package cmd

// account add/rm/set-default must persist ONLY the account mutation: a transient
// --network flag (applied to Cfg in PreRun) must never leak to disk, --config
// must be honored on save (#110), and the reserved master synonyms are rejected
// as aliases (they would be accepted but silently resolve to the MASTER address).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

const (
	acctMaster = "0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e"
	acctSub    = "0x00000000000000000000000000000000000000ab"
)

// acctSetup seeds a testnet config (with one existing alias) at the default
// path, simulates PersistentPreRunE applying `--network mainnet` onto the
// in-memory Cfg, and resets the account flag globals. Keychain is mocked so
// `account rm` never touches a real keychain.
func acctSetup(t *testing.T) {
	t.Helper()
	t.Setenv("DELIVERATOR_HOME", t.TempDir())
	keyring.MockInit()
	seed := config.Default()
	seed.Network = config.NetworkTestnet
	seed.Wallet.MasterAddress = acctMaster
	seed.Accounts = map[string]string{"existing": acctSub}
	if err := seed.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Network = config.NetworkMainnet // the transient flag mutation
	Cfg = loaded
	saveNet, saveCfgFlag := flagNetwork, flagConfig
	saveAlias, saveAddr := aAlias, aAddress
	flagNetwork, flagConfig = "mainnet", ""
	t.Cleanup(func() {
		flagNetwork, flagConfig = saveNet, saveCfgFlag
		aAlias, aAddress = saveAlias, saveAddr
		Cfg = nil
	})
}

// A transient --network must not be persisted by any account mutation, and the
// mutation itself must land.
func TestAccountCommandsIgnoreTransientNetworkFlag(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		acctSetup(t)
		aAlias, aAddress = "vault1", acctSub
		if _, err := runCmd(t, accountAddCmd, nil); err != nil {
			t.Fatalf("account add: %v", err)
		}
		after, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		if after.Network != config.NetworkTestnet {
			t.Fatalf("transient --network leaked to disk: got %q, want testnet", after.Network)
		}
		if after.Accounts["vault1"] != acctSub {
			t.Fatalf("alias not persisted: %v", after.Accounts)
		}
	})
	t.Run("rm", func(t *testing.T) {
		acctSetup(t)
		aAlias = "existing"
		if _, err := runCmd(t, accountRmCmd, nil); err != nil {
			t.Fatalf("account rm: %v", err)
		}
		after, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		if after.Network != config.NetworkTestnet {
			t.Fatalf("transient --network leaked to disk: got %q, want testnet", after.Network)
		}
		if _, ok := after.Accounts["existing"]; ok {
			t.Fatalf("alias not removed: %v", after.Accounts)
		}
	})
	t.Run("set-default", func(t *testing.T) {
		acctSetup(t)
		aAddress = acctSub
		if _, err := runCmd(t, accountSetDefaultCmd, nil); err != nil {
			t.Fatalf("account set-default: %v", err)
		}
		after, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		if after.Network != config.NetworkTestnet {
			t.Fatalf("transient --network leaked to disk: got %q, want testnet", after.Network)
		}
		if after.Wallet.MasterAddress != acctSub {
			t.Fatalf("master not persisted: %q", after.Wallet.MasterAddress)
		}
	})
}

// With --config, the flagged file is the one mutated — the default config must
// not be created or overwritten (#110).
func TestAccountAddHonorsConfigFlag(t *testing.T) {
	acctSetup(t)
	custom := filepath.Join(t.TempDir(), "custom.toml")
	seed := config.Default()
	seed.Network = config.NetworkTestnet
	if err := seed.Save(custom); err != nil {
		t.Fatal(err)
	}
	// Wipe the default config written by acctSetup so we can assert it stays gone.
	if err := os.Remove(config.Path()); err != nil {
		t.Fatal(err)
	}
	flagConfig = custom
	aAlias, aAddress = "vault1", acctSub

	if _, err := runCmd(t, accountAddCmd, nil); err != nil {
		t.Fatalf("account add --config: %v", err)
	}
	got, err := config.Load(custom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accounts["vault1"] != acctSub {
		t.Fatalf("--config target not updated: %v", got.Accounts)
	}
	if got.Network != config.NetworkTestnet {
		t.Fatalf("--config save flipped the network: %q", got.Network)
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatalf("account add --config must not touch the default config (#110)")
	}
}

// The master synonyms are reserved: an alias named main/master/default (any
// case) would be accepted but silently resolve to the MASTER address, so
// `account add` must reject it (exit 10) and persist nothing.
func TestAccountAddRejectsReservedAlias(t *testing.T) {
	for _, name := range []string{"main", "MASTER", "Default"} {
		t.Run(name, func(t *testing.T) {
			acctSetup(t)
			aAlias, aAddress = name, acctSub
			env, err := runCmd(t, accountAddCmd, nil)
			ce, ok := err.(*output.CmdError)
			if !ok || ce.Code != output.ExitValidation {
				t.Fatalf("account add --alias %s must fail validation (exit %d), got %T %v", name, output.ExitValidation, err, err)
			}
			if env.Error.Code != "reserved_alias" {
				t.Errorf("error code = %q, want reserved_alias", env.Error.Code)
			}
			after, lerr := config.Load("")
			if lerr != nil {
				t.Fatal(lerr)
			}
			if _, exists := after.Accounts[name]; exists {
				t.Fatalf("reserved alias must not be persisted: %v", after.Accounts)
			}
		})
	}
}

// The init config template ships the opinionated default: MAINNET (matching
// onboard and the README) — testnet stays one `config set network` away.
func TestConfigTemplateShipsMainnet(t *testing.T) {
	if !strings.Contains(configTemplate, `network = "mainnet"`) {
		t.Fatal(`config template must ship network = "mainnet" (opinionated shipped default)`)
	}
}
