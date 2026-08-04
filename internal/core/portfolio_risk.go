package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
)

// Portfolio-level risk gates (#43). These are ACCOUNT-WIDE, enforced in core
// before signing, so a hallucinating or mis-sized agent cannot exceed them by
// switching invocation — unlike the per-order/per-coin caps, they bound the book
// as a whole. They are evaluated against the RESULTING book (current positions +
// resting non-reduce-only orders' worst-case adds + the proposed NEW exposure —
// without the resting term, sequential single orders each passing against the
// FILLED book tunnel arbitrarily far past every cap); reduce-only/close legs add
// no exposure and are exempt. All gates default to 0 = off, so the safe baseline
// is unchanged until the operator opts in, and the whole path (incl. the live
// snapshot fetch) is skipped when none is configured.
//
// They apply on every path that can GROW worst-case exposure: Place / bracket
// entry / batch+grid / twap, AND modify/modify-batch. A modify REPLACES its own
// resting order, so the gates evaluate the book with the old order swapped for
// the replacement at its new size/price (never double-counting itself — a
// routine grid re-price is not false-rejected); a modify that does not raise
// the order's worst-case contribution is de-risking and skips the gates
// entirely, parity with the all-reduce-only batch exemption (see Modify).

// portfolioGuardsActive reports whether any account-wide gate is configured. When
// false the entire portfolio-check path is skipped (zero added latency / behavior).
func (c *Client) portfolioGuardsActive() bool {
	r := c.riskConfig()
	return r.MaxAccountLeverage > 0 || r.MaxNetExposureUSD > 0 || r.MaxConcentrationPctPerCoin > 0 ||
		r.MaxDrawdownPct > 0 || r.MaxDailyLossUSD > 0 || r.MaxDailyLossPct > 0 ||
		r.MaxOpenPositions > 0
}

// outcomeQuestionCapActive reports whether the per-question outcome cap should run for
// THIS invocation. Unlike the coin-agnostic gates, the cap governs only outcome coins,
// so a non-outcome perp/spot order must not force the (fail-closed) account-state read
// on its behalf — the cap is only relevant when at least one touched coin is an outcome
// coin ("#<enc>"). This keeps a transient read failure from rejecting an unrelated order.
func (c *Client) outcomeQuestionCapActive(deltas []exposureDelta) bool {
	if c.riskConfig().MaxOutcomeQuestionNotionalUSD <= 0 {
		return false
	}
	for _, d := range deltas {
		if strings.HasPrefix(d.coin, "#") {
			return true
		}
	}
	return false
}

// outcomeQuestionKey groups outcome coins that are the SAME bet. A multi-outcome
// question (Question != 0) buckets all its outcomes together; a standalone binary
// (Question == 0) buckets only its own Yes/No pair, keyed on the Outcome id — so two
// unrelated Question==0 binaries never merge into one phantom bucket. Non-outcome
// coins return ("", false).
func outcomeQuestionKey(mk Market) (string, string, bool) {
	if !mk.IsOutcome {
		return "", "", false
	}
	if mk.Question != 0 {
		return "q:" + strconv.Itoa(mk.Question), mk.QuestionName, true
	}
	name := mk.QuestionName
	if name == "" {
		name = mk.Title
	}
	return "o:" + strconv.Itoa(mk.Outcome), name, true
}

// heaviestOutcomeQuestion folds the resulting book's worst-case per-coin notional
// (positions + this order's delta + resting pending-adds) into per-question buckets
// and returns the heaviest one. Each outcome coin is valued at the same worst case
// the per-coin gate uses (max of |v + resting buys| and |v − resting sells|), so the
// per-question cap is consistent with max_concentration_pct_per_coin — it just sums
// the legs the per-coin view cannot see are one bet. Non-outcome coins are ignored.
func (c *Client) heaviestOutcomeQuestion(resulting map[string]float64, pending map[string]pendingAdd) (label string, notional float64) {
	byKey := map[string]float64{}
	nameByKey := map[string]string{}
	seen := map[string]bool{}
	fold := func(coin string, v float64, p pendingAdd) {
		mk, ok := c.meta.Lookup(coin)
		if !ok {
			return
		}
		key, name, isOutcome := outcomeQuestionKey(mk)
		if !isOutcome {
			return
		}
		worst := math.Max(absF(v+p.buy), absF(v-p.sell))
		byKey[key] += worst
		if _, ok := nameByKey[key]; !ok {
			nameByKey[key] = name
		}
	}
	for coin, v := range resulting {
		seen[coin] = true
		fold(coin, v, pending[coin])
	}
	// Coins with only resting orders (no position) still deploy capital toward the
	// question — fold them too.
	for coin, p := range pending {
		if seen[coin] {
			continue
		}
		fold(coin, 0, p)
	}
	for key, n := range byKey {
		if n > notional {
			notional, label = n, key
			if nm := nameByKey[key]; nm != "" {
				label = nm
			}
		}
	}
	return label, notional
}

