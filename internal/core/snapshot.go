package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	hl "github.com/erickuhn19/deliverator/internal/hl"

	"github.com/erickuhn19/deliverator/internal/output"
)

// Snapshot (#45) is the unified one-moment read an agent calls once per tick
// instead of chaining 4+ separate commands. It returns the portfolio (which
// already nests positions, open orders, balances, margin, and per-dex summaries),
// the rate-limit budget, the builder posture, and per-coin market context — each
// section carrying its own ok/error so a partial failure is visible rather than
// failing the whole read. Sections run concurrently against one shared deadline.
//
// It deliberately does NOT add separate `balance` / `open_orders` sections: those
// live inside the portfolio section (data.portfolio.data.{open_orders, perp,
// spot_balances, available_collateral, perp_dexs}); duplicating them would re-issue
// the single most expensive call group (UserState + SpotUserState + N sub-dex) just
// to restate the same numbers, defeating the point of the snapshot.

const maxSnapshotCtxCoins = 50

// Section wraps any read result with its own ok/error. Marshals to {ok,data?,error?}.
type Section struct {
	OK    bool          `json:"ok"`
	Data  any           `json:"data,omitempty"`
	Error *output.Error `json:"error,omitempty"`
}

func okSection(d any) Section { return Section{OK: true, Data: d} }

func errSection(err error) Section {
	var oe *output.Error
	if !errors.As(err, &oe) {
		oe = output.Unknown("error", err.Error())
	}
	return Section{OK: false, Error: oe}
}

// SnapshotView is the unified read. Failed lists the sections whose ok=false (also
// surfaced as command warnings) so an agent sees at a glance what to retry.
type SnapshotView struct {
	Portfolio     Section            `json:"portfolio"`      // data: *PortfolioView (positions+orders+balances+margin)
	Limits        Section            `json:"limits"`         // data: *LimitsView
	BuilderStatus Section            `json:"builder_status"` // data: *BuilderView (effectively always ok)
	Ctx           map[string]Section `json:"ctx"`            // coin -> {ok, data:*CtxView | error}
	Coins         []string           `json:"coins"`          // coins ctx was fetched for (resolved, sorted)
	Failed        []string           `json:"failed,omitempty"`
}

// Snapshot fetches every section concurrently. coins selects which markets to
// fetch ctx for; empty => auto-discover from the portfolio's positions + open
// orders. Only a missing query address (a precondition for every read) returns a
// top-level error; an individual section failure is captured in its Section.
// The returned ReadMeta carries the section warnings (Notes) PLUS the embedded
// portfolio's degradation — so the snapshot envelope surfaces the top-level
// degraded_dexs field on a partial read, exactly like `portfolio` itself: the
// snapshot is the once-per-tick read agents act on, and an agent keying on
// envelope.degraded_dexs (the documented protocol) must see it here too.
func (c *Client) Snapshot(ctx context.Context, coins []string) (*SnapshotView, ReadMeta, error) {
	var rm ReadMeta
	if err := c.requireQueryAddr(); err != nil {
		return nil, rm, err
	}
	sv := &SnapshotView{Ctx: map[string]Section{}, Coins: []string{}}
	var pf *PortfolioView

	// Phase 1: portfolio, limits, builder concurrently. Each writes its own struct
	// field (disjoint addresses, no race); pf is read only after Wait.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if v, err := c.Portfolio(ctx); err != nil {
			sv.Portfolio = errSection(err)
		} else {
			sv.Portfolio, pf = okSection(v), v
		}
	}()
	go func() {
		defer wg.Done()
		if v, err := c.Limits(ctx); err != nil {
			sv.Limits = errSection(err)
		} else {
			sv.Limits = okSection(v)
		}
	}()
	go func() {
		defer wg.Done()
		if v, err := c.BuilderStatus(ctx); err != nil {
			sv.BuilderStatus = errSection(err)
		} else {
			sv.BuilderStatus = okSection(v)
		}
	}()
	wg.Wait()

	if !sv.Portfolio.OK {
		sv.Failed = append(sv.Failed, "portfolio")
	}
	if !sv.Limits.OK {
		sv.Failed = append(sv.Failed, "limits")
	}
	if !sv.BuilderStatus.OK {
		sv.Failed = append(sv.Failed, "builder_status")
	}
	// A PARTIAL portfolio (unreadable sub-dex / spot state) is an ok section —
	// tolerant read — but must be LOUD: the tick loop acts on these numbers
	// (NEXT-2 item 1). Merging its ReadMeta hoists degraded/degraded_dexs to the
	// snapshot's OWN envelope (top-level field + PARTIAL DATA warning); the
	// section data still carries the nested copy.
	if pf != nil {
		rm.merge(pf.ReadMeta())
	}

	// Phase 2: resolve the ctx coin set (needs the portfolio result), then fan out.
	resolved := dedupSortCoins(coins)
	if len(coins) == 0 {
		switch {
		case sv.Portfolio.OK && pf != nil:
			resolved = dedupSortCoins(discoverCtxCoins(pf))
			if len(resolved) > 0 {
				rm.Notes = append(rm.Notes, "ctx coins auto-discovered from positions+orders: "+strings.Join(resolved, ","))
			}
		default:
			rm.Notes = append(rm.Notes, "ctx auto-discovery skipped: portfolio unavailable")
		}
	}
	if len(resolved) > maxSnapshotCtxCoins {
		rm.Notes = append(rm.Notes, fmt.Sprintf("ctx truncated to %d coins (had %d)", maxSnapshotCtxCoins, len(resolved)))
		resolved = resolved[:maxSnapshotCtxCoins]
	}
	sv.Coins = resolved

	if len(resolved) > 0 {
		c.snapshotCtxSections(ctx, resolved, sv)
		for _, coin := range resolved {
			if !sv.Ctx[coin].OK {
				sv.Failed = append(sv.Failed, "ctx["+coin+"]")
			}
		}
	}

	if len(sv.Failed) > 0 {
		rm.Notes = append(rm.Notes, "snapshot sections failed: "+strings.Join(sv.Failed, ", "))
	}
	return sv, rm, nil
}

