package papertrade

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// On-chain builder surface, confirmed against HyperEVM chain 999 on 2026-10-23
// by matching PUSH4 dispatchers and eth_call return values:
//
//   - SessionKeyManager.builderRegistry() -> BuilderRegistry
//   - BuilderRegistry.feeRate() -> uint256 wad (live 5000000000000000 = 0.5%)
//   - BuilderRegistry.isApproved(address) -> bool
//   - Exchange.builderFees(address) -> uint256 unclaimed, 18-decimal raw
//   - Exchange.claimBuilderFees() — no args; the builder address must be msg.sender
//   - SessionKeyManager.sessionKeys(address user, address key) -> (uint64 expiry, address feeAddress)
//     packed in one storage slot (low 64 bits, then the address). The getter
//     selector sessionKeys(address,address) is in the implementation dispatcher.
//
// claimBuilderFees is a HyperEVM transaction paid in HYPE. It credits the
// builder's exchange balance; it does not withdraw. The event is
// BuilderFeesClaimed(address indexed builder, uint256 amount, uint256 paid).
// paid is what the exchange balance received. amount-paid joined the payout queue.

const (
	selBuilderRegistry  = "e737de13" // builderRegistry()
	selFeeRate          = "978bbdb9" // feeRate()
	selIsApproved       = "673448dd" // isApproved(address)
	selBuilderFees      = "9d346583" // builderFees(address)
	selClaimBuilderFees = "69f9a5f0" // claimBuilderFees()
	selSessionKeys      = "96ade1f9" // sessionKeys(address,address)

	claimGasMax = 1_000_000
)

// ClaimBuilderFeesData is the calldata for claimBuilderFees().
func ClaimBuilderFeesData() []byte {
	return crypto.Keccak256([]byte("claimBuilderFees()"))[:4]
}

// SessionBinding is one SessionKeyManager.sessionKeys record.
type SessionBinding struct {
	ExpiryUnix uint64
	FeeAddress common.Address
}

// ChainError classifies a builder chain call. Phase tells the command which
// exit code to use. Hash is set only after eth_sendRawTransaction returned one.
type ChainError struct {
	Phase   string
	Hash    string
	Message string
}

const (
	// ChainPreSend means the RPC request never left this host.
	ChainPreSend = "pre_send"
	// ChainRefused means this process refused to sign. Nothing was broadcast
	// and retrying unchanged will not succeed.
	ChainRefused = "refused"
	// ChainRejected means the node refused the transaction before it was mined.
	ChainRejected = "rejected"
	// ChainSimulated means eth_estimateGas reverted. Nothing was broadcast.
	ChainSimulated = "simulated"
	// ChainPending means a transaction may be in flight or was broadcast and
	// not yet mined. Do not send another claim.
	ChainPending = "pending"
	// ChainReverted means the transaction was mined and failed.
	ChainReverted = "reverted"
)

func (e *ChainError) Error() string {
	if e == nil {
		return "papertrade chain"
	}
	return e.Message
}

// BuilderRegistryAddress reads SessionKeyManager.builderRegistry().
func (c *Client) BuilderRegistryAddress(ctx context.Context) (common.Address, error) {
	raw, err := c.ethCall(ctx, common.HexToAddress(SessionKeyManager), "0x"+selBuilderRegistry)
	if err != nil {
		return common.Address{}, err
	}
	addr, err := decodeAddressWord(raw)
	if err != nil || addr == (common.Address{}) {
		return common.Address{}, fmt.Errorf("builder registry is not set")
	}
	return addr, nil
}

// BuilderFeeRate reads BuilderRegistry.feeRate().
func (c *Client) BuilderFeeRate(ctx context.Context, registry common.Address) (*big.Int, error) {
	raw, err := c.ethCall(ctx, registry, "0x"+selFeeRate)
	if err != nil {
		return nil, err
	}
	return decodeUint256(raw)
}

// BuilderApproved reads BuilderRegistry.isApproved(address). The zero address
// is not an approved builder; registrations use it to name no builder, and the
// caller must not treat that as a failure.
func (c *Client) BuilderApproved(ctx context.Context, registry, builder common.Address) (bool, error) {
	raw, err := c.ethCall(ctx, registry, "0x"+selIsApproved+abiWord(builder))
	if err != nil {
		return false, err
	}
	return decodeBool(raw)
}

