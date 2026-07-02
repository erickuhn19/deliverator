package cmd

// onboard ships the opinionated default: MAINNET (the network is prominent in
// the console and `config set network` warns loudly); --network testnet
// overrides for a paper-funds dry run. The setup is saved to the --config file
// (not silently to the default path, #110).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

const obTestKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// obClient satisfies the verify step of onboard offline.
type obClient struct{ core.ClientAPI }

func (obClient) Balance(context.Context) (*core.BalanceView, error) {
	return &core.BalanceView{AvailableCollateral: "42.0"}, nil
}

// obStdin replaces os.Stdin with a pipe carrying input (the key line;
// term.ReadPassword fails on a pipe and falls back to a line read).
func obStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	save := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = save; _ = r.Close() })
}

// obSetup isolates HOME/keychain/flags and returns the --config target path.
func obSetup(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	keyring.MockInit()
	withFakeClient(t, obClient{})
	custom := filepath.Join(t.TempDir(), "custom.toml")
	if err := config.Default().Save(custom); err != nil {
		t.Fatal(err)
	}
	saveCfg, saveConfig, saveNet, saveMaster := Cfg, flagConfig, flagNetwork, onboardMaster
	Cfg = config.Default()
	flagConfig, flagNetwork = custom, ""
	onboardMaster = acctMaster
	output.Configure(true, io.Discard) // onboard prints prompts; discard them
	t.Cleanup(func() {
		Cfg, flagConfig, flagNetwork, onboardMaster = saveCfg, saveConfig, saveNet, saveMaster
		output.Configure(true, nil)
	})
	return custom
}

// With no --network flag onboard configures MAINNET (the opinionated shipped
// default), and the setup lands in the --config file — the default config path
// is never touched.
func TestOnboardDefaultsMainnetAndHonorsConfigFlag(t *testing.T) {
	custom := obSetup(t)
	obStdin(t, obTestKeyHex+"\n")

	if err := onboardCmd.RunE(onboardCmd, nil); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	got, err := config.Load(custom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != config.NetworkMainnet {
		t.Fatalf("onboard default network = %q, want mainnet (opinionated shipped default)", got.Network)
	}
	if got.Wallet.MasterAddress != acctMaster {
		t.Fatalf("master not saved to --config file: %q", got.Wallet.MasterAddress)
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatalf("onboard --config must not write the default config (#110)")
	}
}

// --network testnet keeps the paper-funds dry run one flag away.
func TestOnboardExplicitTestnetOverride(t *testing.T) {
	custom := obSetup(t)
	flagNetwork = "testnet"
	obStdin(t, obTestKeyHex+"\n")

	if err := onboardCmd.RunE(onboardCmd, nil); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	got, err := config.Load(custom)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != config.NetworkTestnet {
		t.Fatalf("--network testnet saved %q, want testnet", got.Network)
	}
}
