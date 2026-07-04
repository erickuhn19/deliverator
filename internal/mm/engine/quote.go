package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
	"github.com/erickuhn19/deliverator/internal/mm/arb"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
	"github.com/erickuhn19/deliverator/internal/mm/settle"
	"github.com/erickuhn19/deliverator/internal/mm/strategy"
)

// tick is one fast quoting cycle: read authoritative account state once (inventory +
// resting orders + equity), price and build a desired ladder for each active market,
// diff against the resting book, and apply the minimal writes as aggregated signed
// actions (one PlaceBatch + one ModifyBatch across all coins, per-coin cancels) to
// respect the rate budget. In dry-run it computes everything and updates the view but
// never signs.
func (e *Engine) tick(ctx context.Context) {
	e.mu.Lock()
	active := toActive(e.active) // already a fresh slice
	paused := e.paused
	e.mu.Unlock()

	now := time.Now()
	warmup := false

	// One authoritative read: positions (inventory) + open orders (resting) + equity.
	pf, err := e.client.Portfolio(ctx)
	if err != nil {
		e.setError("portfolio: " + err.Error())
		return
	}
	invByCoin := oms.InventoryByCoin(pf.Positions)
	restingByCoin := oms.RestingByCoin(pf.OpenOrders) // one pass, MM-owned only
	equity := equityOf(pf)
	halted := e.client.Halted()

	// Book any silently-settled holdings into realized PnL before marking.
	e.reconcileSettlements(ctx, invByCoin)

	// Markets to quote this tick = the selector's active set PLUS any market we hold a
	// position in. Held-but-unselected markets stay managed (we keep quoting to reduce)
	// rather than being orphaned when max_active shrinks below what we already hold.
	quoteCoins := map[string]bool{}
	var quoteMkts []core.Market
	for _, am := range active {
		if !quoteCoins[am.market.Coin] {
			quoteCoins[am.market.Coin] = true
			quoteMkts = append(quoteMkts, am.market)
		}
	}
	for coin, sh := range invByCoin {
		if sh <= 0 || quoteCoins[coin] {
			continue // not held, or already being quoted via the active set (no lookup needed)
		}
		mk, ok := e.client.Meta().Lookup(coin)
		if !ok || !mk.IsOutcome {
			continue
		}
		// Normalize a held NO leg to its YES-leg market. desiredFor always treats its
		// market as the YES coin and prices the NO sibling at 1−fair with asks capped to
		// held NO; enqueuing a NO coin as a PRIMARY market would misprice it (quoted at
		// the YES probability, asks capped to the wrong side ⇒ the held NO can't be sold).
		if !mm.IsYes(mk) {
			yes, ok := e.client.Meta().Lookup(mm.YesCoin(mk.Outcome))
			if !ok {
				continue // can't resolve the YES leg — skip rather than misquote
			}
			mk = yes
		}
		if quoteCoins[mk.Coin] {
			continue
		}
		quoteCoins[mk.Coin] = true
		quoteMkts = append(quoteMkts, mk)
	}

	var (
		places      []core.OrderReq
		modifies    []core.ModifyReq
		cancelByCn  = map[string][]int64{}
		views       []MarketView
		questionNtl = map[string]float64{}
		markByCoin  = map[string]float64{}
	)

	for _, m := range quoteMkts {
		inv := oms.PairInventory(m, invByCoin)
		book := e.bookTop(ctx, m.Coin) // fresh top-of-book for maker-valid clamping

		sets, gate, fairP, fairConf := e.desiredFor(ctx, m, inv, now, paused, book)
		if gate == gateWarmup {
			warmup = true
		}

		// Diff each coin's desired set (YES, and the NO sibling when quote_no_side is on)
		// against that coin's own resting orders.
		var yesDesired mm.QuoteSet
		for _, desired := range sets {
			if desired.Coin == m.Coin {
				yesDesired = desired
			}
			d := oms.Diff(desired, restingByCoin[desired.Coin], pxTol)
			for _, q := range d.Place {
				places = append(places, e.placeReq(q))
			}
			for _, mo := range d.Modify {
				modifies = append(modifies, core.ModifyReq{Oid: ptrInt64(mo.Oid), Size: itoa(mo.NewSz), Limit: mm.ProbString(mo.NewPx)})
			}
			for _, cx := range d.Cancel {
				cancelByCn[cx.Coin] = append(cancelByCn[cx.Coin], cx.Oid)
			}
		}

		// Track this market's deployed notional per question for the next scan's
		// concentration penalty. YES shares are worth ~fairP, NO shares ~1−fairP.
		questionNtl[mm.QuestionKey(m)] += float64(inv.Yes)*fairP + float64(inv.No)*(1-fairP)
		// Mark BOTH legs for open PnL: YES at fairP, NO (e.g. arb inventory) at 1−fairP.
		if fairP > 0 {
			markByCoin[mm.YesCoin(m.Outcome)] = fairP
			markByCoin[mm.NoCoin(m.Outcome)] = 1 - fairP
		}

		views = append(views, e.marketView(m, inv, fairP, fairConf, book, yesDesired, gate, now))
	}

	// Apply the aggregated writes. Cancels run even during a global halt (core.Cancel
	// is halt-exempt) so a pause/blackout/inv-cap pull-down actually clears the book;
	// places/modifies are skipped while halted. All skipped entirely in shadow.
	signing := e.signing()
	if signing {
		e.flush(ctx, places, modifies, cancelByCn, halted)
	}

	holdings := buildHoldings(pf.Positions, e.client.Meta(), quoteCoins)
	// Open PnL: prefer HL's authoritative per-position unrealized. For any held coin HL
	// left UNMARKED (empty UnrealizedPnl — a priceable coin momentarily absent from the
	// allMids frame) fall back to the accountant's fair-marked open for that coin (basis
	// from fills, or seeded from entry px at startup) so the holding isn't shown flat.
	pnl := e.pnl.View(markByCoin) // realized / fees / fills / volume
	var open float64
	for _, h := range holdings {
		if h.HasPnL {
			open += h.PnL
			continue
		}
		if mark, ok := markByCoin[h.Coin]; ok {
			open += e.pnl.OpenForCoin(h.Coin, mark) // model fair-mark fallback
		}
		// else: neither HL nor the model can mark it this tick ⇒ contribute 0 (not −cost)
	}
	pnl.Open = open
	pnl.Net = pnl.Realized - pnl.Fees + open

	e.mu.Lock()
	e.view.Active = views
	e.view.Holdings = holdings
	e.view.Equity = equity
	e.view.Halted = halted
	e.view.DryRun = !signing // reflect a runtime live toggle in the banner
	e.view.PnL = pnl
	e.view.LastTick = now
	e.view.Warmup = warmup
	e.questionNtl = questionNtl
	e.mu.Unlock()
}

