package core

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/erickuhn19/deliverator/internal/output"
)

// Copy (#27) is non-custodial copy-trading: read a leader's public on-chain perp
// book, scale it to your account, and emit the diff that would bring your book to
// the leader's (default, read-only) — or execute it (CopyExecute). It is diff-first:
// the CLI computes the gap deterministically; the agent decides whether to close it.
//
// STATELESS by design: the set of coins you're mirroring lives in the AGENT loop,
// passed in as CopyParams.Mirrored and handed back as CopyDiff.MirroredNow (the
// leader's current coins) for the loop to persist. The diff is scoped to the
// leader's CURRENT coins ∪ Mirrored, so:
//   - leader holds a coin  -> drive your position to the leader's scaled size,
//   - leader EXITED a coin that's in Mirrored -> close your position (mirror exit),
//   - a coin in neither -> ignored (your own independent positions are untouched).
//
// All RISK is inherited at execute: the legs route through Place/Close, which
// enforce the account-wide gates (#43). Prospective liq uses the #46 machinery.

var copyAddrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// CopyParams parameterizes a copy diff/execute.
type CopyParams struct {
	Leader            string
	ScaleMode         string   // "equity" (proportional to your equity) | "fixed"
	Scale             float64  // multiplier on the leader's sizes
	Mirrored          []string // coins the agent is currently mirroring (for exit detection)
	Coins             []string // optional allowlist filter (intersect)
	MinDiffUSD        float64
	MinLiqDistancePct float64
	MaxLeverage       int // per-leg size clip hint (NOT a risk backstop)
	NoNewOpens        bool
	MaxOrdersPerCycle int
}

// DiffLeg is one coin's move to bring your book toward the leader's.
type DiffLeg struct {
	Coin                string `json:"coin"`
	Class               string `json:"class"`  // open | increase | decrease | close | flip
	Action              string `json:"action"` // buy | sell (the primary leg's side)
	Size                string `json:"size"`   // abs size to trade
	FromSzi             string `json:"from_szi"`
	ToSzi               string `json:"to_szi"`
	NotionalUSD         string `json:"notional_usd"`
	EstLiquidationPx    string `json:"est_liquidation_px,omitempty"` // open/increase/flip only (isolated estimate)
	EstDistanceToLiqPct string `json:"est_distance_to_liq_pct,omitempty"`
}

// SkipLeg records a leg the diff deliberately did not emit, with the reason.
type SkipLeg struct {
	Coin   string `json:"coin"`
	Reason string `json:"reason"`
}

// CopyDiff is the read-only result. MirroredNow is the leader's current coin set —
// the agent persists it and passes it back as Mirrored next tick.
type CopyDiff struct {
	Leader                   string    `json:"leader"`
	ScaleMode                string    `json:"scale_mode"`
	ScaleFactor              string    `json:"scale_factor"` // effective per-size multiplier applied
	LeaderEquity             string    `json:"leader_equity,omitempty"`
	YourEquity               string    `json:"your_equity"`
	Diff                     []DiffLeg `json:"diff"`
	Skipped                  []SkipLeg `json:"skipped"`
	MirroredNow              []string  `json:"mirrored_now"`
	ResultingAccountLeverage string    `json:"resulting_account_leverage,omitempty"`
}