// reduceOnlyFlipErr rejects a reduce-only order that cannot actually REDUCE the
// current open position in that coin (#44, review P0-A):
//
//   - flat (no open position): there is nothing to reduce — the flag is a
//     mislabel (hallucinated, or the position was already closed by another
//     fill), and the wire order would only be rejected/no-op'd by HL anyway;
//   - same side as the position: a "reduce-only" buy on a long (or sell on a
//     short) adds exposure, it never reduces;
//   - oversized on the reducing side: it could only fill by crossing zero into
//     opposite exposure (which HL prevents, silently dropping the excess).
//
// All three reject as risk (CatRisk, exit 20), matching the original flip
// guard: the reduce-only flag exempts an order from every notional/portfolio
// cap, so a reduce-only order that is not actually reducing is a risk-gate
// breach, not merely malformed input. The check is SKIPPED only when the
// position cannot be read at all (no query address / read error) — that is
// deliberate, not a gap: post-P0-A every reduce-only order carries r:true on
// the wire, so the EXCHANGE guarantees it can only reduce (a mislabeled one is
// rejected/clamped server-side). De-risking must never be blocked by a local
// read failure; only exposure-ADDING orders fail closed on unreadable state
// (see currentPositionNotional / checkPortfolioGates).
func (c *Client) reduceOnlyFlipErr(ctx context.Context, coin string, side Side, orderSzAbs float64) error {
	szi, ok := c.positionSziRead(ctx, coin)
	if !ok {
		return nil
	}
	if szi == 0 {
		return output.Risk("reduce_only_flat",
			"reduce-only "+side.String()+" rejected: no open position in "+coin+" — there is nothing to reduce").
			WithHint("open a position first, or drop reduce-only to open new exposure under the risk caps")
	}
	posSide, reducing := "long", Sell
	if szi < 0 {
		posSide, reducing = "short", Buy
	}
	if side != reducing {
		return output.Risk("reduce_only_same_side",
			fmt.Sprintf("reduce-only %s rejected: the open %s position is %s — a same-side order adds exposure, it cannot reduce",
				side, coin, posSide)).
			WithHint("reduce a " + posSide + " with a " + reducing.String() + ", or drop reduce-only to add exposure under the risk caps")
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

// parseRiskFloat is the fail-closed parser for values that feed a signing gate.
// Read-only views may normalize malformed exchange strings to zero, but a risk
// gate must distinguish a real zero from an unreadable value.
func parseRiskFloat(field, s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		if err == nil {
			err = fmt.Errorf("non-finite value")
		}
		return 0, fmt.Errorf("%s %q is invalid: %w", field, s, err)
	}
	return f, nil
}

// equityOf is the DEPRECATED max(perp account_value, available USDC collateral)
// equity heuristic. Both inputs drop by the margin reserved when positions
// open, understating a unified account's equity — the gates, margin_ratio, and
// preview all moved to accountEquity (basis 2, commit 2c000c9 + NEXT-2 item 6).
// Sole remaining caller: copy.go's scale factor (tracked separately in the
// copy-trading cluster — do not add new callers).
func equityOf(accountValue, availableCollateral string) float64 {
	e := parseFloatSafe(accountValue)
	if c := parseFloatSafe(availableCollateral); c > e {
		e = c
	}
	return e
}

// currentEquityBasis versions the equity computation below. When it changes, the
// persisted drawdown/daily-loss anchors (captured under an older basis) are reset
// rather than compared — so a basis change can never manufacture a phantom loss.
const currentEquityBasis = 2

// accountEquity is the TRUE total account equity across every instrument, used by
// the account-wide gates (#43) and the risk status. Unlike equityOf's max()
// heuristic — which reads available_collateral and so DROPS by the margin reserved
// when positions open, manufacturing a phantom drawdown/daily-loss — this counts the
// collateral held as margin and values held HIP-4 outcome shares:
//
//	unified (shared collateral): the margin is held inside the spot USDC balance, so
//	  equity = spot USDC total + perp unrealized PnL (all dexes) + outcome value.
//	pure-perp: collateral lives in the perp wallet(s), whose account_value already
//	  includes margin + free + uPnL, so equity = Σ perp account_value (main +
//	  sub-dex) + spot USDC + outcome value.
//
// For a FLAT account both branches equal the old equityOf, so only the buggy
// margin-reserved case changes.
func accountEquity(pf *PortfolioView) float64 {
	var perpUPnL, outcomeValue float64
	for _, p := range pf.Positions {
		if p.Class == "outcome" {
			outcomeValue += parseFloatSafe(p.PositionValue) // current market value of the Yes/No shares
		} else {
			perpUPnL += parseFloatSafe(p.UnrealizedPnl)
		}
	}
	var usdc float64
	for _, b := range pf.SpotBalances {
		if b.Coin == "USDC" {
			usdc = parseFloatSafe(b.Total)
			break
		}
	}
	if pf.CollateralShared {
		return usdc + perpUPnL + outcomeValue
	}
	av := parseFloatSafe(pf.AccountValue)
	for _, b := range pf.PerpDexs {
		av += parseFloatSafe(b.AccountValue)
	}
	return usdc + av + outcomeValue
}

// accountEquityForRisk is accountEquity with strict parsing. It is deliberately
// separate from the tolerant read-surface helper: malformed 200-response data
// must reject new exposure, not turn into phantom zero equity/exposure.
func accountEquityForRisk(pf *PortfolioView) (float64, error) {
	var perpUPnL, outcomeValue float64
	for _, p := range pf.Positions {
		if _, err := parseRiskFloat("position size for "+p.Coin, p.Szi); err != nil {
			return 0, err
		}
		if p.Class == "outcome" {
			if p.MarkPx == "" {
				return 0, fmt.Errorf("outcome mark for %s is unavailable", p.Coin)
			}
			mark, err := parseRiskFloat("outcome mark for "+p.Coin, p.MarkPx)
			if err != nil {
				return 0, err
			}
			if mark <= 0 || mark > 1 {
				return 0, fmt.Errorf("outcome mark for %s %q is outside (0,1]", p.Coin, p.MarkPx)
			}
			v, err := parseRiskFloat("position value for "+p.Coin, p.PositionValue)
			if err != nil {
				return 0, err
			}
			if v < 0 {
				return 0, fmt.Errorf("position value for %s %q is negative", p.Coin, p.PositionValue)
			}
			outcomeValue += v
			if math.IsInf(outcomeValue, 0) || math.IsNaN(outcomeValue) {
				return 0, fmt.Errorf("aggregate outcome position value is non-finite")
			}
			continue
		}
		pnl, err := parseRiskFloat("unrealized pnl for "+p.Coin, p.UnrealizedPnl)
		if err != nil {
			return 0, err
		}
		perpUPnL += pnl
		if math.IsInf(perpUPnL, 0) || math.IsNaN(perpUPnL) {
			return 0, fmt.Errorf("aggregate perp unrealized pnl is non-finite")
		}
	}
	var usdc float64
	for _, b := range pf.SpotBalances {
		if b.Coin != "USDC" {
			continue
		}
		v, err := parseRiskFloat("spot USDC total", b.Total)
		if err != nil {
			return 0, err
		}
		usdc = v
		break
	}
	if pf.CollateralShared {
		equity := usdc + perpUPnL + outcomeValue
		if math.IsNaN(equity) || math.IsInf(equity, 0) {
			return 0, fmt.Errorf("aggregate account equity is non-finite")
		}
		return equity, nil
	}
	av, err := parseRiskFloat("main-dex account value", pf.AccountValue)
	if err != nil {
		return 0, err
	}
	for dex, b := range pf.PerpDexs {
		v, err := parseRiskFloat("account value for sub-dex "+dex, b.AccountValue)
		if err != nil {
			return 0, err
		}
		av += v
		if math.IsNaN(av) || math.IsInf(av, 0) {
			return 0, fmt.Errorf("aggregate perp account value is non-finite")
		}
	}
	equity := usdc + av + outcomeValue
	if math.IsNaN(equity) || math.IsInf(equity, 0) {
		return 0, fmt.Errorf("aggregate account equity is non-finite")
	}
	return equity, nil
}

// perpMarginEquity is the equity that can actually BACK PERP MARGIN — the
// denominator for the liquidation-proximity read surfaces (portfolio's
// margin_ratio, preview's resulting_account_leverage). It deliberately differs
// from accountEquity (total wealth: the drawdown/daily-loss and gate basis) on
// a NON-unified account, where liquidation depends on the perp wallet(s) alone:
// idle spot USDC and HIP-4 outcome holdings cannot be pulled in to meet a perp
// margin call, so counting them dilutes the ratio and UNDERSTATES liquidation
// proximity — the dangerous direction (an agent's "reduce if margin_ratio > X"
// rule would never fire while the perp wallet liquidates). On a unified account
// the spot USDC balance IS the shared margin pool, so it is kept — but HIP-4
// outcome share value cannot back a perp margin call on EITHER account type and
// is always excluded from this basis (accountEquity/the gate basis keep it).
func perpMarginEquity(pf *PortfolioView) float64 {
	if pf.CollateralShared {
		var outcomeValue float64
		for _, p := range pf.Positions {
			if p.Class == "outcome" {
				outcomeValue += parseFloatSafe(p.PositionValue)
			}
		}
		return accountEquity(pf) - outcomeValue
	}
	av := parseFloatSafe(pf.AccountValue)
	for _, b := range pf.PerpDexs {
		av += parseFloatSafe(b.AccountValue)
	}
	return av
}

// pendingAdd is one coin's worst-case FUTURE exposure from resting orders:
// the remaining size × limit/trigger price of every NON-reduce-only open
// order, split by side. Untriggered non-reduce-only trigger orders count too —
// they can fire. Reduce-only orders (incl. bracket/position TP/SL children)
// can only shrink a position and contribute zero.
type pendingAdd struct {
	buy, sell float64
}

// pendingContribution is ONE open order's worst-case pending-add notional —
// exactly the value pendingAddsFromOrders folds for it, exposed separately so
// the modify paths can take a specific order OUT of the fold (the modified
// order must never double-count itself). Reduce-only / position-TP/SL /
// unpriced / non-finite rows contribute zero.
func pendingContribution(o hl.FrontendOpenOrder) float64 {
	if o.ReduceOnly || o.IsPositionTpSl {
		return 0
	}
	px := o.LimitPx
	if px <= 0 {
		px = o.TriggerPx // pure trigger rows may carry no limit
	}
	n := o.Sz * px // remaining (unfilled) size: the filled part is already in the position
	if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	return n
}

// validateRiskOrders rejects malformed exposure-adding open orders before their
// notionals are folded. Treating an unpriced/non-finite row as zero would make a
// configured cap evaluate against a smaller book than the exchange actually has.
func validateRiskOrders(orders []hl.FrontendOpenOrder) error {
	bySide := map[string]float64{}
	for _, o := range orders {
		if o.ReduceOnly || o.IsPositionTpSl {
			continue
		}
		if math.IsNaN(o.Sz) || math.IsInf(o.Sz, 0) || o.Sz <= 0 {
			return fmt.Errorf("open order oid=%d %s has invalid remaining size", o.Oid, o.Coin)
		}
		if math.IsNaN(o.LimitPx) || math.IsInf(o.LimitPx, 0) || math.IsNaN(o.TriggerPx) || math.IsInf(o.TriggerPx, 0) {
			return fmt.Errorf("open order oid=%d %s has a non-finite price", o.Oid, o.Coin)
		}
		px := o.LimitPx
		if px <= 0 {
			px = o.TriggerPx
		}
		if px <= 0 {
			return fmt.Errorf("open order oid=%d %s cannot be priced", o.Oid, o.Coin)
		}
		if o.Side != hl.OrderSideBid && o.Side != hl.OrderSideAsk {
			return fmt.Errorf("open order oid=%d %s has invalid side %q", o.Oid, o.Coin, o.Side)
		}
		n := o.Sz * px
		if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
			return fmt.Errorf("open order oid=%d %s has invalid notional", o.Oid, o.Coin)
		}
		key := o.Coin + "\x00" + string(o.Side)
		bySide[key] += n
		if math.IsNaN(bySide[key]) || math.IsInf(bySide[key], 0) {
			return fmt.Errorf("aggregate resting order notional for %s is non-finite", o.Coin)
		}
	}
	return nil
}

