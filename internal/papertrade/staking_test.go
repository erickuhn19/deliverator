package papertrade

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Staking golden vectors: viem 2 hashTypedData/signTypedData with the venue's
// Stake/Unstake/Claim types, domain {PaperStaking, 1, 999, PaperStaking}, and
// the public Hardhat key. They lock the domain name and field order.
func TestSignStakingGolden(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKey)
	if err != nil {
		t.Fatal(err)
	}
	user := common.HexToAddress(goldenUser)
	dl := big.NewInt(1800000000)
	thousand, _ := new(big.Int).SetString("1000000000000000000000", 10)
	half, _ := new(big.Int).SetString("500000000000000000", 10)
	cases := []struct {
		name, action, digest, r, s string
		sign                       func() (*Signed, error)
	}{
		{"stake", "paperStake",
			"0xd29e6d53c29b68ac821b9d96e4355e708efe9ad66f3c785a0593d0f058727352",
			"0xad87dbeb56f83d20ab268a4c7dec90d8a81d137e1e13c4b50e4957c7196e85ee",
			"0x7bdf350823206182a23d807d2e4b7ea577ebdcba8ad3a31d280bb72ecd070343",
			func() (*Signed, error) {
				return SignStake(key, StakeMessage{User: user, Amount: thousand, Nonce: big.NewInt(257), Deadline: dl})
			}},
		{"unstake", "paperUnstake",
			"0x77c76ba8d04d5ed5e47cfe8f2d424b7d801e630aa33370dc0520f27de93c793d",
			"0x61c45084d941ed40d33a4623cbdcbbf51aae4fcf0a1c5e49315f4437741891cd",
			"0x67686a6549beb5d6004d6eb009ffb33541c995fc7c54b7b7541eed02ea55bbd0",
			func() (*Signed, error) {
				return SignUnstake(key, StakeMessage{User: user, Amount: half, Nonce: big.NewInt(258), Deadline: dl})
			}},
		{"claim", "paperClaim",
			"0xf15a646ee26bfcaac6417de431e377d767d5b48642031c253aad71299a43090e",
			"0xc3fdbcb15e2e72b4f3826645d2a73b30e5ce0df04c99566c216c08c14d5fc8da",
			"0x15f8018ef7af61e84eea5a3cafe5401944fdc9f62c17f7c0274565bb1b365c7a",
			func() (*Signed, error) {
				return SignClaim(key, ClaimMessage{User: user, Nonce: big.NewInt(259), Deadline: dl})
			}},
	}
	for _, c := range cases {
		s, err := c.sign()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if s.Digest != c.digest || s.R != c.r || s.S != c.s || s.Action != c.action {
			t.Fatalf("%s: got digest %s r %s s %s action %s", c.name, s.Digest, s.R, s.S, s.Action)
		}
		b, _ := json.Marshal(s.Wire)
		if !strings.HasPrefix(string(b), `{"user":"`+goldenUser+`"`) || !strings.Contains(string(b), `"deadline":"1800000000"`) {
			t.Fatalf("%s wire %s", c.name, b)
		}
		if c.action == "paperClaim" && strings.Contains(string(b), "amount") {
			t.Fatalf("claim wire carries amount: %s", b)
		}
	}
	if _, err := SignStake(key, StakeMessage{User: user, Amount: big.NewInt(0), Nonce: big.NewInt(1), Deadline: dl}); err == nil {
		t.Fatal("zero stake must not sign")
	}
	if _, err := SignClaim(nil, ClaimMessage{User: user, Nonce: big.NewInt(1), Deadline: dl}); err == nil {
		t.Fatal("nil key must fail")
	}
}

func TestPendingRewards(t *testing.T) {
	// 1000e18 staked * 2e27 / 1e27 = 2000e18 accrued, minus 1000e18 debt, plus 5 base.
	p, ok := PendingRewards("1000000000000000000000", "5", "1000000000000000000000", "2000000000000000000000000000")
	if !ok || p.String() != "1000000000000000000005" {
		t.Fatalf("pending %v %v", p, ok)
	}
	// Debt above accrual clamps to pendingBase.
	p, ok = PendingRewards("1", "7", "100", "1")
	if !ok || p.String() != "7" {
		t.Fatalf("clamp %v", p)
	}
	if p, ok := PendingRewards("", "", "", ""); !ok || p.Sign() != 0 {
		t.Fatalf("empty %v", p)
	}
	if _, ok := PendingRewards("x", "0", "0", "0"); ok {
		t.Fatal("bad input must fail")
	}
	if _, ok := PendingRewards("-1", "0", "0", "0"); ok {
		t.Fatal("negative must fail")
	}
}

func TestParsePaperAndAccountStakingFields(t *testing.T) {
	if n, err := ParsePaper("1000.5"); err != nil || n.String() != "1000500000000000000000" {
		t.Fatalf("parse %v %v", n, err)
	}
	if _, err := ParsePaper("1e3"); err == nil {
		t.Fatal("scientific must fail")
	}
	a, err := ParseAccount([]byte(`{"address":"0x01","paper":{"balance":"3","staked":"2","pendingBase":"9","rewardDebt":"4"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.PaperPendingBase != "9" || a.PaperRewardDebt != "4" || a.PaperStaked != "2" {
		t.Fatalf("%+v", a)
	}
	if NonceKey(PaperStaking, NonceRoutePaper, goldenUser) != "papertrade.clientNonce.v1:999:"+PaperStaking+":paper:"+goldenUser {
		t.Fatal("paper nonce key")
	}
}
