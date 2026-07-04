package oms

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
	"github.com/erickuhn19/deliverator/internal/mm/fairvalue"
)

// Streamer is the subset of core.ClientAPI the feed needs (so it fakes in tests).
type Streamer interface {
	Stream(ctx context.Context, subs []core.StreamSub, onEvent func(core.StreamEvent)) error
}

// Feed maintains the live underlying marks and realized volatility off the allMids
// stream, and implements mm.UnderlyingFeed for the fair-value model. One long-lived
// allMids subscription carries every coin's mid — the tracked underlyings' perps AND
// every "#<enc>" outcome mid — so the model gets a high-frequency mark + vol without
// a per-coin subscription. The reconnect control frame is ignored: allMids is a full
// snapshot each frame, so the next frame after a drop refreshes state (no dedup).
//
// It is concurrency-safe: Stream dispatches onEvent serially from one goroutine
// (writer), while the engine reads Mark/Vol/Mid from its own loop (readers).
type Feed struct {
	mu          sync.RWMutex
	marks       map[string]float64            // coin -> latest mid (underlyings + "#<enc>")
	markT       map[string]time.Time          // coin -> when that mark last updated (staleness)
	vols        map[string]*fairvalue.EWMAVol // UPPER(underlying) -> realized-vol estimator
	underlyings map[string]bool               // UPPER(underlying) tracked for vol
	lambda      float64
	maxMarkAge  time.Duration
	now         func() time.Time
}

// defaultMaxMarkAge is how long a mark may go without an update before Mark reports it
// stale — long enough to ride out a brief allMids gap, short enough that a frozen or
// silently-dead stream stops the model from quoting off a stale spot (adverse selection).
const defaultMaxMarkAge = 15 * time.Second

// NewFeed builds a feed tracking realized vol for the given underlyings (matched
// case-insensitively). lambda is the EWMA decay (0.94 RiskMetrics when <=0).
func NewFeed(underlyings []string, lambda float64) *Feed {
	if lambda <= 0 {
		lambda = 0.94
	}
	set := make(map[string]bool, len(underlyings))
	for _, u := range underlyings {
		if u = strings.ToUpper(strings.TrimSpace(u)); u != "" {
			set[u] = true
		}
	}
	return &Feed{
		marks:       map[string]float64{},
		markT:       map[string]time.Time{},
		vols:        map[string]*fairvalue.EWMAVol{},
		underlyings: set,
		lambda:      lambda,
		maxMarkAge:  defaultMaxMarkAge,
		now:         time.Now,
	}
}

// Run subscribes to allMids and blocks until ctx is cancelled (the engine runs it in
// a goroutine). It returns the stream's terminal error, if any.
func (f *Feed) Run(ctx context.Context, s Streamer) error {
	return s.Stream(ctx, []core.StreamSub{{Type: core.ChanAllMids}}, f.onEvent)
}

func (f *Feed) onEvent(ev core.StreamEvent) {
	if ev.Channel == "reconnect" {
		return // allMids is a full snapshot; the next frame refreshes everything
	}
	mids := parseAllMids(ev.Data)
	if len(mids) == 0 {
		return
	}
	t := f.now()
	f.mu.Lock()
	for coin, s := range mids {
		v, ok := mm.ParseFloat(s)
		if !ok || v <= 0 {
			continue
		}
		f.marks[coin] = v
		f.markT[coin] = t
		if key := strings.ToUpper(coin); f.underlyings[key] {
			vol := f.vols[key]
			if vol == nil {
				vol = fairvalue.NewEWMAVol(f.lambda)
				f.vols[key] = vol
			}
			vol.Update(v, t)
		}
	}
	f.mu.Unlock()
}

// Mark implements mm.UnderlyingFeed: the latest mark for an underlying (its perp mid
// in allMids, keyed by the bare symbol), matched exact then upper-cased. It returns
// ok=false when the mark is STALE (older than maxMarkAge) — a frozen/dead stream must
// stop the model from quoting off a stale spot, not price forever off the last tick.
func (f *Feed) Mark(underlying string) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, k := range [2]string{underlying, strings.ToUpper(underlying)} {
		v, ok := f.marks[k]
		if !ok || v <= 0 {
			continue
		}
		if f.now().Sub(f.markT[k]) > f.maxMarkAge {
			return 0, false // stale — refuse rather than price off a frozen mark
		}
		return v, true
	}
	return 0, false
}

// Vol implements mm.UnderlyingFeed: the annualized realized vol for an underlying,
// ok=false until the estimator has warmed up.
func (f *Feed) Vol(underlying string) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	vol := f.vols[strings.ToUpper(underlying)]
	if vol == nil {
		return 0, false
	}
	return vol.Sigma()
}

// Mid returns the latest mid for any coin (e.g. an outcome "#<enc>"), ok=false if
// unseen. Used as a fallback probability source and for settlement inference.
func (f *Feed) Mid(coin string) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	v, ok := f.marks[coin]
	return v, ok && v > 0
}

// parseAllMids decodes an allMids frame. The wire wraps the map as {"mids":{...}};
// a bare {...} map is accepted too, for resilience to shape drift.
func parseAllMids(b json.RawMessage) map[string]string {
	var wrapped struct {
		Mids map[string]string `json:"mids"`
	}
	if json.Unmarshal(b, &wrapped) == nil && len(wrapped.Mids) > 0 {
		return wrapped.Mids
	}
	var bare map[string]string
	if json.Unmarshal(b, &bare) == nil && len(bare) > 0 {
		return bare
	}
	return nil
}