// pendingAddsFromOrders folds an open-order book into per-coin pending-add
// notionals. A missing/false reduceOnly field counts as exposure-ADDING —
// conservative: an order we cannot prove reductive is treated as a future add.
func pendingAddsFromOrders(orders []hl.FrontendOpenOrder) map[string]pendingAdd {
	out := map[string]pendingAdd{}
	for _, o := range orders {
		n := pendingContribution(o)
		if n <= 0 {
			continue
		}
		p := out[o.Coin]
		if o.Side == hl.OrderSideBid {
			p.buy += n
		} else {
			p.sell += n
		}
		out[o.Coin] = p
	}
	return out
}

// addPendingSide adds n to one side of a coin's pending-add entry.
func addPendingSide(pending map[string]pendingAdd, coin string, side Side, n float64) {
	p := pending[coin]
	if side == Buy {
		p.buy += n
	} else {
		p.sell += n
	}
	pending[coin] = p
}

// applyModifyReplacement updates a WORKING pending map for one modify leg: the
// old resting order's worst-case contribution (existing.PendingNotional, zero
// for reduce-only) comes out and the replacement's newNotional goes in — after
// a modify the resting book contains the NEW order INSTEAD OF the old one. The
// side never changes across a modify. Floored at zero to absorb float dust
// (the subtraction mirrors the exact value the fold added).
func applyModifyReplacement(pending map[string]pendingAdd, existing restingOrder, newNotional float64) {
	p := pending[existing.Coin]
	if existing.Side == Buy {
		p.buy += newNotional - existing.PendingNotional
		if p.buy < 0 {
			p.buy = 0
		}
	} else {
		p.sell += newNotional - existing.PendingNotional
		if p.sell < 0 {
			p.sell = 0
		}
	}
	pending[existing.Coin] = p
}

