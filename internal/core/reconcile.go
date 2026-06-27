package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"

	"github.com/erickuhn19/deliverator/internal/state"
)

// Reconcile diffs Deliverator's local audit trail against live exchange state so
// an autonomous loop can adopt reality before resuming after a crash/restart (#42).
// It is the prescribed FIRST call after any (re)start: the exchange does not reject
// a duplicate cloid, so a blind resume can double-place — reconcile surfaces the
// orders/positions that exist on-chain but not (or no longer) in the local trail,
// and resolves any in-flight cloids whose outcome is unknown (the exit-42 hazard).
//
// It is read-only — it never places, cancels, or modifies anything.

const reconcileDefaultWindow = 24 * time.Hour

// ReconcileOpts parameterizes a reconcile pass.
type ReconcileOpts struct {
	// SinceMs ignores audit rows older than this (0 => last 24h). A wider window
	// catches more history at the cost of scanning more of the trail.
	SinceMs int64
	// SuspectCloids are in-flight cloids the caller is unsure landed (e.g. an
	// order that timed out, exit 42). Each is resolved against live state.
	SuspectCloids []string
}

// ReconcileOrderRef identifies an order referenced by the audit trail.
type ReconcileOrderRef struct {
	Oid    int64  `json:"oid,omitempty"`
	Cloid  string `json:"cloid,omitempty"`
	Coin   string `json:"coin,omitempty"`
	Side   string `json:"side,omitempty"`
	Status string `json:"last_audit_status,omitempty"`
}

// SuspectResult resolves one in-flight cloid against live exchange state, with a
// recommended action mirroring the §5.4 retry protocol.
type SuspectResult struct {
	Cloid  string `json:"cloid"`
	Found  bool   `json:"found"`
	Status string `json:"status"`        // resting | filled | absent | <terminal> | error
	Oid    int64  `json:"oid,omitempty"` // when found
	Action string `json:"action"`        // adopt | resubmit | inspect
}

// ReconcileView is the structured diff returned to the agent.
type ReconcileView struct {
	Address      string                 `json:"address"`
	AuditPath    string                 `json:"audit_path"`
	AuditSinceMs int64                  `json:"audit_since_ms"`
	AuditRows    int                    `json:"audit_rows_scanned"`
	OpenOrders   []hl.FrontendOpenOrder `json:"open_orders"`
	Positions    []PositionView         `json:"positions"`
	// OrphanOrders are live resting orders this instance has no audit record of
	// placing — placed out-of-band, by another signer, or before the audit window.
	// Trigger / position-tp-sl legs (e.g. a bracket's tp/sl, which aren't
	// individually cloid-tagged or audited) are deliberately excluded to avoid
	// false positives — only a plain resting order is a reliable orphan signal.
	OrphanOrders []hl.FrontendOpenOrder `json:"orphan_orders"`
	// ClosedSince are orders the audit recorded as resting that are no longer live
	// (filled or canceled while the loop was down). Informational — normal churn,
	// not a divergence — but the agent should pull fills to update its PnL/state.
	ClosedSince []ReconcileOrderRef `json:"closed_since"`
	// Suspects resolves each --cloid the caller supplied.
	Suspects []SuspectResult `json:"suspect_cloids,omitempty"`
	// Divergences lists the items that set Clean=false (orphans + unknown suspects).
	Divergences []string `json:"divergences"`
	// Clean is false when there is something that could cause a double-place or
	// that this instance never placed. The command maps !Clean to exit 60.
	Clean bool `json:"clean"`
}

