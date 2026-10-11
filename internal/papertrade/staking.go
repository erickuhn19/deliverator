package papertrade

import (
	"crypto/ecdsa"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// PAPER staking. Source: exchange.papertrade.xyz bundle (typed-data chunk and
// the account hooks), checked 2026-10-10.
//
//   - Stake, Unstake, and Claim are EIP-712 intents in the "PaperStaking" v1
//     domain, verifyingContract = PaperStaking, chain 999.
//   - The connected wallet (the master wallet, the `user`) signs them. The
//     session key does not: the venue client signs open/close with the session
//     signer but stake/unstake/claim with the wallet itself.
//   - They post to /intents as {"action": "paperStake"|"paperUnstake"|"paperClaim",
//     "signed": {...typed fields, v, r, s}} like every other relayer intent.
//   - Nonces use the route "paper" against PaperStaking (see NonceKey).
//   - Rewards are USDC. pending = pendingBase + max(0, staked*acc/1e27 - rewardDebt),
//     where acc is /state/trading accRewardPerShareRaw.
//   - The relayer enforces a per-stake minimum (intake minimumPaperStakeRaw);
//     unstake and claim have none.
const (
	domainStakingName = "PaperStaking"
	domainStakingVer  = "1"

	// NonceRoutePaper is the nonce route for staking intents.
	NonceRoutePaper = "paper"
)

// rewardScale is the accumulator's fixed-point scale (10^27).
var rewardScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)

// StakeMessage is a Stake or Unstake intent. Amount is PAPER in 18-decimal raw units.
type StakeMessage struct {
	User     common.Address
	Amount   *big.Int
	Nonce    *big.Int
	Deadline *big.Int
}

// ClaimMessage claims accrued USDC staking rewards.
type ClaimMessage struct {
	User     common.Address
	Nonce    *big.Int
	Deadline *big.Int
}

func stakingDomain() apitypes.TypedDataDomain {
	return apitypes.TypedDataDomain{
		Name:              domainStakingName,
		Version:           domainStakingVer,
		ChainId:           math.NewHexOrDecimal256(ChainID),
		VerifyingContract: PaperStaking,
	}
}

// ParsePaper parses a base-10 PAPER amount (up to 18 decimals) into raw units.
func ParsePaper(s string) (*big.Int, error) { return parseDecimal18(s) }

// SignStake signs a Stake intent with the master (wallet) key.
func SignStake(key *ecdsa.PrivateKey, m StakeMessage) (*Signed, error) {
	return signAmount(key, "Stake", "paperStake", m)
}

// SignUnstake signs an Unstake intent with the master (wallet) key.
func SignUnstake(key *ecdsa.PrivateKey, m StakeMessage) (*Signed, error) {
	return signAmount(key, "Unstake", "paperUnstake", m)
}

func signAmount(key *ecdsa.PrivateKey, primary, action string, m StakeMessage) (*Signed, error) {
	if m.Amount == nil || m.Amount.Sign() <= 0 {
		return nil, errString("amount must be greater than zero")
	}
	msg := apitypes.TypedDataMessage{
		"user":     addr(m.User),
		"amount":   m.Amount.String(),
		"nonce":    m.Nonce.String(),
		"deadline": m.Deadline.String(),
	}
	types := []apitypes.Type{
		{Name: "user", Type: "address"},
		{Name: "amount", Type: "uint256"},
		{Name: "nonce", Type: "uint256"},
		{Name: "deadline", Type: "uint64"},
	}
	s, err := sign(key, stakingDomain(), primary, types, msg)
	if err != nil {
		return nil, err
	}
	s.Action = action
	s.Wire = stakeWire{
		User: addr(m.User), Amount: m.Amount.String(), Nonce: m.Nonce.String(), Deadline: m.Deadline.String(),
		V: s.V, R: s.R, S: s.S,
	}
	return s, nil
}

// SignClaim signs a Claim intent with the master (wallet) key.
func SignClaim(key *ecdsa.PrivateKey, m ClaimMessage) (*Signed, error) {
	msg := apitypes.TypedDataMessage{
		"user":     addr(m.User),
		"nonce":    m.Nonce.String(),
		"deadline": m.Deadline.String(),
	}
	types := []apitypes.Type{
		{Name: "user", Type: "address"},
		{Name: "nonce", Type: "uint256"},
		{Name: "deadline", Type: "uint64"},
	}
	s, err := sign(key, stakingDomain(), "Claim", types, msg)
	if err != nil {
		return nil, err
	}
	s.Action = "paperClaim"
	s.Wire = claimWire{User: addr(m.User), Nonce: m.Nonce.String(), Deadline: m.Deadline.String(), V: s.V, R: s.R, S: s.S}
	return s, nil
}

// PendingRewards mirrors the venue client: pendingBase + max(0, staked*acc/1e27 - rewardDebt).
// Inputs are decimal strings; an unparsable input yields ok=false.
func PendingRewards(staked, pendingBase, rewardDebt, acc string) (*big.Int, bool) {
	st, ok1 := decInt(staked)
	pb, ok2 := decInt(pendingBase)
	rd, ok3 := decInt(rewardDebt)
	ac, ok4 := decInt(acc)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, false
	}
	n := new(big.Int).Mul(st, ac)
	n.Quo(n, rewardScale)
	n.Sub(n, rd)
	if n.Sign() < 0 {
		n.SetInt64(0)
	}
	return n.Add(n, pb), true
}

func decInt(s string) (*big.Int, bool) {
	if s == "" {
		return big.NewInt(0), true
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 {
		return nil, false
	}
	return n, true
}

type stakeWire struct {
	User     string `json:"user"`
	Amount   string `json:"amount"`
	Nonce    string `json:"nonce"`
	Deadline string `json:"deadline"`
	V        string `json:"v"`
	R        string `json:"r"`
	S        string `json:"s"`
}

type claimWire struct {
	User     string `json:"user"`
	Nonce    string `json:"nonce"`
	Deadline string `json:"deadline"`
	V        string `json:"v"`
	R        string `json:"r"`
	S        string `json:"s"`
}
