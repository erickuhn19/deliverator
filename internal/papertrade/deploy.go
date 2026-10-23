// Package papertrade speaks the Papertrade relayer on HyperEVM (chain 999).
//
// Phase 1 accepts opens, closes, cancels, and session-key registration only as
// signed intents posted to the exchange origin. The session key may sign
// OpenPosition, Close, and CancelIntent. It cannot withdraw, stake, or register
// itself — those authorities stay with the master wallet (see staking.go for
// the master-signed PaperStaking intents). There is no order
// book and no cloid: each intent carries a client-allocated nonce, and the
// intent id is derived from the EIP-712 digest.
package papertrade

import (
	"os"
	"strings"
)

const (
	// ChainID is HyperEVM mainnet. Papertrade has no testnet deployment in the
	// production client.
	ChainID int64 = 999

	// Exchange is the EIP-712 verifying contract for session-key intents
	// (open, close, cancel, register). The domain name signed by the venue
	// client is the literal string "String" — see sign.go.
	Exchange = "0x6cd5661646289fb6e65ea5c032310fded797d0a2"
	// Paper is the PAPER token. Staking intents verify against PaperStaking.
	Paper = "0xe40f17915daa230324030003a197cdaef2261c0e"
	// PaperStaking is the verifying contract for stake / unstake / claim.
	PaperStaking = "0xaad6c7b0cc3014fc80ffedae0ed7ce5967b0f016"
	// DepositProxyFactory exposes computeProxyAddress(address).
	DepositProxyFactory = "0x1629b7d46bef6e13ff754174fc9f32dea309b38f"
	// Collateral is HyperEVM USDC (6 decimals on chain). Internal amounts are
	// 18-decimal raw integers; the venue scales by 10^(18-6).
	Collateral = "0x6b9e773128f453f5c2c60935ee2de2cbc5390a24"

	// FeeAddress is the builder/fee receiver attached to RegisterSessionKey when
	// the operator names none. The venue client hard-codes this zero address
	// (viem zeroAddress). A non-zero feeAddress is a builder code; see builder.go.
	FeeAddress = "0x0000000000000000000000000000000000000000"

	// SessionKeyManager registers session keys. builderRegistry() on it returns
	// the live BuilderRegistry. Confirmed on chain 999, 2026-10-23.
	SessionKeyManager = "0x8326eb8896e358717f10502ec73ce8d28e37b108"
	// BuilderRegistryFallback is the registry published in the contract-address
	// docs and returned by SessionKeyManager.builderRegistry(). Calls use the
	// live pointer; this is the documented address those calls returned.
	BuilderRegistryFallback = "0xfdc6550436dd75ce8a0b145f935b7d3a64f53248"

	// IntentTTLSeconds is the lifetime of a signed intent (floor(now/1000)+3600).
	IntentTTLSeconds int64 = 3600
	// SessionSkewSeconds is how far a session key's expiry must sit past the
	// indexed clock before the venue client will trade (61 minutes).
	SessionSkewSeconds int64 = 3660
	// DefaultSessionDays matches the venue client's 30-day registration.
	DefaultSessionDays = 30

	defaultExchangeURL = "https://exchange.papertrade.xyz"
	defaultRPCURL      = "https://rpc.hyperliquid.xyz/evm"

	// IntentIDPrefix is hashed once and prepended to every intent id.
	IntentIDPrefix = "papertrade:intent-id:v2"
)

// ExchangeURL is the relayer origin. DELIVERATOR_PAPERTRADE_URL overrides it
// (tests, and a future host move). No trailing slash.
func ExchangeURL() string {
	return trimURL(envOr("DELIVERATOR_PAPERTRADE_URL", defaultExchangeURL))
}

// RPCURL is the HyperEVM JSON-RPC used for the deposit-proxy view.
func RPCURL() string {
	return trimURL(envOr("DELIVERATOR_PAPERTRADE_RPC", defaultRPCURL))
}

func trimURL(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
