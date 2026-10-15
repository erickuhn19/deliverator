// Package funding describes how USDC reaches a Hyperliquid account and the
// Papertrade venue. It is read-only reference data: nothing here signs, sends,
// or moves funds.
//
// Every address below was checked against the official source named in its
// Source field, and the bridge's usdcToken() was read on chain. Re-verify
// before you change one.
package funding

import "strings"

// Sources for the reference data.
const (
	HyperliquidBridgeDocs = "https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/bridge2"
	CircleUSDCAddresses   = "https://developers.circle.com/stablecoins/usdc-contract-addresses"
	BridgeSource          = "https://github.com/hyperliquid-dex/contracts/blob/master/Bridge2.sol"
)

// HyperliquidMinDepositUSDC is the bridge minimum. A smaller deposit is not
// credited and is lost (Hyperliquid docs, "Deposit").
const HyperliquidMinDepositUSDC = "5"

// PapertradeMinDepositUSDC matches papertrade.MinDepositRaw (10 USDC).
const PapertradeMinDepositUSDC = "10"

// Bridge is the Arbitrum-side Hyperliquid USDC bridge for one network.
type Bridge struct {
	Chain          string   `json:"chain"`
	ChainID        int      `json:"chain_id"`
	BridgeAddress  string   `json:"bridge_address"`
	BridgeExplorer string   `json:"bridge_explorer"`
	Token          string   `json:"token"`
	TokenAddress   string   `json:"token_address"`
	TokenNote      string   `json:"token_note"`
	MinimumUSDC    string   `json:"minimum_usdc"`
	CreditedTo     string   `json:"credited_to"`
	Gas            string   `json:"gas"`
	Status         string   `json:"status"`
	Sources        []string `json:"sources"`
}

// BridgeFor returns the bridge for "mainnet" or "testnet". ok is false for any
// other network name.
func BridgeFor(network string) (Bridge, bool) {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "mainnet":
		return Bridge{
			Chain:          "Arbitrum One",
			ChainID:        42161,
			BridgeAddress:  "0x2Df1c51E09aECF9cacB7bc98cB1742757f163dF7",
			BridgeExplorer: "https://arbiscan.io/address/0x2df1c51e09aecf9cacb7bc98cb1742757f163df7",
			Token:          "USDC",
			TokenAddress:   "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
			TokenNote:      "Native Circle USDC on Arbitrum. Not bridged USDC.e (0xFF970A61A04b1cA14834A43f5dE4533eBDDB5CC8), which the bridge does not accept.",
			MinimumUSDC:    HyperliquidMinDepositUSDC,
			CreditedTo:     "the address that sends the USDC",
			Gas:            "Arbitrum ETH in the sending wallet",
			Status:         "Hyperliquid docs list this as the legacy bridge and prefer CCTP. The docs still describe its deposit flow. The Hyperliquid app's Deposit page is the simplest path.",
			Sources:        []string{HyperliquidBridgeDocs, CircleUSDCAddresses, BridgeSource},
		}, true
	case "testnet":
		return Bridge{
			Chain:          "Arbitrum Sepolia",
			ChainID:        421614,
			BridgeAddress:  "0x08cfc1B6b2dCF36A1480b99353A354AA8AC56f89",
			BridgeExplorer: "https://sepolia.arbiscan.io/address/0x08cfc1b6b2dcf36a1480b99353a354aa8ac56f89",
			Token:          "USDC2",
			TokenAddress:   "0x1baAbB04529D43a73232B713C0FE471f7c7334d5",
			TokenNote:      "Hyperliquid's testnet USDC (symbol USDC2). It is not Circle's Arbitrum Sepolia USDC and has no value.",
			MinimumUSDC:    HyperliquidMinDepositUSDC,
			CreditedTo:     "the address that sends the USDC",
			Gas:            "Arbitrum Sepolia ETH in the sending wallet",
			Status:         "Testnet. Funds have no value.",
			Sources:        []string{HyperliquidBridgeDocs, BridgeSource},
		}, true
	}
	return Bridge{}, false
}

