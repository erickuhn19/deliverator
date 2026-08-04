package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
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

// PostureSetting is one operator-owned capability/permission toggle — what the
// agent is ALLOWED to trade, as distinct from the numeric risk caps that bound how
// MUCH. Editable in the console (and via `config set`); not a safety cap, so a
// change fires a plain confirm, not the loud risk-cap warning.
type PostureSetting struct {
	Key   string `json:"key"`   // dotted config key, e.g. "automation.limit_only"
	Label string `json:"label"` // human label
	Type  string `json:"type"`  // "bool" | "list" | "enum"
	Value string `json:"value"` // "true"/"false" for bool; comma-joined for list ("" = unset); the value for enum (e.g. "mainnet")
}

// RiskView is the operator's risk-envelope snapshot: live equity, every cap with
// utilization, and the persisted drawdown/daily-loss trajectory. READ-ONLY — it
// never moves the high-water/anchor the agent's gates depend on. Powers both
// `deliverator risk` (machine-readable, agent-readable) and the console TUI.
type RiskView struct {
	Equity         string           `json:"equity"`
	Caps           []RiskCap        `json:"caps"`
	Posture        []PostureSetting `json:"posture"`
	PeakEquity     string           `json:"peak_equity"`
	DrawdownPct    float64          `json:"drawdown_pct"`
	DayAnchor      string           `json:"day_anchor_equity"`
	DailyLossUSD   float64          `json:"daily_loss_usd"`
	DailyLossPct   float64          `json:"daily_loss_pct"`
	RiskStateFound bool             `json:"risk_state_found"`
	Halted         bool             `json:"halted"`

	// Drawdown-anchor provenance (#39). DrawdownWindowDays is 0 for the all-time
	// high-water mark. The reset fields are present only once the anchor has been
	// re-based, so an operator can see that the current floor is a re-based one,
	// when it happened, and what it superseded — a reset reduces protection and
	// must not be invisible afterwards.
	DrawdownWindowDays int     `json:"drawdown_window_days,omitempty"`
	DrawdownAnchor     string  `json:"drawdown_anchor"` // "all-time peak" | "N-day trailing peak"
	PeakResetCount     int     `json:"peak_reset_count,omitempty"`
	PeakResetAtMs      int64   `json:"peak_reset_at_ms,omitempty"`
	PrevPeakEquity     string  `json:"prev_peak_equity,omitempty"`
	DrawdownUtilPct    float64 `json:"drawdown_util_pct,omitempty"` // dd as a % of the cap
	// Warnings carries operator-facing notes about the envelope itself — notably
	// that an all-time-anchored drawdown gate near its cap is effectively a
	// standing halt whose only escapes are re-anchoring or disabling it.
	Warnings []string `json:"warnings,omitempty"`
	// Degraded/DegradedDexs carry the source portfolio's partial-read markers
	// (NEXT-2 item 1): when set, equity/utilization above were computed from an
	// INCOMPLETE book (the gates themselves refuse to act on it — fail closed).
	// Additive, omitted when the snapshot was complete.
	Degraded     []string `json:"degraded,omitempty"`
	DegradedDexs []string `json:"degraded_dexs,omitempty"`
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
	return c.RiskStatusFromPortfolio(pf), nil
}