// snapshotCtxSections fills sv.Ctx for every resolved coin with MINIMAL API
// weight (NEXT-2 item 8): one metaAndAssetCtxs fetch per DISTINCT dex (main dex
// "" included), one spotMetaAndAssetCtxs when any spot pair is requested, one
// allMids when any outcome is requested — each requested coin's view is then
// sliced out locally. The old per-coin fan-out issued N identical heavyweight
// fetches CONCURRENTLY, self-inflicting the per-IP 429s the snapshot exists to
// avoid; fetches here are sequential (a handful at most). Outcome best bid/ask
// still costs one l2Book per outcome coin — inherently per-coin data.
func (c *Client) snapshotCtxSections(ctx context.Context, coins []string, sv *SnapshotView) {
	type target struct {
		coin string
		mk   Market
	}
	perpByDex := map[string][]target{} // dex "" = main
	var spotCoins, outcomeCoins []target
	for _, coin := range coins {
		mk, ok := c.meta.Lookup(coin)
		if !ok {
			sv.Ctx[coin] = errSection(unknownCoin(coin))
			continue
		}
		t := target{coin: coin, mk: mk}
		switch {
		case mk.IsOutcome:
			outcomeCoins = append(outcomeCoins, t)
		case mk.IsSpot:
			spotCoins = append(spotCoins, t)
		default:
			perpByDex[dexOf(coin)] = append(perpByDex[dexOf(coin)], t)
		}
	}
	// Deterministic dex order (map iteration is random; keep request order stable).
	dexs := make([]string, 0, len(perpByDex))
	for d := range perpByDex {
		dexs = append(dexs, d)
	}
	sort.Strings(dexs)
	for _, d := range dexs {
		var dexParam *string
		if d != "" {
			dd := d
			dexParam = &dd
		}
		mac, err := c.info.MetaAndAssetCtxs(ctx, hl.MetaAndAssetCtxsParams{Dex: dexParam})
		for _, t := range perpByDex[d] {
			if err != nil {
				sv.Ctx[t.coin] = errSection(mapNetwork("meta_and_asset_ctxs", err))
				continue
			}
			if v, verr := perpCtxView(mac, t.mk); verr != nil {
				sv.Ctx[t.coin] = errSection(verr)
			} else {
				sv.Ctx[t.coin] = okSection(v)
			}
		}
	}
	if len(spotCoins) > 0 {
		spotMeta, ctxs, err := c.info.SpotMetaAndAssetCtxs(ctx)
		for _, t := range spotCoins {
			if err != nil {
				sv.Ctx[t.coin] = errSection(mapNetwork("spot_meta_and_asset_ctxs", err))
				continue
			}
			if v, verr := spotCtxView(spotMeta, ctxs, t.mk); verr != nil {
				sv.Ctx[t.coin] = errSection(verr)
			} else {
				sv.Ctx[t.coin] = okSection(v)
			}
		}
	}
	if len(outcomeCoins) > 0 {
		mids, err := c.info.AllMids(ctx)
		for _, t := range outcomeCoins {
			if err != nil {
				sv.Ctx[t.coin] = errSection(mapNetwork("all_mids", err))
				continue
			}
			if v, verr := c.outcomeCtxWith(ctx, mids, t.mk); verr != nil {
				sv.Ctx[t.coin] = errSection(verr)
			} else {
				sv.Ctx[t.coin] = okSection(v)
			}
		}
	}
}

// discoverCtxCoins returns the coins the account has exposure to — every position
// and open-order coin — so a default snapshot fetches ctx for exactly those.
func discoverCtxCoins(pf *PortfolioView) []string {
	seen := map[string]bool{}
	var out []string
	add := func(coin string) {
		if coin == "" || seen[coin] {
			return
		}
		seen[coin] = true
		out = append(out, coin)
	}
	for _, p := range pf.Positions {
		add(p.Coin)
	}
	for _, o := range pf.OpenOrders {
		add(o.Coin)
	}
	return out
}

// dedupSortCoins trims, case-insensitively de-dupes, and sorts a coin list for
// deterministic output (the HIP-3 "<dex>:<coin>" form is preserved).
func dedupSortCoins(coins []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range coins {
		c = strings.TrimSpace(c)
		if c == "" || seen[strings.ToUpper(c)] {
			continue
		}
		seen[strings.ToUpper(c)] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