// pendingSideNotional sums the pending-add notional for coin on one side,
// tolerant of the HIP-3 "<dex>:" prefix on either side of the comparison.
func pendingSideNotional(pending map[string]pendingAdd, coin string, side Side) float64 {
	var sum float64
	for oc, p := range pending {
		if !coinMatches(oc, coin) && !coinMatches(coin, oc) {
			continue
		}
		if side == Buy {
			sum += p.buy
		} else {
			sum += p.sell
		}
	}
	return sum
}

// gateBook is ONE write invocation's open-order state, fetched once
// (fail-closed — see fetchGateBook) and threaded through every gate that needs
// it, so a write with both risk.max_position_notional_usd AND portfolio gates
// configured performs a single weight-20 open-orders read instead of one per
// gate. pending is normally pendingAddsFromOrders(orders); the modify paths
// substitute the adjusted fold — the book with the modified order(s) REPLACED
// at their new size/price (see Modify / ModifyBatch).
type gateBook struct {
	orders  []hl.FrontendOpenOrder
	pending map[string]pendingAdd
}

// portfolioEquitySnapshot is the live account state the gates evaluate against.
type portfolioEquitySnapshot struct {
	equity  float64               // perp account_value, or available USDC collateral when flat/unified
	perCoin map[string]float64    // signed notional per coin, across all dexes
	pending map[string]pendingAdd // resting non-reduce-only orders' worst-case adds per coin
}