// UnclaimedBuilderFees reads Exchange.builderFees(address).
func (c *Client) UnclaimedBuilderFees(ctx context.Context, builder common.Address) (*big.Int, error) {
	raw, err := c.ethCall(ctx, common.HexToAddress(Exchange), "0x"+selBuilderFees+abiWord(builder))
	if err != nil {
		return nil, err
	}
	return decodeUint256(raw)
}

// SessionBinding reads SessionKeyManager.sessionKeys(user, key).
func (c *Client) SessionBinding(ctx context.Context, user, key common.Address) (SessionBinding, error) {
	buf, err := c.ethCall(ctx, common.HexToAddress(SessionKeyManager), "0x"+selSessionKeys+abiWord(user)+abiWord(key))
	if err != nil {
		return SessionBinding{}, err
	}
	if len(buf) != 64 {
		return SessionBinding{}, fmt.Errorf("sessionKeys returned %d bytes", len(buf))
	}
	expWord := buf[:32]
	for _, b := range expWord[:24] {
		if b != 0 {
			return SessionBinding{}, fmt.Errorf("sessionKeys expiry is not a uint64")
		}
	}
	fee, err := decodeAddressWord(buf[32:])
	if err != nil {
		return SessionBinding{}, err
	}
	return SessionBinding{ExpiryUnix: new(big.Int).SetBytes(expWord).Uint64(), FeeAddress: fee}, nil
}

// ClaimPlan is the unsigned claimBuilderFees call. Dry-run stops here.
type ClaimPlan struct {
	From      common.Address
	To        common.Address
	Data      []byte
	Nonce     uint64
	Gas       uint64
	FeeCap    *big.Int
	Unclaimed *big.Int
	Balance   *big.Int
}

// PlanClaim checks chain id, pending nonce, unclaimed fees, gas, and HYPE
// balance. It does not sign or broadcast.
func (c *Client) PlanClaim(ctx context.Context, from common.Address) (*ClaimPlan, error) {
	chainID, err := c.chainID(ctx)
	if err != nil {
		return nil, err
	}
	if chainID.Cmp(big.NewInt(ChainID)) != 0 {
		return nil, &ChainError{Phase: ChainRefused, Message: fmt.Sprintf("RPC chain id is %s, Papertrade claims sign chain %d", chainID, ChainID)}
	}
	latest, pending, err := c.noncePair(ctx, from)
	if err != nil {
		return nil, err
	}
	if pending > latest {
		return nil, &ChainError{Phase: ChainRefused, Message: "a transaction from this builder address is already pending; wait for it before claiming again"}
	}
	unclaimed, err := c.UnclaimedBuilderFees(ctx, from)
	if err != nil {
		return nil, err
	}
	if unclaimed.Sign() == 0 {
		return nil, &ChainError{Phase: ChainRefused, Message: "builderFees is zero; there is nothing to claim"}
	}
	data := ClaimBuilderFeesData()
	if hex.EncodeToString(data) != selClaimBuilderFees {
		return nil, &ChainError{Phase: ChainRefused, Message: "claimBuilderFees selector mismatch"}
	}
	to := common.HexToAddress(Exchange)
	gas, err := c.estimateClaim(ctx, from, to, data)
	if err != nil {
		return nil, err
	}
	feeCap, err := c.feeCap(ctx)
	if err != nil {
		return nil, err
	}
	bal, err := c.balance(ctx, from)
	if err != nil {
		return nil, err
	}
	need := new(big.Int).Mul(new(big.Int).SetUint64(gas), feeCap)
	if bal.Cmp(need) < 0 {
		return nil, &ChainError{Phase: ChainRefused, Message: fmt.Sprintf(
			"builder %s has %s HYPE; this claim needs about %s HYPE for gas",
			addr(from), FormatRaw18(bal), FormatRaw18(need),
		)}
	}
	return &ClaimPlan{
		From: from, To: to, Data: data, Nonce: latest, Gas: gas,
		FeeCap: feeCap, Unclaimed: unclaimed, Balance: bal,
	}, nil
}

// NotApprovedError means RegisterSessionKey would be refused for this fee address.
type NotApprovedError struct{ Address common.Address }

func (e *NotApprovedError) Error() string {
	if e == nil {
		return "builder is not approved"
	}
	return addr(e.Address) + " is not an approved Papertrade builder"
}

