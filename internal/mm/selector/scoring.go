package selector

import (
	"math"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// scoreCandidate fills a candidate's L/S/C sub-scores and the composite score, and
// sets Eligible + Reason. It is PURE (no I/O) — the ranking's correctness is unit
// tested here, independent of the read orchestration. The formula is spec §5:
//
//	score = P · (w_L·L + w_S·S + w_C·C)
//
// with a multiplicative concentration factor P and eligibility floors on each core
// axis (a zero on any axis — no flow, no capturable spread, unpriceable — excludes
// the market outright).
func scoreCandidate(c *Candidate, cfg config.MMSelection, bucketNotional, bucketCap float64) {
	c.L = liquidityScore(c.Volume24h, c.Book, cfg)
	c.S = spreadScore(c.Book, cfg)
	c.C = confidenceScore(c.Fair)

	if c.L < cfg.MinL || c.S < cfg.MinS || c.C < cfg.MinC {
		c.Score, c.Eligible = 0, false
		c.Reason = eligibilityReason(c, cfg)
		return
	}
	p := concentrationFactor(bucketNotional, bucketCap)
	c.Eligible = true
	c.Score = p * (cfg.WLiquidity*c.L + cfg.WSpread*c.S + cfg.WConfidence*c.C)
	c.Reason = "eligible"
}

// liquidityScore is L = w_v·Lv + w_d·Ld: a log-scaled volume term (volume is
// heavy-tailed) and a two-sided notional-depth term (a one-sided book scores low).
func liquidityScore(vol float64, book mm.BookTop, cfg config.MMSelection) float64 {
	lv := 0.0
	if cfg.VolSat > 0 && vol > 0 {
		lv = clamp01(math.Log1p(vol) / math.Log1p(cfg.VolSat))
	}
	ld := 0.0
	if cfg.DepthRef > 0 && book.HasBid && book.HasAsk {
		bidNtl := book.BidSz * book.Bid
		askNtl := book.AskSz * book.Ask
		ld = clamp01(math.Min(bidNtl, askNtl) / cfg.DepthRef)
	}
	return clamp01(cfg.WVolume*lv + cfg.WDepth*ld)
}

// spreadScore is S = clamp(((ask−bid)/2 − half_spread_floor) / spread_ref, 0, 1):
// the room to quote inside the book at a profit above the fee-on-close floor. A book
// tighter than the floor (or one-sided / already arbed) scores 0.
func spreadScore(book mm.BookTop, cfg config.MMSelection) float64 {
	if !book.HasBid || !book.HasAsk || cfg.SpreadRef <= 0 {
		return 0
	}
	half := (book.Ask - book.Bid) / 2
	return clamp01((half - cfg.HalfSpreadFloor) / cfg.SpreadRef)
}

// confidenceScore is C — the model's confidence, which already encodes the
// mid-probability plateau (the PriceBinaryModel sets Conf = plateau(p*), broad
// across mid probabilities and falling toward the deep-ITM/OTM extremes where a
// binary is adverse-selection-heavy). An estimate that is stale/invalid at scan
// time scores 0.
func confidenceScore(fair mm.Fair) float64 {
	if fair.P <= 0 || fair.P >= 1 {
		return 0
	}
	return clamp01(fair.Conf)
}

// concentrationFactor is P = 1 − clamp(bucket_notional / bucket_cap, 0, 1): it
// approaches 0 as a question's bucket fills, down-weighting markets in an
// underlying/expiry we are already heavy in (ties to the core per-question gate).
// A non-positive cap disables the penalty (P = 1).
func concentrationFactor(bucketNotional, bucketCap float64) float64 {
	if bucketCap <= 0 {
		return 1
	}
	return 1 - clamp01(bucketNotional/bucketCap)
}

// eligibilityReason explains which core axis floored a market out.
func eligibilityReason(c *Candidate, cfg config.MMSelection) string {
	switch {
	case c.L < cfg.MinL:
		return "excluded: liquidity below floor (thin / one-sided book)"
	case c.S < cfg.MinS:
		return "excluded: no capturable spread (book too tight / already arbed)"
	case c.C < cfg.MinC:
		return "excluded: model confidence too low (extreme probability)"
	default:
		return "excluded"
	}
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) || x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
