package papertrade

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Domain names are copied from the venue client. The exchange domain's name is
// the literal "String"; renaming it changes every digest.
const (
	domainExchangeName = "String"
	domainExchangeVer  = "1"
	domainCancelName   = "PaperTrade Intent Cancel"
	domainCancelVer    = "2"
)

// Signed is the JSON object posted as `signed`. Field order follows the EIP-712
// type, then v, r, s, which is what the relayer stores.
type Signed struct {
	Action string         `json:"action"`
	Fields map[string]any `json:"-"`
	Wire   any            `json:"-"`
	Digest string         `json:"digest"`
	// IntentID is keccak256(prefixHash || digest || signer). Cancel posts do
	// not mint a new id; IntentID is then the id being cancelled.
	IntentID string `json:"intent_id"`
	Signer   string `json:"signer"`
	V        string `json:"v"`
	R        string `json:"r"`
	S        string `json:"s"`
}

// OpenMessage is an OpenPosition intent. SizeRaw is margin in 18-decimal units,
// not a coin size — that is the venue's wire field.
type OpenMessage struct {
	User         common.Address
	InstrumentID uint32
	IsLong       bool
	SizeRaw      *big.Int
	Leverage     uint32
	UseDebt      bool
	Nonce        *big.Int
	Deadline     *big.Int
}

// CloseMessage closes 1–12 positions. Action name is closePositions; the EIP-712
// primary type is Close.
type CloseMessage struct {
	User        common.Address
	PositionIDs []*big.Int
	Nonce       *big.Int
	Deadline    *big.Int
}

// CancelMessage cancels one queued intent. It has no nonce. The POST body drops
// intentId (it is the URL); the digest still covers it.
type CancelMessage struct {
	User     common.Address
	IntentID common.Hash
	Deadline *big.Int
}

// RegisterMessage binds a session key. The master wallet signs it. FeeAddress
// is the builder code. The zero address names no builder, which is the default
// and what the venue client signs. A non-zero address must already be an
// approved builder or the relayer refuses the registration.
type RegisterMessage struct {
	User       common.Address
	Key        common.Address
	Expiry     *big.Int
	FeeAddress common.Address
	Nonce      *big.Int
	Deadline   *big.Int
}

func exchangeDomain() apitypes.TypedDataDomain {
	return apitypes.TypedDataDomain{
		Name:              domainExchangeName,
		Version:           domainExchangeVer,
		ChainId:           math.NewHexOrDecimal256(ChainID),
		VerifyingContract: Exchange,
	}
}

func cancelDomain() apitypes.TypedDataDomain {
	return apitypes.TypedDataDomain{
		Name:              domainCancelName,
		Version:           domainCancelVer,
		ChainId:           math.NewHexOrDecimal256(ChainID),
		VerifyingContract: Exchange,
	}
}

func domainTypes() []apitypes.Type {
	return []apitypes.Type{
		{Name: "name", Type: "string"},
		{Name: "version", Type: "string"},
		{Name: "chainId", Type: "uint256"},
		{Name: "verifyingContract", Type: "address"},
	}
}

// IntentID is keccak256( keccak256(prefix) || digest || signerAddress ).
// The address is 20 bytes, not a 32-byte word. digest is the 32-byte EIP-712 hash.
func IntentID(digest []byte, signer common.Address) (common.Hash, error) {
	if len(digest) != 32 {
		return common.Hash{}, fmt.Errorf("digest must be 32 bytes")
	}
	prefix := crypto.Keccak256([]byte(IntentIDPrefix))
	buf := make([]byte, 0, 32+32+20)
	buf = append(buf, prefix...)
	buf = append(buf, digest...)
	buf = append(buf, signer.Bytes()...)
	return crypto.Keccak256Hash(buf), nil
}

