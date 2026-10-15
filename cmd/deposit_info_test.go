package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/funding"
	"github.com/erickuhn19/deliverator/internal/output"
)

func depositInfoRun(t *testing.T) (ptEnvelope, funding.Info, []string, error) {
	t.Helper()
	var buf strings.Builder
	output.Configure(true, &buf)
	err := depositInfoCmd.RunE(depositInfoCmd, nil)
	var raw struct {
		ptEnvelope
		Warnings []string `json:"warnings"`
	}
	if e := json.Unmarshal([]byte(buf.String()), &raw); e != nil {
		t.Fatalf("expected one JSON envelope, got %q: %v", buf.String(), e)
	}
	var info funding.Info
	if raw.OK {
		if e := json.Unmarshal(raw.Data, &info); e != nil {
			t.Fatal(e)
		}
	}
	return raw.ptEnvelope, info, raw.Warnings, err
}

func TestDepositInfoMainnet(t *testing.T) {
	cap := &ptCapture{}
	ptUse(t, cap)
	t.Cleanup(func() { depositInfoAddress = "" })
	depositInfoAddress = ""
	env, info, warns, err := depositInfoRun(t)
	if rdExitCode(t, err) != output.ExitOK || !env.OK || env.Cmd != "deposit-info" || len(warns) != 0 {
		t.Fatalf("exit %v env %+v warns %v", err, env, warns)
	}
	b := info.Hyperliquid.ArbitrumBridge
	if info.Network != "mainnet" || b.BridgeAddress != "0x2Df1c51E09aECF9cacB7bc98cB1742757f163dF7" ||
		b.TokenAddress != "0xaf88d065e77c8cC2239327C5EDb3A432268e5831" || b.MinimumUSDC != "5" {
		t.Fatalf("bridge %+v", b)
	}
	if info.User != ptTestUser || info.Hyperliquid.ExchangeWithdrawal.Destination != ptTestUser {
		t.Fatalf("user %+v", info)
	}
	// The test RPC answers computeProxyAddress with 0xce42…db84.
	if info.Papertrade.DepositAddress != "0xce424134a6c414c3c2c8d853caebf8b3079adb84" || info.Papertrade.MinimumUSDC != "10" {
		t.Fatalf("papertrade %+v", info.Papertrade)
	}
	if cap.postCount() != 0 {
		t.Fatal("deposit-info must never POST an intent")
	}
}

func TestDepositInfoTestnetAndAddressOverride(t *testing.T) {
	ptUse(t, &ptCapture{})
	t.Cleanup(func() { depositInfoAddress = "" })
	Cfg.Network = config.NetworkTestnet
	depositInfoAddress = "0x00000000000000000000000000000000000000AA"
	_, info, warns, err := depositInfoRun(t)
	if rdExitCode(t, err) != output.ExitOK {
		t.Fatal(err)
	}
	b := info.Hyperliquid.ArbitrumBridge
	if b.BridgeAddress != "0x08cfc1B6b2dCF36A1480b99353A354AA8AC56f89" || b.ChainID != 421614 ||
		b.TokenAddress != "0x1baAbB04529D43a73232B713C0FE471f7c7334d5" {
		t.Fatalf("testnet bridge %+v", b)
	}
	if info.User != "0x00000000000000000000000000000000000000aa" {
		t.Fatalf("--address must win over wallet.master_address: %s", info.User)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "testnet") {
		t.Fatalf("warnings %v", warns)
	}
}

func TestDepositInfoNoAddressBadAddressAndRPCDown(t *testing.T) {
	ptUse(t, &ptCapture{})
	t.Cleanup(func() { depositInfoAddress = "" })

	Cfg.Wallet.MasterAddress = ""
	_, info, warns, err := depositInfoRun(t)
	if rdExitCode(t, err) != output.ExitOK || info.User != "" || len(warns) != 1 ||
		!strings.Contains(info.Papertrade.DepositAddress, "unavailable") {
		t.Fatalf("no address: %v %+v %v", err, info, warns)
	}

	depositInfoAddress = "not-an-address"
	env, _, _, err := depositInfoRun(t)
	if rdExitCode(t, err) != output.ExitValidation || env.OK || env.Err == nil || env.Err.Code != "address" {
		t.Fatalf("bad address: %v %+v", err, env)
	}

	// Proxy RPC down: still succeed, with a warning and a placeholder.
	depositInfoAddress = ptTestUser
	t.Setenv("DELIVERATOR_PAPERTRADE_RPC", "http://127.0.0.1:1")
	_, info, warns, err = depositInfoRun(t)
	if rdExitCode(t, err) != output.ExitOK || len(warns) != 1 || !strings.Contains(warns[0], "deposit proxy") ||
		info.Hyperliquid.ExchangeWithdrawal.Destination != ptTestUser {
		t.Fatalf("rpc down: %v %+v %v", err, info, warns)
	}

	Cfg.Network = "devnet"
	env, _, _, err = depositInfoRun(t)
	if rdExitCode(t, err) != output.ExitValidation || env.Err == nil || env.Err.Code != "network" {
		t.Fatalf("unknown network: %v %+v", err, env)
	}
}