// portfolioEquitySnapshot builds the gate inputs. book, when non-nil, is the
// invocation's already-fetched (complete, fail-closed) open-order state: its
// orders feed the portfolio read (no second weight-20 fetch) and its pending
// fold is used verbatim — possibly modify-adjusted. book==nil fetches inside.
func (c *Client) portfolioEquitySnapshot(ctx context.Context, book *gateBook) (*portfolioEquitySnapshot, error) {
	var preOrders []hl.FrontendOpenOrder
	if book != nil {
		preOrders = book.orders
		if preOrders == nil {
			preOrders = []hl.FrontendOpenOrder{} // fetched-and-empty must not re-fetch
		}
	}
	pf, err := c.portfolioWith(ctx, preOrders)
	if err != nil {
		return nil, err
	}
	// FAIL CLOSED on a PARTIAL snapshot too: a skipped sub-dex read, an
	// unreadable spot state, or unvalued outcome holdings under-count the book the
	// gates evaluate — the gate would run "successfully" against a smaller account.
	// Portfolio() stays tolerant for the read surfaces (which now surface the
	// degradation as warnings + degraded_dexs); only gating refuses. This is also
	// what guarantees observeEquity below can never anchor the drawdown/daily-loss
	// state from degraded (understated) equity.
	if len(pf.Degraded) > 0 {
		return nil, fmt.Errorf("partial account snapshot — unreadable: %s", strings.Join(pf.Degraded, ", "))
	}
	// Equity = the TRUE total account equity (see accountEquity): on a unified
	// account, spot USDC + perp uPnL + outcome value; on a pure-perp account, the
	// perp account value(s) + spot + outcome. The earlier max(account_value,
	// available_collateral) heuristic DROPPED by the margin reserved when a position
	// opened, manufacturing a phantom drawdown/daily-loss.
	equity, err := accountEquityForRisk(pf)
	if err != nil {
		return nil, err
	}
	// Resting orders are worst-case future exposure: without them, sequential
	// single orders (each passing against the FILLED book) tunnel arbitrarily
	// far past every cap. The book was fetched exactly once this invocation —
	// either by the caller (book != nil, whose fold may carry a modify's
	// replacement) or by portfolioWith just above — so counting it adds no
	// API weight.
	if err := validateRiskOrders(pf.OpenOrders); err != nil {
		return nil, err
	}
	pending := pendingAddsFromOrders(pf.OpenOrders)
	if book != nil {
		pending = book.pending
	}
	snap := &portfolioEquitySnapshot{
		equity:  equity,
		perCoin: map[string]float64{},
		pending: pending,
	}
	for _, p := range pf.Positions {
		n, err := parseRiskFloat("position value for "+p.Coin, p.PositionValue)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, fmt.Errorf("position value for %s %q is negative", p.Coin, p.PositionValue)
		}
		szi, err := parseRiskFloat("position size for "+p.Coin, p.Szi)
		if err != nil {
			return nil, err
		}
		if szi < 0 {
			n = -n
		}
		snap.perCoin[p.Coin] += n
		if math.IsNaN(snap.perCoin[p.Coin]) || math.IsInf(snap.perCoin[p.Coin], 0) {
			return nil, fmt.Errorf("aggregate position value for %s is non-finite", p.Coin)
		}
	}
	return snap, nil
}

// portfolioMetrics is the pure book arithmetic the account-wide gates and the
// read-only risk status share — gross/net notional (net at the per-direction
// worst case, see computePortfolioMetrics), the largest single-coin notional,
// and the open-position count (dust-filtered). No caps, no equity, no I/O, so
// the gate evaluator and the status view can never drift.
type portfolioMetrics struct {
	gross, net      float64
	maxCoinNotional float64
	maxCoin         string
	openPositions   int
}

