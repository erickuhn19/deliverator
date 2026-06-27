package wallet

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

const testKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestFromHexRoundTrip(t *testing.T) {
	a, err := fromHex(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Address, "0x") || len(a.Address) != 42 {
		t.Errorf("address format wrong: %q", a.Address)
	}
	if hexKey(a.Key) != testKeyHex {
		t.Errorf("hexKey round-trip mismatch: %q", hexKey(a.Key))
	}
	// 0x prefix and surrounding whitespace are tolerated.
	b, err := fromHex("  0x" + testKeyHex + "\n")
	if err != nil || b.Address != a.Address {
		t.Errorf("prefix/whitespace handling: %v / %q vs %q", err, b.Address, a.Address)
	}
}

func TestFromHexRejectsBad(t *testing.T) {
	// Trivial decode failures AND cryptographic-validity failures (a degenerate
	// signing key must never be accepted).
	bad := []string{
		"", "zz", "0xnothex",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde",  // 63 chars (odd)
		"0000000000000000000000000000000000000000000000000000000000000000", // zero key
	}
	for _, b := range bad {
		if _, err := fromHex(b); err == nil {
			t.Errorf("fromHex(%q) should error", b)
		}
	}
}

func TestKeyringUser(t *testing.T) {
	if keyringUser("") != "agent:main" {
		t.Errorf("default account = %q, want agent:main", keyringUser(""))
	}
	if keyringUser("funding") != "agent:funding" {
		t.Errorf("named account = %q, want agent:funding", keyringUser("funding"))
	}
}

// Generate stores in the keychain; Load reads it back to the same address.
func TestGenerateLoadRoundTrip(t *testing.T) {
	keyring.MockInit()
	gen, err := Generate("main")
	if err != nil {
		t.Fatal(err)
	}
	if !Has("main") {
		t.Fatal("Has should report the generated key present")
	}
	got, err := Load("main")
	if err != nil {
		t.Fatalf("Load after Generate: %v", err)
	}
	if got.Address != gen.Address {
		t.Errorf("loaded address %q != generated %q", got.Address, gen.Address)
	}
	// Two generates differ (non-deterministic).
	gen2, _ := Generate("other")
	if gen2.Address == gen.Address {
		t.Error("two generated keys should differ")
	}
}

// Store imports a hex key; Load returns the matching address.
func TestStoreLoad(t *testing.T) {
	keyring.MockInit()
	stored, err := Store("main", "0x"+testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := fromHex(testKeyHex)
	if stored.Address != want.Address {
		t.Errorf("Store address %q != %q", stored.Address, want.Address)
	}
	got, err := Load("main")
	if err != nil || got.Address != want.Address {
		t.Errorf("Load after Store: %v / %q", err, got.Address)
	}
}

// An invalid key is rejected BEFORE anything is persisted.
func TestStoreRejectsBadBeforePersist(t *testing.T) {
	keyring.MockInit()
	if _, err := Store("main", "not-hex"); err == nil {
		t.Fatal("Store should reject an invalid key")
	}
	if Has("main") {
		t.Fatal("a rejected key must not be persisted")
	}
	if _, err := Load("main"); !errors.Is(err, ErrNoAgentKey) {
		t.Errorf("Load after rejected Store should be ErrNoAgentKey, got %v", err)
	}
}

// Load on an empty keychain returns ErrNoAgentKey so callers can prompt onboard.
func TestLoadMissingIsErrNoAgentKey(t *testing.T) {
	keyring.MockInit()
	_, err := Load("ghost")
	if !errors.Is(err, ErrNoAgentKey) {
		t.Errorf("missing key should be ErrNoAgentKey, got %v", err)
	}
}

// Delete is idempotent and account-isolated.
func TestDeleteIdempotentAndIsolated(t *testing.T) {
	keyring.MockInit()
	if err := Delete("never-stored"); err != nil {
		t.Errorf("Delete of absent key should be nil, got %v", err)
	}
	if _, err := Store("a", "0x"+testKeyHex); err != nil {
		t.Fatal(err)
	}
	if _, err := Store("b", "0x"+testKeyHex); err != nil {
		t.Fatal(err)
	}
	if err := Delete("a"); err != nil {
		t.Fatal(err)
	}
	if Has("a") {
		t.Error("account a key should be gone")
	}
	if !Has("b") {
		t.Error("deleting account a must not affect account b")
	}
}

// A keychain backend failure is surfaced (wrapped), not swallowed.
func TestStoreBackendError(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain unavailable"))
	t.Cleanup(keyring.MockInit) // restore a working mock for later tests
	if _, err := Store("main", "0x"+testKeyHex); err == nil ||
		!strings.Contains(err.Error(), "store agent key in keychain") {
		t.Errorf("backend error should be wrapped, got %v", err)
	}
}

// The secret must never appear in an error string (errors embed the account name
// and wrap keyring errors — guard against a future leak).
func TestErrorsDoNotLeakSecret(t *testing.T) {
	keyring.MockInitWithError(errors.New("boom"))
	t.Cleanup(keyring.MockInit)
	_, err := Store("main", "0x"+testKeyHex)
	if err != nil && strings.Contains(err.Error(), testKeyHex) {
		t.Fatal("Store error must not contain the secret")
	}
}

// The DELIVERATOR_AGENT_KEY env override is used when set (headless/CI injection).
func TestLoadEnvOverride(t *testing.T) {
	keyring.MockInit() // empty keychain
	t.Setenv(EnvKeyVar, "0x"+testKeyHex)
	if ActiveSource() != "env" {
		t.Fatalf("ActiveSource should be env when set, got %q", ActiveSource())
	}
	got, err := Load("main") // no keychain entry — must come from env
	if err != nil {
		t.Fatalf("env-override Load: %v", err)
	}
	want, _ := fromHex(testKeyHex)
	if got.Address != want.Address {
		t.Errorf("env-loaded address %q != %q", got.Address, want.Address)
	}
	if !Has("main") {
		t.Error("Has should be true when the env override is set")
	}
}

// An unset/empty env never hides the keychain key (the bug that motivated the
// keychain-default model).
func TestLoadEmptyEnvFallsBackToKeychain(t *testing.T) {
	keyring.MockInit()
	if _, err := Store("main", "0x"+testKeyHex); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKeyVar, "") // explicitly empty
	if ActiveSource() != "keychain" {
		t.Fatalf("empty env should resolve to keychain, got %q", ActiveSource())
	}
	if _, err := Load("main"); err != nil {
		t.Fatalf("empty env must fall back to keychain, got %v", err)
	}
}

// When both are present, the env override wins (explicit injection beats the store).
func TestEnvOverridesKeychain(t *testing.T) {
	keyring.MockInit()
	kcGen, _ := Generate("main") // a key in the keychain
	t.Setenv(EnvKeyVar, "0x"+testKeyHex)
	got, err := Load("main")
	if err != nil {
		t.Fatal(err)
	}
	envWant, _ := fromHex(testKeyHex)
	if got.Address != envWant.Address {
		t.Errorf("env should override keychain: got %q, want env %q (keychain was %q)", got.Address, envWant.Address, kcGen.Address)
	}
}