// CheckBuilder reads the live registry. The zero address names no builder and
// skips the approval call. A non-zero address must be approved. rate may be
// nil only when builder is zero.
func (c *Client) CheckBuilder(ctx context.Context, builder common.Address) (registry common.Address, rate *big.Int, err error) {
	if builder == (common.Address{}) {
		return common.Address{}, nil, nil
	}
	registry, err = c.BuilderRegistryAddress(ctx)
	if err != nil {
		return common.Address{}, nil, err
	}
	ok, err := c.BuilderApproved(ctx, registry, builder)
	if err != nil {
		return registry, nil, err
	}
	if !ok {
		return registry, nil, &NotApprovedError{Address: builder}
	}
	rate, err = c.BuilderFeeRate(ctx, registry)
	if err != nil {
		return registry, nil, err
	}
	return registry, rate, nil
}

// ClaimResult is a mined claimBuilderFees transaction.
type ClaimResult struct {
	Hash     common.Hash
	Amount   *big.Int
	Paid     *big.Int
	HasEvent bool
	Block    string
}

// ClaimBuilderFees sends claimBuilderFees() from key. The key's address must
// be the builder. A zero unclaimed balance is refused before broadcast, as is
// a second claim while this address already has a pending transaction.
func (c *Client) ClaimBuilderFees(ctx context.Context, key *ecdsa.PrivateKey) (*ClaimResult, error) {
	if key == nil {
		return nil, &ChainError{Phase: ChainRefused, Message: "nil signing key"}
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	plan, err := c.PlanClaim(ctx, from)
	if err != nil {
		return nil, err
	}
	chainID := big.NewInt(ChainID)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     plan.Nonce,
		GasTipCap: big.NewInt(0),
		GasFeeCap: plan.FeeCap,
		Gas:       plan.Gas,
		To:        &plan.To,
		Value:     big.NewInt(0),
		Data:      plan.Data,
	})
	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, key)
	if err != nil {
		return nil, &ChainError{Phase: ChainPreSend, Message: err.Error()}
	}
	got, err := types.Sender(signer, signed)
	if err != nil || got != from {
		return nil, &ChainError{Phase: ChainRefused, Message: "claim transaction recovered a different sender"}
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return nil, &ChainError{Phase: ChainPreSend, Message: err.Error()}
	}
	hash, err := c.sendRaw(ctx, raw)
	if err != nil {
		return nil, err
	}
	if hash != signed.Hash() {
		return nil, &ChainError{Phase: ChainPending, Hash: hash.Hex(), Message: "node returned a different transaction hash than the one signed"}
	}
	rec, err := c.waitReceipt(ctx, hash)
	if err != nil {
		return nil, err
	}
	if rec.Status != "0x1" {
		return nil, &ChainError{Phase: ChainReverted, Hash: hash.Hex(), Message: "claimBuilderFees was mined and reverted. Nothing was claimed."}
	}
	amount, paid, ok := parseClaimLogs(plan.To, from, rec.Logs)
	out := &ClaimResult{Hash: hash, Amount: amount, Paid: paid, HasEvent: ok, Block: rec.Block}
	return out, nil
}

func parseClaimLogs(exchange, builder common.Address, logs []rpcLog) (*big.Int, *big.Int, bool) {
	topic := crypto.Keccak256Hash([]byte("BuilderFeesClaimed(address,uint256,uint256)"))
	want := common.LeftPadBytes(builder.Bytes(), 32)
	for _, lg := range logs {
		if !strings.EqualFold(lg.Address, exchange.Hex()) || len(lg.Topics) < 2 {
			continue
		}
		if !strings.EqualFold(lg.Topics[0], topic.Hex()) {
			continue
		}
		if !bytes.Equal(common.FromHex(lg.Topics[1]), want) {
			continue
		}
		data := common.FromHex(lg.Data)
		if len(data) != 64 {
			continue
		}
		return new(big.Int).SetBytes(data[:32]), new(big.Int).SetBytes(data[32:]), true
	}
	return nil, nil, false
}

type rpcLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type rpcReceipt struct {
	Status string   `json:"status"`
	Block  string   `json:"blockNumber"`
	Logs   []rpcLog `json:"logs"`
}

func (c *Client) waitReceipt(ctx context.Context, hash common.Hash) (*rpcReceipt, error) {
	var last error
	for {
		rec, err := c.receipt(ctx, hash)
		if err == nil && rec != nil {
			return rec, nil
		}
		last = err
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			msg := "claimBuilderFees was submitted but the receipt did not arrive"
			if last != nil {
				msg = msg + ": " + last.Error()
			}
			return nil, &ChainError{Phase: ChainPending, Hash: hash.Hex(), Message: msg}
		case <-timer.C:
		}
	}
}

