package core

// Reduce-only contract (review 2026-07-01, cluster P0-A):
//  1. a reduce-only MARKET order must carry reduce-only on the wire ("r":true) —
//     the single-order market path routed through hl.MarketOpen, which
//     hard-coded r:false while the risk layer had already skipped every cap
//     because the request claimed reduce-only;
//  2. the envelope's reduce_only must report what actually went on the wire;
//  3. the always-on flip guard must reject a reduce-only order that cannot
//     reduce: same side as the position, or no open position at all.

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// wireOrder records the reduce-only flag of the first order posted to /exchange.
type wireOrder struct {
	seen bool
	r    bool
}

// reduceOnlyResp serves the reads Place needs and captures the wire r flag.
func reduceOnlyResp(clearing string, wire *wireOrder, exResp string) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "allMids":
				return 200, `{"BTC":"64000"}`
			case "clearinghouseState":
				return 200, clearing
			case "frontendOpenOrders":
				return 200, `[]`
			case "spotClearinghouseState":
				return 200, noSpot
			}
			return 200, `{}`
		}
		if action, ok := body["action"].(map[string]any); ok {
			if orders, ok := action["orders"].([]any); ok && len(orders) > 0 {
				if o, ok := orders[0].(map[string]any); ok {
					wire.seen = true
					wire.r, _ = o["r"].(bool)
				}
			}
		}
		return 200, exResp
	}
}

// A reduce-only market order must be reduce-only ON THE WIRE. Before the fix it
// was signed with r:false (a plain aggressive IOC order) while the envelope
// claimed reduce_only:true.
func TestPlaceReduceOnlyMarketWiresReduceOnly(t *testing.T) {
	clearing := clearingWith("100000", posWith("BTC", "0.1", "6400")) // long 0.1 BTC
	var wire wireOrder
	c, ctx := newTestClient(t, config.Default(), Options{},
		reduceOnlyResp(clearing, &wire, okOrder(`{"filled":{"totalSz":"0.05","avgPx":"64000","oid":9}}`)))
	res, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Sell, Size: "0.05", ReduceOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !wire.seen {
		t.Fatal("no order reached the wire")
	}
	if !wire.r {
		t.Fatal(`reduce-only market order was sent with "r":false — not reduce-only on the wire`)
	}
	if !res.ReduceOnly {
		t.Fatalf("envelope reduce_only must match the wire (r=%v): %+v", wire.r, res)
	}
}

// …and the envelope must match the wire in the negative direction too: a plain
// market order wires r:false and reports reduce_only:false.
func TestPlaceMarketEnvelopeReduceOnlyMatchesWire(t *testing.T) {
	clearing := clearingWith("100000")
	var wire wireOrder
	c, ctx := newTestClient(t, config.Default(), Options{},
		reduceOnlyResp(clearing, &wire, okOrder(`{"resting":{"oid":3}}`)))
	res, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "0.01"})
	if err != nil {
		t.Fatal(err)
	}
	if !wire.seen {
		t.Fatal("no order reached the wire")
	}
	if wire.r || res.ReduceOnly {
		t.Fatalf("plain market order must wire r:false and report reduce_only:false (wire=%v env=%v)", wire.r, res.ReduceOnly)
	}
}

// The review's runtime repro, end to end: long 1 BTC with a $100 order cap. A
// "reduce-only" market BUY (same side — a hallucinated flag) previously skipped
// every cap and went to the wire as a plain IOC buy, DOUBLING the position. It
// must reject as risk (exit 20) with nothing signed or sent.
func TestPlaceReduceOnlySameSideRejectedBeforeWire(t *testing.T) {
	clearing := clearingWith("100000", posWith("BTC", "1", "64000")) // long 1 BTC
	var wire wireOrder
	c, ctx := newTestClient(t, riskCfg(config.Risk{MaxOrderNotionalUSD: 100}), Options{},
		reduceOnlyResp(clearing, &wire, okOrder(`{"filled":{"totalSz":"1","avgPx":"64000","oid":9}}`)))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Buy, Size: "1", ReduceOnly: true})
	assertRiskCode(t, err, "reduce_only_same_side")
	if wire.seen {
		t.Fatal("a same-side reduce-only order must never reach the wire")
	}
}

// Flat: nothing to reduce (e.g. the position was already closed by a TP fill) —
// the order must reject as risk (exit 20), not open fresh exposure.
func TestPlaceReduceOnlyFlatRejectedBeforeWire(t *testing.T) {
	clearing := clearingWith("100000") // no positions
	var wire wireOrder
	c, ctx := newTestClient(t, config.Default(), Options{},
		reduceOnlyResp(clearing, &wire, okOrder(`{"filled":{"totalSz":"1","avgPx":"64000","oid":9}}`)))
	_, _, err := c.Place(ctx, OrderReq{Coin: "BTC", Side: Sell, Size: "1", ReduceOnly: true})
	assertRiskCode(t, err, "reduce_only_flat")
	if wire.seen {
		t.Fatal("a flat reduce-only order must never reach the wire")
	}
}
