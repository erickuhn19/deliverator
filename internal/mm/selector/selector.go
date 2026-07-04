// Package selector ranks the daily-rotating HIP-4 outcome universe into the small
// active set the quoting engine works (spec §5). Selection is STRATEGY, not safety —
// it lives in the MM, never in core. The scoring math is pure and unit-tested; the
// scan orchestration reads volume (candle-sum) and top-of-book through a narrow
// MarketData seam so it fakes cleanly in tests.
package selector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// MarketData is the read surface the selector needs — a strict subset of
// core.ClientAPI, so *core.Client satisfies it and tests can fake it trivially.
type MarketData interface {
	Candles(ctx context.Context, coin, interval string, since *int64) ([]hl.Candle, error)
	Bbo(ctx context.Context, coin string) (*core.BboView, error)
}

// Candidate is one outcome market evaluated for quoting, with its sub-scores and an
// include/exclude reason surfaced in the MM TUI's selection panel.
type Candidate struct {
	Market    core.Market
	Fair      mm.Fair
	Volume24h float64
	Book      mm.BookTop
	L, S, C   float64 // liquidity / spread-opportunity / confidence sub-scores in [0,1]
	Score     float64 // composite P·(wL·L + wS·S + wC·C); 0 when excluded
	Eligible  bool    // passed the hard filters AND the sub-score floors
	Active    bool    // chosen into the active set this cycle
	Reason    string  // human-readable include/exclude explanation
}

// Inputs carries the per-scan state the pure ranking needs from the engine.
type Inputs struct {
	Now              time.Time
	QuestionNotional map[string]float64 // current $ deployed per question bucket (concentration penalty)
	BucketCap        float64            // per-question $ cap the penalty saturates at; <=0 disables the penalty
	PrevActive       map[string]bool    // coin -> was active last cycle (hysteresis)
}

// Selection is the scan result: the chosen active set plus the full ranked pool
// (with reasons) for the operator's selection panel.
type Selection struct {
	Active []Candidate
	Pool   []Candidate
}

type volEntry struct {
	vol float64
	at  time.Time
}

// Selector holds the read seam, the fair-value model, the config, and a slow-moving
// volume cache (one candle read per market, reused across scans).
type Selector struct {
	md       MarketData
	fv       mm.FairValuer
	cfg      config.MMSelection
	volTTL   time.Duration
	volCache map[string]volEntry
}

// New builds a Selector. volTTL bounds how long a market's summed candle volume is
// reused before a re-fetch (volume moves slowly; default 30m when <=0).
func New(md MarketData, fv mm.FairValuer, cfg config.MMSelection) *Selector {
	return &Selector{md: md, fv: fv, cfg: cfg, volTTL: 30 * time.Minute, volCache: map[string]volEntry{}}
}

// SetSelection swaps the selection config (used by the engine to pick up live [mm]
// edits on the slow scan cadence). Safe because only the scan goroutine touches it.
func (s *Selector) SetSelection(cfg config.MMSelection) { s.cfg = cfg }

// priceableSet lowercases the configured underlyings into an upper-keyed set for
// mm.Priceable.
func (s *Selector) priceableSet() map[string]bool {
	set := make(map[string]bool, len(s.cfg.PriceableUnderlyings))
	for _, u := range s.cfg.PriceableUnderlyings {
		set[strings.ToUpper(strings.TrimSpace(u))] = true
	}
	return set
}

