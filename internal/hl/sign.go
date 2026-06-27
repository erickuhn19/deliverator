package hl

// L1 action signing, ported from the official Hyperliquid Python SDK
// (hyperliquid/utils/signing.py) and cross-checked byte-for-byte against the
// reference Go port sonirico/go-hyperliquid (MIT). The scheme:
//
//  1. msgpack-encode the action (struct field order is load-bearing; compact
//     ints; NO map-key sorting), then rewrite str16 headers to str8 to match
//     Python's msgpack output exactly.
//  2. action_hash = keccak256( msgpack || nonce(8, big-endian) || vault-flag
//     || optional expiresAfter ).
//  3. phantom agent = {source: "a" mainnet / "b" testnet, connectionId: hash}.
//  4. EIP-712 sign the phantom agent under the fixed "Exchange" domain
//     (chainId 1337, zero verifying contract).
//
// Only L1 (agent-signable) actions are supported here by design.

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/vmihailenco/msgpack/v5"
)

// SignatureResult is the {r,s,v} signature posted alongside an action.
type SignatureResult struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

func addressToBytes(address string) []byte {
	address = strings.TrimPrefix(address, "0x")
	b, _ := hex.DecodeString(address)
	return b
}

// convertStr16ToStr8 rewrites msgpack str16 (0xda + 2-byte len) headers to str8
// (0xd9 + 1-byte len) for strings <256 bytes, to match Python's msgpack output.
// It walks the structure so a 0xda byte occurring inside e.g. a uint64 value is
// never mistaken for a string header.
func convertStr16ToStr8(data []byte) []byte {
	result := make([]byte, 0, len(data))
	pos := 0
	for pos < len(data) {
		before := len(result)
		consumed := walkMsgpackValue(data, pos, &result)
		if consumed <= 0 {
			// A truncated/malformed value: a container may have already appended its
			// header + partial children before bailing, so roll back to `before` and
			// emit the remaining raw bytes verbatim — appending data[pos:] WITHOUT the
			// rollback would duplicate whatever the failed walk wrote. (Real actions
			// are always well-formed msgpack, so this salvage path is never hit in
			// production; the rollback keeps the walker total-and-lossless regardless.)
			result = append(result[:before], data[pos:]...)
			break
		}
		pos += consumed
	}
	return result
}

func walkMsgpackValue(data []byte, pos int, result *[]byte) int {
	if pos >= len(data) {
		return 0
	}
	b := data[pos]
	remaining := len(data) - pos

	// Fixed-length single-byte types: fixint (pos/neg), nil, unused, bool.
	if b <= 0x7f || b >= 0xe0 || (b >= 0xc0 && b <= 0xc3) {
		*result = append(*result, b)
		return 1
	}

	// fixstr (0xa0-0xbf): 1 header + N data bytes.
	if b >= 0xa0 && b <= 0xbf {
		n := int(b & 0x1f)
		total := 1 + n
		if remaining < total {
			return 0
		}
		*result = append(*result, data[pos:pos+total]...)
		return total
	}

	// fixmap (0x80-0x8f): N key/value pairs.
	if b >= 0x80 && b <= 0x8f {
		count := int(b & 0x0f)
		*result = append(*result, b)
		consumed := 1
		for i := 0; i < count*2; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	}

	// fixarray (0x90-0x9f): N elements.
	if b >= 0x90 && b <= 0x9f {
		count := int(b & 0x0f)
		*result = append(*result, b)
		consumed := 1
		for i := 0; i < count; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	}

	switch b {
	case 0xca: // float32
		return copyFixed(data, pos, 5, result)
	case 0xcb: // float64
		return copyFixed(data, pos, 9, result)
	case 0xcc: // uint8
		return copyFixed(data, pos, 2, result)
	case 0xcd: // uint16
		return copyFixed(data, pos, 3, result)
	case 0xce: // uint32
		return copyFixed(data, pos, 5, result)
	case 0xcf: // uint64
		return copyFixed(data, pos, 9, result)
	case 0xd0: // int8
		return copyFixed(data, pos, 2, result)
	case 0xd1: // int16
		return copyFixed(data, pos, 3, result)
	case 0xd2: // int32
		return copyFixed(data, pos, 5, result)
	case 0xd3: // int64
		return copyFixed(data, pos, 9, result)
	case 0xd4: // fixext1
		return copyFixed(data, pos, 3, result)
	case 0xd5: // fixext2
		return copyFixed(data, pos, 4, result)
	case 0xd6: // fixext4
		return copyFixed(data, pos, 6, result)
	case 0xd7: // fixext8
		return copyFixed(data, pos, 10, result)
	case 0xd8: // fixext16
		return copyFixed(data, pos, 18, result)
	case 0xc4: // bin8
		return copyVarLen(data, pos, 1, result)
	case 0xc5: // bin16
		return copyVarLen(data, pos, 2, result)
	case 0xc6: // bin32
		return copyVarLen(data, pos, 4, result)
	case 0xc7: // ext8
		return copyExtVarLen(data, pos, 1, result)
	case 0xc8: // ext16
		return copyExtVarLen(data, pos, 2, result)
	case 0xc9: // ext32
		return copyExtVarLen(data, pos, 4, result)
	case 0xd9: // str8 — already compact
		return copyVarLen(data, pos, 1, result)
	case 0xda: // str16 — the conversion target
		if remaining < 3 {
			return 0
		}
		length := (int(data[pos+1]) << 8) | int(data[pos+2])
		total := 3 + length
		if remaining < total {
			return 0
		}
		if length < 256 {
			*result = append(*result, 0xd9)
			*result = append(*result, byte(length))
			*result = append(*result, data[pos+3:pos+total]...)
		} else {
			*result = append(*result, data[pos:pos+total]...)
		}
		return total
	case 0xdb: // str32
		return copyVarLen(data, pos, 4, result)
	case 0xdc: // array16
		if remaining < 3 {
			return 0
		}
		count := (int(data[pos+1]) << 8) | int(data[pos+2])
		*result = append(*result, data[pos:pos+3]...)
		consumed := 3
		for i := 0; i < count; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	case 0xdd: // array32
		if remaining < 5 {
			return 0
		}
		count := (int(data[pos+1]) << 24) | (int(data[pos+2]) << 16) | (int(data[pos+3]) << 8) | int(data[pos+4])
		*result = append(*result, data[pos:pos+5]...)
		consumed := 5
		for i := 0; i < count; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	case 0xde: // map16
		if remaining < 3 {
			return 0
		}
		count := (int(data[pos+1]) << 8) | int(data[pos+2])
		*result = append(*result, data[pos:pos+3]...)
		consumed := 3
		for i := 0; i < count*2; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	case 0xdf: // map32
		if remaining < 5 {
			return 0
		}
		count := (int(data[pos+1]) << 24) | (int(data[pos+2]) << 16) | (int(data[pos+3]) << 8) | int(data[pos+4])
		*result = append(*result, data[pos:pos+5]...)
		consumed := 5
		for i := 0; i < count*2; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	default:
		*result = append(*result, b)
		return 1
	}
}

