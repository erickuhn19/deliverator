package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	hl "github.com/erickuhn19/deliverator/internal/hl"

	"github.com/erickuhn19/deliverator/internal/output"
)

// ---------- view types (agent-friendly, strings for px/sz) ----------

// PositionView is a flattened perp position.
type PositionView struct {
	Coin           string `json:"coin"`
	Szi            string `json:"szi"`  // signed size
	Side           string `json:"side"` // long | short | flat
	EntryPx        string `json:"entry_px,omitempty"`
	PositionValue  string `json:"position_value"`
	UnrealizedPnl  string `json:"unrealized_pnl"`
	ReturnOnEquity string `json:"roe"`
	LiquidationPx  string `json:"liquidation_px,omitempty"`
	// DistanceToLiqPct is how far the mark price can move (as a % of mark) before
	// this position is liquidated: |mark − liquidationPx| / mark * 100. Computed
	// from HL's own figures (mark = position_value / |szi|), so it is exact. Empty
	// when the position has no liquidation price (e.g. flat / fully hedged cross).
	DistanceToLiqPct string `json:"distance_to_liq_pct,omitempty"`
	MarginUsed       string `json:"margin_used"`
	LeverageType     string `json:"leverage_type,omitempty"`
	LeverageValue    int    `json:"leverage_value,omitempty"`

	// HIP-4 outcome-only (set when Class=="outcome"): the holding is a Yes/No share
	// balance ("+<enc>" token), not a leveraged perp — price is a probability and
	// there is no liquidation. Szi is the share count, mark_px the current
	// probability, at_stake_usd the cost (max loss), max_gain_usd = size − cost.
	Class            string `json:"class,omitempty"` // "outcome" for HIP-4 (omitted for perp/sub-dex)
	OutcomeSide      string `json:"outcome_side,omitempty"`
	Title            string `json:"title,omitempty"`
	MarkPx           string `json:"mark_px,omitempty"`
	AtStakeUSD       string `json:"at_stake_usd,omitempty"`
	MaxGainUSD       string `json:"max_gain_usd,omitempty"`
	ResolutionStatus string `json:"resolution_status,omitempty"`
	Expiry           string `json:"expiry,omitempty"`
}

// PortfolioView is the full one-call snapshot (§9 portfolio).
type PortfolioView struct {
	Address             string                 `json:"address"`
	AccountValue        string                 `json:"account_value"`
	TotalMarginUsed     string                 `json:"total_margin_used"`
	TotalNotionalPos    string                 `json:"total_notional_position"`
	Withdrawable        string                 `json:"withdrawable"`
	Positions           []PositionView         `json:"positions"`
	OpenOrders          []hl.FrontendOpenOrder `json:"open_orders"`
	SpotBalances        []hl.SpotBalance       `json:"spot_balances"`
	AvailableCollateral string                 `json:"available_collateral,omitempty"`
	// MaintenanceMargin is the total maintenance margin required across all
	// positions (Σ |position_value| × the coin's tier maintenance fraction); the
	// account is liquidated as equity approaches it. MarginRatio = maintenance /
	// the equity that can BACK perp margin (perpMarginEquity — 0..1; higher =
	// closer to liquidation): on a unified account the same corrected equity the
	// risk gates use (accountEquity, basis 2); on a NON-unified account the perp
	// wallet(s) alone, since idle spot USDC / outcome holdings cannot meet a
	// perp margin call. Both empty when flat.
	MaintenanceMargin string `json:"maintenance_margin,omitempty"`
	MarginRatio       string `json:"margin_ratio,omitempty"`
	// CollateralShared flags a unified account: available_collateral is the single
	// spendable balance backing the MAIN dex AND every sub-dex. When true, a 0.0
	// account_value (here or under perp_dexs) means flat, NOT out-of-funds — gate
	// "can I trade?" on available_collateral, never account_value. See sharedCollateralNote.
	CollateralShared bool `json:"collateral_shared,omitempty"`
	// PerpDexs reports each configured HIP-3 sub-dex's clearinghouse (margin used,
	// notional, account value). The headline totals above are the MAIN dex only;
	// sub-dex positions are listed under Positions, so their margin/notional is
	// surfaced here to keep the snapshot internally consistent (parity with
	// `balance`, which already breaks out per-dex collateral).
	PerpDexs map[string]PerpBalance `json:"perp_dexs,omitempty"`

	// Degraded lists the gate-relevant inputs this snapshot could NOT read:
	// spot state, a configured sub-dex clearinghouse or open-orders sweep, or
	// the data needed to value held outcome tokens. Read surfaces stay tolerant
	// of a partial snapshot but must SHOW it (this field + envelope warnings —
	// a silently smaller book is a money bug); the account-wide risk gates FAIL
	// CLOSED on it — a partial book would let an order pass a gate the full
	// book breaches (see portfolioEquitySnapshot). DegradedDexs is the
	// machine-readable subset: just the sub-dex names whose clearinghouse or
	// open-orders read failed. Both additive (schema v1), omitted when clean.
	Degraded     []string `json:"degraded,omitempty"`
	DegradedDexs []string `json:"degraded_dexs,omitempty"`
}

// ReadMeta renders the portfolio's degradation as read-health metadata for the
// envelope (warnings + degraded_dexs).
func (v *PortfolioView) ReadMeta() ReadMeta {
	return ReadMeta{Degraded: v.Degraded, DegradedDexs: v.DegradedDexs}
}

// sharedCollateralNote annotates a per-dex (or main perp) balance block on a
// unified account, so a reader doesn't misread account_value 0.0 as "can't trade".
const sharedCollateralNote = "unified account: trades here draw from the shared available_collateral; account_value is this dex's margin in use (0.0 when flat), not a separate balance"

// usdcCollateral returns the USDC (token 0) available to open positions — the
// unified-account perp collateral — or "" if the field is absent.
func usdcCollateral(ss *hl.SpotUserState) string {
	if ss == nil {
		return ""
	}
	for _, t := range ss.TokenToAvailableAfterMaintenance {
		if t.Token == 0 {
			return t.Available
		}
	}
	return ""
}

// BalanceView is perp + spot balances and withdrawable.
type BalanceView struct {
	Perp PerpBalance      `json:"perp"`
	Spot []hl.SpotBalance `json:"spot"`
	// AvailableCollateral is the USDC usable to open perp positions. On a unified
	// account this is the spot USDC (perp account_value reads 0 with no open
	// positions), so it is the number that actually says "you can trade".
	AvailableCollateral string `json:"available_collateral,omitempty"`
	// CollateralShared flags a unified account: available_collateral backs the main
	// dex AND every sub-dex. When true, a 0.0 account_value (perp or perp_dexs) means
	// flat, not out-of-funds — gate trades on available_collateral, not account_value.
	CollateralShared bool `json:"collateral_shared,omitempty"`
	// PerpDexs reports each configured HIP-3 sub-dex's clearinghouse (margin used,
	// notional, account value). On a unified account collateral is shared, so this
	// mainly surfaces per-dex margin/positions, not separate funds.
	PerpDexs map[string]PerpBalance `json:"perp_dexs,omitempty"`
}

type PerpBalance struct {
	AccountValue     string `json:"account_value"`
	TotalMarginUsed  string `json:"total_margin_used"`
	TotalNotionalPos string `json:"total_notional_position"`
	Withdrawable     string `json:"withdrawable"`
	// Note clarifies a sub-dex block on a unified account (set only for perp_dexs
	// entries) so account_value 0.0 isn't misread as "can't trade here".
	Note string `json:"note,omitempty"`
}

