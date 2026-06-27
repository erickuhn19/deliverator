// Package wallet loads and derives the agent/API signing key. The key is an
// agent wallet that CANNOT withdraw — the non-custodial guarantee (§4).
//
// Source model (deliberately NOT a stored config field — that indirection was the
// "key looks deleted" bug):
//   - default: the OS keychain (where `onboard`/`init` put it).
//   - override: the DELIVERATOR_AGENT_KEY env var, used ONLY when set + non-empty,
//     for headless/CI hosts that have no GUI keychain.
//
// Because the env var is consulted only when present, an unset/empty env can never
// hide the keychain key. The secret is never logged or round-tripped through stdout.
package wallet

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/zalando/go-keyring"
)

const keyringService = "deliverator"

// EnvKeyVar is the explicit env-injection override for headless/CI (no keychain).
// When set + non-empty it takes precedence over the keychain; otherwise it is
// ignored entirely (so it can never hide a stored keychain key).
const EnvKeyVar = "DELIVERATOR_AGENT_KEY"

// ErrNoAgentKey is returned by Load when no key is available from the env override
// or the keychain — callers should direct the operator to run `deliverator onboard`.
var ErrNoAgentKey = errors.New("no agent key (keychain empty and DELIVERATOR_AGENT_KEY unset)")

// Agent is a loaded agent/API wallet ready to sign.
type Agent struct {
	Key     *ecdsa.PrivateKey
	Address string // 0x-checksummed agent address
}

// keyringUser namespaces the stored key per account alias so sub-accounts get
// isolated keys (one agent key per sub-account → nonce isolation, §8).
func keyringUser(account string) string {
	if account == "" {
		account = "main"
	}
	return "agent:" + account
}

// Load loads the agent key: the DELIVERATOR_AGENT_KEY env override when set, else
// the OS keychain. When neither yields a key it returns ErrNoAgentKey (wrapped) so
// the caller can prompt the operator to run `deliverator onboard`.
func Load(account string) (*Agent, error) {
	if h := strings.TrimSpace(os.Getenv(EnvKeyVar)); h != "" {
		return fromHex(h)
	}
	h, err := keyring.Get(keyringService, keyringUser(account))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, fmt.Errorf("%w for account %q", ErrNoAgentKey, account)
		}
		return nil, fmt.Errorf("read agent key from keychain for account %q: %w", account, err)
	}
	return fromHex(h)
}

// ActiveSource reports where Load would read the key from for diagnostics: "env"
// when the override is set, else "keychain".
func ActiveSource() string {
	if strings.TrimSpace(os.Getenv(EnvKeyVar)) != "" {
		return "env"
	}
	return "keychain"
}

// Has reports whether an agent key is available for the account (env override or
// keychain), without surfacing the secret.
func Has(account string) bool {
	if strings.TrimSpace(os.Getenv(EnvKeyVar)) != "" {
		return true
	}
	_, err := keyring.Get(keyringService, keyringUser(account))
	return err == nil
}

// Generate creates a fresh agent keypair and stores it in the keychain. The secret
// is never returned or printed — only its public address is surfaced.
func Generate(account string) (*Agent, error) {
	pk, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	a := &Agent{Key: pk, Address: crypto.PubkeyToAddress(pk.PublicKey).Hex()}
	if err := keyring.Set(keyringService, keyringUser(account), hexKey(pk)); err != nil {
		return nil, fmt.Errorf("store agent key in keychain: %w", err)
	}
	return a, nil
}

// Store imports an existing hex private key into the keychain for an account. The
// key is validated before anything is persisted, so a bad key is never stored.
func Store(account, hexPriv string) (*Agent, error) {
	a, err := fromHex(hexPriv)
	if err != nil {
		return nil, err
	}
	if err := keyring.Set(keyringService, keyringUser(account), strings.TrimPrefix(strings.TrimSpace(hexPriv), "0x")); err != nil {
		return nil, fmt.Errorf("store agent key in keychain: %w", err)
	}
	return a, nil
}

// Delete removes an account's agent key from the keychain (idempotent).
func Delete(account string) error {
	err := keyring.Delete(keyringService, keyringUser(account))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func fromHex(hexPriv string) (*Agent, error) {
	h := strings.TrimPrefix(strings.TrimSpace(hexPriv), "0x")
	pk, err := crypto.HexToECDSA(h)
	if err != nil {
		return nil, fmt.Errorf("invalid agent private key (want 64 hex chars): %w", err)
	}
	return &Agent{Key: pk, Address: crypto.PubkeyToAddress(pk.PublicKey).Hex()}, nil
}

func hexKey(pk *ecdsa.PrivateKey) string { return hex.EncodeToString(crypto.FromECDSA(pk)) }
