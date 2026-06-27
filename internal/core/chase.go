package core

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/output"
)

// chase (#51) is the passive maker / limit-following helper: place a post-only
// limit pegged to the BBO and re-price it (via modify, which preserves the cloid)
// as the touch moves, so the order keeps following the book instead of going
// stale. It is a bounded modify-loop — poll the BBO every Interval, reprice when
// the rounded peg moves, and stop on a full fill, Ctrl-C, --timeout, or
// --max-reprices. Place/Modify enforce the usual risk gauntlet, so chase adds no
// risk code of its own.

// ChaseParams parameterizes a chase run.
type ChaseParams struct {
	Coin   string
	Side   Side
	Size   string
	Offset float64 // distance BEHIND the touch (>=0 is passive): buy peg = bid-offset, sell peg = ask+offset
	Tif    string  // Alo (default, post-only) | Gtc

	Interval     time.Duration // poll/reprice cadence (default 2s) — also the natural churn cap
	MaxReprices  int           // 0 = unlimited
	Timeout      time.Duration // 0 = until filled / interrupted
	LeaveResting bool          // on exit without a full fill, keep the order (default: cancel it)
	Cloid        string        // "" => generated
}

// ChaseEvent is one step of a chase, emitted as NDJSON.
type ChaseEvent struct {
	Event       string `json:"event"` // placed|repriced|partial_fill|filled|canceled|timeout|max_reprices|stopped|error
	Cloid       string `json:"cloid,omitempty"`
	Oid         *int64 `json:"oid,omitempty"`
	Px          string `json:"px,omitempty"` // the current peg / resting price
	Bid         string `json:"bid,omitempty"`
	Ask         string `json:"ask,omitempty"`
	FilledSz    string `json:"filled_sz,omitempty"`    // cumulative filled so far
	RemainingSz string `json:"remaining_sz,omitempty"` // size still resting
	Reprices    int    `json:"reprices"`
	Detail      string `json:"detail,omitempty"`
	Ts          int64  `json:"ts"`
}

// pegPrice computes the passive peg for a side: a buy sits at bid-offset, a sell
// at ask+offset (offset>=0 keeps it behind the touch; <0 improves into the
// spread). Returns false when the relevant side of the book is empty.
func pegPrice(side Side, bid, ask, offset float64) (float64, bool) {
	if side == Buy {
		if bid <= 0 {
			return 0, false
		}
		return bid - offset, true
	}
	if ask <= 0 {
		return 0, false
	}
	return ask + offset, true
}

// pegString returns the tick-rounded peg string for the current book, or ("",false)
// if the book side is empty / the peg is non-positive.
func pegString(side Side, mk Market, bbo *BboView, offset float64) (string, bool) {
	bid := parseFloatSafe(bbo.Bid)
	ask := parseFloatSafe(bbo.Ask)
	peg, ok := pegPrice(side, bid, ask, offset)
	if !ok || peg <= 0 {
		return "", false
	}
	out, _, err := RoundPrice(strconv.FormatFloat(peg, 'f', -1, 64), mk.SzDecimals, mk.IsSpot)
	if err != nil {
		return "", false
	}
	return out, true
}