// BookLevel / BookView mirror an L2 snapshot with string px/sz.
type BookLevel struct {
	Px string `json:"px"`
	Sz string `json:"sz"`
	N  int    `json:"n"`
}
type BookView struct {
	Coin string      `json:"coin"`
	Time int64       `json:"time"`
	Bids []BookLevel `json:"bids"`
	Asks []BookLevel `json:"asks"`
}

// BboView is the top of book.
type BboView struct {
	Coin   string `json:"coin"`
	Time   int64  `json:"time"`
	Bid    string `json:"bid,omitempty"`
	BidSz  string `json:"bid_sz,omitempty"`
	Ask    string `json:"ask,omitempty"`
	AskSz  string `json:"ask_sz,omitempty"`
	Mid    string `json:"mid,omitempty"`
	Spread string `json:"spread,omitempty"`
}

// CtxView is per-asset market context (§9 ctx).
type CtxView struct {
	Coin         string `json:"coin"`
	IsSpot       bool   `json:"is_spot,omitempty"`
	MarkPx       string `json:"mark_px"`
	OraclePx     string `json:"oracle_px,omitempty"`
	MidPx        string `json:"mid_px,omitempty"`
	Funding      string `json:"funding,omitempty"`
	OpenInterest string `json:"open_interest,omitempty"`
	Premium      string `json:"premium,omitempty"`
	// ImpactPxs are the [bid, ask] impact prices — the average fill price for a
	// notional-sized marketable order — used to estimate slippage before sizing a
	// large order. Perp-only (spot ctx leaves it empty).
	ImpactPxs []string `json:"impact_pxs,omitempty"`
	DayNtlVlm string   `json:"day_ntl_vlm"`
	PrevDayPx string   `json:"prev_day_px"`
	// Spot-only: token supply (perp ctx leaves these empty).
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	TotalSupply       string `json:"total_supply,omitempty"`
	// HIP-4 outcome-only: MarkPx/MidPx carry the probability (in (0,1)); there is no
	// funding/OI/oracle. ComplementMid is the other side's probability (~1 − mid).
	IsOutcome        bool   `json:"is_outcome,omitempty"`
	Side             string `json:"side,omitempty"`
	Title            string `json:"title,omitempty"`
	ResolutionStatus string `json:"resolution_status,omitempty"`
	Expiry           string `json:"expiry,omitempty"`
	BestBid          string `json:"best_bid,omitempty"`
	BestAsk          string `json:"best_ask,omitempty"`
	ComplementMid    string `json:"complement_mid,omitempty"`
}

// LimitsView is the per-address rate-limit budget (§7).
type LimitsView struct {
	CumVlm    string `json:"cum_vlm"`
	Used      int    `json:"n_requests_used"`
	Cap       int    `json:"n_requests_cap"`
	Surplus   int    `json:"n_requests_surplus"`
	Remaining int    `json:"remaining"`
}

// BuilderView is the builder fee posture (§9 builder status).
type BuilderView struct {
	Address           string `json:"address,omitempty"`
	FeeTenthsBps      int    `json:"fee_tenths_bps"`
	AttachMode        string `json:"attach_mode"`
	ApprovedMaxTenths *int   `json:"approved_max_tenths_bps,omitempty"`
}

// ---------- read methods ----------

func sideFromSzi(szi string) string {
	switch {
	case strings.HasPrefix(szi, "-"):
		return "short"
	case szi == "" || szi == "0" || szi == "0.0":
		return "flat"
	default:
		return "long"
	}
}

// bareCoin strips a HIP-3 "<dex>:" prefix, returning the plain coin ("xyz:GOLD"
// -> "GOLD"). A spot pair ("PURR/USDC", no ':') is returned unchanged.
func bareCoin(s string) string {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[i+1:]
	}
	return s
}

// matchesCoinFilter reports whether a position/order coin matches a user's --coin
// filter, tolerating the HIP-3 "<dex>:" prefix on EITHER side. markets/mids use
// the prefixed form ("xyz:GOLD"), a sub-dex clearinghouse may report the bare form
// ("GOLD"), and a user may type either — all four combinations must match so a
// sub-dex position/order is never silently filtered out (the read-path analog of
// coinMatches, which the write path already uses).
func matchesCoinFilter(haveCoin, want string) bool {
	if strings.EqualFold(haveCoin, want) {
		return true
	}
	return strings.EqualFold(bareCoin(haveCoin), bareCoin(want))
}

func toPositionViews(s *hl.UserState, coin string) []PositionView {
	out := []PositionView{}
	for _, ap := range s.AssetPositions {
		p := ap.Position
		if coin != "" && !matchesCoinFilter(p.Coin, coin) {
			continue
		}
		pv := PositionView{
			Coin:           p.Coin,
			Szi:            p.Szi,
			Side:           sideFromSzi(p.Szi),
			PositionValue:  p.PositionValue,
			UnrealizedPnl:  p.UnrealizedPnl,
			ReturnOnEquity: p.ReturnOnEquity,
			MarginUsed:     p.MarginUsed,
			LeverageType:   p.Leverage.Type,
			LeverageValue:  p.Leverage.Value,
		}
		if p.EntryPx != nil {
			pv.EntryPx = *p.EntryPx
		}
		if p.LiquidationPx != nil {
			pv.LiquidationPx = *p.LiquidationPx
			// mark = position_value / |szi| (HL's own mark, no extra fetch).
			if liq := parseFloatSafe(*p.LiquidationPx); liq > 0 {
				szi := parseFloatSafe(p.Szi)
				pval := parseFloatSafe(p.PositionValue)
				if szi != 0 && pval > 0 {
					if mark := pval / absF(szi); mark > 0 {
						pv.DistanceToLiqPct = f2s(absF(mark-liq) / mark * 100)
					}
				}
			}
		}
		out = append(out, pv)
	}
	return out
}

// round8 formats a computed float to ≤8 decimals, stripping float64 artifacts
// (e.g. 0.039061250000000006 → "0.03906125").
func round8(f float64) string { return f2s(math.Round(f*1e8) / 1e8) }

// hasOutcomeBalance reports whether any "+<enc>" outcome token is held.
func hasOutcomeBalance(bals []hl.SpotBalance) bool {
	for _, b := range bals {
		if len(b.Coin) >= 2 && b.Coin[0] == '+' && parseFloatSafe(b.Total) > 0 {
			return true
		}
	}
	return false
}