func copyFixed(data []byte, pos, size int, result *[]byte) int {
	if len(data)-pos < size {
		return 0
	}
	*result = append(*result, data[pos:pos+size]...)
	return size
}

func copyVarLen(data []byte, pos, lenBytes int, result *[]byte) int {
	headerSize := 1 + lenBytes
	if len(data)-pos < headerSize {
		return 0
	}
	length := readLen(data, pos+1, lenBytes)
	total := headerSize + length
	if len(data)-pos < total {
		return 0
	}
	*result = append(*result, data[pos:pos+total]...)
	return total
}

func copyExtVarLen(data []byte, pos, lenBytes int, result *[]byte) int {
	headerSize := 1 + lenBytes + 1
	if len(data)-pos < headerSize {
		return 0
	}
	length := readLen(data, pos+1, lenBytes)
	total := headerSize + length
	if len(data)-pos < total {
		return 0
	}
	*result = append(*result, data[pos:pos+total]...)
	return total
}

func readLen(data []byte, pos, size int) int {
	switch size {
	case 1:
		return int(data[pos])
	case 2:
		return (int(data[pos]) << 8) | int(data[pos+1])
	case 4:
		return (int(data[pos]) << 24) | (int(data[pos+1]) << 16) | (int(data[pos+2]) << 8) | int(data[pos+3])
	default:
		return 0
	}
}

// actionHash computes the connection-id hashed by the phantom agent. It returns
// an error — rather than panicking — on an unencodable action, a negative nonce,
// or a negative expiresAfter. All three are reachable (a grossly-wrong host
// clock yields a pre-1970 / negative nonce; a caller bug yields the others), and
// a panic here would escape mid-sign with a raw Go stack trace and exit code 2,
// outside the schema-v1 envelope and exit-code matrix an agent branches on. As
// errors they thread through SignL1Action → the write path → output.Fail
// (audit #91 / S6). The well-formed path is unchanged, so signed bytes do not move.
func actionHash(action any, vaultAddress string, nonce int64, expiresAfter *int64) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	// Do NOT sort map keys — Python preserves insertion order, and Go structs
	// serialize in declaration order. Compact ints to match Python output.
	enc.UseCompactInts(true)
	if err := enc.Encode(action); err != nil {
		return nil, fmt.Errorf("failed to marshal action: %w", err)
	}
	data := convertStr16ToStr8(buf.Bytes())

	if nonce < 0 {
		return nil, fmt.Errorf("nonce cannot be negative: %d", nonce)
	}
	nonceBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(nonceBytes, uint64(nonce))
	data = append(data, nonceBytes...)

	if vaultAddress == "" {
		data = append(data, 0x00)
	} else {
		data = append(data, 0x01)
		data = append(data, addressToBytes(vaultAddress)...)
	}

	if expiresAfter != nil {
		if *expiresAfter < 0 {
			return nil, fmt.Errorf("expiresAfter cannot be negative: %d", *expiresAfter)
		}
		data = append(data, 0x00)
		expiresAfterBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(expiresAfterBytes, uint64(*expiresAfter))
		data = append(data, expiresAfterBytes...)
	}

	return crypto.Keccak256(data), nil
}

