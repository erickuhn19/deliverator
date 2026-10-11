package cmd

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/papertrade"
)

// ptStakeUse runs staking commands offline with a PAPER-holding wallet frame.
// The Hardhat key is both agent and master; wallet.master_address matches it.
func ptStakeUse(t *testing.T, paper string) *ptCapture {
	t.Helper()
	cap := &ptCapture{next: ptOKReply}
	ptUse(t, cap)
	old, oldYes, oldAmt := ptPaperJSON, flagYes, ptAmount
	ptPaperJSON, flagYes, ptAmount = paper, false, ""
	t.Cleanup(func() { ptPaperJSON, flagYes, ptAmount = old, oldYes, oldAmt })
	Cfg.Wallet.MasterAddress = ptHardhatAddr
	return cap
}

const ptPaper2000 = `{"balance":"2000000000000000000000","staked":"1500000000000000000000","pendingBase":"3000000000000000000","rewardDebt":"0"}`

func TestPapertradeStakeFlow(t *testing.T) {
	cap := ptStakeUse(t, ptPaper2000)

	// Missing and bad amounts.
	for _, a := range []string{"", "0", "-1", "1e3"} {
		ptAmount = a
		if env, err := ptRun(t, ptStakeCmd); rdExitCode(t, err) == output.ExitOK || env.Err.Code != "amount" {
			t.Fatalf("amount %q accepted: %v", a, err)
		}
	}
	// Over the free balance.
	ptAmount = "2000.1"
	if env, err := ptRun(t, ptStakeCmd); rdExitCode(t, err) == output.ExitOK || env.Err.Code != "amount" {
		t.Fatalf("over balance %v", err)
	}
	// Dry run never reads the master key and never posts.
	ptAmount = "1000"
	flagDryRun = true
	env, err := ptRun(t, ptStakeCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"dry_run":true`) || cap.postCount() != 0 {
		t.Fatalf("dry run %v %s", err, env.Data)
	}
	flagDryRun = false
	// Live without --yes refuses before signing.
	if env, err := ptRun(t, ptStakeCmd); rdExitCode(t, err) == output.ExitOK || env.Err.Code != "confirm" || cap.postCount() != 0 {
		t.Fatalf("no --yes %v", err)
	}
	flagYes = true
	// No master key.
	if env, err := ptRun(t, ptStakeCmd); rdExitCode(t, err) == output.ExitOK || env.Err.Code != "master_key" {
		t.Fatalf("no master key %v", err)
	}
	t.Setenv("DELIVERATOR_MASTER_KEY", "0x"+ptTestKey)
	env, err = ptRun(t, ptStakeCmd)
	if rdExitCode(t, err) != output.ExitOK || cap.postCount() != 1 {
		t.Fatalf("stake %v %+v", err, env.Err)
	}
	var body struct {
		Action string            `json:"action"`
		Signed map[string]string `json:"signed"`
	}
	if err := json.Unmarshal(cap.post(0), &body); err != nil {
		t.Fatal(err)
	}
	if body.Action != "paperStake" || body.Signed["amount"] != "1000000000000000000000" ||
		body.Signed["user"] != ptHardhatAddr || body.Signed["v"] == "" {
		t.Fatalf("posted %s", cap.post(0))
	}

	// Unstake: capped at staked PAPER, then posts paperUnstake.
	ptAmount = "1500.000000000000000001"
	if env, err := ptRun(t, ptUnstakeCmd); rdExitCode(t, err) == output.ExitOK || env.Err.Code != "amount" {
		t.Fatalf("over staked %v", err)
	}
	ptAmount = "1500"
	if _, err := ptRun(t, ptUnstakeCmd); rdExitCode(t, err) != output.ExitOK || cap.postCount() != 2 ||
		!strings.Contains(string(cap.post(1)), `"action":"paperUnstake"`) {
		t.Fatalf("unstake %v %s", err, cap.post(1))
	}

	// Claim: pendingBase is 3 USDC, so it posts paperClaim with no amount.
	if _, err := ptRun(t, ptClaimCmd); rdExitCode(t, err) != output.ExitOK || cap.postCount() != 3 ||
		!strings.Contains(string(cap.post(2)), `"action":"paperClaim"`) || strings.Contains(string(cap.post(2)), "amount") {
		t.Fatalf("claim %v", err)
	}
}

func TestPapertradeStakeGuards(t *testing.T) {
	cap := ptStakeUse(t, `{"balance":"5","staked":"0","pendingBase":"0","rewardDebt":"0"}`)
	flagYes = true
	t.Setenv("DELIVERATOR_MASTER_KEY", "0x"+ptTestKey)
	// Nothing to claim.
	if env, err := ptRun(t, ptClaimCmd); rdExitCode(t, err) == output.ExitOK || env.Err.Code != "nothing_to_claim" {
		t.Fatalf("claim nothing %v", err)
	}
	// Master key for a different wallet than --address.
	ptAmount = "0.000000000000000001"
	ptAddress = "0x0000000000000000000000000000000000000002"
	if _, err := ptRun(t, ptStakeCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("--address mismatch must fail")
	}
	ptAddress = ""
	// Non-mainnet refuses.
	Cfg.Network = "testnet"
	if _, err := ptRun(t, ptStakeCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("testnet must refuse")
	}
	if cap.postCount() != 0 {
		t.Fatal("guards must not post")
	}
}

func TestPapertradeStakeMinimumAndAccountView(t *testing.T) {
	ptStakeUse(t, ptPaper2000)
	m := paperTradingWithIntake(`["1","1000000000000000000000","0",["paperClaim"]]`)
	mk, err := parseTestMarkets(m)
	if err != nil {
		t.Fatal(err)
	}
	acct := ptTestAccount(t)
	s := paperStakingSummary(acct, mk)
	if s.MinimumStake != "1000" || s.PendingRewardsUSDC != "3" || s.PaperStaked != "1500" {
		t.Fatalf("summary %+v", s)
	}
	small, _ := parsePaperForTest("999")
	if err := paperStakingCheck("paperStake", small, acct, mk, s); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("minimum %v", err)
	}
	if mk.ActionEnabled("paperClaim") {
		t.Fatal("disabled claim must read false")
	}

	// account includes the staking block.
	env, err := ptRun(t, ptAccountCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"staking"`) ||
		!strings.Contains(string(env.Data), `"pending_rewards_usdc":"3"`) {
		t.Fatalf("account %v %s", err, env.Data)
	}
}

func paperTradingWithIntake(intake string) string {
	return strings.Replace(ptTradingBody, `"intake": ["1","0","0",[]]`, `"intake": `+intake, 1)
}

func parseTestMarkets(body string) (papertrade.Markets, error) {
	return papertrade.ParseMarkets([]byte(body))
}

func parsePaperForTest(s string) (*big.Int, error) { return papertrade.ParsePaper(s) }

func ptTestAccount(t *testing.T) papertrade.Account {
	t.Helper()
	a, err := papertrade.ParseAccount([]byte(ptCompact(ptAccountBody())))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