// outcomePositionViews maps held "+<enc>" outcome tokens into position rows
// enriched with side/title/mark + payoff bounds (at-stake = cost = max loss,
// max-gain = size − cost). mids supplies the current probability (allMids "#<enc>").
func (c *Client) outcomePositionViews(spot []hl.SpotBalance, mids map[string]string, coin string) []PositionView {
	out := []PositionView{}
	for _, b := range spot {
		if len(b.Coin) < 2 || b.Coin[0] != '+' {
			continue
		}
		outCoin := "#" + b.Coin[1:] // "+6410" -> "#6410"
		mk, ok := c.meta.Lookup(outCoin)
		if !ok || !mk.IsOutcome {
			continue
		}
		total := parseFloatSafe(b.Total)
		if total <= 0 {
			continue
		}
		if coin != "" && !matchesCoinFilter(outCoin, coin) && !strings.EqualFold(b.Coin, coin) {
			continue
		}
		entryNtl := parseFloatSafe(b.EntryNtl)
		markStr := mids[outCoin]
		mark := parseFloatSafe(markStr)
		pv := PositionView{
			Coin:             outCoin,
			Class:            "outcome",
			Szi:              b.Total,
			Side:             "long", // you hold the side's shares
			PositionValue:    b.EntryNtl,
			MarginUsed:       b.EntryNtl, // fully collateralized: cost = collateral locked
			OutcomeSide:      mk.Side,
			Title:            mk.Title,
			AtStakeUSD:       b.EntryNtl,
			ResolutionStatus: mk.ResolutionStatus,
			Expiry:           mk.Expiry,
		}
		if entryNtl > 0 {
			pv.EntryPx = round8(entryNtl / total)
			pv.MaxGainUSD = round8(total - entryNtl) // resolves to 1 per share
		}
		if mark > 0 {
			markValue := total * mark
			pv.MarkPx = markStr // raw mid string (already canonical)
			pv.PositionValue = round8(markValue)
			pv.UnrealizedPnl = round8(markValue - entryNtl)
			if entryNtl > 0 {
				pv.ReturnOnEquity = round8((markValue - entryNtl) / entryNtl)
			}
		}
		out = append(out, pv)
	}
	return out
}

// outcomePositionsFromSpot surfaces held HIP-4 outcome tokens ("+<enc>" spot
// balances) as class:"outcome" position rows. It fetches spot to detect holdings
// and, when any are held, lazily loads the outcome universe to decorate them — so
// `positions` shows outcome EXPOSURE even when outcomes weren't pre-enabled (#104:
// previously this returned early when outcomes were unloaded, hiding open bets).
// Costs one extra spot read on the positions path; returns nil when nothing held.
// Every silently-skipped input is REPORTED into deg (NEXT-2 item 1): an
// unreadable spot state hides outcome holdings entirely; a missing universe or
// mids hides/undervalues them.
func (c *Client) outcomePositionsFromSpot(ctx context.Context, coin string, deg *ReadMeta) []PositionView {
	ss, err := c.info.SpotUserState(ctx, c.queryAddr)
	if err != nil {
		deg.addDegraded("spot state (outcome holdings undetectable)", "")
		return nil
	}
	if ss == nil || !hasOutcomeBalance(ss.Balances) {
		return nil
	}
	if c.meta.OutcomeMeta() == nil {
		if err := c.EnsureOutcomes(ctx); err != nil || c.meta.OutcomeMeta() == nil {
			deg.addDegraded("outcome universe (held outcome tokens invisible)", "")
			return nil // can't decorate without the universe
		}
	}
	mids, err := c.info.AllMids(ctx)
	if err != nil {
		// surface the holding without a mark rather than failing the read
		deg.addDegraded("outcome mids (holdings shown without marks)", "")
		mids = nil
	}
	return c.outcomePositionViews(ss.Balances, mids, coin)
}

// Portfolio returns the full snapshot in one logical call (§9).
func (c *Client) Portfolio(ctx context.Context) (*PortfolioView, error) {
	return c.portfolioWith(ctx, nil)
}

// portfolioWith is Portfolio with an optionally PRE-FETCHED open-orders book:
// a gated write that already swept the book this invocation (fail-closed,
// every dex — see fetchGateBook) passes it here so ONE weight-20 fetch serves
// both the per-coin cap and the portfolio gates. preOrders==nil fetches as
// usual; a non-nil (possibly empty) slice is trusted as the COMPLETE sweep.
func (c *Client) portfolioWith(ctx context.Context, preOrders []hl.FrontendOpenOrder) (*PortfolioView, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	st, err := c.info.UserState(ctx, c.queryAddr)
	if err != nil {
		return nil, mapNetwork("clearinghouse_state", err)
	}
	// deg collects the inputs that fail below; the read surfaces tolerate a
	// partial snapshot but SURFACE it (Degraded/DegradedDexs + warnings), the
	// risk gates fail closed on it.
	var deg ReadMeta
	orders := preOrders
	if orders == nil {
		var staleOrderDexs []string
		orders, staleOrderDexs, err = c.allOpenOrdersStale(ctx)
		if err != nil {
			return nil, mapNetwork("open_orders", err)
		}
		for _, d := range staleOrderDexs {
			deg.addDegraded("open orders ("+d+")", d)
		}
	}
	if orders == nil {
		orders = []hl.FrontendOpenOrder{}
	}
	var spot []hl.SpotBalance
	var collateral string
	if ss, serr := c.info.SpotUserState(ctx, c.queryAddr); serr == nil && ss != nil {
		spot = ss.Balances
		collateral = usdcCollateral(ss)
	} else {
		deg.addDegraded("spot state", "")
	}
	if spot == nil {
		spot = []hl.SpotBalance{}
	}
	// Query each configured sub-dex clearinghouse ONCE, deriving both its positions
	// (normalized to "<dex>:<coin>") and its margin summary — so the snapshot lists
	// sub-dex positions AND surfaces the per-dex margin/notional behind them (the
	// headline totals are main-dex only). Slice order keeps the output deterministic.
	positions := toPositionViews(st, "")
	var perpDexs map[string]PerpBalance
	for _, dex := range c.cfg.PerpDexs {
		d := strings.ToLower(strings.TrimSpace(dex))
		if d == "" {
			continue
		}
		dst, derr := c.info.UserStateForDex(ctx, c.queryAddr, d)
		if derr != nil {
			deg.addDegraded("sub-dex state ("+d+")", d)
			continue
		}
		positions = append(positions, normalizedDexPositions(dst, d, "")...)
		if perpDexs == nil {
			perpDexs = map[string]PerpBalance{}
		}
		pb := PerpBalance{
			AccountValue:     dst.MarginSummary.AccountValue,
			TotalMarginUsed:  dst.MarginSummary.TotalMarginUsed,
			TotalNotionalPos: dst.MarginSummary.TotalNtlPos,
			Withdrawable:     dst.Withdrawable,
		}
		if collateral != "" {
			pb.Note = sharedCollateralNote
		}
		perpDexs[d] = pb
	}
	// HIP-4 outcome holdings (the "+<enc>" spot tokens), enriched with side/mark +
	// payoff, surfaced as class:"outcome" positions. spot is already fetched, so
	// when outcome tokens are held we lazily load the universe (free here) to
	// decorate them — exposure shows even if outcomes weren't pre-enabled (#104).
	// allMids is fetched only when outcomes are held.
	if hasOutcomeBalance(spot) {
		if c.meta.OutcomeMeta() == nil {
			_ = c.EnsureOutcomes(ctx)
		}
		if c.meta.OutcomeMeta() != nil {
			mids, merr := c.info.AllMids(ctx)
			if merr != nil {
				deg.addDegraded("outcome mids", "") // held outcome tokens can't be valued
			}
			positions = append(positions, c.outcomePositionViews(spot, mids, "")...)
		} else {
			deg.addDegraded("outcome universe", "") // held outcome tokens invisible
		}
	}
	// Account-level liquidation health: total maintenance margin across positions
	// (tier-based) and its ratio to equity.
	maint := 0.0
	for _, p := range positions {
		if pv := parseFloatSafe(p.PositionValue); pv > 0 {
			maint += pv * c.meta.MaintenanceMarginFraction(p.Coin, pv)
		}
	}
	view := &PortfolioView{
		Address:             c.queryAddr,
		CollateralShared:    collateral != "",
		AccountValue:        st.MarginSummary.AccountValue,
		TotalMarginUsed:     st.MarginSummary.TotalMarginUsed,
		TotalNotionalPos:    st.MarginSummary.TotalNtlPos,
		Withdrawable:        st.Withdrawable,
		Positions:           positions,
		OpenOrders:          orders,
		SpotBalances:        spot,
		AvailableCollateral: collateral,
		PerpDexs:            perpDexs,
		Degraded:            deg.Degraded,
		DegradedDexs:        deg.DegradedDexs,
	}
	if maint > 0 {
		view.MaintenanceMargin = f2s(maint)
		// Divide by the equity that can actually BACK perp margin
		// (perpMarginEquity): on a unified account the same corrected equity the
		// risk gates use (accountEquity, basis 2 — NOT the deprecated equityOf
		// max() heuristic, which overstated this ratio once margin was reserved);
		// on a NON-unified account the perp wallet(s) alone — idle spot USDC
		// cannot meet a perp margin call, and counting it would UNDERSTATE
		// liquidation proximity (NEXT-2 item 6 + fix-up).
		if eq := perpMarginEquity(view); eq > 0 {
			view.MarginRatio = f2s(maint / eq)
		}
	}
	return view, nil
}