// Reconcile builds the diff. Read-only.
func (c *Client) Reconcile(ctx context.Context, opts ReconcileOpts) (*ReconcileView, error) {
	if err := c.requireQueryAddr(); err != nil {
		return nil, err
	}
	since := opts.SinceMs
	if since <= 0 {
		since = time.Now().Add(-reconcileDefaultWindow).UnixMilli()
	}

	liveOrders, err := c.allOpenOrders(ctx)
	if err != nil {
		return nil, mapNetwork("open_orders", err)
	}
	positions, err := c.Positions(ctx, "")
	if err != nil {
		return nil, err
	}

	rows, _ := state.ReadSince(c.audit.Path(), since) // best-effort: no trail => empty diff

	// Every order id/cloid this instance ever recorded touching. A live order
	// absent from these sets is an orphan.
	knownOids := map[int64]bool{}
	knownCloids := map[string]bool{}
	// Orders the audit saw resting, to detect ones that have since vanished.
	restingByOid := map[int64]ReconcileOrderRef{}
	for _, m := range rows {
		collectOrderIDs(m, knownOids, knownCloids)
		switch auditStr(m["action"]) {
		case "order", "bracket", "close":
			if auditStr(m["status"]) == "resting" {
				if oid := auditInt(m["oid"]); oid > 0 {
					restingByOid[oid] = ReconcileOrderRef{
						Oid: oid, Cloid: auditStr(m["cloid"]), Coin: auditStr(m["coin"]),
						Side: auditStr(m["side"]), Status: "resting",
					}
				}
			}
		}
	}

	liveOids := map[int64]bool{}
	orphans := []hl.FrontendOpenOrder{}
	for _, o := range liveOrders {
		liveOids[o.Oid] = true
		if o.IsTrigger || o.IsPositionTpSl {
			continue // bracket/tp-sl legs aren't individually audited — skip
		}
		known := knownOids[o.Oid]
		if !known && o.Cloid != nil {
			known = knownCloids[strings.ToLower(*o.Cloid)]
		}
		if !known {
			orphans = append(orphans, o)
		}
	}

	closedSince := []ReconcileOrderRef{}
	for oid, ref := range restingByOid {
		if !liveOids[oid] {
			closedSince = append(closedSince, ref)
		}
	}
	sort.Slice(closedSince, func(i, j int) bool { return closedSince[i].Oid < closedSince[j].Oid })

	var suspects []SuspectResult
	for _, raw := range opts.SuspectCloids {
		if cl := strings.TrimSpace(raw); cl != "" {
			suspects = append(suspects, c.resolveSuspect(ctx, cl))
		}
	}

	if liveOrders == nil {
		liveOrders = []hl.FrontendOpenOrder{}
	}
	if positions == nil {
		positions = []PositionView{}
	}
	view := &ReconcileView{
		Address:      c.queryAddr,
		AuditPath:    c.audit.Path(),
		AuditSinceMs: since,
		AuditRows:    len(rows),
		OpenOrders:   liveOrders,
		Positions:    positions,
		OrphanOrders: orphans,
		ClosedSince:  closedSince,
		Suspects:     suspects,
	}

	div := []string{}
	for _, o := range orphans {
		div = append(div, fmt.Sprintf("orphan order oid=%d %s — no audit record of placing it", o.Oid, o.Coin))
	}
	for _, s := range suspects {
		if s.Status == "absent" || s.Status == "error" {
			div = append(div, fmt.Sprintf("suspect cloid %s is %s — outcome unknown, %s", s.Cloid, s.Status, s.Action))
		}
	}
	view.Divergences = div
	view.Clean = len(div) == 0
	return view, nil
}

// resolveSuspect resolves one in-flight cloid against live exchange state and maps
// it to a recommended action (§5.4). A live resting order carrying the cloid wins
// over a stale historical record (OrderStatus already prefers the live order).
func (c *Client) resolveSuspect(ctx context.Context, cloid string) SuspectResult {
	res := SuspectResult{Cloid: cloid}
	q, err := c.OrderStatus(ctx, nil, cloid)
	if err != nil {
		res.Status, res.Action = "error", "inspect"
		return res
	}
	if q == nil || q.Status != hl.OrderQueryStatusSuccess {
		// unknownOid: the exchange has no order for this cloid → it never landed.
		// Per the retry protocol it is safe to resubmit the SAME cloid.
		res.Status, res.Action = "absent", "resubmit"
		return res
	}
	res.Found = true
	res.Oid = q.Order.Order.Oid
	switch strings.ToLower(string(q.Order.Status)) {
	case "open", "waitingfortrigger", "triggered":
		res.Status, res.Action = "resting", "adopt"
	case "filled":
		res.Status, res.Action = "filled", "adopt"
	default:
		// canceled / rejected / marginCanceled / ... — it existed but is gone; adopt
		// the terminal outcome, do NOT resubmit.
		res.Status, res.Action = strings.ToLower(string(q.Order.Status)), "inspect"
	}
	return res
}

// collectOrderIDs harvests every order id/cloid referenced by an audit row — the
// top-level oid/cloid plus any nested in a batch "legs" array — into the known
// sets, so orphan detection recognizes orders placed by any action shape.
func collectOrderIDs(m map[string]any, oids map[int64]bool, cloids map[string]bool) {
	if oid := auditInt(m["oid"]); oid > 0 {
		oids[oid] = true
	}
	if cl := auditStr(m["cloid"]); cl != "" {
		cloids[strings.ToLower(cl)] = true
	}
	if legs, ok := m["legs"].([]any); ok {
		for _, l := range legs {
			if lm, ok := l.(map[string]any); ok {
				collectOrderIDs(lm, oids, cloids)
			}
		}
	}
}

func auditStr(v any) string {
	s, _ := v.(string)
	return s
}

func auditInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