// desiredFor computes a question's desired quote sets (one per coin) and a human gate
// label. It returns empty sets (→ cancel resting, place none) when paused, in the
// settlement blackout, settled, or when the fair value is stale/unwarmed. When
// quote_no_side is enabled it also quotes the sibling NO coin at 1−fair, skewed by the
// NEGATIVE net exposure, so the MM can be two-sided/neutral while flat: it "shorts YES"
// by buying NO (the venue forbids naked-shorting YES). Sets[0] is always the YES coin.
func (e *Engine) desiredFor(ctx context.Context, m core.Market, inv mm.Inventory, now time.Time, paused bool, book mm.BookTop) ([]mm.QuoteSet, string, float64, float64) {
	empties := []mm.QuoteSet{{Coin: m.Coin}}
	if e.mmcfg.Strategy.QuoteNoSide {
		empties = append(empties, mm.QuoteSet{Coin: mm.NoCoin(m.Outcome)})
	}
	if paused {
		return empties, "paused", 0, 0
	}
	switch e.policy.Status(m, now) {
	case settle.Settled:
		return empties, "settled", 0, 0
	case settle.Blackout, settle.Unknown:
		if ttl, ok := mm.TTL(m, now); ok && ttl > 0 {
			return empties, fmt.Sprintf("blackout (%dm to expiry)", int(ttl.Minutes())), 0, 0
		}
		return empties, "blackout", 0, 0
	}

	fair, err := e.fair.Estimate(ctx, m)
	if err != nil {
		return empties, gateWarmup, 0, 0 // no mark/vol yet or unpriceable
	}
	if !fair.Valid(now) {
		return empties, "stale fair", fair.P, fair.Conf
	}

	netInv := inv.Net()
	// YES coin: fair p, skewed against net long; asks capped to held YES.
	sets := []mm.QuoteSet{
		strategy.BuildQuoteSet(m.Coin, fair.P, book, netInv, e.strategyParams(fair.Conf, inv.Yes)),
	}
	// NO coin (sibling): priced at 1−fair; net exposure flips sign (long YES ⇒ eager to
	// buy NO to neutralize); asks capped to held NO. This is the two-sided-while-flat leg.
	// Skipped on ultra-deep-ITM markets (NO price below NoSideMinPrice), where the $10 min
	// order would force a wildly oversized NO leg — a lottery ticket, not a neutral hedge.
	// Still quote it if we already HOLD NO there, so an existing position stays managed.
	if e.mmcfg.Strategy.QuoteNoSide {
		noCoin := mm.NoCoin(m.Outcome)
		if 1-fair.P >= e.mmcfg.Strategy.NoSideMinPrice || inv.No > 0 {
			noBook := e.bookTop(ctx, noCoin)
			sets = append(sets, strategy.BuildQuoteSet(noCoin, 1-fair.P, noBook, -netInv, e.strategyParams(fair.Conf, inv.No)))
		} else {
			// NO side gated off (deep-ITM tail, none held): still emit an EMPTY set so the
			// diff CANCELS any NO order left resting from when it was quoted — otherwise a
			// stale NO bid keeps resting and can fill into the exact position the gate exists
			// to prevent (this mirrors how the paused/blackout branches return empty NO sets).
			sets = append(sets, mm.QuoteSet{Coin: noCoin})
		}
		// Keep the paired YES/NO bids coherent (never pay >$1 for a complete set).
		strategy.ReconcilePairedBids(&sets[0], &sets[1])
	}

	gate := "quoting"
	if netInv >= e.mmcfg.Strategy.MaxInventoryShares {
		gate = "inv cap (long) — reducing only"
	} else if netInv <= -e.mmcfg.Strategy.MaxInventoryShares {
		gate = "inv cap (short) — reducing only"
	}
	return sets, gate, fair.P, fair.Conf
}

