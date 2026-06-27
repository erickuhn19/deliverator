package core

import (
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

const copyLeader = "0xabcdef0123456789abcdef0123456789abcdef01"

// copyResp serves leader vs self clearinghouse states by branching on the request's
// "user" field (the harness keys other reads by type).
func copyResp(leaderState, selfState, mids string) respFn {
	return func(_, typ string, body map[string]any) (int, string) {
		user, _ := body["user"].(string)
		switch typ {
		case "clearinghouseState":
			if strings.EqualFold(user, copyLeader) {
				return 200, leaderState
			}
			return 200, selfState
		case "spotClearinghouseState":
			return 200, `{"balances":[]}`
		case "frontendOpenOrders":
			return 200, `[]`
		case "allMids":
			return 200, mids
		}
		return 200, `{}`
	}
}

// THE regression: a coin you hold but the leader does NOT (and not in --mirrored)
// must yield no leg at all — never a spurious close of your own position.
func TestCopyScalesAndIgnoresOwnPositions(t *testing.T) {
	leaderState := clearingWith("100000", posWith("BTC", "1.0", "50000")) // leader: 1 BTC long
	selfState := clearingWith("10000", posWith("BTC", "0.05", "2500"), posWith("ETH", "1.0", "2000"))
	c, ctx := newTestClient(t, config.Default(), Options{}, copyResp(leaderState, selfState, `{"BTC":"50000","ETH":"2000"}`))

	d, err := c.Copy(ctx, CopyParams{Leader: copyLeader, ScaleMode: "equity", Scale: 1})
	if err != nil {
		t.Fatal(err)
	}
	// factor = your 10000 / leader 100000 = 0.1 → BTC target 0.1; you hold 0.05 → increase 0.05
	if len(d.Diff) != 1 || d.Diff[0].Coin != "BTC" || d.Diff[0].Class != "increase" {
		t.Fatalf("want one BTC increase leg, got %+v", d.Diff)
	}
	for _, l := range d.Diff {
		if l.Coin == "ETH" {
			t.Fatalf("ETH (your own, leader has none) must be IGNORED, got leg %+v", l)
		}
	}
	for _, s := range d.Skipped {
		if s.Coin == "ETH" {
			t.Fatalf("ETH must be ignored, not even skip-listed: %+v", s)
		}
	}
	if len(d.MirroredNow) != 1 || d.MirroredNow[0] != "BTC" {
		t.Fatalf("mirrored_now should be the leader's current coins [BTC], got %v", d.MirroredNow)
	}
}

// With --mirrored, a coin the leader has since exited closes; without it, ignored.
func TestCopyMirrorsLeaderExit(t *testing.T) {
	leaderState := clearingWith("100000") // leader now FLAT
	selfState := clearingWith("10000", posWith("BTC", "0.05", "2500"))
	c, ctx := newTestClient(t, config.Default(), Options{}, copyResp(leaderState, selfState, `{"BTC":"50000"}`))

	// no --mirrored: BTC out of scope (leader flat, your BTC is "your own") → no leg
	d, err := c.Copy(ctx, CopyParams{Leader: copyLeader, ScaleMode: "fixed", Scale: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Diff) != 0 {
		t.Fatalf("without --mirrored, your BTC must be ignored, got %+v", d.Diff)
	}
	// with --mirrored BTC: leader exited → close your BTC
	d2, err := c.Copy(ctx, CopyParams{Leader: copyLeader, ScaleMode: "fixed", Scale: 1, Mirrored: []string{"BTC"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Diff) != 1 || d2.Diff[0].Coin != "BTC" || d2.Diff[0].Class != "close" {
		t.Fatalf("with --mirrored, the leader's exit must close BTC, got %+v", d2.Diff)
	}
}

func TestClassifyLeg(t *testing.T) {
	c := newCfgClient(t, config.Default())
	mk, _ := c.meta.Lookup("BTC")
	cases := []struct {
		name               string
		your, target       float64
		wantClass, wantAct string
	}{
		{"open", 0, 0.1, "open", "buy"},
		{"open-short", 0, -0.1, "open", "sell"},
		{"close", 0.1, 0, "close", "sell"},
		{"increase", 0.1, 0.2, "increase", "buy"},
		{"decrease", 0.2, 0.1, "decrease", "sell"},
		{"flip", 0.1, -0.1, "flip", "sell"},
	}
	for _, tc := range cases {
		leg, _, ok := classifyLeg("BTC", tc.your, tc.target, 50000, mk, 10, CopyParams{}, c)
		if !ok || leg.Class != tc.wantClass || leg.Action != tc.wantAct {
			t.Fatalf("%s: want %s/%s, got %s/%s (ok=%v)", tc.name, tc.wantClass, tc.wantAct, leg.Class, leg.Action, ok)
		}
	}
	// at target → no leg
	if _, _, ok := classifyLeg("BTC", 0.1, 0.1, 50000, mk, 10, CopyParams{}, c); ok {
		t.Fatal("equal current/target must produce no leg")
	}
}

func TestCopyRejectsBadLeaderAndSelf(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, copyResp(clearingWith("1"), clearingWith("1"), `{}`))
	if _, err := c.Copy(ctx, CopyParams{Leader: "not-an-address"}); err == nil {
		t.Fatal("bad leader address must error")
	}
	if _, err := c.Copy(ctx, CopyParams{Leader: testMaster}); err == nil {
		t.Fatal("self-copy must be rejected")
	}
}