// Positions returns perp positions, optionally filtered by coin, plus the
// read-health metadata naming any input (sub-dex clearinghouse, spot state for
// outcome detection, outcome universe/mids) it could not include — a partial
// book must be visible, never silent (NEXT-2 item 1). Tolerant: exit 0.
func (c *Client) Positions(ctx context.Context, coin string) ([]PositionView, ReadMeta, error) {
	var rm ReadMeta
	if err := c.requireQueryAddr(); err != nil {
		return nil, rm, err
	}
	st, err := c.info.UserState(ctx, c.queryAddr)
	if err != nil {
		return nil, rm, mapNetwork("clearinghouse_state", err)
	}
	sub, stale := c.subDexPositionsTimeout(ctx, coin, 0)
	for _, d := range stale {
		rm.addDegraded("sub-dex state ("+d+")", d)
	}
	positions := append(toPositionViews(st, coin), sub...)
	return append(positions, c.outcomePositionsFromSpot(ctx, coin, &rm)...), rm, nil
}

// subDexPositions gathers positions from each configured HIP-3 sub-dex — their
// positions live in separate per-dex clearinghouses, so they need their own
// query. A failed dex query is skipped (main positions still return).
// normalizedDexPositions maps one sub-dex clearinghouse state to PositionViews
// whose coin is the canonical "<dex>:<coin>" form (markets/mids use it) regardless
// of whether the clearinghouse reported it bare or prefixed, then applies the
// coin filter against the normalized name. This is what keeps the snapshot
// consistent and stops `positions --coin xyz:GOLD` from dropping an open sub-dex
// position (filtering runs here, post-normalization, not in toPositionViews).
func normalizedDexPositions(st *hl.UserState, dex, coin string) []PositionView {
	var out []PositionView
	for _, pv := range toPositionViews(st, "") {
		if dexOf(pv.Coin) == "" {
			pv.Coin = dex + ":" + pv.Coin
		}
		if coin == "" || matchesCoinFilter(pv.Coin, coin) {
			out = append(out, pv)
		}
	}
	return out
}

func (c *Client) subDexPositions(ctx context.Context, coin string) []PositionView {
	pvs, _ := c.subDexPositionsTimeout(ctx, coin, 0)
	return pvs
}

// subDexPositionsTimeout is subDexPositions with an optional per-dex timeout and
// failure reporting. timeout<=0 keeps the client/ctx deadline (the one-shot read
// path, e.g. Positions); a positive timeout bounds each sub-dex query so a slow
// dex can't stall a hot caller — Watch evaluates this on every stream frame and
// must stay responsive. The second return lists the dexes whose query failed (or
// timed out) this call, so a streaming caller can surface degraded coverage
// instead of silently dropping a sub-dex's liquidation risk.
func (c *Client) subDexPositionsTimeout(ctx context.Context, coin string, timeout time.Duration) ([]PositionView, []string) {
	var out []PositionView
	var stale []string
	for _, dex := range c.cfg.PerpDexs {
		d := strings.ToLower(strings.TrimSpace(dex))
		if d == "" {
			continue
		}
		cctx, cancel := ctx, func() {}
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, timeout)
		}
		st, err := c.info.UserStateForDex(cctx, c.queryAddr, d)
		cancel()
		if err != nil {
			stale = append(stale, d)
			continue
		}
		out = append(out, normalizedDexPositions(st, d, coin)...)
	}
	return out, stale
}

// allOpenOrders returns resting orders across the main dex AND every configured
// HIP-3 sub-dex. frontendOpenOrders is per-dex (like positions), so without this
// sweep sub-dex resting orders are invisible to orders / cancel / modify / panic.
func (c *Client) allOpenOrders(ctx context.Context) ([]hl.FrontendOpenOrder, error) {
	orders, _, err := c.allOpenOrdersStale(ctx)
	return orders, err
}

// allOpenOrdersStale is allOpenOrders with the silently-skipped sub-dexes
// REPORTED: stale lists each configured sub-dex whose sweep failed (its resting
// orders are missing from the result). Read surfaces stay tolerant of a partial
// sweep (one slow sub-dex must not hide main-dex orders), but the risk gates
// consume stale to FAIL CLOSED — invisible resting orders are invisible
// exposure (see fetchGateBook / portfolioEquitySnapshot).
func (c *Client) allOpenOrdersStale(ctx context.Context) (orders []hl.FrontendOpenOrder, stale []string, err error) {
	orders, err = c.info.FrontendOpenOrders(ctx, c.queryAddr)
	if err != nil {
		return nil, nil, err
	}
	for _, dex := range c.cfg.PerpDexs {
		d := strings.ToLower(strings.TrimSpace(dex))
		if d == "" {
			continue
		}
		sub, serr := c.info.FrontendOpenOrdersForDex(ctx, c.queryAddr, d)
		if serr != nil {
			stale = append(stale, d)
			continue
		}
		orders = append(orders, sub...)
	}
	return orders, stale, nil
}

// Orders returns resting orders, optionally filtered by coin, plus read-health
// metadata naming any sub-dex whose sweep failed — those resting orders are
// MISSING from the result, which the agent must see (NEXT-2 item 1). Tolerant:
// exit 0 with warnings, never a silently smaller book.
func (c *Client) Orders(ctx context.Context, coin string) ([]hl.FrontendOpenOrder, ReadMeta, error) {
	var rm ReadMeta
	if err := c.requireQueryAddr(); err != nil {
		return nil, rm, err
	}
	orders, stale, err := c.allOpenOrdersStale(ctx)
	if err != nil {
		return nil, rm, mapNetwork("open_orders", err)
	}
	for _, d := range stale {
		rm.addDegraded("open orders ("+d+")", d)
	}
	out := []hl.FrontendOpenOrder{}
	for _, o := range orders {
		// Tolerate the HIP-3 prefix on either side: sub-dex orders come back as
		// "xyz:GOLD", so `orders --coin GOLD` (bare) must still match them.
		if coin == "" || matchesCoinFilter(o.Coin, coin) {
			out = append(out, o)
		}
	}
	return out, rm, nil
}