// RiskStatusFromPortfolio builds the RiskView from an ALREADY-FETCHED portfolio, so
// a caller that already has the snapshot (e.g. the console refreshing every few
// seconds, which also needs the portfolio for its account panel) gets risk without a
// SECOND Portfolio round-trip — halving the per-IP request weight and the 429s that
// caused. It performs no network I/O (caps re-read from the local config file).
func (c *Client) RiskStatusFromPortfolio(pf *PortfolioView) *RiskView {
	equity := accountEquity(pf)
	perCoin := map[string]float64{}
	for _, p := range pf.Positions {
		n := parseFloatSafe(p.PositionValue)
		if p.Side == "short" {
			n = -n
		}
		perCoin[p.Coin] += n
	}
	// Include resting non-reduce-only orders' worst-case adds, so the view shows
	// exactly the utilization the gates enforce (they count resting orders too).
	m := computePortfolioMetrics(perCoin, pendingAddsFromOrders(pf.OpenOrders))
	// Read caps from disk, not the in-memory snapshot: a long-running console (and
	// `risk` after a `config set`) must reflect edits to config.toml. The client's
	// c.cfg is only the startup load. Falls back to the snapshot if there's no file.
	guards := c.currentGuards()
	r := guards.risk
	network := c.cfg.Network
	outcomes := c.cfg.Outcomes
	limitOnly := guards.automation.LimitOnly
	allowedCoins := guards.automation.AllowedCoins
	perpDexs := c.cfg.PerpDexs
	if p := c.cfg.SourcePath(); p != "" {
		if fresh, ferr := config.Load(p); ferr == nil {
			r = fresh.Risk
			network = fresh.Network
			outcomes = fresh.Outcomes
			limitOnly = fresh.Automation.LimitOnly
			allowedCoins = fresh.Automation.AllowedCoins
			perpDexs = fresh.PerpDexs
		}
	}

	// The drawdown/daily-loss anchors are keyed per network+account (a testnet
	// peak must never shadow a mainnet account, or one account another's). Read
	// AFTER the on-disk caps are resolved, so the window this measures with is the
	// same one the gates will enforce — a monitor and an enforcer disagreeing
	// about the anchor is the shape that hid a two-day outage (#41).
	st, ddPct, dlUSD, dlPct, found := ReadRiskState(c.network, c.queryAddr, equity, r.DrawdownWindowDays)

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
	// Operator-owned trading posture: WHAT the agent may trade (capabilities +
	// permissions), distinct from the caps above that bound HOW MUCH. Editable in the
	// console; a change is a plain confirm, not the loud risk-cap warning.
	posture := []PostureSetting{
		{Key: "network", Label: "Network", Type: "enum", Value: network},
		{Key: "outcomes", Label: "Outcome markets", Type: "bool", Value: strconv.FormatBool(outcomes)},
		{Key: "automation.limit_only", Label: "Limit-only orders", Type: "bool", Value: strconv.FormatBool(limitOnly)},
		{Key: "automation.allowed_coins", Label: "Allowed coins", Type: "list", Value: strings.Join(allowedCoins, ",")},
		{Key: "perp_dexs", Label: "Sub-dexes (HIP-3)", Type: "list", Value: strings.Join(perpDexs, ",")},
	}
	anchor := "all-time peak"
	if r.DrawdownWindowDays > 0 {
		anchor = fmt.Sprintf("%d-day trailing peak", r.DrawdownWindowDays)
	}
	var rvWarnings []string
	var ddUtil float64
	if r.MaxDrawdownPct > 0 {
		ddUtil = ddPct / r.MaxDrawdownPct * 100
		// The same warning the gate raises, so an operator reading `risk` sees the
		// standing-halt risk BEFORE a rejection rather than after.
		if ddUtil > 90 && r.DrawdownWindowDays <= 0 {
			rvWarnings = append(rvWarnings, fmt.Sprintf(
				"drawdown is at %.0f%% of the %.1f%% cap and the anchor is the ALL-TIME peak — this gate is "+
					"effectively a standing halt. Escapes: `deliverator risk reset-anchor --yes` (re-base the peak "+
					"to current equity) or risk.drawdown_window_days (make the peak trailing). Setting the cap to "+
					"100 turns the ruin backstop OFF and is strictly worse than either",
				ddUtil, r.MaxDrawdownPct))
		}
	}
	if r.MaxDrawdownPct >= 100 {
		rvWarnings = append(rvWarnings, "risk.max_drawdown_pct is 100 — THE RUIN BACKSTOP IS EFFECTIVELY OFF. "+
			"Prefer a real cap plus risk.drawdown_window_days (a trailing peak) or a reset anchor")
	}
	return &RiskView{
		Equity:             f2s(equity),
		Caps:               caps,
		Posture:            posture,
		PeakEquity:         f2s(st.PeakEquity),
		DrawdownPct:        ddPct,
		DayAnchor:          f2s(st.DayAnchorEquity),
		DailyLossUSD:       dlUSD,
		DailyLossPct:       dlPct,
		RiskStateFound:     found,
		Halted:             c.Halted(),
		DrawdownWindowDays: r.DrawdownWindowDays,
		DrawdownAnchor:     anchor,
		DrawdownUtilPct:    ddUtil,
		PeakResetCount:     st.PeakResetCount,
		PeakResetAtMs:      st.PeakResetAtMs,
		PrevPeakEquity:     f2sOmitZero(st.PrevPeakEquity),
		Warnings:           rvWarnings,
		// Carry the source portfolio's partial-read markers: equity/utilization
		// above were computed from an incomplete book and must say so (NEXT-2
		// item 1). Note this view is strictly READ-ONLY (ReadRiskState) — a
		// degraded equity here can never move the persisted anchors.
		Degraded:     pf.Degraded,
		DegradedDexs: pf.DegradedDexs,
	}
}

