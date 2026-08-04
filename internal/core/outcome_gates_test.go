package core

import (
	"strings"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// expiryIn formats an expiry d from now in the Market.Expiry layout the gate parses.
func expiryIn(d time.Duration) string {
	return time.Now().UTC().Add(d).Format(outcomeExpiryLayout)
}

func blackoutCfg(blackoutMins int) *config.Config {
	c := config.Default()
	c.Risk.OutcomeSettleBlackoutMins = blackoutMins
	return c
}

func TestOutcomeSettleGate(t *testing.T) {
	openFar := Market{Coin: "#6410", IsOutcome: true, ResolutionStatus: "open", Expiry: expiryIn(3 * time.Hour)}
	openNear := Market{Coin: "#6410", IsOutcome: true, ResolutionStatus: "open", Expiry: expiryIn(10 * time.Minute)}
	settled := Market{Coin: "#6410", IsOutcome: true, ResolutionStatus: "settled", Expiry: expiryIn(3 * time.Hour)}
	badExpiry := Market{Coin: "#6410", IsOutcome: true, ResolutionStatus: "open", Expiry: "garbage"}
	// Event / multi-outcome markets carry NO expiry metadata (only priceBinary does).
	noExpiry := Market{Coin: "#7000", IsOutcome: true, ResolutionStatus: "open", Expiry: ""}
	perp := Market{Coin: "BTC", IsOutcome: false}

	buy := OrderReq{Side: Buy}
	sell := OrderReq{Side: Sell} // plain sell (not flagged) — de-risking on long-only outcome spot
	reduce := OrderReq{Side: Sell, ReduceOnly: true}
	closing := OrderReq{Side: Sell, Closing: true}

	cases := []struct {
		name    string
		cfg     *config.Config
		mk      Market
		req     OrderReq
		wantErr string // "" = allow; else a substring of the error code/message
	}{
		{"perp always ok", blackoutCfg(15), perp, buy, ""},
		{"settled rejects even with no blackout", blackoutCfg(0), settled, buy, "settled"},
		{"settled reduce-only allowed", blackoutCfg(0), settled, reduce, ""},
		{"settled closing allowed", blackoutCfg(0), settled, closing, ""},
		{"open no blackout configured ok", blackoutCfg(0), openNear, buy, ""},
		{"open far outside blackout ok", blackoutCfg(15), openFar, buy, ""},
		{"open near inside blackout rejects", blackoutCfg(15), openNear, buy, "blackout"},
		{"blackout reduce-only allowed", blackoutCfg(15), openNear, reduce, ""},
		{"unparseable expiry with blackout fails closed", blackoutCfg(15), badExpiry, buy, "expiry"},
		{"unparseable expiry no blackout ok", blackoutCfg(0), badExpiry, buy, ""},
		// Regression: an absent expiry is structural (event/multi-outcome market), not stale
		// metadata — it must NOT fail closed and ban the whole class regardless of the knob.
		{"empty expiry event market not blocked by blackout", blackoutCfg(15), noExpiry, buy, ""},
		// Regression: a plain Sell is always de-risking on long-only outcome spot, so it may
		// unwind during the blackout (only Buys open/add exposure).
		{"blackout plain sell allowed (long-only de-risk)", blackoutCfg(15), openNear, sell, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := &Client{cfg: c.cfg}
			err := cl.outcomeSettleGate(c.mk, c.req)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want allow, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestOutcomeQuestionKey(t *testing.T) {
	if _, _, ok := outcomeQuestionKey(Market{IsOutcome: false}); ok {
		t.Fatal("non-outcome should return ok=false")
	}
	if k, name, ok := outcomeQuestionKey(Market{IsOutcome: true, Question: 5, QuestionName: "WC"}); !ok || k != "q:5" || name != "WC" {
		t.Fatalf("multi-outcome question key wrong: %q %q %v", k, name, ok)
	}
	// Standalone binary (Question==0) keys on the Outcome pair, not the question.
	if k, _, ok := outcomeQuestionKey(Market{IsOutcome: true, Question: 0, Outcome: 641}); !ok || k != "o:641" {
		t.Fatalf("standalone binary key wrong: %q %v", k, ok)
	}
}

// outcomeGateMeta builds a MetaStore with a standalone BTC binary (outcome 641), a
// second standalone binary (642), and a two-outcome question 5 (outcomes 700, 701).
func outcomeGateMeta(t *testing.T) *MetaStore {
	t.Helper()
	yn := []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}
	desc := "class:priceBinary|underlying:BTC|expiry:20260702-1436|targetPrice:76000"
	om := &hl.OutcomeMeta{
		Outcomes: []hl.OutcomeInfo{
			{Outcome: 641, SideSpecs: yn, Description: desc},
			{Outcome: 642, SideSpecs: yn, Description: desc},
			{Outcome: 700, SideSpecs: yn, Description: "class:priceBinary|underlying:ETH|expiry:20260702-1436|targetPrice:4000"},
			{Outcome: 701, SideSpecs: yn, Description: "class:priceBinary|underlying:ETH|expiry:20260702-1436|targetPrice:5000"},
		},
		Questions: []hl.OutcomeQuestion{
			{Question: 5, Name: "World Cup", NamedOutcomes: []int{700, 701}},
		},
	}
	ms := NewMetaStore("testnet", &hl.Meta{}, nil, time.Now())
	ms.AddOutcomes(om)
	return ms
}

func TestHeaviestOutcomeQuestion(t *testing.T) {
	c := &Client{cfg: config.Default(), meta: outcomeGateMeta(t)}

	// Both legs of question 5 sum into one bucket.
	label, n := c.heaviestOutcomeQuestion(map[string]float64{"#7000": 60, "#7010": 60}, nil)
	if n != 120 || label != "World Cup" {
		t.Fatalf("question 5 bucket: got (%q, %.0f), want (World Cup, 120)", label, n)
	}

	// A standalone binary's Yes+No count together (Outcome 641 pair).
	_, n = c.heaviestOutcomeQuestion(map[string]float64{"#6410": 40, "#6411": 40}, nil)
	if n != 80 {
		t.Fatalf("standalone binary pair: got %.0f, want 80", n)
	}

	// Two UNRELATED standalone binaries must NOT merge (641 vs 642).
	_, n = c.heaviestOutcomeQuestion(map[string]float64{"#6410": 40, "#6420": 40}, nil)
	if n != 40 {
		t.Fatalf("unrelated binaries must not merge: got %.0f, want 40 (heaviest single)", n)
	}

	// Multi-outcome question outweighs a standalone binary in the same book.
	label, n = c.heaviestOutcomeQuestion(map[string]float64{"#7000": 60, "#7010": 60, "#6410": 40}, nil)
	if n != 120 || label != "World Cup" {
		t.Fatalf("heaviest should be question 5: got (%q, %.0f)", label, n)
	}

	// Resting-only coins (no position) still deploy capital toward the question.
	_, n = c.heaviestOutcomeQuestion(nil, map[string]pendingAdd{"#7000": {buy: 50}})
	if n != 50 {
		t.Fatalf("pending-only question notional: got %.0f, want 50", n)
	}

	// Worst-case per coin (position + resting buys) matches the per-coin gate basis.
	_, n = c.heaviestOutcomeQuestion(map[string]float64{"#7000": 30}, map[string]pendingAdd{"#7000": {buy: 20}})
	if n != 50 {
		t.Fatalf("worst-case fold: got %.0f, want 50 (30 pos + 20 resting buy)", n)
	}
}

// Regression (#2): the per-question outcome cap governs only outcome coins, so a
// non-outcome perp/spot order must NOT activate it — otherwise it forces a fail-closed
// account-state read on that order's behalf, and a transient read failure would reject an
// unrelated BTC order. It must also stay OUT of the coin-agnostic portfolioGuardsActive.
func TestOutcomeQuestionCapActive(t *testing.T) {
	capOnly := &Client{cfg: &config.Config{Risk: config.Risk{MaxOutcomeQuestionNotionalUSD: 50}}}

	if capOnly.portfolioGuardsActive() {
		t.Fatal("outcome cap alone must NOT flag the coin-agnostic guards active (would force a read on non-outcome orders)")
	}
	if capOnly.outcomeQuestionCapActive([]exposureDelta{{coin: "BTC", signedNotional: 100}}) {
		t.Error("a non-outcome (BTC) delta must not activate the outcome-question cap")
	}
	if !capOnly.outcomeQuestionCapActive([]exposureDelta{{coin: "#7290", signedNotional: 10}}) {
		t.Error("an outcome (#7290) delta must activate the cap")
	}
	if !capOnly.outcomeQuestionCapActive([]exposureDelta{{coin: "BTC"}, {coin: "#7290"}}) {
		t.Error("a mixed batch touching an outcome coin must activate the cap")
	}

	off := &Client{cfg: &config.Config{Risk: config.Risk{}}}
	if off.outcomeQuestionCapActive([]exposureDelta{{coin: "#7290"}}) {
		t.Error("cap unset (0) must never activate")
	}
}