// OrderStatus queries one order by oid or cloid (§5.4 retry protocol).
func (c *Client) OrderStatus(ctx context.Context, oid *int64, cloid string) (*hl.OrderQueryResult, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	var (
		res *hl.OrderQueryResult
		err error
	)
	if oid != nil {
		res, err = c.info.QueryOrderByOid(ctx, c.queryAddr, *oid)
	} else {
		// A cloid can map to several orders over its lifetime: a modify cancels
		// the old order and re-places a replacement carrying the same cloid.
		// HL's orderStatus-by-cloid may return the original (now canceled)
		// order, which would mislead the retry protocol into thinking a live
		// order is gone — risking a double-place. Prefer a live resting order
		// carrying this cloid; fall back to the historical query only when the
		// live sweep read EVERY dex successfully and found none. A DEGRADED
		// sweep (main read failed, or any configured sub-dex skipped) that does
		// not find the order proves nothing — the order may be resting on the
		// unreadable dex, so answering with the historical (possibly canceled
		// predecessor) row would invite "absent → resubmit" → double-place
		// (NEXT-2 item 2). That case is a retryable exit 40, never a confident
		// absent.
		live, found, sweepErr := c.openOrderByCloid(ctx, cloid)
		if found {
			return live, nil
		}
		if sweepErr != nil {
			return nil, output.Network("order_status_unconfirmed",
				"cannot confirm order "+cloid+" is gone: the live open-orders sweep was degraded ("+sweepErr.Error()+") — it may still be resting on an unreadable dex").Retry().
				WithHint("transient read failure — re-run `order status --cloid " + cloid + "` when reads recover; do NOT resubmit on this answer")
		}
		res, err = c.info.QueryOrderByCloid(ctx, c.queryAddr, cloid)
	}
	if err != nil {
		return nil, mapNetwork("order_status", err)
	}
	return res, nil
}

// openOrderByCloid returns the live resting order carrying cloid (shaped as an
// OrderQueryResult), if one exists. Used so order-status reflects the live order
// after a modify, not a stale canceled predecessor sharing the same cloid.
// found=false with a non-nil error means the sweep was DEGRADED (total read
// failure, or a configured sub-dex silently skipped) — "not found" from such a
// sweep is inconclusive and must not fall through to the historical query.
func (c *Client) openOrderByCloid(ctx context.Context, cloid string) (res *hl.OrderQueryResult, found bool, sweepErr error) {
	orders, stale, err := c.allOpenOrdersStale(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, o := range orders {
		if o.Cloid == nil || !strings.EqualFold(*o.Cloid, cloid) {
			continue
		}
		oo := o
		return &hl.OrderQueryResult{
			Status: hl.OrderQueryStatusSuccess,
			Order: hl.OrderQueryResponse{
				Status:          "open",
				StatusTimestamp: oo.Timestamp,
				Order: hl.QueriedOrder{
					Coin: oo.Coin, Side: oo.Side,
					LimitPx: f2s(oo.LimitPx), Sz: f2s(oo.Sz), Oid: oo.Oid,
					Timestamp: oo.Timestamp, TriggerCondition: oo.TriggerCondition,
					IsTrigger: oo.IsTrigger, TriggerPx: f2s(oo.TriggerPx),
					IsPositionTpsl: oo.IsPositionTpSl, ReduceOnly: oo.ReduceOnly,
					OrderType: oo.OrderType, OrigSz: f2s(oo.OrigSz), Cloid: oo.Cloid,
				},
			},
		}, true, nil
	}
	if len(stale) > 0 {
		return nil, false, fmt.Errorf("open orders unreadable on sub-dex(es): %s", strings.Join(stale, ", "))
	}
	return nil, false, nil
}

// Fills returns recent fills, optionally since a timestamp, capped at limit.
// A time-bounded read (since != nil) is paged to completion past HL's per-page
// cap (NEXT-2 item 7); ReadMeta.Truncated reports a read stopped at the hard
// safety cap — never a silent shortfall.
func (c *Client) Fills(ctx context.Context, since *int64, limit int) ([]hl.Fill, ReadMeta, error) {
	var rm ReadMeta
	if err := c.requireQueryAddr(); err != nil {
		return nil, rm, err
	}
	var (
		fills []hl.Fill
		err   error
	)
	if since != nil {
		fills, rm, err = pageByTime(*since, fillsPageSize, "fills",
			func(f hl.Fill) int64 { return f.Time },
			func(start int64) ([]hl.Fill, error) {
				return c.info.UserFillsByTime(ctx, c.queryAddr, start, nil, nil)
			})
	} else {
		fills, err = c.info.UserFills(ctx, hl.UserFillsParams{Address: c.queryAddr})
	}
	if err != nil {
		return nil, ReadMeta{}, mapNetwork("user_fills", err)
	}
	// newest first
	sort.Slice(fills, func(i, j int) bool { return fills[i].Time > fills[j].Time })
	if limit > 0 && len(fills) > limit {
		fills = fills[:limit]
	}
	if fills == nil {
		fills = []hl.Fill{}
	}
	return fills, rm, nil
}

// TwapRunning is one live TWAP's progress (a flattened, snake_cased TwapState).
type TwapRunning struct {
	TwapID           int64  `json:"twap_id"`
	Coin             string `json:"coin"`
	Side             string `json:"side,omitempty"` // buy | sell
	Size             string `json:"size,omitempty"`
	ExecutedSize     string `json:"executed_size,omitempty"`
	ExecutedNotional string `json:"executed_notional,omitempty"`
	Minutes          int    `json:"minutes,omitempty"`
	ReduceOnly       bool   `json:"reduce_only,omitempty"`
	Randomize        bool   `json:"randomize,omitempty"`
	// ProgressPct is executed_size / size * 100 (empty when size is unknown).
	ProgressPct string `json:"progress_pct,omitempty"`
	StartedMs   int64  `json:"started_ms,omitempty"`
}

// TwapStatusView is the `twap status` payload: the live TWAPs (with progress) and
// their per-slice fills, optionally filtered by coin and/or id.
type TwapStatusView struct {
	Running    []TwapRunning      `json:"running"`
	SliceFills []hl.TwapSliceFill `json:"slice_fills"`
}

// twapSideStr maps HL's raw twap side ("B"/"A") to the buy|sell vocabulary used
// across the rest of the surface; an unrecognized value passes through.
func twapSideStr(s string) string {
	switch s {
	case "B":
		return "buy"
	case "A":
		return "sell"
	default:
		return s
	}
}

// TwapStatus reports the user's live TWAPs and their slice fills. A TWAP drops off
// the running list once it completes, so an id that returns no running entry has
// finished (or never started) — this is the read an agent runs on a TWAP-place
// timeout (exit 42) to confirm a submission before resubmitting. coin/id are
// optional filters (id 0 = any, coin "" = all).
func (c *Client) TwapStatus(ctx context.Context, coin string, id int64) (*TwapStatusView, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	states, err := c.info.TwapStates(ctx, c.queryAddr)
	if err != nil {
		return nil, mapNetwork("twap_states", err)
	}
	view := &TwapStatusView{Running: []TwapRunning{}, SliceFills: []hl.TwapSliceFill{}}
	for _, s := range states {
		if coin != "" && !matchesCoinFilter(s.Coin, coin) {
			continue
		}
		if id != 0 && s.ID != id {
			continue
		}
		r := TwapRunning{
			TwapID: s.ID, Coin: s.Coin, Side: twapSideStr(s.Side), Size: s.Sz,
			ExecutedSize: s.ExecutedSz, ExecutedNotional: s.ExecutedNtl,
			Minutes: s.Minutes, ReduceOnly: s.ReduceOnly, Randomize: s.Randomize,
			StartedMs: s.Timestamp,
		}
		if sz := parseFloatSafe(s.Sz); sz > 0 {
			r.ProgressPct = f2s(parseFloatSafe(s.ExecutedSz) / sz * 100)
		}
		view.Running = append(view.Running, r)
	}
	// Slice-fill detail is supplementary — the running list is the headline, so a
	// fills fetch error must not fail the whole status read.
	if fills, ferr := c.info.UserTwapSliceFills(ctx, c.queryAddr); ferr == nil {
		for _, f := range fills {
			if id != 0 && f.TwapID != id {
				continue
			}
			if coin != "" && !matchesCoinFilter(f.Fill.Coin, coin) {
				continue
			}
			view.SliceFills = append(view.SliceFills, f)
		}
	}
	return view, nil
}

// HistoricalOrders returns the user's recent closed-order lifecycle (filled,
// canceled, rejected, expired) — the post-mortem / reconciliation read. Newest
// first; limit<=0 returns all. Returns the same per-order shape as `order status`.
func (c *Client) HistoricalOrders(ctx context.Context, limit int) ([]hl.OrderQueryResponse, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	orders, err := c.info.HistoricalOrders(ctx, c.queryAddr)
	if err != nil {
		return nil, mapNetwork("historical_orders", err)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].StatusTimestamp > orders[j].StatusTimestamp })
	if limit > 0 && len(orders) > limit {
		orders = orders[:limit]
	}
	if orders == nil {
		orders = []hl.OrderQueryResponse{}
	}
	return orders, nil
}