// Copy computes the read-only diff. It never places an order.
func (c *Client) Copy(ctx context.Context, p CopyParams) (*CopyDiff, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	leader := strings.ToLower(strings.TrimSpace(p.Leader))
	if !copyAddrRe.MatchString(leader) {
		return nil, output.Validation("bad_leader", "leader must be a 0x-prefixed 40-hex address")
	}
	if strings.EqualFold(leader, c.queryAddr) {
		return nil, output.Validation("self_copy", "cannot copy your own account")
	}

	// --- read leader book (main perp dex; sub-dex leaders are v1-out-of-scope) ---
	ls, err := c.info.UserState(ctx, leader)
	if err != nil {
		return nil, mapNetwork("leader_state", err)
	}
	leaderSzi := map[string]float64{}
	leaderLev := map[string]int{}
	for _, ap := range ls.AssetPositions {
		if szi := parseFloatSafe(ap.Position.Szi); szi != 0 {
			leaderSzi[ap.Position.Coin] = szi
			leaderLev[ap.Position.Coin] = ap.Position.Leverage.Value
		}
	}

	// --- read your book + equity ---
	pf, err := c.Portfolio(ctx)
	if err != nil {
		return nil, err
	}
	yourSzi := map[string]float64{}
	for _, pv := range pf.Positions {
		if szi := parseFloatSafe(pv.Szi); szi != 0 {
			yourSzi[pv.Coin] = szi
		}
	}
	yourEquity := equityOf(pf.AccountValue, pf.AvailableCollateral)

	// --- scale factor ---
	mode := p.ScaleMode
	scale := p.Scale
	if scale <= 0 {
		scale = 1
	}
	var factor, leaderEquity float64
	switch mode {
	case "fixed":
		factor = scale
	default:
		mode = "equity"
		lc := ""
		if lss, e := c.info.SpotUserState(ctx, leader); e == nil {
			lc = usdcCollateral(lss)
		}
		leaderEquity = equityOf(ls.MarginSummary.AccountValue, lc)
		if leaderEquity <= 0 {
			return nil, output.Validation("leader_equity", "leader equity reads 0 — cannot scale by equity; use --scale-mode fixed")
		}
		if yourEquity <= 0 {
			return nil, output.Validation("your_equity", "your equity is 0 — fund the account or use --scale-mode fixed")
		}
		factor = (yourEquity / leaderEquity) * scale
	}

	// --- in-scope coins = leader's current coins ∪ Mirrored (then --coins / allowlist) ---
	inScope := map[string]bool{}
	for coin := range leaderSzi {
		inScope[coin] = true
	}
	for _, m := range p.Mirrored {
		if m = strings.TrimSpace(m); m != "" {
			inScope[m] = true
		}
	}
	coinFilter := map[string]bool{}
	for _, cn := range p.Coins {
		if cn = strings.TrimSpace(cn); cn != "" {
			coinFilter[strings.ToUpper(cn)] = true
		}
	}

	mids, _ := c.info.AllMids(ctx)
	midOf := func(coin string) float64 { return parseFloatSafe(mids[coin]) }

	diff := []DiffLeg{}
	skipped := []SkipLeg{}

	scopeList := make([]string, 0, len(inScope))
	for coin := range inScope {
		scopeList = append(scopeList, coin)
	}
	sort.Strings(scopeList)

	for _, coin := range scopeList {
		if len(coinFilter) > 0 && !coinFilter[strings.ToUpper(coin)] {
			continue // --coins filter: out of scope entirely (not even reported)
		}
		mk, ok := c.meta.Lookup(coin)
		if !ok {
			skipped = append(skipped, SkipLeg{coin, "not in the tradable universe"})
			continue
		}
		if !c.coinAllowed(coin) {
			skipped = append(skipped, SkipLeg{coin, "not in automation.allowed_coins"})
			continue
		}
		mid := midOf(coin)
		if mid <= 0 {
			skipped = append(skipped, SkipLeg{coin, "no mid price"})
			continue
		}
		target := leaderSzi[coin] * factor // 0 when the leader has exited this coin
		your := yourSzi[coin]
		leg, skip, ok := classifyLeg(coin, your, target, mid, mk, leaderLev[coin], p, c)
		if skip != nil {
			skipped = append(skipped, *skip)
			continue
		}
		if ok {
			diff = append(diff, leg)
		}
	}

	// Order legs: risk-reducing first (close/decrease), then flips, then opens/increases —
	// so reductions free up margin/exposure budget before any new exposure is added.
	sort.SliceStable(diff, func(i, j int) bool {
		ri, rj := classRank(diff[i].Class), classRank(diff[j].Class)
		if ri != rj {
			return ri < rj
		}
		return diff[i].Coin < diff[j].Coin
	})

	// Resulting account leverage = gross of the resulting book / your equity.
	gross := 0.0
	for coin, szi := range yourSzi {
		s := szi
		if inScope[coin] {
			s = leaderSzi[coin] * factor
		}
		gross += absF(s) * midOf(coin)
	}
	for coin := range inScope {
		if _, have := yourSzi[coin]; !have {
			gross += absF(leaderSzi[coin]*factor) * midOf(coin)
		}
	}

	mirroredNow := make([]string, 0, len(leaderSzi))
	for coin := range leaderSzi {
		mirroredNow = append(mirroredNow, coin)
	}
	sort.Strings(mirroredNow)

	out := &CopyDiff{
		Leader: leader, ScaleMode: mode, ScaleFactor: f2s(factor),
		YourEquity: f2s(yourEquity), Diff: diff, Skipped: skipped, MirroredNow: mirroredNow,
	}
	if mode == "equity" {
		out.LeaderEquity = f2s(leaderEquity)
	}
	if yourEquity > 0 {
		out.ResultingAccountLeverage = f2s(gross / yourEquity)
	}
	return out, nil
}

