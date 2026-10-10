package papertrade

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Golden vectors were produced with viem against the venue's typed-data, using
// the public Hardhat test key. They lock the domain name "String", the decimal
// uint encoding, and the 84-byte intent-id preimage. Do not POST them.
const (
	goldenKey    = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	goldenUser   = "0x0000000000000000000000000000000000000001"
	goldenDigest = "0x486edcbd8bd3758bcab5278b35c2cd6be8b37ef3372ea8d4786af0e18ca3890d"
	goldenPrefix = "0xf42e60f8efa2a9e7f0871f979962930f4f0da165cafc8ebb9fd1ddaa48a8f1a0"
	goldenIntent = "0x37748c06ef6b88695386f9f2f0205afe293a25685c0e2c6421b75bf084437389"
	goldenR      = "0x644c41c4b5560b18db415a42e8e50fa9dd91191fabf36fcb88f6f1b58774f40d"
	goldenS      = "0x3ce4fd0e1cf534cba106afdcbd8c27903033621eb79b7a14e84d5a3b660ca714"
	goldenClose  = "0xdd348cf6bf9d99a5a445488b401e88dbd99c7832b49a3e083adbc46e48e95522"
	goldenCancel = "0xba8077042984ba82df74e54f7f4d1fd231c3990a37b6aa8cd3a7d181c7a8bade"
)

func TestSignOpenGolden(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKey)
	if err != nil {
		t.Fatal(err)
	}
	signer := crypto.PubkeyToAddress(key.PublicKey)
	if !strings.EqualFold(signer.Hex(), "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266") {
		t.Fatalf("signer %s", signer.Hex())
	}
	prefix := crypto.Keccak256Hash([]byte(IntentIDPrefix)).Hex()
	if prefix != goldenPrefix {
		t.Fatalf("prefix hash %s", prefix)
	}
	deadline := big.NewInt(1800000000)
	signed, err := SignOpen(key, OpenMessage{
		User:         common.HexToAddress(goldenUser),
		InstrumentID: 0,
		IsLong:       true,
		SizeRaw:      mustParse("10"),
		Leverage:     10,
		UseDebt:      false,
		Nonce:        big.NewInt(256),
		Deadline:     deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if signed.Digest != goldenDigest {
		t.Fatalf("digest %s", signed.Digest)
	}
	if !strings.EqualFold(signed.IntentID, goldenIntent) {
		t.Fatalf("intent %s", signed.IntentID)
	}
	if signed.V != "27" || !strings.EqualFold(signed.R, goldenR) || !strings.EqualFold(signed.S, goldenS) {
		t.Fatalf("sig v=%s r=%s s=%s", signed.V, signed.R, signed.S)
	}
	raw, err := json.Marshal(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["instrumentId"] != "0" || wire["leverage"] != "10" || wire["size"] != "10000000000000000000" {
		t.Fatalf("wire uints must be decimal strings: %s", raw)
	}
	if _, ok := wire["intentId"]; ok {
		t.Fatalf("open wire must not carry intentId: %s", raw)
	}

	id, err := IntentID(common.FromHex(goldenDigest), signer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(id.Hex(), goldenIntent) {
		t.Fatalf("IntentID %s", id.Hex())
	}
}

func TestSignCloseAndCancelDigests(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKey)
	if err != nil {
		t.Fatal(err)
	}
	user := common.HexToAddress(goldenUser)
	deadline := big.NewInt(1800000000)
	closed, err := SignClose(key, CloseMessage{
		User:        user,
		PositionIDs: []*big.Int{big.NewInt(1), big.NewInt(12)},
		Nonce:       big.NewInt(257),
		Deadline:    deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Digest != goldenClose {
		t.Fatalf("close digest %s", closed.Digest)
	}
	if closed.Action != "closePositions" {
		t.Fatalf("action %s", closed.Action)
	}
	cancelled, err := SignCancel(key, CancelMessage{
		User:     user,
		IntentID: common.HexToHash(goldenIntent),
		Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Digest != goldenCancel {
		t.Fatalf("cancel digest %s", cancelled.Digest)
	}
	if !strings.EqualFold(cancelled.IntentID, goldenIntent) {
		t.Fatalf("cancel intent id %s", cancelled.IntentID)
	}
	raw, _ := json.Marshal(cancelled.Wire)
	if strings.Contains(string(raw), "intentId") {
		t.Fatalf("cancel wire drops intentId (it is the URL): %s", raw)
	}
}

func mustParse(s string) *big.Int {
	n, err := ParseMargin(s)
	if err != nil {
		panic(err)
	}
	return n
}