// PredictedFundings returns the forward (next-interval) funding forecast per coin
// and venue — the funding-carry signal. coin "" returns all; otherwise filters.
func (c *Client) PredictedFundings(ctx context.Context, coin string) ([]hl.PredictedFunding, error) {
	all, err := c.info.PredictedFundings(ctx)
	if err != nil {
		return nil, mapNetwork("predicted_fundings", err)
	}
	if coin == "" {
		return all, nil
	}
	out := []hl.PredictedFunding{}
	for _, pf := range all {
		if matchesCoinFilter(pf.Coin, coin) {
			out = append(out, pf)
		}
	}
	return out, nil
}

func defaultSince(since *int64, d time.Duration) int64 {
	if since != nil {
		return *since
	}
	return time.Now().Add(-d).UnixMilli()
}

// Funding returns funding payments since a timestamp (default 7d), paged to
// completion past HL's per-page cap (NEXT-2 item 7).
func (c *Client) Funding(ctx context.Context, since *int64) ([]hl.UserFundingHistory, ReadMeta, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, ReadMeta{}, err
	}
	f, rm, err := pageByTime(defaultSince(since, 7*24*time.Hour), ledgerPageSize, "funding",
		func(r hl.UserFundingHistory) int64 { return r.Time },
		func(start int64) ([]hl.UserFundingHistory, error) {
			return c.info.UserFundingHistory(ctx, c.queryAddr, start, nil)
		})
	if err != nil {
		return nil, ReadMeta{}, mapNetwork("user_funding", err)
	}
	if f == nil {
		f = []hl.UserFundingHistory{}
	}
	return f, rm, nil
}

// Ledger returns non-funding ledger updates since a timestamp (default 30d),
// paged to completion past HL's per-page cap (NEXT-2 item 7).
func (c *Client) Ledger(ctx context.Context, since *int64) ([]hl.UserNonFundingLedgerUpdates, ReadMeta, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, ReadMeta{}, err
	}
	l, rm, err := pageByTime(defaultSince(since, 30*24*time.Hour), ledgerPageSize, "ledger",
		func(r hl.UserNonFundingLedgerUpdates) int64 { return r.Time },
		func(start int64) ([]hl.UserNonFundingLedgerUpdates, error) {
			return c.info.UserNonFundingLedgerUpdates(ctx, c.queryAddr, start, nil)
		})
	if err != nil {
		return nil, ReadMeta{}, mapNetwork("user_ledger", err)
	}
	if l == nil {
		l = []hl.UserNonFundingLedgerUpdates{}
	}
	return l, rm, nil
}

// Balance returns perp + spot balances.
func (c *Client) Balance(ctx context.Context) (*BalanceView, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	st, err := c.info.UserState(ctx, c.queryAddr)
	if err != nil {
		return nil, mapNetwork("clearinghouse_state", err)
	}
	bv := &BalanceView{
		Perp: PerpBalance{
			AccountValue:     st.MarginSummary.AccountValue,
			TotalMarginUsed:  st.MarginSummary.TotalMarginUsed,
			TotalNotionalPos: st.MarginSummary.TotalNtlPos,
			Withdrawable:     st.Withdrawable,
		},
		Spot: []hl.SpotBalance{},
	}
	if ss, serr := c.info.SpotUserState(ctx, c.queryAddr); serr == nil && ss != nil {
		bv.Spot = ss.Balances
		bv.AvailableCollateral = usdcCollateral(ss)
	}
	// A unified account: available_collateral is the single spendable balance behind
	// the main dex AND every sub-dex. Flag it so account_value 0.0 isn't misread.
	bv.CollateralShared = bv.AvailableCollateral != ""
	for _, dex := range c.cfg.PerpDexs {
		d := strings.ToLower(strings.TrimSpace(dex))
		if d == "" {
			continue
		}
		dst, derr := c.info.UserStateForDex(ctx, c.queryAddr, d)
		if derr != nil {
			continue
		}
		if bv.PerpDexs == nil {
			bv.PerpDexs = map[string]PerpBalance{}
		}
		pb := PerpBalance{
			AccountValue:     dst.MarginSummary.AccountValue,
			TotalMarginUsed:  dst.MarginSummary.TotalMarginUsed,
			TotalNotionalPos: dst.MarginSummary.TotalNtlPos,
			Withdrawable:     dst.Withdrawable,
		}
		if bv.CollateralShared {
			pb.Note = sharedCollateralNote
		}
		bv.PerpDexs[d] = pb
	}
	return bv, nil
}

// PnlPoint is one (timestamp, value) sample of a portfolio time series.
type PnlPoint struct {
	Time  int64  `json:"time"`
	Value string `json:"value"`
}

// PnlWindow is one named window (day/week/month/allTime) of the portfolio series.
type PnlWindow struct {
	Window              string     `json:"window"`
	AccountValueHistory []PnlPoint `json:"account_value_history"`
	PnlHistory          []PnlPoint `json:"pnl_history"`
	Vlm                 string     `json:"vlm"`
}