// computePortfolioMetrics folds positions (signed per-coin notional, incl. any
// proposed deltas) and resting-order pending-adds into the gate metrics. Each
// coin is valued at its WORST CASE: |position + resting buys| vs |position −
// resting sells|, whichever is larger — the book the account holds if the
// adverse side fills. Net exposure is worst-case per DIRECTION the same way:
// netHigh = Σ(position + resting buys), netLow = Σ(position − resting sells),
// and the measured net is whichever has the larger magnitude. Pending orders
// fill independently, so a resting sell must never OFFSET a long book — signed
// netting (buys − sells) let a parked, never-filling sell read |net| below the
// filled book and un-gate buys past max_net_exposure; a resting order may only
// TIGHTEN the measured net. A coin with only resting non-reduce-only orders
// counts as an open position (it becomes one on fill). pending may be nil
// (positions only — old behavior, where netHigh == netLow == the signed sum).
func computePortfolioMetrics(perCoin map[string]float64, pending map[string]pendingAdd) portfolioMetrics {
	var m portfolioMetrics
	var netHigh, netLow float64 // all resting buys fill / all resting sells fill
	coins := make(map[string]bool, len(perCoin)+len(pending))
	for coin := range perCoin {
		coins[coin] = true
	}
	for coin := range pending {
		coins[coin] = true
	}
	for coin := range coins {
		v, p := perCoin[coin], pending[coin]
		worst := math.Max(absF(v+p.buy), absF(v-p.sell))
		m.gross += worst
		netHigh += v + p.buy
		netLow += v - p.sell
		if worst > m.maxCoinNotional {
			m.maxCoinNotional, m.maxCoin = worst, coin
		}
		if worst > 0.005 { // ignore dust / a flip that lands at ~0
			m.openPositions++
		}
	}
	m.net = netHigh
	if absF(netLow) > absF(netHigh) {
		m.net = netLow
	}
	return m
}

// checkPortfolioGates enforces the configured account-wide gates against the
// resulting book (live positions + resting non-reduce-only orders' worst-case
// adds + the proposed new-exposure deltas). It is a no-op unless a portfolio
// gate is set. It FAILS CLOSED: when the account snapshot cannot be read —
// fully or partially — the order is refused (retryable, exit 40) rather than
// allowed to bypass the gate. The returned warnings (e.g. freshly initialized
// drawdown/daily-loss anchors) must reach the result envelope.
func (c *Client) checkPortfolioGates(ctx context.Context, deltas []exposureDelta) ([]string, error) {
	return c.checkPortfolioGatesBook(ctx, deltas, nil)
}

