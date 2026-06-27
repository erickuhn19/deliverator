package core

import (
	"context"
	"encoding/json"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"

	"github.com/erickuhn19/deliverator/internal/output"
)

// watch (#51) is the reactive counterpart to the dead-man's switch. The DMS is
// passive — it cancels resting orders only if a heartbeat lapses, and never
// closes a position. `watch` instead consumes the user-state stream and
// evaluates a risk metric on every frame, so a mid-interval liquidation approach
// that a periodic tick structurally cannot catch fires a guarded action within
// seconds. Watch itself never signs: it computes the breach and hands the
// decision to the caller (cmd dispatches alert/dms/panic), keeping the signing
// surface in the engine's write path and this loop pure and testable.

// WatchMetric identifies the quantity Watch evaluates.
type WatchMetric string

// WatchLiqDistancePct is the only v1 metric: the minimum, across open positions,
// of distance_to_liq_pct — how far the mark can move (as a % of mark) before a
// position liquidates. Lower is more dangerous.
const WatchLiqDistancePct WatchMetric = "liq_distance_pct"

// subDexWatchTimeout bounds each per-frame sub-dex read so a slow sub-dex can't
// stall the (serial) stream read loop the monitor runs on — a failsafe must stay
// responsive, and the main-dex liq risk (already in the pushed frame) must not
// queue behind a sub-dex query. A sub-dex exceeding it is reported in
// WatchEval.StaleDexs for that frame rather than blocking the evaluation.
const subDexWatchTimeout = 3 * time.Second

// WatchConfig parameterizes a watch run.
type WatchConfig struct {
	Metric   WatchMetric   // metric to evaluate (only liq_distance_pct in v1)
	Below    float64       // breach when the metric drops below this
	Coin     string        // optional: restrict the metric to one coin
	Cooldown time.Duration // minimum gap between triggers (debounce a flapping metric)
}

// WatchEval is one evaluation of the metric, emitted as NDJSON per frame.
type WatchEval struct {
	Metric    string `json:"metric"`
	Value     string `json:"value,omitempty"`      // current metric value (string, like distance_to_liq_pct)
	Threshold string `json:"threshold"`            // the --below threshold
	WorstCoin string `json:"worst_coin,omitempty"` // the position driving the metric
	HasValue  bool   `json:"has_value"`            // false when nothing is measurable (e.g. flat)
	Breached  bool   `json:"breached"`             // value < threshold
	Positions int    `json:"positions"`            // positions that contributed a value
	// StaleDexs are sub-dexes whose per-frame read failed this evaluation — their
	// liquidation risk is NOT reflected in Value, so a non-empty list means the
	// failsafe is running with degraded coverage. Omitted when everything is read.
	StaleDexs []string `json:"stale_dexs,omitempty"`
	Ts        int64    `json:"ts"`
}

// evalLiqDistance computes the liq-distance metric from a position set. It mirrors
// PositionView.DistanceToLiqPct exactly (it reads that already-computed field), so
// `watch` and `positions` can never disagree.
func (cfg WatchConfig) evalLiqDistance(positions []PositionView) WatchEval {
	ev := WatchEval{
		Metric:    string(WatchLiqDistancePct),
		Threshold: f2s(cfg.Below),
		Ts:        output.Now(),
	}
	var minDist float64
	found := false
	for _, p := range positions {
		if p.DistanceToLiqPct == "" {
			continue // flat / fully-hedged cross has no liquidation price
		}
		d := parseFloatSafe(p.DistanceToLiqPct)
		ev.Positions++
		if !found || d < minDist {
			minDist, found = d, true
			ev.WorstCoin = p.Coin
		}
	}
	if !found {
		return ev // HasValue stays false: nothing to measure
	}
	ev.HasValue = true
	ev.Value = f2s(minDist)
	ev.Breached = minDist < cfg.Below
	return ev
}

// cooldownGate debounces triggers so a metric oscillating around the threshold
// cannot fire the action repeatedly. allow returns true (and arms the gate) only
// when at least cooldown has elapsed since the last allowed trigger.
type cooldownGate struct {
	cooldown time.Duration
	last     time.Time
	armed    bool
}

func (g *cooldownGate) allow(now time.Time) bool {
	if g.armed && now.Sub(g.last) < g.cooldown {
		return false
	}
	g.last, g.armed = now, true
	return true
}

// Watch subscribes to the user-state stream (webData2) and evaluates cfg.Metric on
// every frame. onEval is called for each evaluation (emit it as NDJSON); onBreach
// is called when the metric crosses the threshold, subject to the cooldown. The
// caller's onBreach performs the guarded action — Watch never signs. Stream
// invokes the callbacks serially from a single goroutine, so the cooldown gate
// needs no lock. Returns when ctx is cancelled (clean shutdown).
func (c *Client) Watch(ctx context.Context, cfg WatchConfig, onEval func(WatchEval), onBreach func(WatchEval)) error {
	addr := c.QueryAddr()
	if addr == "" {
		return output.Auth("no_address", "watch needs the master address — set wallet.master_address")
	}
	gate := cooldownGate{cooldown: cfg.Cooldown}
	subs := []StreamSub{{Type: ChanWebData2, User: addr}}
	return c.Stream(ctx, subs, func(ev StreamEvent) {
		if ev.Channel != ChanWebData2 {
			return // ignore reconnect markers and any unrelated frames
		}
		var wd struct {
			ClearinghouseState hl.UserState `json:"clearinghouseState"`
		}
		if json.Unmarshal(ev.Data, &wd) != nil {
			return
		}
		// webData2 carries the MAIN perp dex only. HIP-3 sub-dex positions live in
		// separate clearinghouses, so — like Positions — merge them in when any
		// sub-dex is configured; otherwise a sub-dex position's liq risk is invisible
		// to the failsafe. The read is bounded (subDexWatchTimeout) so a slow dex
		// can't stall this serial stream loop, and any dex that fails/times out is
		// reported in eval.StaleDexs (degraded coverage is observable, never silent)
		// rather than killing the monitor. The extra per-frame read only runs for
		// sub-dex users.
		positions := toPositionViews(&wd.ClearinghouseState, cfg.Coin)
		var stale []string
		if len(c.cfg.PerpDexs) > 0 {
			var sub []PositionView
			sub, stale = c.subDexPositionsTimeout(ctx, cfg.Coin, subDexWatchTimeout)
			positions = append(positions, sub...)
		}
		eval := cfg.evalLiqDistance(positions)
		eval.StaleDexs = stale
		onEval(eval)
		if eval.Breached && onBreach != nil && gate.allow(time.Now()) {
			onBreach(eval)
		}
	})
}