// SignOpen signs an OpenPosition with the session key.
func SignOpen(key *ecdsa.PrivateKey, m OpenMessage) (*Signed, error) {
	// Every uint is a decimal string. The venue client hashes the integer and
	// posts BigInt.toString(), and a JSON number would round a uint256 nonce.
	msg := apitypes.TypedDataMessage{
		"user":         addr(m.User),
		"instrumentId": strconv.FormatUint(uint64(m.InstrumentID), 10),
		"isLong":       m.IsLong,
		"size":         m.SizeRaw.String(),
		"leverage":     strconv.FormatUint(uint64(m.Leverage), 10),
		"useDebt":      m.UseDebt,
		"nonce":        m.Nonce.String(),
		"deadline":     m.Deadline.String(),
	}
	types := []apitypes.Type{
		{Name: "user", Type: "address"},
		{Name: "instrumentId", Type: "uint32"},
		{Name: "isLong", Type: "bool"},
		{Name: "size", Type: "uint128"},
		{Name: "leverage", Type: "uint32"},
		{Name: "useDebt", Type: "bool"},
		{Name: "nonce", Type: "uint256"},
		{Name: "deadline", Type: "uint64"},
	}
	s, err := sign(key, exchangeDomain(), "OpenPosition", types, msg)
	if err != nil {
		return nil, err
	}
	s.Action = "openPosition"
	s.Wire = openWire{
		User: addr(m.User), InstrumentID: strconv.FormatUint(uint64(m.InstrumentID), 10), IsLong: m.IsLong,
		Size: m.SizeRaw.String(), Leverage: strconv.FormatUint(uint64(m.Leverage), 10), UseDebt: m.UseDebt,
		Nonce: m.Nonce.String(), Deadline: m.Deadline.String(),
		V: s.V, R: s.R, S: s.S,
	}
	return s, nil
}

// SignClose signs a Close with the session key.
func SignClose(key *ecdsa.PrivateKey, m CloseMessage) (*Signed, error) {
	ids := make([]string, len(m.PositionIDs))
	for i, id := range m.PositionIDs {
		ids[i] = id.String()
	}
	msg := apitypes.TypedDataMessage{
		"user":        addr(m.User),
		"positionIds": ids,
		"nonce":       m.Nonce.String(),
		"deadline":    m.Deadline.String(),
	}
	types := []apitypes.Type{
		{Name: "user", Type: "address"},
		{Name: "positionIds", Type: "uint256[]"},
		{Name: "nonce", Type: "uint256"},
		{Name: "deadline", Type: "uint64"},
	}
	s, err := sign(key, exchangeDomain(), "Close", types, msg)
	if err != nil {
		return nil, err
	}
	s.Action = "closePositions"
	s.Wire = closeWire{
		User: addr(m.User), PositionIDs: ids, Nonce: m.Nonce.String(), Deadline: m.Deadline.String(),
		V: s.V, R: s.R, S: s.S,
	}
	return s, nil
}

// SignCancel signs a CancelIntent with the session key. The returned Wire omits
// intentId because the relayer takes it from the URL path.
func SignCancel(key *ecdsa.PrivateKey, m CancelMessage) (*Signed, error) {
	id := m.IntentID.Hex()
	msg := apitypes.TypedDataMessage{
		"user":     addr(m.User),
		"intentId": id,
		"deadline": m.Deadline.String(),
	}
	types := []apitypes.Type{
		{Name: "user", Type: "address"},
		{Name: "intentId", Type: "bytes32"},
		{Name: "deadline", Type: "uint256"},
	}
	s, err := sign(key, cancelDomain(), "CancelIntent", types, msg)
	if err != nil {
		return nil, err
	}
	s.Action = "cancelIntent"
	// The cancelled id is the protocol handle, not a newly derived intent id.
	s.IntentID = strings.ToLower(id)
	s.Wire = cancelWire{User: addr(m.User), Deadline: m.Deadline.String(), V: s.V, R: s.R, S: s.S}
	return s, nil
}