// Pnl returns the account-value / PnL / volume time series (§9 pnl). The raw
// endpoint is a loosely-typed [label, AccountHistory] tuple array; we parse it.
func (c *Client) Pnl(ctx context.Context) ([]PnlWindow, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	raw, err := c.info.Portfolio(ctx, c.queryAddr)
	if err != nil {
		return nil, mapNetwork("portfolio", err)
	}
	out := []PnlWindow{}
	for _, entry := range raw {
		if len(entry) < 2 {
			continue
		}
		label, _ := entry[0].String()
		var ah hl.AccountHistory
		if entry[1].Parse(&ah) != nil {
			continue
		}
		out = append(out, PnlWindow{
			Window:              label,
			Vlm:                 ah.Vlm,
			AccountValueHistory: parsePnlPoints(ah.AccountValueHistory),
			PnlHistory:          parsePnlPoints(ah.PnlHistory),
		})
	}
	return out, nil
}

func parsePnlPoints(rows []hl.MixedArray) []PnlPoint {
	pts := []PnlPoint{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		var ts int64
		_ = row[0].Parse(&ts)
		val, _ := row[1].String()
		pts = append(pts, PnlPoint{Time: ts, Value: val})
	}
	return pts
}

// Book returns the L2 order book, trimmed to levels per side.
func (c *Client) Book(ctx context.Context, coin string, levels int) (*BookView, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	b, err := c.info.L2Snapshot(ctx, mk.Coin)
	if err != nil {
		return nil, mapNetwork("l2_book", err)
	}
	bv := &BookView{Coin: b.Coin, Time: b.Time, Bids: []BookLevel{}, Asks: []BookLevel{}}
	if len(b.Levels) > 0 {
		bv.Bids = trimLevels(b.Levels[0], levels)
	}
	if len(b.Levels) > 1 {
		bv.Asks = trimLevels(b.Levels[1], levels)
	}
	return bv, nil
}

func trimLevels(in []hl.Level, n int) []BookLevel {
	out := []BookLevel{}
	for i, l := range in {
		if n > 0 && i >= n {
			break
		}
		out = append(out, BookLevel{Px: f2s(l.Px), Sz: f2s(l.Sz), N: l.N})
	}
	return out
}

// Bbo returns the best bid/offer derived from the L2 snapshot.
func (c *Client) Bbo(ctx context.Context, coin string) (*BboView, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	b, err := c.info.L2Snapshot(ctx, mk.Coin)
	if err != nil {
		return nil, mapNetwork("l2_book", err)
	}
	v := &BboView{Coin: b.Coin, Time: b.Time}
	if len(b.Levels) > 0 && len(b.Levels[0]) > 0 {
		v.Bid = f2s(b.Levels[0][0].Px)
		v.BidSz = f2s(b.Levels[0][0].Sz)
	}
	if len(b.Levels) > 1 && len(b.Levels[1]) > 0 {
		v.Ask = f2s(b.Levels[1][0].Px)
		v.AskSz = f2s(b.Levels[1][0].Sz)
	}
	if v.Bid != "" && v.Ask != "" {
		// Derive mid/spread with decimal math on the (clean) string prices, NOT
		// float64 — (bid+ask)/2 and ask−bid in binary float render as noise like
		// "0.020000000004074536", violating the clean-string contract. Dividing a
		// sum of tick-aligned prices by 2 terminates exactly, so no precision is
		// lost; .String() trims trailing zeros.
		bid, errB := decimal.NewFromString(v.Bid)
		ask, errA := decimal.NewFromString(v.Ask)
		if errB == nil && errA == nil {
			v.Mid = ask.Add(bid).Div(decimal.NewFromInt(2)).String()
			v.Spread = ask.Sub(bid).String()
		}
	}
	return v, nil
}

// Mids returns all mid prices.
func (c *Client) Mids(ctx context.Context) (map[string]string, error) {
	m, err := c.info.AllMids(ctx)
	if err != nil {
		return nil, mapNetwork("all_mids", err)
	}
	// Merge each configured HIP-3 sub-dex's mids (keyed by "<dex>:<coin>").
	for _, dex := range c.cfg.PerpDexs {
		d := strings.ToLower(strings.TrimSpace(dex))
		if d == "" {
			continue
		}
		if dm, derr := c.info.AllMidsForDex(ctx, d); derr == nil {
			for k, v := range dm {
				m[k] = v
			}
		}
	}
	return m, nil
}

// RawInfo posts an arbitrary {type, ...params} body to the HL /info endpoint and
// returns the decoded JSON. It is the escape hatch for info endpoints deliverator
// has no dedicated command for (historicalOrders, userTwapSliceFills,
// userTwapHistory, spotMetaAndAssetCtxs, predictedFundings, fundingHistory,
// vaultDetails, tokenDetails, ...).
func (c *Client) RawInfo(ctx context.Context, body map[string]any) (any, error) {
	var out any
	if err := c.InfoPost(ctx, body, &out); err != nil {
		return nil, mapNetwork("info", err)
	}
	return out, nil
}

// Candles returns OHLCV candles for a coin/interval since a timestamp.
func (c *Client) Candles(ctx context.Context, coin, interval string, since *int64) ([]hl.Candle, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	start := defaultSince(since, 24*time.Hour)
	end := time.Now().UnixMilli()
	cs, err := c.info.CandlesSnapshot(ctx, mk.Coin, interval, start, end)
	if err != nil {
		return nil, mapNetwork("candles", err)
	}
	if cs == nil {
		cs = []hl.Candle{}
	}
	return cs, nil
}

// Ctx returns the market context for a coin.
func (c *Client) Ctx(ctx context.Context, coin string) (*CtxView, error) {
	mk, ok := c.meta.Lookup(coin)
	if !ok {
		return nil, unknownCoin(coin)
	}
	if mk.IsOutcome {
		return c.outcomeCtx(ctx, mk)
	}
	if mk.IsSpot {
		return c.spotCtx(ctx, mk)
	}
	// A sub-dex coin's context lives in that HIP-3 dex's metaAndAssetCtxs.
	var dexParam *string
	if d := dexOf(coin); d != "" {
		dexParam = &d
	}
	mac, err := c.info.MetaAndAssetCtxs(ctx, hl.MetaAndAssetCtxsParams{Dex: dexParam})
	if err != nil {
		return nil, mapNetwork("meta_and_asset_ctxs", err)
	}
	return perpCtxView(mac, mk)
}

// perpCtxView slices one coin's context out of an ALREADY-FETCHED
// metaAndAssetCtxs payload — shared by Ctx (one coin, one fetch) and Snapshot,
// which fetches each dex's payload ONCE and derives every requested coin from
// it locally (NEXT-2 item 8: N concurrent heavyweight fetches per snapshot
// self-inflicted the per-IP 429s the snapshot exists to avoid).
func perpCtxView(mac *hl.MetaAndAssetCtxs, mk Market) (*CtxView, error) {
	for i, a := range mac.Meta.Universe {
		if strings.EqualFold(a.Name, mk.Coin) && i < len(mac.Ctxs) {
			ac := mac.Ctxs[i]
			return &CtxView{
				Coin:         a.Name,
				MarkPx:       ac.MarkPx,
				OraclePx:     ac.OraclePx,
				MidPx:        ac.MidPx,
				Funding:      ac.Funding,
				OpenInterest: ac.OpenInterest,
				Premium:      ac.Premium,
				ImpactPxs:    ac.ImpactPxs,
				DayNtlVlm:    ac.DayNtlVlm,
				PrevDayPx:    ac.PrevDayPx,
			}, nil
		}
	}
	return nil, unknownCoin(mk.Coin)
}