// strategyParams maps config + a confidence-driven spread widener into strategy.Params.
// Low model confidence widens the spread (up to 3x) rather than pulling outright.
// heldShares is the inventory of the specific coin being quoted (caps its ask ladder —
// no naked short).
func (e *Engine) strategyParams(conf float64, heldShares int64) strategy.Params {
	s := e.mmcfg.Strategy
	mult := 1.0
	if conf > 0 {
		mult = math.Min(3, 1/math.Max(conf, 0.34))
	}
	return strategy.Params{
		BaseSpread:         s.BaseSpread,
		LevelStep:          s.LevelStep,
		Levels:             s.Levels,
		BaseSizeShares:     s.BaseSizeShares,
		SizeStepShares:     s.SizeStepShares,
		InventorySkew:      s.InventorySkew,
		MaxInventoryShares: s.MaxInventoryShares,
		MinEdge:            s.MinEdge,
		MinNotionalUSD:     e.cfg.Risk.MinOrderNotionalUSD,
		SpreadMult:         mult,
		MidAnchorWeight:    s.MidAnchorWeight,
		HeldShares:         heldShares,
	}
}

func (e *Engine) placeReq(q mm.Quote) core.OrderReq {
	return core.OrderReq{
		Coin:  q.Coin,
		Side:  q.Side,
		Size:  itoa(q.Sz),
		Limit: mm.ProbString(q.Px),
		Tif:   "Alo",            // post-only maker quote
		Cloid: oms.NewMMCloid(), // tag it so RestingByCoin recognizes our own orders
	}
}

// flush applies the aggregated writes as few signed actions as possible. CANCELS run
// FIRST — freeing notional/concentration budget before the new places (so a re-ladder
// near a cap isn't rejected on transient old+new double-counting) and clearing the
// book on a pull-down; core.Cancel is exempt from the global halt, so this also lets a
// halted engine flatten its quotes. Places/modifies are skipped while halted. Every
// write still passes core's guardrail gauntlet; errors are recorded but non-fatal.
func (e *Engine) flush(ctx context.Context, places []core.OrderReq, modifies []core.ModifyReq, cancelByCoin map[string][]int64, halted bool) {
	for coin, oids := range cancelByCoin {
		if _, err := e.client.Cancel(ctx, core.CancelReq{Coin: coin, Oids: oids}); err != nil {
			e.setError("cancel: " + err.Error())
		}
	}
	if halted {
		return // during a halt only cancels are allowed; places/modifies would reject
	}
	for _, cn := range mm.Chunk(modifies, 100) {
		if _, _, err := e.client.ModifyBatch(ctx, cn); err != nil {
			e.setError("modify: " + err.Error())
		}
	}
	for _, cn := range mm.Chunk(places, 100) {
		if _, _, err := e.client.PlaceBatch(ctx, cn); err != nil {
			e.setError("place: " + err.Error())
		}
	}
}