// f2sOmitZero formats a float like f2s but yields "" for zero, so an anchor that
// has never been re-based simply omits the field rather than claiming "0".
func f2sOmitZero(v float64) string {
	if v == 0 {
		return ""
	}
	return f2s(v)
}

// ResetDrawdownAnchor re-bases the drawdown high-water mark to current equity.
//
// OPERATOR ACTION, NOT AN AGENT ONE. Re-basing the anchor reduces protection, so
// it is gated on explicit confirmation, written to the audit trail, and reached
// only from `deliverator risk reset-anchor` — never from the order path. A
// trading loop that could move its own floor to unblock itself would not be a
// risk gate at all.
//
// It reads equity through the normal portfolio path, which REFUSES a degraded
// snapshot: re-anchoring off a partially-read book could set the floor from an
// understated equity and silently deepen the real drawdown allowance.
func (c *Client) ResetDrawdownAnchor(ctx context.Context, confirm bool) (*AnchorReset, error) {
	if !confirm {
		return nil, output.Validation("confirm_required",
			"re-basing the drawdown anchor REDUCES protection: it forgives the drawdown measured so far and "+
				"restarts the gate from today's equity").
			WithHint("re-run with --yes if you are acknowledging a realized loss. Consider risk.drawdown_window_days " +
				"instead for an ongoing policy — a trailing peak forgives history automatically and needs no manual step")
	}
	pf, err := c.Portfolio(ctx)
	if err != nil {
		return nil, err
	}
	// Fail closed on a partial read: an understated equity would set the new floor
	// too low and quietly widen the real loss allowance.
	if len(pf.Degraded) > 0 || len(pf.DegradedDexs) > 0 {
		return nil, output.Network("degraded_equity",
			"account equity was read from an INCOMPLETE book — refusing to re-anchor the drawdown gate from it").
			Retry().WithHint("retry when the venue is answering fully; a floor set from understated equity would widen the real loss allowance")
	}
	equity := accountEquity(pf)
	if equity <= 0 {
		return nil, output.Validation("no_equity",
			"account equity is 0 or unreadable — cannot re-anchor the drawdown gate").
			WithHint("fund the account or set wallet.master_address")
	}
	res, err := ResetPeakAnchor(c.network, c.queryAddr, equity)
	if err != nil {
		return nil, output.Network("risk_state", "cannot re-anchor drawdown state: "+err.Error()).Retry()
	}
	// An audit record is the point: this is a safety-reducing operator action and
	// must be reviewable after the fact.
	c.audit.Append(map[string]any{
		"action":           "risk_reset_anchor",
		"network":          c.network,
		"account":          riskStateComponent(c.queryAddr),
		"prev_peak_equity": res.PrevPeakEquity,
		"new_peak_equity":  res.NewPeakEquity,
		"drawdown_was_pct": res.DrawdownWasPct,
		"peak_reset_count": res.ResetCount,
	})
	return &res, nil
}
