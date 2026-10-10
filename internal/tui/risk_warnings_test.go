package tui

import (
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/core"
)

// The console is the surface an operator actually watches. A warning that only
// appears in `risk --json` is a warning nobody sees — which is exactly how a
// disabled ruin backstop stayed invisible while the account drew down 32.9%.

func TestRiskPanelRendersEnvelopeWarnings(t *testing.T) {
	m := Model{ready: true, risk: &core.RiskView{
		Caps: []core.RiskCap{{Key: "risk.max_drawdown_pct", Label: "Max drawdown", Unit: "pct", Value: "100", Active: true}},
		Warnings: []string{
			"risk.max_drawdown_pct is 100 — THE RUIN BACKSTOP IS EFFECTIVELY OFF. " +
				"Prefer a real cap plus risk.drawdown_window_days (a trailing peak) or a reset anchor",
		},
	}}
	got := m.renderRisk()
	if !strings.Contains(got, "RUIN BACKSTOP IS EFFECTIVELY OFF") {
		t.Errorf("the envelope warning is not rendered in the console:\n%s", got)
	}
	if !strings.Contains(got, "⚠") {
		t.Errorf("the warning should be visually marked, got:\n%s", got)
	}
}

func TestRiskPanelWithoutWarningsIsUnchanged(t *testing.T) {
	m := Model{ready: true, risk: &core.RiskView{
		Caps: []core.RiskCap{{Key: "risk.max_drawdown_pct", Label: "Max drawdown", Unit: "pct", Value: "25", Active: true}},
	}}
	if got := m.renderRisk(); strings.Contains(got, "⚠") {
		t.Errorf("no warnings were set but one rendered:\n%s", got)
	}
}

// A CAP AT ITS MAXIMUM IS NOT A CAP. `max_drawdown_pct = 100` rendered as
// "100 pct · cur 32.9% · 33%" with a comfortable green utilization bar — reading
// as a gate with headroom when it can never fire.
func TestPercentCapAt100RendersAsOffNotAsHealthy(t *testing.T) {
	util := 32.9
	m := Model{ready: true, risk: &core.RiskView{Caps: []core.RiskCap{{
		Key: "risk.max_drawdown_pct", Label: "Max drawdown", Unit: "pct",
		Value: "100", Active: true, Current: &util, UtilPct: &util,
	}}}}
	got := m.renderRisk()
	if !strings.Contains(got, "OFF — cannot fire") {
		t.Errorf("a 100%% cap must not render as a healthy gate:\n%s", got)
	}
	if strings.Contains(got, "33%") {
		t.Errorf("the utilization bar is meaningless against a disabled gate but was shown:\n%s", got)
	}
}

// A real cap keeps its utilization bar.
func TestActivePercentCapKeepsItsUtilization(t *testing.T) {
	util := 32.9
	m := Model{ready: true, risk: &core.RiskView{Caps: []core.RiskCap{{
		Key: "risk.max_drawdown_pct", Label: "Max drawdown", Unit: "pct",
		Value: "25", Active: true, Current: &util, UtilPct: &util,
	}}}}
	got := m.renderRisk()
	if strings.Contains(got, "OFF — cannot fire") {
		t.Errorf("an enforceable cap must keep its utilization bar:\n%s", got)
	}
}

// A non-percentage cap at 100 (e.g. 100 USD) is a perfectly normal limit and
// must not be mislabelled as disabled.
func TestNonPercentCapAt100IsNotTreatedAsDisabled(t *testing.T) {
	v := 10.0
	m := Model{ready: true, risk: &core.RiskView{Caps: []core.RiskCap{{
		Key: "risk.max_order_notional_usd", Label: "Max order notional", Unit: "usd",
		Value: "100", Active: true, Current: &v, UtilPct: &v,
	}}}}
	if got := m.renderRisk(); strings.Contains(got, "OFF — cannot fire") {
		t.Errorf("a 100 USD cap is a real limit, not a disabled one:\n%s", got)
	}
}

func TestWrapToIndentsContinuationLines(t *testing.T) {
	got := wrapTo("aaa bbb ccc ddd eee", 11, ">>")
	if !strings.Contains(got, "\n>>") {
		t.Errorf("continuation lines should be indented, got %q", got)
	}
	for _, ln := range strings.Split(got, "\n") {
		if len(ln) > 13 {
			t.Errorf("line exceeds width+indent: %q", ln)
		}
	}
	if wrapTo("", 10, "  ") != "" {
		t.Error("empty input should wrap to empty")
	}
}