func constructPhantomAgent(hash []byte, isMainnet bool) map[string]any {
	source := "b" // testnet
	if isMainnet {
		source = "a" // mainnet
	}
	return map[string]any{"source": source, "connectionId": hash}
}

func l1Payload(phantomAgent map[string]any) apitypes.TypedData {
	// chainId is 1337 for both networks — it is only a signing-domain identifier.
	chainID := gethmath.HexOrDecimal256(*big.NewInt(1337))
	return apitypes.TypedData{
		Domain: apitypes.TypedDataDomain{
			ChainId:           &chainID,
			Name:              "Exchange",
			Version:           "1",
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Types: apitypes.Types{
			"Agent": []apitypes.Type{
				{Name: "source", Type: "string"},
				{Name: "connectionId", Type: "bytes32"},
			},
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
		},
		PrimaryType: "Agent",
		Message:     phantomAgent,
	}
}

// hashStructLenient hashes only the fields declared in the type, converting
// uint64 fields to *big.Int the way apitypes expects (matches eth_account).
func hashStructLenient(typedData apitypes.TypedData, primaryType string, message map[string]any) ([]byte, error) {
	types := typedData.Types[primaryType]
	filtered := make(map[string]any)
	for _, t := range types {
		val, ok := message[t.Name]
		if !ok {
			continue
		}
		if t.Type == "uint64" {
			var u uint64
			switch v := val.(type) {
			case uint64:
				u = v
			case int64:
				if v < 0 {
					return nil, fmt.Errorf("cannot convert negative int64 %d to uint64", v)
				}
				u = uint64(v)
			case int:
				if v < 0 {
					return nil, fmt.Errorf("cannot convert negative int %d to uint64", v)
				}
				u = uint64(v)
			case float64:
				if v < 0 || v != float64(uint64(v)) {
					return nil, fmt.Errorf("invalid float64 value %f for uint64", v)
				}
				u = uint64(v)
			case json.Number:
				p, err := strconv.ParseUint(string(v), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("failed to parse json.Number %s to uint64 for %s: %w", v, t.Name, err)
				}
				u = p
			case string:
				p, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("failed to parse string %s to uint64 for %s: %w", v, t.Name, err)
				}
				u = p
			case *big.Int:
				filtered[t.Name] = v
				continue
			default:
				return nil, fmt.Errorf("unsupported type for uint64 field %s", t.Name)
			}
			filtered[t.Name] = new(big.Int).SetUint64(u)
			continue
		}
		filtered[t.Name] = val
	}
	return typedData.HashStruct(primaryType, filtered)
}

func signInner(privateKey *ecdsa.PrivateKey, typedData apitypes.TypedData) (SignatureResult, error) {
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to hash domain: %w", err)
	}
	typedDataHash, err := hashStructLenient(typedData, typedData.PrimaryType, typedData.Message)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to hash typed data: %w", err)
	}
	rawData := []byte{0x19, 0x01}
	rawData = append(rawData, domainSeparator...)
	rawData = append(rawData, typedDataHash...)
	msgHash := crypto.Keccak256Hash(rawData)

	signature, err := crypto.Sign(msgHash.Bytes(), privateKey)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to sign message: %w", err)
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:64])
	v := int(signature[64]) + 27
	return SignatureResult{R: hexutil.EncodeBig(r), S: hexutil.EncodeBig(s), V: v}, nil
}

// SignL1Action signs an L1 action: msgpack action hash -> phantom agent ->
// EIP-712 signature. isMainnet selects the phantom-agent source.
func SignL1Action(
	privateKey *ecdsa.PrivateKey,
	action any,
	vaultAddress string,
	timestamp int64,
	expiresAfter *int64,
	isMainnet bool,
) (SignatureResult, error) {
	hash, err := actionHash(action, vaultAddress, timestamp, expiresAfter)
	if err != nil {
		return SignatureResult{}, err
	}
	phantomAgent := constructPhantomAgent(hash, isMainnet)
	return signInner(privateKey, l1Payload(phantomAgent))
}
