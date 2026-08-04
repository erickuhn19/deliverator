package core

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Tests for #39. An all-time-anchored drawdown gate permanently re-litigates a
// realized loss: a live account sat at 98.7% utilization with $1.52 of losable
// equity, and the only escape was setting the cap to 100 — i.e. disabling the
// ruin backstop, which is strictly worse than a well-anchored one.

// THE FAILURE THIS FIXES. Peak 168, equity 52: with an all-time anchor that is a
// 69% drawdown against a 70% cap — a standing halt. A 7-day trailing window lets
// the old peak age out and restores a real, non-zero floor.
func TestTrailingWindowReleasesAnAgedOutPeak(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	day := func(d int) string { return now.AddDate(0, 0, d).Format("2006-01-02") }

	st := riskState{
		PeakEquity: 168,
		DailyHighs: []dayHigh{
			{Day: day(-30), High: 168}, // the old peak, well outside a 7-day window
			{Day: day(-3), High: 60},
			{Day: day(0), High: 52},
		},
	}

	if got := effectivePeak(st, 0, now); got != 168 {
		t.Errorf("all-time peak = %v, want 168 (the default must be unchanged)", got)
	}
	if got := effectivePeak(st, 7, now); got != 60 {
		t.Errorf("7-day trailing peak = %v, want 60 — the 30-day-old peak must age out", got)
	}

	// The live numbers from the issue: peak 168, equity 52, cap 70%. That is a
	// 69.0% drawdown — 98.6% of the cap, i.e. ~$1.52 of losable equity left. Not
	// yet breached, but a standing halt in practice, and the next small loss trips
	// it. This is the state whose only escape was setting the cap to 100.
	const cap = 70.0
	ddAllTime := (168 - 52.0) / 168 * 100
	utilAllTime := ddAllTime / cap * 100
	if utilAllTime < 98 {
		t.Fatalf("precondition: all-time utilization %.1f%% should be the near-halt from the issue", utilAllTime)
	}
	losable := 52 - 168*(1-cap/100)
	if losable > 2 {
		t.Fatalf("precondition: only ~$1.52 should remain losable, got $%.2f", losable)
	}

	// With a 7-day window the peak is 60, so the same equity is a 13.3% drawdown —
	// 19% of the cap. A real floor is restored WITHOUT disabling the backstop.
	ddTrailing := (60 - 52.0) / 60 * 100
	if utilTrailing := ddTrailing / cap * 100; utilTrailing > 25 {
		t.Errorf("trailing utilization %.1f%% is still near a halt; the window bought nothing", utilTrailing)
	}
}

// A window long enough to still contain the peak must NOT release it — the gate
// has to keep biting when the loss really is recent.
func TestTrailingWindowStillBitesInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	day := func(d int) string { return now.AddDate(0, 0, d).Format("2006-01-02") }
	st := riskState{
		PeakEquity: 168,
		DailyHighs: []dayHigh{{Day: day(-2), High: 168}, {Day: day(0), High: 52}},
	}
	if got := effectivePeak(st, 7, now); got != 168 {
		t.Errorf("peak inside the window = %v, want 168 — a recent loss must still gate", got)
	}
}

// FAIL SAFE, NOT FAIL OPEN. If the window yields nothing (empty series, a clock
// moved backwards), fall back to the all-time peak. Returning 0 would make the
// drawdown compute as 0 and silently disable the gate.
func TestEffectivePeakNeverFallsOpen(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		st   riskState
	}{
		{"empty series", riskState{PeakEquity: 168}},
		{"all samples in the future", riskState{PeakEquity: 168, DailyHighs: []dayHigh{{Day: "2099-01-01", High: 0}}}},
		{"zero highs", riskState{PeakEquity: 168, DailyHighs: []dayHigh{{Day: "2026-08-04", High: 0}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectivePeak(tc.st, 7, now); got != 168 {
				t.Errorf("effectivePeak = %v, want the all-time 168 — a 0 peak silently disables the gate", got)
			}
		})
	}
}

// The rolling series must stay bounded: it is rewritten on every gate check.
func TestDailyHighsStayBounded(t *testing.T) {
	var highs []dayHigh
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxDailyHighs+50; i++ {
		highs = recordDailyHigh(highs, base.AddDate(0, 0, i).Format("2006-01-02"), float64(i))
	}
	if len(highs) != maxDailyHighs {
		t.Errorf("series length = %d, want it capped at %d", len(highs), maxDailyHighs)
	}
	if highs[len(highs)-1].High != float64(maxDailyHighs+49) {
		t.Errorf("the newest sample must survive truncation, got %v", highs[len(highs)-1])
	}
}