// SignRegister signs RegisterSessionKey with the master key.
func SignRegister(key *ecdsa.PrivateKey, m RegisterMessage) (*Signed, error) {
	fee := m.FeeAddress
	if fee == (common.Address{}) {
		fee = common.HexToAddress(FeeAddress)
	}
	msg := apitypes.TypedDataMessage{
		"user":       addr(m.User),
		"key":        addr(m.Key),
		"expiry":     m.Expiry.String(),
		"feeAddress": addr(fee),
		"nonce":      m.Nonce.String(),
		"deadline":   m.Deadline.String(),
	}
	types := []apitypes.Type{
		{Name: "user", Type: "address"},
		{Name: "key", Type: "address"},
		{Name: "expiry", Type: "uint64"},
		{Name: "feeAddress", Type: "address"},
		{Name: "nonce", Type: "uint256"},
		{Name: "deadline", Type: "uint64"},
	}
	s, err := sign(key, exchangeDomain(), "RegisterSessionKey", types, msg)
	if err != nil {
		return nil, err
	}
	s.Action = "registerSessionKey"
	s.Wire = registerWire{
		User: addr(m.User), Key: addr(m.Key), Expiry: m.Expiry.String(), FeeAddress: addr(fee),
		Nonce: m.Nonce.String(), Deadline: m.Deadline.String(),
		V: s.V, R: s.R, S: s.S,
	}
	return s, nil
}

func sign(key *ecdsa.PrivateKey, domain apitypes.TypedDataDomain, primary string, fields []apitypes.Type, msg apitypes.TypedDataMessage) (*Signed, error) {
	if key == nil {
		return nil, fmt.Errorf("nil signing key")
	}
	td := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainTypes(),
			primary:        fields,
		},
		PrimaryType: primary,
		Domain:      domain,
		Message:     msg,
	}
	digest, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		return nil, err
	}
	sig, err := crypto.Sign(digest, key)
	if err != nil {
		return nil, err
	}
	// crypto.Sign yields recovery id 0/1. The venue client sends v = 27 + id.
	rec := sig[64]
	if rec >= 27 {
		rec -= 27
	}
	sig[64] = rec
	pub, err := crypto.SigToPub(digest, sig)
	if err != nil {
		return nil, err
	}
	signer := crypto.PubkeyToAddress(*pub)
	want := crypto.PubkeyToAddress(key.PublicKey)
	if signer != want {
		return nil, fmt.Errorf("recovered signer %s, key is %s", signer.Hex(), want.Hex())
	}
	intent, err := IntentID(digest, signer)
	if err != nil {
		return nil, err
	}
	return &Signed{
		Digest:   "0x" + hex.EncodeToString(digest),
		IntentID: intent.Hex(),
		Signer:   strings.ToLower(signer.Hex()),
		V:        fmt.Sprintf("%d", 27+rec),
		R:        "0x" + hex.EncodeToString(sig[:32]),
		S:        "0x" + hex.EncodeToString(sig[32:64]),
	}, nil
}

func addr(a common.Address) string { return strings.ToLower(a.Hex()) }

// Wire structs. JSON field order matches the typed-data field order so a stored
// body resends stably. Every uint is a decimal string, including instrumentId
// and leverage. The relayer hashes the integer the client posts with
// BigInt.toString(), and a JSON number would lose precision on a uint256 nonce.

type openWire struct {
	User         string `json:"user"`
	InstrumentID string `json:"instrumentId"`
	IsLong       bool   `json:"isLong"`
	Size         string `json:"size"`
	Leverage     string `json:"leverage"`
	UseDebt      bool   `json:"useDebt"`
	Nonce        string `json:"nonce"`
	Deadline     string `json:"deadline"`
	V            string `json:"v"`
	R            string `json:"r"`
	S            string `json:"s"`
}

type closeWire struct {
	User        string   `json:"user"`
	PositionIDs []string `json:"positionIds"`
	Nonce       string   `json:"nonce"`
	Deadline    string   `json:"deadline"`
	V           string   `json:"v"`
	R           string   `json:"r"`
	S           string   `json:"s"`
}

type cancelWire struct {
	User     string `json:"user"`
	Deadline string `json:"deadline"`
	V        string `json:"v"`
	R        string `json:"r"`
	S        string `json:"s"`
}

type registerWire struct {
	User       string `json:"user"`
	Key        string `json:"key"`
	Expiry     string `json:"expiry"`
	FeeAddress string `json:"feeAddress"`
	Nonce      string `json:"nonce"`
	Deadline   string `json:"deadline"`
	V          string `json:"v"`
	R          string `json:"r"`
	S          string `json:"s"`
}