func (c *Client) receipt(ctx context.Context, hash common.Hash) (*rpcReceipt, error) {
	var raw json.RawMessage
	if err := c.rpc(ctx, "eth_getTransactionReceipt", []any{hash.Hex()}, &raw); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, fmt.Errorf("receipt not mined")
	}
	var rec rpcReceipt
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	if rec.Status == "" {
		return nil, fmt.Errorf("receipt has no status")
	}
	return &rec, nil
}

func (c *Client) sendRaw(ctx context.Context, raw []byte) (common.Hash, error) {
	var result string
	err := c.rpc(ctx, "eth_sendRawTransaction", []any{"0x" + hex.EncodeToString(raw)}, &result)
	if err != nil {
		if ce, ok := err.(*ChainError); ok {
			// Keep pre_send and pending as classified. A JSON-RPC error from
			// the node means the transaction was not accepted.
			return common.Hash{}, ce
		}
		return common.Hash{}, &ChainError{Phase: ChainPending, Message: "eth_sendRawTransaction did not return a hash: " + err.Error()}
	}
	hexHash := strings.TrimPrefix(strings.TrimSpace(result), "0x")
	if len(hexHash) != 64 {
		return common.Hash{}, &ChainError{Phase: ChainPending, Message: "eth_sendRawTransaction returned " + result}
	}
	return common.HexToHash(result), nil
}

func (c *Client) estimateClaim(ctx context.Context, from, to common.Address, data []byte) (uint64, error) {
	var result string
	call := map[string]string{
		"from": addr(from), "to": addr(to), "data": "0x" + hex.EncodeToString(data), "value": "0x0",
	}
	err := c.rpc(ctx, "eth_estimateGas", []any{call}, &result)
	if err != nil {
		if ce, ok := err.(*ChainError); ok && ce.Phase != ChainPreSend {
			ce.Phase = ChainSimulated
			if ce.Message == "" {
				ce.Message = "claimBuilderFees simulation reverted"
			}
			return 0, ce
		}
		return 0, err
	}
	n, err := hexQuantity(result)
	if err != nil || n.Sign() <= 0 || !n.IsUint64() {
		return 0, &ChainError{Phase: ChainSimulated, Message: "eth_estimateGas returned " + result}
	}
	est := n.Uint64()
	if est > claimGasMax {
		return 0, &ChainError{Phase: ChainSimulated, Message: fmt.Sprintf("claim gas estimate %d is above the %d ceiling", est, claimGasMax)}
	}
	gas := est * 6 / 5
	if gas < est {
		gas = est
	}
	if gas > claimGasMax {
		gas = claimGasMax
	}
	return gas, nil
}

func (c *Client) feeCap(ctx context.Context) (*big.Int, error) {
	var block struct {
		BaseFeePerGas string `json:"baseFeePerGas"`
	}
	if err := c.rpc(ctx, "eth_getBlockByNumber", []any{"latest", false}, &block); err != nil {
		return nil, err
	}
	base, _ := hexQuantity(block.BaseFeePerGas)
	if base == nil {
		base = big.NewInt(0)
	}
	cap := new(big.Int).Lsh(base, 1) // 2x base fee
	var price string
	if err := c.rpc(ctx, "eth_gasPrice", []any{}, &price); err == nil {
		if gp, e := hexQuantity(price); e == nil && gp.Cmp(cap) > 0 {
			cap = gp
		}
	}
	if cap.Sign() == 0 {
		cap = big.NewInt(1)
	}
	return cap, nil
}

func (c *Client) balance(ctx context.Context, account common.Address) (*big.Int, error) {
	var result string
	if err := c.rpc(ctx, "eth_getBalance", []any{account.Hex(), "latest"}, &result); err != nil {
		return nil, err
	}
	n, err := hexQuantity(result)
	if err != nil {
		return nil, &ChainError{Phase: ChainPreSend, Message: "eth_getBalance: " + err.Error()}
	}
	return n, nil
}

func (c *Client) noncePair(ctx context.Context, account common.Address) (latest, pending uint64, err error) {
	latest, err = c.nonceAt(ctx, account, "latest")
	if err != nil {
		return 0, 0, err
	}
	pending, err = c.nonceAt(ctx, account, "pending")
	return latest, pending, err
}

func (c *Client) nonceAt(ctx context.Context, account common.Address, tag string) (uint64, error) {
	var result string
	if err := c.rpc(ctx, "eth_getTransactionCount", []any{account.Hex(), tag}, &result); err != nil {
		return 0, err
	}
	n, err := hexQuantity(result)
	if err != nil || !n.IsUint64() {
		return 0, &ChainError{Phase: ChainPreSend, Message: "transaction count " + result}
	}
	return n.Uint64(), nil
}