// Scan evaluates the outcome universe into a ranked pool and picks the active set.
// It runs the hard filters first (free — metadata only), then spends a volume +
// book read only on the survivors, then scores and selects. Read failures on a
// single market drop that market (with a reason) rather than failing the whole scan.
func (s *Selector) Scan(ctx context.Context, universe []core.Market, in Inputs) (Selection, error) {
	priceable := s.priceableSet()
	blacklist := toSet(s.cfg.Blacklist)
	pins := toSet(s.cfg.Pins)

	var pool []Candidate
	for _, mk := range universe {
		// Only ever quote the YES leg of a binary; the NO leg is its mirror and is
		// handled by the engine off the same market, so ranking both double-counts.
		if !mm.IsYes(mk) {
			continue
		}
		// Skip pure event/categorical markets entirely — v1 cannot price them (they
		// need the v2 LLM layer), so listing the hundreds of them just buries the
		// priceable candidates the operator can actually act on. A priceBinary always
		// carries an underlying + target.
		if mk.Underlying == "" || mk.TargetPrice == "" {
			continue
		}
		c := Candidate{Market: mk}
		if blacklist[strings.ToUpper(mk.Coin)] {
			c.Reason = "excluded: blacklisted"
			pool = append(pool, c)
			continue
		}
		if reason, ok := s.hardFilter(mk, in.Now, priceable); !ok {
			c.Reason = reason
			pool = append(pool, c)
			continue
		}
		// Survivor: spend the reads.
		c.Volume24h = s.volume(ctx, mk.Coin, in.Now)
		c.Book = s.bookTop(ctx, mk.Coin)
		if fair, err := s.fv.Estimate(ctx, mk); err == nil {
			c.Fair = fair
		} else {
			c.Reason = "excluded: not priceable (" + err.Error() + ")"
			pool = append(pool, c)
			continue
		}
		bucket := in.QuestionNotional[mm.QuestionKey(mk)]
		scoreCandidate(&c, s.cfg, bucket, in.BucketCap)
		if pins[strings.ToUpper(mk.Coin)] {
			// A pin forces eligibility (operator override) but keeps the computed score
			// for ranking within the pinned set.
			c.Eligible = true
			if c.Reason == "" {
				c.Reason = "pinned"
			}
		}
		pool = append(pool, c)
	}

	active := s.pickActive(pool, in, pins)
	return Selection{Active: active, Pool: pool}, nil
}

// hardFilter applies the must-pass gates: open + priceable priceBinary, and expiry
// inside the configured [min_ttl, max_ttl] band. Returns an exclude reason on fail.
func (s *Selector) hardFilter(mk core.Market, now time.Time, priceable map[string]bool) (string, bool) {
	if !mm.Priceable(mk, priceable) {
		if strings.EqualFold(mk.ResolutionStatus, "settled") {
			return "excluded: settled", false
		}
		if mk.Underlying == "" || mk.TargetPrice == "" {
			return "excluded: not a priceBinary (event market — needs v2)", false
		}
		return "excluded: underlying not in priceable set", false
	}
	ttl, ok := mm.TTL(mk, now)
	if !ok {
		return "excluded: unparseable expiry", false
	}
	if mins := ttl.Minutes(); mins < float64(s.cfg.MinTTLMins) {
		return fmt.Sprintf("excluded: TTL %.0fm < min %dm (blackout territory)", mins, s.cfg.MinTTLMins), false
	} else if s.cfg.MaxTTLMins > 0 && mins > float64(s.cfg.MaxTTLMins) {
		return fmt.Sprintf("excluded: TTL %.0fm > max %dm", mins, s.cfg.MaxTTLMins), false
	}
	return "", true
}

// volume returns the market's 24h summed candle volume, cached for volTTL. A read
// failure returns 0 (the liquidity term degrades to near-zero, deprioritizing the
// market) rather than failing the scan.
func (s *Selector) volume(ctx context.Context, coin string, now time.Time) float64 {
	if e, ok := s.volCache[coin]; ok && now.Sub(e.at) < s.volTTL {
		return e.vol
	}
	since := now.Add(-24 * time.Hour).UnixMilli()
	candles, err := s.md.Candles(ctx, coin, "1h", &since)
	if err != nil {
		return 0
	}
	var sum float64
	for _, c := range candles {
		if v, ok := mm.ParseFloat(c.Volume); ok {
			sum += v
		}
	}
	s.volCache[coin] = volEntry{vol: sum, at: now}
	return sum
}

// bookTop reads the current top-of-book as an mm.BookTop (probability units).
func (s *Selector) bookTop(ctx context.Context, coin string) mm.BookTop {
	bbo, err := s.md.Bbo(ctx, coin)
	if err != nil {
		return mm.BookTop{}
	}
	return mm.ParseBookTop(bbo)
}

