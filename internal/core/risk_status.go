package core

import (
	"context"
	"strconv"
)

// RiskCap is one configured risk-envelope limit plus, where measurable, the live
// current value and how much of the cap is in use. It is the operator-facing view
// of the limits the agent is bound by (and may change only loudly, never silently).
type RiskCap struct {
	Key     string   `json:"key"`                // dotted config key, e.g. "risk.max_account_leverage"
	Label   string   `json:"label"`              // human label
	Unit    string   `json:"unit"`               // usd | x | pct | count | bps | secs
	Value   string   `json:"value"`              // configured cap, stringified ("0" = off)
	Active  bool     `json:"active"`             // true when the cap is enforced (value > 0)
	Current *float64 `json:"current,omitempty"`  // live measured value, when the cap has one
	UtilPct *float64 `json:"util_pct,omitempty"` // Current/Value*100, when both known and active
}

// RiskView is the operator's risk-envelope snapshot: live equity, every cap with
// utilization, and the persisted drawdown/daily-loss trajectory. READ-ONLY — it
// never moves the high-water/anchor the agent's gates depend on. Powers both
// `deliverator risk` (machine-readable, agent-readable) and the console TUI.
type RiskView struct {
	Equity         string    `json:"equity"`
	Caps           []RiskCap `json:"caps"`
	PeakEquity     string    `json:"peak_equity"`
	DrawdownPct    float64   `json:"drawdown_pct"`
	DayAnchor      string    `json:"day_anchor_equity"`
	DailyLossUSD   float64   `json:"daily_loss_usd"`
	DailyLossPct   float64   `json:"daily_loss_pct"`
	RiskStateFound bool      `json:"risk_state_found"`
	Halted         bool      `json:"halted"`
}

// RiskStatus reports the configured risk envelope + live utilization. READ-ONLY:
// it reads the persisted drawdown/daily-loss state via ReadRiskState and never
// mutates it (a passive monitor must not move the agent's gate anchors). The
// utilization is computed with the same computePortfolioMetrics the gates use, so
// the view can never disagree with what would actually be enforced.
func (c *Client) RiskStatus(ctx context.Context) (*RiskView, error) {
	pf, err := c.Portfolio(ctx)
	if err != nil {
		return nil, err
	}
	equity := equityOf(pf.AccountValue, pf.AvailableCollateral)
	perCoin := map[string]float64{}
	for _, p := range pf.Positions {
		n := parseFloatSafe(p.PositionValue)
		if p.Side == "short" {
			n = -n
		}
		perCoin[p.Coin] += n
	}
	m := computePortfolioMetrics(perCoin)
	st, ddPct, dlUSD, dlPct, found := ReadRiskState(equity)
	r := c.cfg.Risk

	f := func(v float64) *float64 { return &v }
	mk := func(key, label, unit, value string, active bool, current *float64, capVal float64) RiskCap {
		rc := RiskCap{Key: key, Label: label, Unit: unit, Value: value, Active: active, Current: current}
		if active && current != nil && capVal > 0 {
			u := *current / capVal * 100
			rc.UtilPct = &u
		}
		return rc
	}
	lev, conc := 0.0, 0.0
	if equity > 0 {
		lev = m.gross / equity
		conc = m.maxCoinNotional / equity * 100
	}
	// Utilization-bearing caps first (most operationally relevant), then static caps.
	caps := []RiskCap{
		mk("risk.max_account_leverage", "Account leverage", "x", f2s(r.MaxAccountLeverage), r.MaxAccountLeverage > 0, f(lev), r.MaxAccountLeverage),
		mk("risk.max_net_exposure_usd", "Net exposure", "usd", f2s(r.MaxNetExposureUSD), r.MaxNetExposureUSD > 0, f(absF(m.net)), r.MaxNetExposureUSD),
		mk("risk.max_concentration_pct_per_coin", "Per-coin concentration", "pct", f2s(r.MaxConcentrationPctPerCoin), r.MaxConcentrationPctPerCoin > 0, f(conc), r.MaxConcentrationPctPerCoin),
		mk("risk.max_open_positions", "Open positions", "count", strconv.Itoa(r.MaxOpenPositions), r.MaxOpenPositions > 0, f(float64(m.openPositions)), float64(r.MaxOpenPositions)),
		mk("risk.max_position_notional_usd", "Max position notional", "usd", f2s(r.MaxPositionNotionalUSD), r.MaxPositionNotionalUSD > 0, f(m.maxCoinNotional), r.MaxPositionNotionalUSD),
		mk("risk.max_drawdown_pct", "Max drawdown", "pct", f2s(r.MaxDrawdownPct), r.MaxDrawdownPct > 0, f(ddPct), r.MaxDrawdownPct),
		mk("risk.max_daily_loss_usd", "Daily loss", "usd", f2s(r.MaxDailyLossUSD), r.MaxDailyLossUSD > 0, f(dlUSD), r.MaxDailyLossUSD),
		mk("risk.max_daily_loss_pct", "Daily loss %", "pct", f2s(r.MaxDailyLossPct), r.MaxDailyLossPct > 0, f(dlPct), r.MaxDailyLossPct),
		{Key: "risk.max_order_notional_usd", Label: "Max order notional", Unit: "usd", Value: f2s(r.MaxOrderNotionalUSD), Active: r.MaxOrderNotionalUSD > 0},
		{Key: "risk.min_order_notional_usd", Label: "Min order notional", Unit: "usd", Value: f2s(r.MinOrderNotionalUSD), Active: r.MinOrderNotionalUSD > 0},
		{Key: "risk.max_leverage", Label: "Max leverage", Unit: "x", Value: strconv.Itoa(r.MaxLeverage), Active: r.MaxLeverage > 0},
		{Key: "risk.dead_man_switch_secs", Label: "Dead-man's switch", Unit: "secs", Value: strconv.Itoa(r.DeadManSwitchSecs), Active: r.DeadManSwitchSecs > 0},
		{Key: "risk.max_priority_bps", Label: "Max priority fee", Unit: "bps", Value: strconv.Itoa(r.MaxPriorityBps), Active: r.MaxPriorityBps > 0},
	}
	return &RiskView{
		Equity:         f2s(equity),
		Caps:           caps,
		PeakEquity:     f2s(st.PeakEquity),
		DrawdownPct:    ddPct,
		DayAnchor:      f2s(st.DayAnchorEquity),
		DailyLossUSD:   dlUSD,
		DailyLossPct:   dlPct,
		RiskStateFound: found,
		Halted:         c.Halted(),
	}, nil
}
