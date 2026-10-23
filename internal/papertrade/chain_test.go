package papertrade

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func word(n *big.Int) string {
	return "0x" + hexPad(n.Bytes())
}

func hexPad(b []byte) string {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return common.Bytes2Hex(out)
}

func addrWord(a common.Address) string { return "0x" + hexPad(a.Bytes()) }

type rpcFake struct {
	chain     string
	latest    string
	pending   string
	balance   string
	baseFee   string
	gasPrice  string
	approved  bool
	unclaimed *big.Int
	rate      *big.Int
	registry  common.Address
	session   []byte
	estimate  string
	estErr    string
	receipt   any
	sent      *types.Transaction
}

func (f *rpcFake) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var result any
	var rpcErr string
	switch req.Method {
	case "eth_chainId":
		result = f.chain
	case "eth_getTransactionCount":
		tag, _ := req.Params[1].(string)
		if tag == "pending" {
			result = f.pending
		} else {
			result = f.latest
		}
	case "eth_getBalance":
		result = f.balance
	case "eth_gasPrice":
		result = f.gasPrice
	case "eth_getBlockByNumber":
		result = map[string]any{"baseFeePerGas": f.baseFee}
	case "eth_call":
		call, _ := req.Params[0].(map[string]any)
		data, _ := call["data"].(string)
		switch {
		case strings.HasPrefix(data, "0x"+selBuilderRegistry):
			result = addrWord(f.registry)
		case strings.HasPrefix(data, "0x"+selFeeRate):
			result = word(f.rate)
		case strings.HasPrefix(data, "0x"+selIsApproved):
			if f.approved {
				result = word(big.NewInt(1))
			} else {
				result = word(big.NewInt(0))
			}
		case strings.HasPrefix(data, "0x"+selBuilderFees):
			result = word(f.unclaimed)
		case strings.HasPrefix(data, "0x"+selSessionKeys):
			result = "0x" + common.Bytes2Hex(f.session)
		default:
			rpcErr = "unknown call"
		}
	case "eth_estimateGas":
		if f.estErr != "" {
			rpcErr = f.estErr
		} else {
			result = f.estimate
		}
	case "eth_sendRawTransaction":
		raw, _ := req.Params[0].(string)
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(common.FromHex(raw)); err != nil {
			rpcErr = err.Error()
			break
		}
		f.sent = tx
		result = tx.Hash().Hex()
	case "eth_getTransactionReceipt":
		result = f.receipt
	default:
		rpcErr = "unexpected " + req.Method
	}
	env := map[string]any{"jsonrpc": "2.0", "id": 1}
	if rpcErr != "" {
		env["error"] = map[string]any{"code": 3, "message": rpcErr}
	} else {
		env["result"] = result
	}
	_ = json.NewEncoder(w).Encode(env)
}

func readyFake() *rpcFake {
	return &rpcFake{
		chain: "0x3e7", latest: "0x2", pending: "0x2",
		balance: "0x3635c9adc5dea00000", baseFee: "0x5f5e100", gasPrice: "0x5f5e100",
		approved: true, unclaimed: big.NewInt(1_000_000_000_000_000_000),
		rate:     big.NewInt(5_000_000_000_000_000),
		registry: common.HexToAddress(BuilderRegistryFallback),
		estimate: "0x5208",
		receipt:  map[string]any{"status": "0x1", "blockNumber": "0x10", "logs": []any{}},
	}
}

func fakeClient(t *testing.T, f *rpcFake) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := NewClient(time.Second)
	c.RPC = srv.URL
	c.Exchange = srv.URL
	return c
}

func TestChainReads(t *testing.T) {
	f := readyFake()
	exp := make([]byte, 64)
	exp[31] = 9
	copy(exp[44:], common.HexToAddress("0x00000000000000000000000000000000000000aa").Bytes())
	f.session = exp
	c := fakeClient(t, f)
	ctx := context.Background()
	reg, err := c.BuilderRegistryAddress(ctx)
	if err != nil || !strings.EqualFold(reg.Hex(), BuilderRegistryFallback) {
		t.Fatalf("registry %v %s", err, reg.Hex())
	}
	rate, err := c.BuilderFeeRate(ctx, reg)
	if err != nil || rate.Cmp(f.rate) != 0 {
		t.Fatalf("rate %v %v", err, rate)
	}
	ok, err := c.BuilderApproved(ctx, reg, common.HexToAddress("0x1"))
	if err != nil || !ok {
		t.Fatalf("approved %v %v", err, ok)
	}
	fees, err := c.UnclaimedBuilderFees(ctx, common.HexToAddress("0x1"))
	if err != nil || fees.Cmp(f.unclaimed) != 0 {
		t.Fatalf("fees %v %v", err, fees)
	}
	b, err := c.SessionBinding(ctx, common.HexToAddress("0x1"), common.HexToAddress("0x2"))
	if err != nil || b.ExpiryUnix != 9 || b.FeeAddress != common.HexToAddress("0xaa") {
		t.Fatalf("binding %+v %v", b, err)
	}
	_, _, err = c.CheckBuilder(ctx, common.Address{})
	if err != nil {
		t.Fatal(err)
	}
	f.approved = false
	_, _, err = c.CheckBuilder(ctx, common.HexToAddress("0x1"))
	if _, ok := err.(*NotApprovedError); !ok {
		t.Fatalf("not approved: %v", err)
	}
}