// pickActive ranks the eligible pool and selects the active set under diversification
// caps, top-N, and hysteresis. Pinned markets are always included first (up to N).
func (s *Selector) pickActive(pool []Candidate, in Inputs, pins map[string]bool) []Candidate {
	n := s.cfg.MaxActiveMarkets
	if n <= 0 {
		return nil
	}
	eligible := make([]Candidate, 0, len(pool))
	for _, c := range pool {
		if c.Eligible {
			eligible = append(eligible, c)
		}
	}
	// Highest score first; stable by coin for determinism on ties.
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Score != eligible[j].Score {
			return eligible[i].Score > eligible[j].Score
		}
		return eligible[i].Market.Coin < eligible[j].Market.Coin
	})

	div := newDiversifier(s.cfg.MaxPerUnderlying, s.cfg.MaxPerExpiry)
	chosen := make([]string, 0, n) // coins, in selection order
	take := func(c Candidate, pinned bool) bool {
		if len(chosen) >= n {
			return false
		}
		if pinned {
			div.record(c.Market) // pins bypass the caps but still COUNT, so non-pins diversify around them
		} else if !div.admit(c.Market) {
			return false
		}
		chosen = append(chosen, c.Market.Coin)
		return true
	}

	// 1) Pins first — an operator override always makes the set (up to N), bypassing
	//    the per-underlying/per-expiry diversification caps.
	for _, c := range eligible {
		if pins[strings.ToUpper(c.Market.Coin)] {
			take(c, true)
		}
	}
	// 2) Incumbents still above drop_threshold stick (hysteresis: a market holds its
	//    slot until it decays past drop_threshold), subject to diversification.
	for _, c := range eligible {
		if contains(chosen, c.Market.Coin) {
			continue
		}
		if in.PrevActive[c.Market.Coin] && c.Score >= s.cfg.DropThreshold {
			take(c, false)
		}
	}
	// 3) Best fresh challengers fill any remaining slots. There is no forced
	//    displacement of an incumbent (that path bypassed diversification and churned
	//    on noise); an incumbent leaves only when it falls below drop_threshold above.
	for _, c := range eligible {
		if contains(chosen, c.Market.Coin) {
			continue
		}
		take(c, false)
	}

	// Materialize the active set in selection order and tag it.
	byCoin := map[string]Candidate{}
	for i := range pool {
		byCoin[pool[i].Market.Coin] = pool[i]
	}
	out := make([]Candidate, 0, len(chosen))
	for _, coin := range chosen {
		c := byCoin[coin]
		c.Active = true
		if c.Reason == "" {
			c.Reason = "active"
		}
		out = append(out, c)
	}
	return out
}

// ---- diversification ----

type diversifier struct {
	perUnd, perExp int
	undCount       map[string]int
	expCount       map[string]int
}

func newDiversifier(perUnd, perExp int) *diversifier {
	return &diversifier{perUnd: perUnd, perExp: perExp, undCount: map[string]int{}, expCount: map[string]int{}}
}

// admit reports whether taking mk keeps under the per-underlying / per-expiry caps,
// and records it when so. A cap of 0 means "no cap" for that dimension.
func (d *diversifier) admit(mk core.Market) bool {
	u := strings.ToUpper(mk.Underlying)
	if d.perUnd > 0 && d.undCount[u] >= d.perUnd {
		return false
	}
	if d.perExp > 0 && d.expCount[mk.Expiry] >= d.perExp {
		return false
	}
	d.undCount[u]++
	d.expCount[mk.Expiry]++
	return true
}

// record counts mk toward the diversification budget WITHOUT enforcing the caps —
// used for pins, which bypass the caps but must still be counted so ordinary picks
// diversify around them.
func (d *diversifier) record(mk core.Market) {
	d.undCount[strings.ToUpper(mk.Underlying)]++
	d.expCount[mk.Expiry]++
}

// ---- small helpers ----

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			m[strings.ToUpper(x)] = true
		}
	}
	return m
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
