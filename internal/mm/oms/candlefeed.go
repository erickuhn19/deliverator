package oms

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// CandleReader is the read surface CandleFeed needs (core.Client satisfies it).
type CandleReader interface {
	Candles(ctx context.Context, coin, interval string, since *int64) ([]hl.Candle, error)
}

const (
	// candleVolWindow is how far back we pull 1m candles. A day of 1m bars is a single
	// request per underlying (request-weight unchanged) yet gives both a fresh mark (last
	// 1m close) and enough history to resample into coarse bars for a stable vol.
	candleVolWindow = 24 * time.Hour
	// minutesPerYear annualizes a per-minute (1m-candle) realized vol (fallback path).
	minutesPerYear = 365.0 * 24.0 * 60.0
	// volResampleStep thins 1m closes to ~30m bars for the vol estimate. 1m realized vol
	// is inflated by microstructure noise and annualizes far too high, dragging the
	// digital fair toward 0.5; 30m bars price much closer to what the market implies.
	volResampleStep = 30
	// volPeriodsPerYear annualizes a 30m-bar realized vol.
	volPeriodsPerYear = 365.0 * 24.0 * 2.0
)

// CandleFeed sources the underlying MARK and realized VOL for the fair-value model
// from the perp's recent candles (a bounded REST read, refreshed each scan) rather
// than the WS allMids stream. allMids delivers only ~1 frame every several seconds on
// this venue, so an EWMA vol off it takes minutes to warm and the model can't price at
// startup; candles give a fresh mark (latest close) and a stable annualized vol (stdev
// of 1m log-returns) immediately, with no warm-up. It implements mm.UnderlyingFeed.
type CandleFeed struct {
	mu    sync.RWMutex
	marks map[string]float64 // UPPER(underlying) -> latest close
	vols  map[string]float64 // UPPER(underlying) -> annualized realized vol
	now   func() time.Time
}

// NewCandleFeed builds an empty candle feed. Call Refresh before first use.
func NewCandleFeed() *CandleFeed {
	return &CandleFeed{marks: map[string]float64{}, vols: map[string]float64{}, now: time.Now}
}

// Mark implements mm.UnderlyingFeed: the latest perp close for an underlying.
func (f *CandleFeed) Mark(underlying string) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	v, ok := f.marks[strings.ToUpper(strings.TrimSpace(underlying))]
	return v, ok && v > 0
}

// Vol implements mm.UnderlyingFeed: the annualized realized vol of the underlying.
func (f *CandleFeed) Vol(underlying string) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	v, ok := f.vols[strings.ToUpper(strings.TrimSpace(underlying))]
	return v, ok && v > 0
}

// Refresh re-fetches recent 1m candles for each underlying and recomputes its mark +
// realized vol. A per-underlying read failure or too-few candles leaves the prior
// values in place (better a slightly stale mark than none). Meant to run on the slow
// scan cadence.
func (f *CandleFeed) Refresh(ctx context.Context, r CandleReader, underlyings []string) {
	seen := map[string]bool{}
	for _, u := range underlyings {
		u = strings.ToUpper(strings.TrimSpace(u))
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		since := f.now().Add(-candleVolWindow).UnixMilli()
		candles, err := r.Candles(ctx, u, "1m", &since)
		if err != nil {
			continue
		}
		mark, vol, ok := markAndVol(candles)
		if !ok {
			continue
		}
		f.mu.Lock()
		f.marks[u] = mark
		if vol > 0 {
			f.vols[u] = vol
		}
		f.mu.Unlock()
	}
}

// markAndVol computes the latest close (freshest 1m bar) and the annualized realized
// vol from a candle slice. The vol is measured on ~30m-resampled bars, not the raw 1m
// series, because 1m realized vol is microstructure-noise-inflated and annualizes to an
// unrealistically high number that drags the digital fair toward 0.5. It sorts by open
// time so a caller need not assume ordering, and ignores non-positive closes.
func markAndVol(candles []hl.Candle) (mark, volAnnual float64, ok bool) {
	cs := append([]hl.Candle(nil), candles...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].TimeOpen < cs[j].TimeOpen })
	closes := make([]float64, 0, len(cs))
	for _, c := range cs {
		if v, ok := mm.ParseFloat(c.Close); ok && v > 0 {
			closes = append(closes, v)
		}
	}
	if len(closes) < 5 {
		return 0, 0, false // too little history to trust a vol
	}
	mark = closes[len(closes)-1] // freshest 1m close, regardless of vol resampling
	sampled, periodsPerYear := resampleForVol(closes)
	if len(sampled) < 5 {
		return mark, 0, true // mark is usable; vol unknown until more history
	}
	return mark, annualizedVol(sampled, periodsPerYear), true
}

// resampleForVol thins 1m closes to ~30m bars (every volResampleStep-th, anchored on the
// latest close) and returns them chronological with the matching annualization factor.
// Falls back to the raw 1m series (and its factor) when there isn't enough history to
// form at least a few coarse bars.
func resampleForVol(closes []float64) (sampled []float64, periodsPerYear float64) {
	if volResampleStep < 2 || len(closes) < volResampleStep*5 {
		return closes, minutesPerYear
	}
	for i := len(closes) - 1; i >= 0; i -= volResampleStep {
		sampled = append(sampled, closes[i])
	}
	for i, j := 0, len(sampled)-1; i < j; i, j = i+1, j-1 {
		sampled[i], sampled[j] = sampled[j], sampled[i]
	}
	return sampled, volPeriodsPerYear
}

// annualizedVol is the sample stdev of close-to-close log returns × √periodsPerYear.
func annualizedVol(closes []float64, periodsPerYear float64) float64 {
	if len(closes) < 2 {
		return 0
	}
	rets := make([]float64, 0, len(closes)-1)
	var mean float64
	for i := 1; i < len(closes); i++ {
		r := math.Log(closes[i] / closes[i-1])
		rets = append(rets, r)
		mean += r
	}
	mean /= float64(len(rets))
	var ss float64
	for _, r := range rets {
		d := r - mean
		ss += d * d
	}
	return math.Sqrt(ss/float64(len(rets)-1)) * math.Sqrt(periodsPerYear)
}