// checkPortfolioGatesBook is checkPortfolioGates with the invocation's
// already-fetched open-order state threaded through (see gateBook): the gates
// re-use that single weight-20 read — and, on the modify paths, its
// replacement-adjusted pending fold — instead of fetching the book again.
// book==nil behaves exactly like checkPortfolioGates.
func (c *Client) checkPortfolioGatesBook(ctx context.Context, deltas []exposureDelta, book *gateBook) ([]string, error) {
	if !c.portfolioGuardsActive() && !c.outcomeQuestionCapActive(deltas) {
		return nil, nil
	}
	snap, err := c.portfolioEquitySnapshot(ctx, book)
	if err != nil {
		return nil, output.Network("portfolio_state",
			"cannot read account state to enforce portfolio risk gates: "+err.Error()).Retry().
			WithHint("transient read failure — back off and retry; the gates fail closed rather than assume a smaller book")
	}

	resulting := make(map[string]float64, len(snap.perCoin)+len(deltas))
	for k, v := range snap.perCoin {
		resulting[k] = v
	}
	for _, d := range deltas {
		resulting[d.coin] += d.signedNotional
	}
	m := computePortfolioMetrics(resulting, snap.pending)
	gross, net, maxCoinNotional, maxCoin := m.gross, m.net, m.maxCoinNotional, m.maxCoin

	r := c.riskConfig()
	// Net exposure needs no equity, so it is always checkable.
	if cap := r.MaxNetExposureUSD; cap > 0 && absF(net) > cap {
		return nil, output.Risk("max_net_exposure",
			fmt.Sprintf("resulting net exposure $%.2f exceeds cap $%.2f (worst case: resting orders count on their own side, they never offset)", net, cap)).
			WithHint(fmt.Sprintf("trim the heavier side (or cancel resting orders) so |long − short| <= $%.2f", cap))
	}
	// Open-position count: opening a NEW coin while at the cap is rejected; adding
	// to an existing position (count unchanged) is not. A coin holding only
	// resting non-reduce-only orders counts — it becomes a position on fill.
	if cap := r.MaxOpenPositions; cap > 0 {
		count := m.openPositions
		if count > cap {
			return nil, output.Risk("max_open_positions",
				fmt.Sprintf("this would make %d open positions (resting orders on a coin count), over the max of %d", count, cap)).
				WithHint(fmt.Sprintf("close a position, cancel resting orders on unopened coins, or add to an existing one (cap %d concurrent)", cap))
		}
	}

	// Per-question outcome concentration needs no equity ($ notional cap), so it is
	// always checkable — like net-exposure and open-positions above. It backstops the
	// per-coin concentration cap, which cannot tell that the Yes and No legs of one
	// binary (two different "#<enc>" coins) are the same bet.
	if cap := r.MaxOutcomeQuestionNotionalUSD; cap > 0 && c.outcomeQuestionCapActive(deltas) {
		if label, notional := c.heaviestOutcomeQuestion(resulting, snap.pending); notional > cap {
			return nil, output.Risk("max_outcome_question_notional",
				fmt.Sprintf("outcome question %q would hold $%.2f across its legs (resting orders count), over the $%.2f cap", label, notional, cap)).
				WithHint(fmt.Sprintf("reduce size or cancel resting orders so the question's total notional <= $%.2f", cap))
		}
	}

	equityGates := r.MaxAccountLeverage > 0 || r.MaxConcentrationPctPerCoin > 0 ||
		r.MaxDrawdownPct > 0 || r.MaxDailyLossUSD > 0 || r.MaxDailyLossPct > 0
	if equityGates && snap.equity <= 0 {
		return nil, output.Risk("account_equity_unavailable",
			"account equity is 0 or unreadable — cannot enforce leverage/drawdown/daily-loss gates").
			WithHint("fund the account or set wallet.master_address; these gates fail closed without equity")
	}
	if cap := r.MaxAccountLeverage; cap > 0 {
		if lev := gross / snap.equity; lev > cap {
			return nil, output.Risk("max_account_leverage",
				fmt.Sprintf("resulting account leverage %.2fx exceeds cap %.2fx (gross $%.2f incl. resting orders / equity $%.2f)",
					lev, cap, gross, snap.equity)).
				WithHint(fmt.Sprintf("reduce size (or cancel resting orders) so gross notional <= $%.2f", cap*snap.equity))
		}
	}
	if cap := r.MaxConcentrationPctPerCoin; cap > 0 && maxCoinNotional > 0 {
		if pct := maxCoinNotional / snap.equity * 100; pct > cap {
			return nil, output.Risk("max_concentration",
				fmt.Sprintf("%s would be %.1f%% of equity (resting orders count), over the %.1f%% per-coin cap", maxCoin, pct, cap)).
				WithHint(fmt.Sprintf("reduce %s (or cancel its resting orders) so its notional <= $%.2f", maxCoin, cap/100*snap.equity))
		}
	}

	// Trajectory gates: record the latest equity into the persistent high-water /
	// daily-anchor state (keyed per network+account) and gate on the resulting
	// drawdown / daily loss.
	var warnings []string
	if r.MaxDrawdownPct > 0 || r.MaxDailyLossUSD > 0 || r.MaxDailyLossPct > 0 {
		dd, dlUSD, dlPct, fresh, oerr := observeEquity(c.network, c.queryAddr, snap.equity)
		if oerr != nil {
			return nil, output.Network("risk_state", "cannot update drawdown/daily-loss state: "+oerr.Error()).Retry()
		}
		if fresh {
			// The operator must know: fresh anchors under-protect (no drawdown/daily
			// loss is measurable) until a new peak forms / the next UTC day anchors.
			warnings = append(warnings, fmt.Sprintf(
				"drawdown/daily-loss anchors (re)initialized at current equity for %s/%s — these gates measure nothing until the anchors move (next UTC day / new equity peak); anchors from other networks/accounts are never reused",
				c.network, riskStateComponent(c.queryAddr)))
		}
		if cap := r.MaxDrawdownPct; cap > 0 && dd > cap {
			return warnings, output.Risk("max_drawdown",
				fmt.Sprintf("drawdown %.1f%% from peak exceeds the %.1f%% cap — trading paused", dd, cap)).
				WithHint("recover above the threshold, raise the cap, or clear this network+account's risk_state file to reset the peak")
		}
		if cap := r.MaxDailyLossUSD; cap > 0 && dlUSD > cap {
			return warnings, output.Risk("max_daily_loss",
				fmt.Sprintf("today's loss $%.2f exceeds the $%.2f daily cap — trading paused", dlUSD, cap)).
				WithHint("the daily anchor resets at UTC midnight; or raise risk.max_daily_loss_usd")
		}
		if cap := r.MaxDailyLossPct; cap > 0 && dlPct > cap {
			return warnings, output.Risk("max_daily_loss_pct",
				fmt.Sprintf("today's loss %.1f%% exceeds the %.1f%% daily cap — trading paused", dlPct, cap)).
				WithHint("the daily anchor resets at UTC midnight; or raise risk.max_daily_loss_pct")
		}
	}
	return warnings, nil
}

// ---- persistent drawdown / daily-loss state (high-water + UTC-day anchor) ----

// The state is keyed PER NETWORK+ACCOUNT (one file each, like meta.json is
// network-tagged): the anchors describe one account's equity trajectory, so a
// shared unkeyed file let a testnet faucet peak read as a 90% mainnet drawdown
// (bricking trading) and let multi-account use silently neuter the daily-loss
// gate (whichever account was observed first each UTC day set the anchor).
//
// The LEGACY unkeyed risk_state.json is deliberately IGNORED — never read,
// never written, left on disk. Its anchors belong to an unknowable mix of
// contexts, and wrong anchors are worse than fresh ones; leaving the file
// untouched also keeps a downgrade working exactly as before. Each
// network+account instead anchors fresh on first observation, with a loud
// envelope warning (see checkPortfolioGates).

