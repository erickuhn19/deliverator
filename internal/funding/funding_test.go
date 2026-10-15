package funding

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Verified 2026-10-15 against the Hyperliquid bridge docs, Circle's USDC list,
// and an on-chain usdcToken() read of each bridge. A change here must be
// re-verified at the source, not edited to make the test pass.
func TestBridgeAddressesAreThePinnedOnes(t *testing.T) {
	cases := map[string][3]string{
		"mainnet": {"0x2Df1c51E09aECF9cacB7bc98cB1742757f163dF7", "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", "42161"},
		"testnet": {"0x08cfc1B6b2dCF36A1480b99353A354AA8AC56f89", "0x1baAbB04529D43a73232B713C0FE471f7c7334d5", "421614"},
	}
	for net, want := range cases {
		b, ok := BridgeFor(net)
		if !ok {
			t.Fatalf("%s missing", net)
		}
		if b.BridgeAddress != want[0] || b.TokenAddress != want[1] {
			t.Fatalf("%s: bridge %s token %s", net, b.BridgeAddress, b.TokenAddress)
		}
		for _, a := range []string{b.BridgeAddress, b.TokenAddress} {
			if !common.IsHexAddress(a) || common.HexToAddress(a).Hex() != a {
				t.Fatalf("%s: %s is not a checksummed address", net, a)
			}
		}
		if itoa(b.ChainID) != want[2] || b.MinimumUSDC != "5" {
			t.Fatalf("%s: chain %d min %s", net, b.ChainID, b.MinimumUSDC)
		}
		if !strings.Contains(strings.ToLower(b.BridgeExplorer), strings.ToLower(b.BridgeAddress)) {
			t.Fatalf("%s: explorer link %s", net, b.BridgeExplorer)
		}
	}
	if _, ok := BridgeFor("devnet"); ok {
		t.Fatal("unknown network must not resolve")
	}
	if b, _ := BridgeFor(" MAINNET "); b.ChainID != 42161 {
		t.Fatal("network name is case and space tolerant")
	}
}

func TestBuild(t *testing.T) {
	user := "0x9ccaca47f0318faef9c8175767a15aee1586177e"
	proxy := "0x90df7d81a9bc4ded5435ed926271f67fe77be4b3"
	info, ok := Build("mainnet", user, proxy)
	if !ok {
		t.Fatal("build")
	}
	if info.Hyperliquid.ExchangeWithdrawal.Destination != user || info.Papertrade.DepositAddress != proxy ||
		info.Papertrade.MinimumUSDC != "10" {
		t.Fatalf("info %+v", info)
	}
	all := strings.Join(append(info.Steps, info.Safety...), "\n")
	for _, must := range []string{"SENDS", "exchange", "USDC.e", "deposit register", proxy, user, "0x2Df1c51E09aECF9cacB7bc98cB1742757f163dF7"} {
		if !strings.Contains(all, must) {
			t.Errorf("steps/safety missing %q", must)
		}
	}
	blank, _ := Build("testnet", "", "")
	if !strings.Contains(blank.Hyperliquid.ExchangeWithdrawal.Destination, "--address") ||
		!strings.Contains(blank.Papertrade.DepositAddress, "deposit-address") {
		t.Fatalf("placeholders %+v", blank)
	}
	if _, ok := Build("nope", user, proxy); ok {
		t.Fatal("unknown network")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