// Chase places the initial pegged order, then polls the BBO every Interval and
// re-prices (modify, same cloid) whenever the rounded peg moves, until the order
// fully fills, ctx is cancelled, the timeout elapses, or MaxReprices is hit. Each
// step is reported via onEvent. On exit without a full fill it cancels the resting
// order unless LeaveResting. Chase never adds risk code — Place/Modify gate it.
func (c *Client) Chase(ctx context.Context, p ChaseParams, onEvent func(ChaseEvent)) error {
	mk, ok := c.meta.Lookup(p.Coin)
	if !ok {
		return unknownCoin(p.Coin)
	}
	tif := p.Tif
	if tif == "" {
		tif = "Alo"
	}
	interval := p.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	cloid, err := normalizeCloid(p.Cloid)
	if err != nil {
		return err
	}

	placedSz := parseFloatSafe(p.Size)

	// Initial placement at the current peg.
	bbo, err := c.Bbo(ctx, mk.Coin)
	if err != nil {
		return err
	}
	pegStr, ok := pegString(p.Side, mk, bbo, p.Offset)
	if !ok {
		return output.Risk("no_book", "cannot peg: no "+sideTouch(p.Side)+" in the book for "+mk.Coin)
	}
	res, _, err := c.Place(ctx, OrderReq{Coin: mk.Coin, Side: p.Side, Size: p.Size, Limit: pegStr, Tif: tif, Cloid: cloid})
	if err != nil {
		return err
	}
	onEvent(ChaseEvent{
		Event: "placed", Cloid: cloid, Oid: res.Oid, Px: pegStr,
		Bid: bbo.Bid, Ask: bbo.Ask, RemainingSz: p.Size, Ts: output.Now(),
	})
	// A marketable/aggressive placement could fill immediately.
	if res.Status == "filled" {
		onEvent(ChaseEvent{Event: "filled", Cloid: cloid, Oid: res.Oid, Px: pegStr, FilledSz: res.FilledSz, Ts: output.Now()})
		return nil
	}

	reprices := 0
	var deadline time.Time
	if p.Timeout > 0 {
		deadline = time.Now().Add(p.Timeout)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.chaseFinish(ctx, p, cloid, onEvent, "stopped", "interrupted")
			return nil
		case <-ticker.C:
			if !deadline.IsZero() && time.Now().After(deadline) {
				c.chaseFinish(ctx, p, cloid, onEvent, "timeout", "timeout elapsed")
				return nil
			}
			done := c.chaseStep(ctx, &chaseState{cloid: cloid, side: p.Side, mk: mk, offset: p.Offset, placedSz: placedSz, tif: tif}, p, &reprices, onEvent)
			if done {
				return nil
			}
		}
	}
}

// chaseState is the immutable-per-run context a step needs.
type chaseState struct {
	cloid    string
	side     Side
	mk       Market
	offset   float64
	placedSz float64
	tif      string
}

