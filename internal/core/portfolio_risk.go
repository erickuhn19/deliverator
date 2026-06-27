package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
)

// Portfolio-level risk gates (#43). These are ACCOUNT-WIDE, enforced in core
// before signing, so a hallucinating or mis-sized agent cannot exceed them by
// switching invocation — unlike the per-order/per-coin caps, they bound the book
// as a whole. They are evaluated against the RESULTING book (current positions +
// the proposed NEW exposure); reduce-only/close legs add no exposure and are
// exempt. All gates default to 0 = off, so the safe baseline is unchanged until
// the operator opts in, and the whole path (incl. the live snapshot fetch) is
// skipped when none is configured.
//
// They apply on the exposure-OPENING paths (Place / bracket entry / batch+grid /
// twap). A modify re-prices an EXISTING order, so it adds no new order and is not
// portfolio-gated here (the per-coin max_position_notional cap still applies);
// gating a modify would false-reject a routine grid re-price.

// portfolioGuardsActive reports whether any account-wide gate is configured. When
// false the entire portfolio-check path is skipped (zero added latency / behavior).
func (c *Client) portfolioGuardsActive() bool {
	r := c.cfg.Risk
	return r.MaxAccountLeverage > 0 || r.MaxNetExposureUSD > 0 || r.MaxConcentrationPctPerCoin > 0 ||
		r.MaxDrawdownPct > 0 || r.MaxDailyLossUSD > 0 || r.MaxDailyLossPct > 0 ||
		r.MaxOpenPositions > 0
}

// reduceOnlyFlipErr rejects a reduce-only order whose size exceeds the CURRENT
// open position in that coin: it could only fill by crossing zero into opposite
// exposure (which HL prevents, silently dropping the excess), so an oversize
// request is a sizing mistake worth surfacing as a clear error (#44). It is
// SKIPPED when flat — a reduce-only order may legitimately be placed before its
// position exists (a bracket's tp/sl, a pre-armed stop) and reduces whatever
// exists at fill time.
func (c *Client) reduceOnlyFlipErr(ctx context.Context, coin string, orderSzAbs float64) error {
	szi, ok := c.positionSzi(ctx, coin)
	if !ok || szi == 0 {
		return nil
	}
	if orderSzAbs > absF(szi)+1e-9 {
		return output.Risk("reduce_only_flip",
			fmt.Sprintf("reduce-only size %s exceeds the open %s position %s — it can only cross zero, not reduce",
				strconv.FormatFloat(orderSzAbs, 'f', -1, 64), coin, strconv.FormatFloat(absF(szi), 'f', -1, 64))).
			WithHint("size the reduce-only order to at most the current position, or drop reduce-only to open opposite exposure")
	}
	return nil
}

// exposureDelta is one coin's signed notional change from a proposed new-exposure
// order — buy/long positive, sell/short negative.
type exposureDelta struct {
	coin           string
	signedNotional float64
}

func signedNotional(side Side, notional float64) float64 {
	if side == Sell {
		return -notional
	}
	return notional
}