// arbScan looks for a near-riskless cross-book capture on each active market and lifts
// both legs (buy YES ask + buy NO ask) when the edge clears costs. Runs on the slow
// scan cadence (fleeting-opportunity responsiveness is a v1.1 concern). It honors the
// pause flag and the global halt, and reuses the YES book the selector already
// fetched. Off by default (the two IOC legs are not atomic — a one-leg fill leaves a
// naked position). Best-effort.
func (e *Engine) arbScan(ctx context.Context, cands []selector.Candidate) {
	e.mu.Lock()
	paused := e.paused
	e.mu.Unlock()
	if !e.mmcfg.Arb.Enabled || !e.signing() || paused || e.client.Halted() {
		return
	}
	p := arb.Params{
		MinEdge:        e.mmcfg.Arb.MinEdge,
		MaxSizeShares:  e.mmcfg.Arb.MaxSizeShares,
		SettleFeeFrac:  e.mmcfg.Fees.CloseFeeBps / 10000,
		MinNotionalUSD: e.cfg.Risk.MinOrderNotionalUSD,
	}
	for _, c := range cands {
		m := c.Market
		yes := c.Book // the YES top-of-book the selector already fetched this scan
		no := e.bookTop(ctx, mm.NoCoin(m.Outcome))
		op, ok := arb.ScanCrossBook(m, yes, no, p)
		if !ok {
			continue
		}
		legs := []core.OrderReq{
			{Coin: op.YesCoin, Side: core.Buy, Size: itoa(op.Shares), Limit: mm.ProbString(op.YesPx), Tif: "Ioc", Cloid: oms.NewMMCloid()},
			{Coin: op.NoCoin, Side: core.Buy, Size: itoa(op.Shares), Limit: mm.ProbString(op.NoPx), Tif: "Ioc", Cloid: oms.NewMMCloid()},
		}
		if _, _, err := e.client.PlaceBatch(ctx, legs); err != nil {
			e.setError("arb: " + err.Error())
		}
	}
}

func (e *Engine) bookTop(ctx context.Context, coin string) mm.BookTop {
	bbo, err := e.client.Bbo(ctx, coin)
	if err != nil {
		return mm.BookTop{}
	}
	return mm.ParseBookTop(bbo)
}

// reconcileSettlements books a silently-settled holding into realized PnL: a coin the
// accountant still holds but that has left the live position set (HIP-4 settles shares
// to USDC with no fill) is realized at its inferred payout — 1 if its last mark was
// ≥ 0.5 (its side won), else 0. Guarded so a coin merely closed via fills (market
// still open) is not mis-booked, and skipped when there is no last mark to judge by.
func (e *Engine) reconcileSettlements(ctx context.Context, invByCoin map[string]int64) {
	ingested := false
	for _, coin := range e.pnl.HeldCoins() {
		if invByCoin[coin] > 0 {
			continue // still held on-chain
		}
		if !e.marketResolved(coin) {
			continue // position gone but market open → an ordinary close (already booked via fills)
		}
		// The position left the book AND the market resolved. Before booking a settlement
		// against the accountant's (possibly stale) basis, refresh fills ONCE: fills lag the
		// tick cadence, so a position emptied by an un-ingested SELL would otherwise be
		// mis-booked as payout*staleShares. After the refresh the sell is booked as a normal
		// close and RealizeSettlement below no-ops (shares==0) instead of over-booking.
		if !ingested {
			e.ingestFills(ctx)
			ingested = true
		}
		mid, ok := e.feed.Mid(coin)
		if !ok {
			continue // no final mark to infer the winner — retry next tick
		}
		payout := 0.0
		if mid >= 0.5 {
			payout = 1
		}
		e.pnl.RealizeSettlement(coin, payout) // no-op if the refresh already zeroed this coin
	}
}

