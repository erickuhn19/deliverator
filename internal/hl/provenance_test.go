package hl

import (
	"encoding/hex"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func canonicalOrder() OrderAction {
	return OrderAction{Type: "order", Orders: []OrderWire{{Asset: 1, IsBuy: true, LimitPx: "65000.5", Size: "0.25", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}}, Grouping: "na"}
}

// eip712Hash recomputes the 32-byte digest SignL1Action signs (same pipeline as
// signInner) so a recovered signer can be checked against the known key.
func eip712Hash(t *testing.T, action any, vault string, nonce int64, expires *int64, mainnet bool) []byte {
	t.Helper()
	ah, err := actionHash(action, vault, nonce, expires)
	if err != nil {
		t.Fatalf("actionHash: %v", err)
	}
	td := l1Payload(constructPhantomAgent(ah, mainnet))
	domainSep, err := td.HashStruct("EIP712Domain", td.Domain.Map())
	if err != nil {
		t.Fatalf("domain hash: %v", err)
	}
	typedHash, err := hashStructLenient(td, td.PrimaryType, td.Message)
	if err != nil {
		t.Fatalf("typed hash: %v", err)
	}
	raw := append([]byte{0x19, 0x01}, domainSep...)
	raw = append(raw, typedHash...)
	return crypto.Keccak256(raw)
}

// recoverSigner reconstructs the 65-byte signature from (r,s,v) — left-padding the
// minimal-hex r/s to 32 bytes and mapping v(27/28) to the recovery id — and returns
// the recovered signer address (0x-hex).
func recoverSigner(t *testing.T, hash []byte, sig SignatureResult) string {
	t.Helper()
	r, ok1 := new(big.Int).SetString(strings.TrimPrefix(sig.R, "0x"), 16)
	s, ok2 := new(big.Int).SetString(strings.TrimPrefix(sig.S, "0x"), 16)
	if !ok1 || !ok2 {
		t.Fatalf("bad r/s hex: %q %q", sig.R, sig.S)
	}
	var raw [65]byte
	r.FillBytes(raw[0:32])
	s.FillBytes(raw[32:64])
	raw[64] = byte(sig.V - 27)
	pub, err := crypto.SigToPub(hash, raw[:])
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	return crypto.PubkeyToAddress(*pub).Hex()
}

// TestSignatureProvenanceEcrecover proves every vector's (r,s,v) is a valid
// signature of the exact EIP-712 hash under the known key — an INDEPENDENT crypto
// check (catches v-parity / r-s-encoding / zero-padding bugs) that does not trust
// the frozen (r,s,v) literals.
func TestSignatureProvenanceEcrecover(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	want := crypto.PubkeyToAddress(key.PublicKey).Hex()
	for _, c := range vectorActions() {
		sig, serr := SignL1Action(key, c.action, "", goldenNonce, nil, true)
		if serr != nil {
			t.Fatalf("%s: sign: %v", c.name, serr)
		}
		if got := recoverSigner(t, eip712Hash(t, c.action, "", goldenNonce, nil, true), sig); got != want {
			t.Errorf("%s: recovered %s, want %s", c.name, got, want)
		}
	}
}

// TestCanonicalConnectionID pins the action_hash (connectionId) for the canonical
// order — the keccak input to the phantom agent, an intermediate a reviewer can
// recompute against eth_account/web3.py without re-running deliverator.
func TestCanonicalConnectionID(t *testing.T) {
	const want = "ea5390f8698f5aeae8b1a50616e18470bf78f816391d26adf23292cf587733a5"
	ah, err := actionHash(canonicalOrder(), "", goldenNonce, nil)
	if err != nil {
		t.Fatalf("actionHash: %v", err)
	}
	if got := hex.EncodeToString(ah); got != want {
		t.Fatalf("connectionId drift:\n got:    %s\n golden: %s", got, want)
	}
}

// TestSignL1ActionTrailers pins the signing branches the per-family vectors miss:
// a non-empty vaultAddress (0x01||addr trailer), a non-nil expiresAfter (0x00||8B
// trailer), and testnet (phantom-agent source "b"). Each is frozen (drift), proven
// to DIFFER from the mainnet-no-trailer baseline (so the trailer/source actually
// entered the signed hash), and Ecrecover-verified.
func TestSignL1ActionTrailers(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	wantAddr := crypto.PubkeyToAddress(key.PublicKey).Hex()
	canon := canonicalOrder()
	exp := goldenNonce + 5000
	const vault = "0x1234567890abcdef1234567890abcdef12345678"
	base, err := SignL1Action(key, canon, "", goldenNonce, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		vault        string
		expires      *int64
		mainnet      bool
		wantR, wantS string
		wantV        int
	}{
		{"vault", vault, nil, true, "0x7ef7f73b1b534d828fe679907deb87963c42a6c12e077a767ad585461314773c", "0x4a25064ec2efbb11d6a51b4349766b961d1e892ec996ccf8bd2e09ac7f418cd4", 28},
		{"expires", "", &exp, true, "0xfe8512035668b55a02b5a5a9110093dd82684f022591adb1a880ece0dc019cc7", "0x32d369ee3f09bc5f4986fb2d37c77f112c859cbeebac5c7bc813d1842d6309bd", 27},
		{"testnet", "", nil, false, "0x4a90d4043843e0764a665596a977da30c3489a579b808978172dcbdc4bba41fb", "0x227e887afe0ef05936dfd085163ef744501006d1b4560e09124900a2897b1e33", 28},
		{"vault_expires", vault, &exp, true, "0x66c2bd3648d67e82adecf070875c49912959da11bef4ed996db48df21cee734b", "0x1f72a7943a4f8dadfe260a82b11002936eb59882ff7689d14ababa1351a99c62", 28},
	}
	for _, c := range cases {
		sig, serr := SignL1Action(key, canon, c.vault, goldenNonce, c.expires, c.mainnet)
		if serr != nil {
			t.Fatalf("%s: sign: %v", c.name, serr)
		}
		if sig.R != c.wantR || sig.S != c.wantS || sig.V != c.wantV {
			t.Errorf("%s drift: got r=%s s=%s v=%d", c.name, sig.R, sig.S, sig.V)
		}
		if sig.R == base.R && sig.S == base.S {
			t.Errorf("%s did not change the signature vs the no-trailer baseline", c.name)
		}
		if got := recoverSigner(t, eip712Hash(t, canon, c.vault, goldenNonce, c.expires, c.mainnet), sig); got != wantAddr {
			t.Errorf("%s: recovered %s, want %s", c.name, got, wantAddr)
		}
	}
}

func actionTypeOf(a any) string { return reflect.ValueOf(a).FieldByName("Type").String() }

// TestEveryActionTypeHasVector is the checklist guard: every signable action type
// must have at least one golden vector. Adding a new action type to wire.go (or a
// vector for one) without updating the expected set fails this test, so a new
// signing surface can't ship unpinned.
func TestEveryActionTypeHasVector(t *testing.T) {
	want := map[string]bool{
		"order": true, "cancel": true, "cancelByCloid": true, "modify": true,
		"batchModify": true, "updateLeverage": true, "updateIsolatedMargin": true,
		"scheduleCancel": true, "twapOrder": true, "twapCancel": true, "setReferrer": true,
	}
	got := map[string]bool{}
	for _, c := range vectorActions() {
		got[actionTypeOf(c.action)] = true
	}
	for typ := range want {
		if !got[typ] {
			t.Errorf("no signing vector covers action type %q", typ)
		}
	}
	for typ := range got {
		if !want[typ] {
			t.Errorf("vector covers unexpected action type %q — add it to the expected set", typ)
		}
	}
}