// Same day observed repeatedly keeps the HIGH, not the last value.
func TestDailyHighKeepsTheDaysMaximum(t *testing.T) {
	var highs []dayHigh
	highs = recordDailyHigh(highs, "2026-08-04", 100)
	highs = recordDailyHigh(highs, "2026-08-04", 140)
	highs = recordDailyHigh(highs, "2026-08-04", 90)
	if len(highs) != 1 || highs[0].High != 140 {
		t.Errorf("got %+v, want a single 2026-08-04 entry at 140", highs)
	}
}

// A reset re-bases the anchor, PRESERVES what it superseded, and counts itself.
func TestResetPeakAnchorRebasesAndKeepsHistory(t *testing.T) {
	testHome(t)
	if _, _, _, _, err := observeEquity("testnet", testMaster, 168, 0); err != nil {
		t.Fatal(err)
	}
	if _, dd, _, _, _ := ReadRiskState("testnet", testMaster, 52, 0); dd < 68 || dd > 70 {
		t.Fatalf("precondition: drawdown from the 168 peak should be ~69%%, got %.1f", dd)
	}

	res, err := ResetPeakAnchor("testnet", testMaster, 52)
	if err != nil {
		t.Fatal(err)
	}
	if res.PrevPeakEquity != 168 || res.NewPeakEquity != 52 {
		t.Errorf("reset = %+v, want prev 168 -> new 52", res)
	}
	if res.ResetCount != 1 {
		t.Errorf("reset count = %d, want 1", res.ResetCount)
	}

	st, dd, _, _, found := ReadRiskState("testnet", testMaster, 52, 0)
	if !found {
		t.Fatal("state should still be present after a reset")
	}
	if dd != 0 {
		t.Errorf("drawdown after re-anchoring at current equity = %.2f, want 0", dd)
	}
	if st.PrevPeakEquity != 168 {
		t.Errorf("the superseded peak must be PRESERVED, got %v", st.PrevPeakEquity)
	}
	if st.PeakResetAtMs == 0 {
		t.Error("the reset time must be recorded")
	}
}

// A reset must not resurrect the acknowledged peak through the trailing window.
func TestResetAlsoClearsTheTrailingHistory(t *testing.T) {
	testHome(t)
	if _, _, _, _, err := observeEquity("testnet", testMaster, 168, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := ResetPeakAnchor("testnet", testMaster, 52); err != nil {
		t.Fatal(err)
	}
	if _, dd, _, _, _ := ReadRiskState("testnet", testMaster, 52, 7); dd != 0 {
		t.Errorf("drawdown = %.2f under a 7-day window after a reset, want 0 — "+
			"the window must not resurrect the peak the operator just re-based away from", dd)
	}
}

// The DAY anchor is a different horizon and must survive a peak reset: clearing
// it would quietly hand back the day's loss budget too.
func TestResetDoesNotClearTheDailyAnchor(t *testing.T) {
	testHome(t)
	if _, _, _, _, err := observeEquity("testnet", testMaster, 100, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ResetPeakAnchor("testnet", testMaster, 80); err != nil {
		t.Fatal(err)
	}
	st, _, dlUSD, _, _ := ReadRiskState("testnet", testMaster, 80, 0)
	if st.DayAnchorEquity != 100 {
		t.Errorf("day anchor = %v, want it preserved at 100", st.DayAnchorEquity)
	}
	if dlUSD != 20 {
		t.Errorf("daily loss = %v, want 20 — the day's budget must not be handed back", dlUSD)
	}
}

// Nonsense equity must be refused rather than persisted as a floor.
func TestResetPeakAnchorRejectsBadEquity(t *testing.T) {
	testHome(t)
	for _, eq := range []float64{0, -5} {
		if _, err := ResetPeakAnchor("testnet", testMaster, eq); err == nil {
			t.Errorf("equity %v should be refused as an anchor", eq)
		}
	}
}

// SOURCE-LEVEL PIN, sibling to TestObserveEquityHasExactlyOneCaller. Re-basing
// the anchor reduces protection, so it must stay OPERATOR-ONLY: reachable from
// the risk command, never from the order path. An agent able to move its own
// floor to unblock itself would not be gated at all.
func TestResetPeakAnchorIsOperatorOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`ResetPeakAnchor\(`)
	callers := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func ResetPeakAnchor(") {
				continue
			}
			if n := len(re.FindAllString(line, -1)); n > 0 {
				callers[name] += n
			}
		}
	}
	total := 0
	for _, n := range callers {
		total += n
	}
	if total != 1 || callers["risk_status.go"] != 1 {
		t.Fatalf("ResetPeakAnchor must have exactly ONE caller, ResetDrawdownAnchor in risk_status.go; found %v — "+
			"a new caller must be proven to be operator-initiated and off the order path", callers)
	}

	// And the order path must not reach it: engine_writes.go signs orders.
	if b, err := os.ReadFile("engine_writes.go"); err == nil {
		if re.Match(b) || strings.Contains(string(b), "ResetDrawdownAnchor") {
			t.Fatal("the order path must never re-anchor the drawdown gate")
		}
	}
}