// marketResolved reports whether an outcome coin's market has settled or expired
// (dropped from meta on the next rotation, flipped to "settled", or past its expiry).
func (e *Engine) marketResolved(coin string) bool {
	mk, ok := e.client.Meta().Lookup(coin)
	if !ok {
		return true // rotated out of the universe → settled/gone
	}
	if strings.EqualFold(mk.ResolutionStatus, "settled") {
		return true
	}
	if ttl, ok := mm.TTL(mk, time.Now()); ok && ttl <= 0 {
		return true
	}
	return false
}

// buildHoldings turns ALL held outcome positions into holding rows, regardless of
// whether the market is still in the active quote set (Active flags the ones the MM is
// still managing). This is what makes a position visible after its market drops out of
// selection — otherwise the operator can't see exposure they still carry.
func buildHoldings(positions []core.PositionView, meta *core.MetaStore, active map[string]bool) []HoldingView {
	var out []HoldingView
	for _, p := range positions {
		if p.Class != "outcome" {
			continue
		}
		sh, ok := mm.ParseFloat(p.Szi)
		if !ok || sh == 0 {
			continue
		}
		title := p.Title
		if title == "" {
			if mk, ok := meta.Lookup(p.Coin); ok {
				title = mk.Title
			}
		}
		val, _ := mm.ParseFloat(p.PositionValue)
		entry, _ := mm.ParseFloat(p.EntryPx)
		mark, _ := mm.ParseFloat(p.MarkPx)
		pnl, hasPnL := mm.ParseFloat(p.UnrealizedPnl) // empty ⇒ HL couldn't mark it (no live mid)
		out = append(out, HoldingView{
			Coin: p.Coin, Title: title, Side: p.OutcomeSide,
			Shares: int64(math.Round(math.Abs(sh))),
			Value:  val, Entry: entry, Mark: mark, PnL: pnl, HasPnL: hasPnL, Active: active[p.Coin],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Coin < out[j].Coin })
	return out
}

// marketView assembles the per-market row for the dashboard. The market mid + best
// bid/ask come from the live book; our bid/ask are the extremes of the desired ladder.
func (e *Engine) marketView(m core.Market, inv mm.Inventory, fairP, fairConf float64, book mm.BookTop, desired mm.QuoteSet, gate string, now time.Time) MarketView {
	ourBid, ourAsk := bestQuotes(desired.Quotes)
	ttl, _ := mm.TTL(m, now)
	mid, _ := book.Mid()
	var bestBid, bestAsk float64
	if book.HasBid {
		bestBid = book.Bid
	}
	if book.HasAsk {
		bestAsk = book.Ask
	}
	return MarketView{
		Coin:       m.Coin,
		Title:      m.Title,
		Underlying: m.Underlying,
		Expiry:     m.Expiry,
		TTL:        ttl,
		FairP:      fairP,
		FairConf:   fairConf,
		Mid:        mid,
		BestBid:    bestBid,
		BestAsk:    bestAsk,
		OurBid:     ourBid,
		OurAsk:     ourAsk,
		InvYes:     inv.Yes,
		InvNo:      inv.No,
		Gate:       gate,
	}
}

const gateWarmup = "warming up (no mark/vol yet)"

// ---- active-set snapshot type + helpers ----

type activeMkt struct{ market core.Market }

func toActive(cands []selector.Candidate) []activeMkt {
	out := make([]activeMkt, 0, len(cands))
	for _, c := range cands {
		out = append(out, activeMkt{market: c.Market})
	}
	return out
}

func bestQuotes(qs []mm.Quote) (bid, ask float64) {
	for _, q := range qs {
		if q.Side == core.Buy {
			if q.Px > bid {
				bid = q.Px
			}
		} else if ask == 0 || q.Px < ask {
			ask = q.Px
		}
	}
	return bid, ask
}

func equityOf(pf *core.PortfolioView) float64 {
	av, _ := mm.ParseFloat(pf.AccountValue)
	if c, ok := mm.ParseFloat(pf.AvailableCollateral); ok && c > av {
		av = c
	}
	return av
}

func itoa(n int64) string     { return strconv.FormatInt(n, 10) }
func ptrInt64(v int64) *int64 { return &v }