// classifyLeg turns (current, target) signed sizes into a DiffLeg, a SkipLeg, or
// nothing (already at target). c is used only for the tier-based liq estimate.
func classifyLeg(coin string, your, target, mid float64, mk Market, leaderLev int, p CopyParams, c *Client) (DiffLeg, *SkipLeg, bool) {
	const eps = 1e-12
	absT, absY := absF(target), absF(your)
	var class, action string
	var size float64
	switch {
	case absY < eps && absT < eps:
		return DiffLeg{}, nil, false
	case absY < eps: // open
		class, size, action = "open", absT, sideStr(target)
	case absT < eps: // close (mirror the leader's exit / a coin no longer held)
		class, size, action = "close", absY, oppSideOf(your)
	case sameSign(your, target) && absT > absY+eps:
		class, size, action = "increase", absT-absY, sideStr(your)
	case sameSign(your, target) && absT < absY-eps:
		class, size, action = "decrease", absY-absT, oppSideOf(your)
	case sameSign(your, target): // equal -> nothing
		return DiffLeg{}, nil, false
	default: // opposite signs -> flip (close old fully, open new side)
		class, size, action = "flip", absT, sideStr(target)
	}

	size = roundSizeF(size, mk.SzDecimals)
	if size <= 0 {
		return DiffLeg{}, &SkipLeg{coin, "rounds to zero size"}, false
	}
	notional := size * mid
	if class != "close" && p.MinDiffUSD > 0 && notional < p.MinDiffUSD {
		return DiffLeg{}, &SkipLeg{coin, fmt.Sprintf("below min_diff $%.2f", p.MinDiffUSD)}, false
	}
	if class == "open" && p.NoNewOpens {
		return DiffLeg{}, &SkipLeg{coin, "no_new_opens"}, false
	}

	leg := DiffLeg{
		Coin: coin, Class: class, Action: action, Size: f2s(size),
		FromSzi: f2s(your), ToSzi: f2s(target), NotionalUSD: f2s(notional),
	}
	// Prospective liq for new-exposure legs (isolated estimate via the #46 machinery).
	if class == "open" || class == "increase" || class == "flip" {
		lev := leaderLev
		if lev <= 0 {
			lev = mk.MaxLeverage
		}
		if p.MaxLeverage > 0 && lev > p.MaxLeverage {
			lev = p.MaxLeverage
		}
		mmf := c.meta.MaintenanceMarginFraction(mk.Coin, notional)
		if liq := isolatedLiqPrice(mid, sideSign(action), lev, mmf); liq > 0 {
			dist := absF(mid-liq) / mid * 100
			leg.EstLiquidationPx = f2s(liq)
			leg.EstDistanceToLiqPct = f2s(dist)
			if p.MinLiqDistancePct > 0 && dist < p.MinLiqDistancePct {
				return DiffLeg{}, &SkipLeg{coin, fmt.Sprintf("est liq distance %.1f%% < min %.1f%%", dist, p.MinLiqDistancePct)}, false
			}
		}
	}
	return leg, nil, true
}

func classRank(class string) int {
	switch class {
	case "close", "decrease":
		return 0
	case "flip":
		return 1
	default: // open, increase
		return 2
	}
}

func sideStr(szi float64) string {
	if szi < 0 {
		return "sell"
	}
	return "buy"
}

func oppSideOf(szi float64) string { // the side that REDUCES a position of sign(szi)
	if szi > 0 {
		return "sell"
	}
	return "buy"
}

func sideSign(action string) float64 {
	if action == "sell" {
		return -1
	}
	return 1
}

func sameSign(a, b float64) bool { return (a > 0) == (b > 0) }

func roundSizeF(size float64, szDecimals int) float64 {
	pow := math.Pow(10, float64(szDecimals))
	return math.Round(size*pow) / pow
}