func TestClaimBuilderFees(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKey)
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	f := readyFake()
	amount := common.LeftPadBytes(big.NewInt(1000).Bytes(), 32)
	paid := common.LeftPadBytes(big.NewInt(400).Bytes(), 32)
	topic := crypto.Keccak256Hash([]byte("BuilderFeesClaimed(address,uint256,uint256)")).Hex()
	f.receipt = map[string]any{
		"status": "0x1", "blockNumber": "0x10",
		"logs": []any{map[string]any{
			"address": Exchange,
			"topics":  []string{topic, addrWord(from)},
			"data":    "0x" + common.Bytes2Hex(amount) + common.Bytes2Hex(paid),
		}},
	}
	c := fakeClient(t, f)
	res, err := c.ClaimBuilderFees(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasEvent || res.Amount.Cmp(big.NewInt(1000)) != 0 || res.Paid.Cmp(big.NewInt(400)) != 0 {
		t.Fatalf("%+v", res)
	}
	tx := f.sent
	if tx == nil || tx.ChainId().Int64() != ChainID || tx.Value().Sign() != 0 {
		t.Fatalf("tx %+v", tx)
	}
	if !strings.EqualFold(tx.To().Hex(), Exchange) || common.Bytes2Hex(tx.Data()) != selClaimBuilderFees {
		t.Fatalf("call to %s data %x", tx.To().Hex(), tx.Data())
	}
	if tx.GasTipCap().Sign() != 0 {
		t.Fatalf("tip %s", tx.GasTipCap())
	}

	f.unclaimed = big.NewInt(0)
	_, err = c.PlanClaim(context.Background(), from)
	ce, ok := err.(*ChainError)
	if !ok || ce.Phase != ChainRefused || !strings.Contains(ce.Message, "nothing to claim") {
		t.Fatalf("zero fees: %v", err)
	}
	f.unclaimed = big.NewInt(1)
	f.pending = "0x3"
	_, err = c.PlanClaim(context.Background(), from)
	ce, ok = err.(*ChainError)
	if !ok || ce.Phase != ChainRefused || !strings.Contains(ce.Message, "pending") {
		t.Fatalf("in flight: %v", err)
	}
	f.pending = "0x2"
	f.balance = "0x0"
	_, err = c.PlanClaim(context.Background(), from)
	ce, ok = err.(*ChainError)
	if !ok || ce.Phase != ChainRefused || !strings.Contains(ce.Message, "HYPE") {
		t.Fatalf("gas: %v", err)
	}
	f.balance = "0x3635c9adc5dea00000"
	f.chain = "0x1"
	_, err = c.PlanClaim(context.Background(), from)
	ce, ok = err.(*ChainError)
	if !ok || ce.Phase != ChainRefused || !strings.Contains(ce.Message, "chain") {
		t.Fatalf("chain: %v", err)
	}
	f.chain = "0x3e7"
	f.estErr = "execution reverted"
	_, err = c.PlanClaim(context.Background(), from)
	ce, ok = err.(*ChainError)
	if !ok || ce.Phase != ChainSimulated {
		t.Fatalf("simulate: %v", err)
	}
	f.estErr = ""
	f.receipt = map[string]any{"status": "0x0", "blockNumber": "0x11", "logs": []any{}}
	_, err = c.ClaimBuilderFees(context.Background(), key)
	ce, ok = err.(*ChainError)
	if !ok || ce.Phase != ChainReverted || ce.Hash == "" {
		t.Fatalf("reverted: %v", err)
	}
	f.receipt = nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.ClaimBuilderFees(ctx, key)
	ce, ok = err.(*ChainError)
	if !ok || ce.Phase != ChainPending || ce.Hash == "" {
		t.Fatalf("pending: %v", err)
	}
}