// parseFloatSafe parses an exchange numeric string, returning 0 for unparseable
// OR non-finite input. strconv.ParseFloat accepts "NaN"/"Inf", so without the
// finite check a poisoned field would propagate NaN/Inf through the risk math
// (where NaN > cap is false, defeating a gate) and into the JSON envelope (audit S2).
func parseFloatSafe(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// equityOf is the equity portfolio risk math divides by: the GREATER of perp
// account_value and available USDC collateral. See portfolioEquitySnapshot for
// why (a unified account holds its equity in spot, where account_value reads only
// the open-position margin slice). Shared by the gates (#43) and margin_ratio (#46).
func equityOf(accountValue, availableCollateral string) float64 {
	e := parseFloatSafe(accountValue)
	if c := parseFloatSafe(availableCollateral); c > e {
		e = c
	}
	return e
}

// portfolioEquitySnapshot is the live account state the gates evaluate against.
type portfolioEquitySnapshot struct {
	equity  float64            // perp account_value, or available USDC collateral when flat/unified
	perCoin map[string]float64 // signed notional per coin, across all dexes
}

func (c *Client) portfolioEquitySnapshot(ctx context.Context) (*portfolioEquitySnapshot, error) {
	pf, err := c.Portfolio(ctx)
	if err != nil {
		return nil, err
	}
	// Equity = the GREATER of perp account_value and available USDC collateral.
	// On a UNIFIED (spot-collateral) account the equity lives in spot: perp
	// account_value reads only the margin slice backing open positions (~0 when
	// flat, e.g. $3 holding a $15 position) while the bulk sits in collateral —
	// so account_value alone would over-state leverage ~50x and false-trip
	// drawdown/daily-loss the instant a position opens (live-confirmed). On a
	// pure-perp account it is the reverse: account_value is the full equity and
	// collateral the free part. max() picks the meaningful figure in both, and
	// since the two pools are disjoint it never exceeds true equity — it can only
	// ever be conservative (over-reject), never unsafe.
	equity := equityOf(pf.AccountValue, pf.AvailableCollateral)
	snap := &portfolioEquitySnapshot{equity: equity, perCoin: map[string]float64{}}
	for _, p := range pf.Positions {
		n := parseFloatSafe(p.PositionValue)
		if p.Side == "short" {
			n = -n
		}
		snap.perCoin[p.Coin] += n
	}
	return snap, nil
}

// checkPortfolioGates enforces the configured account-wide gates against the
// resulting book (live positions + the proposed new-exposure deltas). It is a
// no-op unless a portfolio gate is set. It FAILS CLOSED: when the account snapshot
// cannot be read, the order is refused rather than allowed to bypass the gate.
func (c *Client) checkPortfolioGates(ctx context.Context, deltas []exposureDelta) error {
	if !c.portfolioGuardsActive() {
		return nil
	}
	snap, err := c.portfolioEquitySnapshot(ctx)
	if err != nil {
		return output.Network("portfolio_state",
			"cannot read account state to enforce portfolio risk gates: "+err.Error()).Retry()
	}

	resulting := make(map[string]float64, len(snap.perCoin)+len(deltas))
	for k, v := range snap.perCoin {
		resulting[k] = v
	}
	for _, d := range deltas {
		resulting[d.coin] += d.signedNotional
	}
	gross, net, maxCoinNotional, maxCoin := 0.0, 0.0, 0.0, ""
	for coin, v := range resulting {
		gross += absF(v)
		net += v
		if absF(v) > maxCoinNotional {
			maxCoinNotional, maxCoin = absF(v), coin
		}
	}

	r := c.cfg.Risk
	// Net exposure needs no equity, so it is always checkable.
	if cap := r.MaxNetExposureUSD; cap > 0 && absF(net) > cap {
		return output.Risk("max_net_exposure",
			fmt.Sprintf("resulting net exposure $%.2f exceeds cap $%.2f", net, cap)).
			WithHint(fmt.Sprintf("trim the heavier side so |long − short| <= $%.2f", cap))
	}
	// Open-position count: opening a NEW coin while at the cap is rejected; adding
	// to an existing position (count unchanged) is not. Needs no equity.
	if cap := r.MaxOpenPositions; cap > 0 {
		count := 0
		for _, v := range resulting {
			if absF(v) > 0.005 { // ignore dust / a flip that lands at ~0
				count++
			}
		}
		if count > cap {
			return output.Risk("max_open_positions",
				fmt.Sprintf("this would make %d open positions, over the max of %d", count, cap)).
				WithHint(fmt.Sprintf("close a position or add to an existing one (cap %d concurrent)", cap))
		}
	}

	equityGates := r.MaxAccountLeverage > 0 || r.MaxConcentrationPctPerCoin > 0 ||
		r.MaxDrawdownPct > 0 || r.MaxDailyLossUSD > 0 || r.MaxDailyLossPct > 0
	if equityGates && snap.equity <= 0 {
		return output.Risk("account_equity_unavailable",
			"account equity is 0 or unreadable — cannot enforce leverage/drawdown/daily-loss gates").
			WithHint("fund the account or set wallet.master_address; these gates fail closed without equity")
	}
	if cap := r.MaxAccountLeverage; cap > 0 {
		if lev := gross / snap.equity; lev > cap {
			return output.Risk("max_account_leverage",
				fmt.Sprintf("resulting account leverage %.2fx exceeds cap %.2fx (gross $%.2f / equity $%.2f)",
					lev, cap, gross, snap.equity)).
				WithHint(fmt.Sprintf("reduce size so gross notional <= $%.2f", cap*snap.equity))
		}
	}
	if cap := r.MaxConcentrationPctPerCoin; cap > 0 && maxCoinNotional > 0 {
		if pct := maxCoinNotional / snap.equity * 100; pct > cap {
			return output.Risk("max_concentration",
				fmt.Sprintf("%s would be %.1f%% of equity, over the %.1f%% per-coin cap", maxCoin, pct, cap)).
				WithHint(fmt.Sprintf("reduce %s so its notional <= $%.2f", maxCoin, cap/100*snap.equity))
		}
	}

	// Trajectory gates: record the latest equity into the persistent high-water /
	// daily-anchor state and gate on the resulting drawdown / daily loss.
	if r.MaxDrawdownPct > 0 || r.MaxDailyLossUSD > 0 || r.MaxDailyLossPct > 0 {
		dd, dlUSD, dlPct, oerr := observeEquity(snap.equity)
		if oerr != nil {
			return output.Network("risk_state", "cannot update drawdown/daily-loss state: "+oerr.Error()).Retry()
		}
		if cap := r.MaxDrawdownPct; cap > 0 && dd > cap {
			return output.Risk("max_drawdown",
				fmt.Sprintf("drawdown %.1f%% from peak exceeds the %.1f%% cap — trading paused", dd, cap)).
				WithHint("recover above the threshold, raise the cap, or clear risk_state.json to reset the peak")
		}
		if cap := r.MaxDailyLossUSD; cap > 0 && dlUSD > cap {
			return output.Risk("max_daily_loss",
				fmt.Sprintf("today's loss $%.2f exceeds the $%.2f daily cap — trading paused", dlUSD, cap)).
				WithHint("the daily anchor resets at UTC midnight; or raise risk.max_daily_loss_usd")
		}
		if cap := r.MaxDailyLossPct; cap > 0 && dlPct > cap {
			return output.Risk("max_daily_loss_pct",
				fmt.Sprintf("today's loss %.1f%% exceeds the %.1f%% daily cap — trading paused", dlPct, cap)).
				WithHint("the daily anchor resets at UTC midnight; or raise risk.max_daily_loss_pct")
		}
	}
	return nil
}

// ---- persistent drawdown / daily-loss state (high-water + UTC-day anchor) ----

func riskStatePath() string     { return filepath.Join(config.Dir(), "risk_state.json") }
func riskStateLockPath() string { return filepath.Join(config.Dir(), "risk_state.lock") }

type riskState struct {
	PeakEquity      float64 `json:"peak_equity"`
	Day             string  `json:"day"`               // UTC date, YYYY-MM-DD
	DayAnchorEquity float64 `json:"day_anchor_equity"` // equity at the day's first observation
}

// observeEquity records equity into the persistent high-water / daily-anchor state
// (serialized across processes by a flock, like the rate cap) and returns the
// resulting drawdown-from-peak and daily-loss figures. A new UTC day re-anchors the
// daily figure; a missing or corrupt state file is treated as a fresh start (no
// drawdown yet) rather than a hard error, so a bad file never bricks trading.
func observeEquity(equity float64) (drawdownPct, dailyLossUSD, dailyLossPct float64, err error) {
	lk, err := state.Lock(riskStateLockPath())
	if err != nil {
		return 0, 0, 0, err
	}
	defer lk.Unlock()

	var st riskState
	if b, e := os.ReadFile(riskStatePath()); e == nil {
		_ = json.Unmarshal(b, &st) // corrupt => treated as fresh
	}
	today := time.Now().UTC().Format("2006-01-02")
	if st.Day != today || st.DayAnchorEquity <= 0 {
		st.Day = today
		st.DayAnchorEquity = equity
	}
	if equity > st.PeakEquity {
		st.PeakEquity = equity
	}
	if st.PeakEquity > 0 && equity < st.PeakEquity {
		drawdownPct = (st.PeakEquity - equity) / st.PeakEquity * 100
	}
	if st.DayAnchorEquity > 0 && equity < st.DayAnchorEquity {
		dailyLossUSD = st.DayAnchorEquity - equity
		dailyLossPct = dailyLossUSD / st.DayAnchorEquity * 100
	}
	if b, e := json.Marshal(st); e == nil {
		_ = os.MkdirAll(filepath.Dir(riskStatePath()), 0o700)
		// Atomic+fsync: a crash mid-write must not zero the drawdown/daily-loss
		// anchors that gate signing (audit #91 / S12).
		_ = state.WriteFileAtomic(riskStatePath(), b, 0o600)
	}
	return drawdownPct, dailyLossUSD, dailyLossPct, nil
}