// chaseStep performs one poll: locate the live order, detect a fill, and reprice
// if the peg moved. Returns true when the chase is finished (filled / vanished /
// reprice-capped / error). reprices is advanced in place. Factored out of the loop
// so it can be unit-tested against canned book/order states without a real ticker.
func (c *Client) chaseStep(ctx context.Context, st *chaseState, p ChaseParams, reprices *int, onEvent func(ChaseEvent)) bool {
	// Read the open book directly so a transient read error is distinguishable from
	// "the order is gone" — a network blip must retry, never be misread as a
	// fill/cancel that ends the chase (and would orphan a still-resting order).
	orders, err := c.allOpenOrders(ctx)
	if err != nil {
		onEvent(ChaseEvent{Event: "error", Cloid: st.cloid, Detail: err.Error(), Reprices: *reprices, Ts: output.Now()})
		return false // transient: retry next tick
	}
	var (
		found     bool
		remaining float64
		curPxF    float64
		oid       int64
	)
	for i := range orders {
		o := orders[i]
		if o.Cloid != nil && strings.EqualFold(*o.Cloid, st.cloid) {
			remaining, curPxF, oid, found = o.Sz, o.LimitPx, o.Oid, true
			break
		}
	}
	if !found {
		// Confirmed absent from a SUCCESSFUL read → fully filled or canceled out.
		gone := c.classifyGoneOrder(ctx, st.cloid)
		onEvent(ChaseEvent{Event: gone, Cloid: st.cloid, Reprices: *reprices, Ts: output.Now()})
		return true
	}
	curPxStr := f2s(curPxF)
	// Partial fill: less resting than we placed.
	if st.placedSz > 0 && remaining > 0 && remaining < st.placedSz-1e-12 {
		onEvent(ChaseEvent{
			Event: "partial_fill", Cloid: st.cloid, Oid: &oid, Px: curPxStr,
			FilledSz: f2s(st.placedSz - remaining), RemainingSz: f2s(remaining), Reprices: *reprices, Ts: output.Now(),
		})
	}

	bbo, err := c.Bbo(ctx, st.mk.Coin)
	if err != nil {
		onEvent(ChaseEvent{Event: "error", Cloid: st.cloid, Detail: err.Error(), Reprices: *reprices, Ts: output.Now()})
		return false // transient: keep chasing on the next tick
	}
	pegStr, ok := pegString(st.side, st.mk, bbo, st.offset)
	if !ok {
		return false // empty book this tick; try again next tick
	}
	// Already pegged (compare on the tick grid) → nothing to do.
	curRounded, _, _ := RoundPrice(curPxStr, st.mk.SzDecimals, st.mk.IsSpot)
	if pegStr == curRounded {
		return false
	}
	// After partial fills the unfilled remainder can fall below the exchange
	// minimum; a modify cancel+replaces, so HL would reject the sub-minimum
	// replacement. Leave the dust resting at its current price (cancel-on-exit
	// cleans it up) instead of spamming a reject every tick.
	pegF := parseFloatSafe(pegStr)
	if c.cfg.Risk.MinOrderNotionalUSD > 0 && remaining*pegF < c.cfg.Risk.MinOrderNotionalUSD {
		return false
	}
	if p.MaxReprices > 0 && *reprices >= p.MaxReprices {
		c.chaseFinish(ctx, p, st.cloid, onEvent, "max_reprices", "reprice cap reached")
		return true
	}
	// Reprice the REMAINING size (not OrigSz) to the new peg — passing the live
	// remaining avoids re-growing a partially-filled order back to its original
	// size. Modify preserves the cloid (the oid changes).
	mres, _, merr := c.Modify(ctx, nil, st.cloid, f2s(remaining), pegStr)
	if merr != nil {
		onEvent(ChaseEvent{Event: "error", Cloid: st.cloid, Px: pegStr, Detail: merr.Error(), Reprices: *reprices, Ts: output.Now()})
		return false // e.g. a momentary crossing reject — retry next tick
	}
	*reprices++
	onEvent(ChaseEvent{
		Event: "repriced", Cloid: st.cloid, Oid: mres.Oid, Px: pegStr,
		Bid: bbo.Bid, Ask: bbo.Ask, RemainingSz: f2s(remaining), Reprices: *reprices, Ts: output.Now(),
	})
	return false
}

// classifyGoneOrder decides whether a cloid that is no longer resting filled or was
// canceled, by consulting its historical status. Defaults to "filled" only on an
// explicit filled status; anything else (or an unreadable status) is "canceled".
func (c *Client) classifyGoneOrder(ctx context.Context, cloid string) string {
	st, err := c.OrderStatus(ctx, nil, cloid)
	if err == nil && st != nil && st.Order.Status == "filled" {
		return "filled"
	}
	return "canceled"
}

// chaseFinish cancels the still-resting order (unless LeaveResting) and emits the
// terminal event. Best-effort: a cancel error is reported in the event detail.
func (c *Client) chaseFinish(ctx context.Context, p ChaseParams, cloid string, onEvent func(ChaseEvent), event, detail string) {
	ev := ChaseEvent{Event: event, Cloid: cloid, Detail: detail, Ts: output.Now()}
	if !p.LeaveResting {
		// Use a fresh bounded context: the run ctx may already be cancelled (Ctrl-C).
		cctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout+5*time.Second)
		defer cancel()
		if _, cerr := c.Cancel(cctx, CancelReq{Cloid: cloid}); cerr != nil {
			ev.Detail = detail + "; cancel failed: " + cerr.Error()
		} else {
			ev.Detail = detail + "; resting order canceled"
		}
	}
	onEvent(ev)
}

func sideTouch(s Side) string {
	if s == Buy {
		return "bid"
	}
	return "ask"
}
