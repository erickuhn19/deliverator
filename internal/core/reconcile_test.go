package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// one live resting BTC limit order, oid 7, carrying cloid ...aa.
const liveOrderAA = `[{"coin":"BTC","oid":7,"cloid":"0x000000000000000000000000000000aa","limitPx":"60000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0"}]`

func reconcileResp(orders, clearing, orderStatus string) respFn {
	return func(_, typ string, _ map[string]any) (int, string) {
		switch typ {
		case "frontendOpenOrders":
			return 200, orders
		case "clearinghouseState":
			return 200, clearing
		case "orderStatus":
			return 200, orderStatus
		case "spotClearinghouseState":
			return 200, `{"balances":[]}`
		}
		return 200, `{}`
	}
}

// A live order with no audit record of placing it is an orphan → not clean.
func TestReconcileFlagsOrphan(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, reconcileResp(liveOrderAA, emptyState, `{}`))
	v, err := c.Reconcile(ctx, ReconcileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.OrphanOrders) != 1 || v.OrphanOrders[0].Oid != 7 {
		t.Fatalf("want 1 orphan oid=7, got %+v", v.OrphanOrders)
	}
	if v.Clean {
		t.Fatal("orphan present => Clean must be false")
	}
}

// The same order, but recorded in the audit trail, is not an orphan.
func TestReconcileKnownOrderIsClean(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, reconcileResp(liveOrderAA, emptyState, `{}`))
	c.audit.Append(map[string]any{
		"action": "order", "oid": 7, "cloid": "0x000000000000000000000000000000aa",
		"coin": "BTC", "side": "buy", "status": "resting",
	})
	v, err := c.Reconcile(ctx, ReconcileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.OrphanOrders) != 0 {
		t.Fatalf("known order must not be an orphan, got %+v", v.OrphanOrders)
	}
	if !v.Clean {
		t.Fatalf("expected clean, got divergences %v", v.Divergences)
	}
}

// An in-flight cloid the exchange has never heard of resolves to absent/resubmit
// (the exit-42 safe-to-resubmit case) and trips the divergence.
func TestReconcileSuspectAbsent(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, reconcileResp(`[]`, emptyState, `{"status":"unknownOid"}`))
	v, err := c.Reconcile(ctx, ReconcileOpts{SuspectCloids: []string{"0x000000000000000000000000000000bb"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Suspects) != 1 || v.Suspects[0].Status != "absent" || v.Suspects[0].Action != "resubmit" {
		t.Fatalf("want absent/resubmit, got %+v", v.Suspects)
	}
	if v.Clean {
		t.Fatal("absent suspect => Clean must be false")
	}
}

// A suspect cloid carried by a live resting order resolves to resting/adopt and,
// with that order recorded, the whole pass is clean.
func TestReconcileSuspectRestingAdopt(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, reconcileResp(liveOrderAA, emptyState, `{}`))
	c.audit.Append(map[string]any{
		"action": "order", "oid": 7, "cloid": "0x000000000000000000000000000000aa",
		"coin": "BTC", "side": "buy", "status": "resting",
	})
	v, err := c.Reconcile(ctx, ReconcileOpts{SuspectCloids: []string{"0x000000000000000000000000000000aa"}})
	if err != nil {
		t.Fatal(err)
	}
	s := v.Suspects
	if len(s) != 1 || !s[0].Found || s[0].Status != "resting" || s[0].Action != "adopt" || s[0].Oid != 7 {
		t.Fatalf("want found resting/adopt oid=7, got %+v", s)
	}
	if !v.Clean {
		t.Fatalf("resting suspect + known order => clean, got %v", v.Divergences)
	}
}

// An audited resting order that is no longer live is reported as closed_since but
// is informational — it must NOT trip the divergence (normal fill/cancel churn).
func TestReconcileClosedSinceIsInformational(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, reconcileResp(`[]`, emptyState, `{}`))
	c.audit.Append(map[string]any{
		"action": "order", "oid": 5, "cloid": "0x0000000000000000000000000000aabb",
		"coin": "BTC", "side": "buy", "status": "resting",
	})
	v, err := c.Reconcile(ctx, ReconcileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.ClosedSince) != 1 || v.ClosedSince[0].Oid != 5 {
		t.Fatalf("want closed_since oid=5, got %+v", v.ClosedSince)
	}
	if !v.Clean {
		t.Fatalf("closed_since is informational => Clean must stay true, got %v", v.Divergences)
	}
}