// Info is the data payload of `deliverator deposit-info`.
type Info struct {
	Network     string             `json:"network"`
	User        string             `json:"user"`
	Hyperliquid HyperliquidFunding `json:"hyperliquid"`
	Papertrade  PapertradeFunding  `json:"papertrade"`
	Steps       []string           `json:"steps"`
	Safety      []string           `json:"safety"`
}

// HyperliquidFunding is the two ways to fund the Hyperliquid account.
type HyperliquidFunding struct {
	ArbitrumBridge     Bridge             `json:"arbitrum_bridge"`
	ExchangeWithdrawal ExchangeWithdrawal `json:"exchange_withdrawal"`
}

// ExchangeWithdrawal is a withdrawal from a centralized exchange that supports
// the Hyperliquid network.
type ExchangeWithdrawal struct {
	Destination string `json:"destination"`
	Network     string `json:"network"`
	Note        string `json:"note"`
}

// PapertradeFunding is the HyperCore → Papertrade step.
type PapertradeFunding struct {
	DepositAddress string   `json:"deposit_address"`
	Route          string   `json:"route"`
	MinimumUSDC    string   `json:"minimum_usdc"`
	Commands       []string `json:"commands"`
	Note           string   `json:"note"`
}

// Build assembles Info. user is the lowercase 0x address or "" when unknown;
// proxy is the Papertrade deposit proxy or "" when it could not be read.
func Build(network, user, proxy string) (Info, bool) {
	b, ok := BridgeFor(network)
	if !ok {
		return Info{}, false
	}
	dest := user
	if dest == "" {
		dest = "(set wallet.master_address or pass --address)"
	}
	pproxy := proxy
	if pproxy == "" {
		pproxy = "(unavailable; run `deliverator papertrade deposit-address`)"
	}
	info := Info{
		Network: strings.ToLower(network),
		User:    user,
		Hyperliquid: HyperliquidFunding{
			ArbitrumBridge: b,
			ExchangeWithdrawal: ExchangeWithdrawal{
				Destination: dest,
				Network:     "Hyperliquid",
				Note:        "If your exchange supports withdrawals on the Hyperliquid network, withdraw USDC straight to this address. No Arbitrum gas is needed. Do not send an exchange withdrawal to the Arbitrum bridge.",
			},
		},
		Papertrade: PapertradeFunding{
			DepositAddress: pproxy,
			Route:          "HyperCore USDC send (spot transfer) from your Hyperliquid account to the deposit proxy, then register it with the venue",
			MinimumUSDC:    PapertradeMinDepositUSDC,
			Commands: []string{
				"deliverator papertrade deposit-address",
				"deliverator papertrade deposit status",
				"deliverator papertrade deposit register",
			},
			Note: "Papertrade is production HyperEVM only. Arbitrum and direct EVM deposits to Papertrade are not supported. The first deposit to a new proxy costs 1 USDC to activate it.",
		},
		Steps: []string{
			"1. Fund Hyperliquid: send at least " + b.MinimumUSDC + " " + b.Token + " (" + b.Chain + ", token " + b.TokenAddress + ") from your own wallet to the bridge " + b.BridgeAddress + ", or withdraw USDC from an exchange on the Hyperliquid network to " + dest + ".",
			"2. Check it arrived: deliverator balance.",
			"3. For Papertrade: send at least " + PapertradeMinDepositUSDC + " USDC on HyperCore from " + dest + " to the deposit proxy " + pproxy + ".",
			"4. Run deliverator papertrade deposit register, then deliverator papertrade account.",
		},
		Safety: []string{
			"The bridge credits the address that SENDS the USDC. Send only from a wallet you control. Never send to the bridge from an exchange; the exchange's address gets the credit and the funds are lost to you.",
			"Deposits under " + b.MinimumUSDC + " USDC to the bridge are not credited and are lost.",
			"Use native USDC (" + b.TokenAddress + "). USDC.e and other tokens sent to the bridge are lost.",
			"Deliverator never sends funds. This command only reads.",
		},
	}
	return info, true
}