func (c *Client) chainID(ctx context.Context) (*big.Int, error) {
	var result string
	if err := c.rpc(ctx, "eth_chainId", []any{}, &result); err != nil {
		return nil, err
	}
	return hexQuantity(result)
}

func (c *Client) ethCall(ctx context.Context, to common.Address, data string) ([]byte, error) {
	var result string
	call := map[string]string{"to": addr(to), "data": data}
	if err := c.rpc(ctx, "eth_call", []any{call, "latest"}, &result); err != nil {
		return nil, err
	}
	raw := strings.TrimPrefix(strings.TrimSpace(result), "0x")
	if raw == "" {
		return nil, &ChainError{Phase: ChainRejected, Message: "eth_call returned empty data"}
	}
	buf, err := hex.DecodeString(raw)
	if err != nil {
		return nil, &ChainError{Phase: ChainRejected, Message: "eth_call returned non-hex data"}
	}
	return buf, nil
}

func (c *Client) rpc(ctx context.Context, method string, params []any, out any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RPC, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		phase := ChainPending
		if preSend(err) {
			phase = ChainPreSend
		}
		// A read that never leaves the host is pre-send. A send that dies
		// after the bytes may have gone out is pending. Callers that are
		// reads pass through ChainPreSend only when the dial failed; a reset
		// after write is treated as pending by sendRaw and as a retryable
		// network error by readers (they did not broadcast a transaction).
		if method != "eth_sendRawTransaction" {
			phase = ChainPreSend
		}
		return &ChainError{Phase: phase, Message: method + ": " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		phase := ChainPreSend
		if method == "eth_sendRawTransaction" {
			phase = ChainPending
		}
		return &ChainError{Phase: phase, Message: method + ": " + err.Error()}
	}
	if resp.StatusCode >= 300 {
		phase := ChainRejected
		if method != "eth_sendRawTransaction" {
			phase = ChainPreSend
		}
		if resp.StatusCode >= 500 && method == "eth_sendRawTransaction" {
			phase = ChainPending
		}
		return &ChainError{Phase: phase, Message: fmt.Sprintf("%s: HTTP %d", method, resp.StatusCode)}
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		phase := ChainPreSend
		if method == "eth_sendRawTransaction" {
			phase = ChainPending
		}
		return &ChainError{Phase: phase, Message: method + ": " + err.Error()}
	}
	if env.Error != nil {
		return &ChainError{Phase: ChainRejected, Message: method + ": " + env.Error.Message}
	}
	if out == nil {
		return nil
	}
	if len(env.Result) == 0 {
		return &ChainError{Phase: ChainRejected, Message: method + " returned no result"}
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &ChainError{Phase: ChainRejected, Message: method + ": " + err.Error()}
	}
	return nil
}

func abiWord(a common.Address) string {
	return hex.EncodeToString(common.LeftPadBytes(a.Bytes(), 32))
}

func decodeAddressWord(raw []byte) (common.Address, error) {
	if len(raw) < 32 {
		return common.Address{}, fmt.Errorf("short address word")
	}
	word := raw[len(raw)-32:]
	for _, b := range word[:12] {
		if b != 0 {
			return common.Address{}, fmt.Errorf("address word is not left-padded")
		}
	}
	return common.BytesToAddress(word[12:]), nil
}

func decodeUint256(raw []byte) (*big.Int, error) {
	if len(raw) != 32 {
		return nil, fmt.Errorf("uint256 is %d bytes", len(raw))
	}
	return new(big.Int).SetBytes(raw), nil
}

func decodeBool(raw []byte) (bool, error) {
	n, err := decodeUint256(raw)
	if err != nil {
		return false, err
	}
	switch n.Cmp(big.NewInt(1)) {
	case -1:
		if n.Sign() != 0 {
			return false, fmt.Errorf("bool is not 0 or 1")
		}
		return false, nil
	case 0:
		return true, nil
	default:
		return false, fmt.Errorf("bool is not 0 or 1")
	}
}

func hexQuantity(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0x" {
		return big.NewInt(0), nil
	}
	s = strings.TrimPrefix(s, "0x")
	n, ok := new(big.Int).SetString(s, 16)
	if !ok || n.Sign() < 0 {
		return nil, fmt.Errorf("bad quantity")
	}
	return n, nil
}
