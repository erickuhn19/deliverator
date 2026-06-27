package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

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
func (c *Client) Snapshot(ctx context.Context, coins []string) (*SnapshotView, []string, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, nil, err
	}
	sv := &SnapshotView{Ctx: map[string]Section{}, Coins: []string{}}
	var pf *PortfolioView
	var warnings []string

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

	// Phase 2: resolve the ctx coin set (needs the portfolio result), then fan out.
	resolved := dedupSortCoins(coins)
	if len(coins) == 0 {
		switch {
		case sv.Portfolio.OK && pf != nil:
			resolved = dedupSortCoins(discoverCtxCoins(pf))
			if len(resolved) > 0 {
				warnings = append(warnings, "ctx coins auto-discovered from positions+orders: "+strings.Join(resolved, ","))
			}
		default:
			warnings = append(warnings, "ctx auto-discovery skipped: portfolio unavailable")
		}
	}
	if len(resolved) > maxSnapshotCtxCoins {
		warnings = append(warnings, fmt.Sprintf("ctx truncated to %d coins (had %d)", maxSnapshotCtxCoins, len(resolved)))
		resolved = resolved[:maxSnapshotCtxCoins]
	}
	sv.Coins = resolved

	if len(resolved) > 0 {
		var mu sync.Mutex // Go maps race on concurrent writes even to distinct keys
		var cwg sync.WaitGroup
		cwg.Add(len(resolved))
		for _, coin := range resolved {
			go func(coin string) {
				defer cwg.Done()
				sec := okSection(nil)
				if v, err := c.Ctx(ctx, coin); err != nil {
					sec = errSection(err)
				} else {
					sec = okSection(v)
				}
				mu.Lock()
				sv.Ctx[coin] = sec
				mu.Unlock()
			}(coin)
		}
		cwg.Wait()
		for _, coin := range resolved {
			if !sv.Ctx[coin].OK {
				sv.Failed = append(sv.Failed, "ctx["+coin+"]")
			}
		}
	}

	if len(sv.Failed) > 0 {
		warnings = append(warnings, "snapshot sections failed: "+strings.Join(sv.Failed, ", "))
	}
	return sv, warnings, nil
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