// spotCtx maps a spot pair's context (no funding/OI/oracle; adds supply) into the
// shared CtxView. The ctxs slice is keyed by the pair's universe INDEX, not its
// position in the universe slice — the two diverge for most pairs and the ctxs
// slice is longer than the universe — so index by p.Index (matching meta.go's
// asset-id mapping), never by the loop position.
func (c *Client) spotCtx(ctx context.Context, mk Market) (*CtxView, error) {
	spotMeta, ctxs, err := c.info.SpotMetaAndAssetCtxs(ctx)
	if err != nil {
		return nil, mapNetwork("spot_meta_and_asset_ctxs", err)
	}
	return spotCtxView(spotMeta, ctxs, mk)
}

// spotCtxView slices one pair's context out of an already-fetched
// spotMetaAndAssetCtxs payload (shared by spotCtx and Snapshot — see
// perpCtxView for why).
func spotCtxView(spotMeta *hl.SpotMeta, ctxs []hl.SpotAssetCtx, mk Market) (*CtxView, error) {
	for _, p := range spotMeta.Universe {
		if !strings.EqualFold(p.Name, mk.Coin) {
			continue
		}
		if p.Index < 0 || p.Index >= len(ctxs) {
			return nil, unknownCoin(mk.Coin)
		}
		ac := ctxs[p.Index]
		return &CtxView{
			Coin:              p.Name,
			IsSpot:            true,
			MarkPx:            ac.MarkPx,
			MidPx:             ac.MidPx,
			DayNtlVlm:         ac.DayNtlVlm,
			PrevDayPx:         ac.PrevDayPx,
			CirculatingSupply: ac.CirculatingSupply,
			TotalSupply:       ac.TotalSupply,
		}, nil
	}
	return nil, unknownCoin(mk.Coin)
}

// outcomeCtx builds a HIP-4 outcome's market context. There is no outcome ctx
// endpoint (no outcomeMetaAndAssetCtxs), so it is assembled from the cached market
// metadata + the "#<enc>" mid (allMids) + the best bid/ask (l2Book). The price is a
// probability in (0,1); ComplementMid is the other side's implied probability.
func (c *Client) outcomeCtx(ctx context.Context, mk Market) (*CtxView, error) {
	mids, err := c.info.AllMids(ctx)
	if err != nil {
		return nil, mapNetwork("all_mids", err)
	}
	return c.outcomeCtxWith(ctx, mids, mk)
}

// outcomeCtxWith is outcomeCtx with the allMids payload PRE-FETCHED (Snapshot
// fetches it once for all outcome coins); the per-coin l2Book fetch remains —
// best bid/ask is inherently per-coin data.
func (c *Client) outcomeCtxWith(ctx context.Context, mids map[string]string, mk Market) (*CtxView, error) {
	mid := mids[mk.Coin]
	cv := &CtxView{
		Coin:             mk.Coin,
		IsOutcome:        true,
		Side:             mk.Side,
		Title:            mk.Title,
		ResolutionStatus: mk.ResolutionStatus,
		Expiry:           mk.Expiry,
		MarkPx:           mid,
		MidPx:            mid,
	}
	if m := parseFloatSafe(mid); m > 0 && m < 1 {
		cv.ComplementMid = f2s(1 - m)
	}
	if book, err := c.info.L2Snapshot(ctx, mk.Coin); err == nil && len(book.Levels) == 2 {
		if len(book.Levels[0]) > 0 {
			cv.BestBid = f2s(book.Levels[0][0].Px)
		}
		if len(book.Levels[1]) > 0 {
			cv.BestAsk = f2s(book.Levels[1][0].Px)
		}
	}
	return cv, nil
}

// Limits queries the per-address rate-limit budget (§7).
func (c *Client) Limits(ctx context.Context) (*LimitsView, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	var r struct {
		CumVlm           string `json:"cumVlm"`
		NRequestsUsed    int    `json:"nRequestsUsed"`
		NRequestsCap     int    `json:"nRequestsCap"`
		NRequestsSurplus int    `json:"nRequestsSurplus"`
	}
	if err := c.InfoPost(ctx, map[string]any{"type": "userRateLimit", "user": c.queryAddr}, &r); err != nil {
		return nil, mapNetwork("user_rate_limit", err)
	}
	return &LimitsView{
		CumVlm:    r.CumVlm,
		Used:      r.NRequestsUsed,
		Cap:       r.NRequestsCap,
		Surplus:   r.NRequestsSurplus,
		Remaining: r.NRequestsCap - r.NRequestsUsed,
	}, nil
}

// BuilderStatus reports the configured builder fee and, if configured, the
// master-approved maximum (§9 builder status).
func (c *Client) BuilderStatus(ctx context.Context) (*BuilderView, error) {
	v := &BuilderView{
		Address:      c.cfg.Builder.Address,
		FeeTenthsBps: c.cfg.Builder.FeeTenthsBps,
		AttachMode:   c.cfg.Builder.AttachMode,
	}
	if c.cfg.Builder.Address != "" && c.queryAddr != "" {
		var raw float64
		if err := c.InfoPost(ctx, map[string]any{
			"type":    "maxBuilderFee",
			"user":    c.queryAddr,
			"builder": strings.ToLower(c.cfg.Builder.Address),
		}, &raw); err == nil {
			n := int(raw)
			v.ApprovedMaxTenths = &n
		}
	}
	return v, nil
}

// ---------- small helpers ----------

// f2s formats a float for the JSON envelope. A non-finite value renders as "" (an
// omitempty field disappears) rather than the literal "NaN"/"+Inf", so a downstream
// agent never has to branch on a non-numeric metric (audit S2).
func f2s(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func unknownCoin(coin string) error {
	return output.Validation("unknown_coin", "unknown coin "+coin).
		WithHint("run `deliverator markets` for the tradable universe")
}

// mapNetwork categorizes a read-path transport error. A bare network failure is
// exit 40, but a 429 (per-IP rate-limit) and a timeout each get their own code so
// an agent can react correctly: back off with retry_after on 41, and distinguish
// "no answer yet" from "unreachable" on 42. Reads are side-effect-free, so an
// hl.TransportError lands here as a plain retryable network error regardless of
// its Sent phase (it unwraps cleanly through the checks below) — only the WRITE
// mapper, mapExchangeErr, branches on Sent, because there a possibly-sent
// failure means the order outcome is unknown (§5.4).
func mapNetwork(code string, err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	// A Hyperliquid 429 carries the HTTP status on APIError (a non-JSON 429 body
	// falls through to the string check below).
	var apiErr hl.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
		return output.RateLimit("ip_rate_limited", "Hyperliquid returned 429 (per-IP weight exceeded)").
			WithRetryAfter(2000)
	}
	s := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(s, "deadline") || strings.Contains(s, "timeout") || strings.Contains(s, "timed out"):
		// A read timed out. Reads are side-effect-free, so a retry is always safe —
		// no "outcome unknown" hazard (that §5.4 caveat is write-only).
		return output.Timeout("timeout", "read request timed out").
			WithHint("transient — safe to retry the read").Retry()
	case strings.Contains(s, "429") || strings.Contains(s, "too many") || strings.Contains(s, "rate limit"):
		return output.RateLimit("rate_limited", err.Error()).WithRetryAfter(10000)
	}
	return output.Network(code, err.Error()).Retry()
}