// riskStateComponent sanitizes a network/account string for use in a state
// filename: lowercased, [a-z0-9] kept, everything else dropped; empty => "default".
func riskStateComponent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func riskStateFile(network, account string) string {
	return "risk_state_" + riskStateComponent(network) + "_" + riskStateComponent(account)
}

func riskStatePath(network, account string) string {
	return filepath.Join(config.Dir(), riskStateFile(network, account)+".json")
}

func riskStateLockPath(network, account string) string {
	return filepath.Join(config.Dir(), riskStateFile(network, account)+".lock")
}

type riskState struct {
	PeakEquity      float64 `json:"peak_equity"`
	Day             string  `json:"day"`               // UTC date, YYYY-MM-DD
	DayAnchorEquity float64 `json:"day_anchor_equity"` // equity at the day's first observation
	Basis           int     `json:"basis,omitempty"`   // equity-computation version (currentEquityBasis)
}

// observeEquity records equity into the persistent high-water / daily-anchor state
// for one network+account (serialized across processes by a flock, like the rate
// cap) and returns the resulting drawdown-from-peak and daily-loss figures. A new
// UTC day re-anchors the daily figure; a missing or corrupt state file is treated
// as a fresh start (no drawdown yet) rather than a hard error, so a bad file never
// bricks trading. fresh=true reports that the anchors were (re)initialized from
// the current equity — the caller must surface that as a warning, because fresh
// anchors measure nothing until they move.
func observeEquity(network, account string, equity float64) (drawdownPct, dailyLossUSD, dailyLossPct float64, fresh bool, err error) {
	if math.IsNaN(equity) || math.IsInf(equity, 0) {
		return 0, 0, 0, false, fmt.Errorf("equity must be finite")
	}
	lk, err := state.Lock(riskStateLockPath(network, account))
	if err != nil {
		return 0, 0, 0, false, err
	}
	defer lk.Unlock()

	var st riskState
	if b, e := os.ReadFile(riskStatePath(network, account)); e == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return 0, 0, 0, false, fmt.Errorf("decode risk state: %w", err)
		}
	} else if !os.IsNotExist(e) {
		return 0, 0, 0, false, fmt.Errorf("read risk state: %w", e)
	}
	if st.Basis != currentEquityBasis {
		st = riskState{} // equity basis changed: re-anchor instead of comparing stale figures
	}
	fresh = st.PeakEquity <= 0 // no usable prior anchors for THIS network+account
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
	st.Basis = currentEquityBasis
	b, err := json.Marshal(st)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("encode risk state: %w", err)
	}
	path := riskStatePath(network, account)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, 0, 0, false, fmt.Errorf("create risk state directory: %w", err)
	}
	// Atomic+fsync: a crash mid-write must not zero the drawdown/daily-loss
	// anchors that gate signing (audit #91 / S12). A failed write is a failed gate,
	// not a fresh anchor that may silently reset again on the next process.
	if err := state.WriteFileAtomic(path, b, 0o600); err != nil {
		return 0, 0, 0, false, fmt.Errorf("persist risk state: %w", err)
	}
	return drawdownPct, dailyLossUSD, dailyLossPct, fresh, nil
}

// ReadRiskState reads the persistent drawdown/daily-loss state of one
// network+account WITHOUT mutating it and computes the drawdown-from-peak +
// daily-loss figures against the supplied live equity — the read-only
// counterpart to observeEquity, for status/monitoring surfaces (`deliverator
// risk`, the console TUI). It NEVER updates the high-water mark or the day
// anchor: a passive monitor must not move the figures the agent's gates depend
// on. found=false means there is no usable state file yet for this context
// (fresh box or corrupt file → zeroed figures, never an error). The daily
// figure is reported only when the stored anchor is from the current UTC day
// (a new day re-anchors on the next observeEquity write); drawdown uses the
// stored peak as-is. The legacy unkeyed risk_state.json is never consulted.
func ReadRiskState(network, account string, equity float64) (st riskState, drawdownPct, dailyLossUSD, dailyLossPct float64, found bool) {
	b, e := os.ReadFile(riskStatePath(network, account))
	if e != nil {
		return riskState{}, 0, 0, 0, false
	}
	if json.Unmarshal(b, &st) != nil {
		return riskState{}, 0, 0, 0, false
	}
	if st.Basis != currentEquityBasis {
		return riskState{}, 0, 0, 0, false // stale basis: treat as fresh until observeEquity re-anchors
	}
	found = true
	if st.PeakEquity > 0 && equity < st.PeakEquity {
		drawdownPct = (st.PeakEquity - equity) / st.PeakEquity * 100
	}
	today := time.Now().UTC().Format("2006-01-02")
	if st.Day == today && st.DayAnchorEquity > 0 && equity < st.DayAnchorEquity {
		dailyLossUSD = st.DayAnchorEquity - equity
		dailyLossPct = dailyLossUSD / st.DayAnchorEquity * 100
	}
	return st, drawdownPct, dailyLossUSD, dailyLossPct, found
}
