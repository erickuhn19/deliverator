package oms

import (
	"math"

	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// RestingByCoin buckets a core open-order book into the MM's OWN resting orders per
// coin, in ONE pass. It uses Sz (the REMAINING unfilled size), drops trigger /
// reduce-only / position-TP-SL rows (the MM's quotes are plain resting limits), and —
// crucially — adopts only orders carrying the MM cloid marker (IsMMCloid), so the
// quote-diff never modifies or cancels a manual order or a co-tenant strategy's order
// on the same account. Share counts round to an integer (outcome szDecimals is 0).
func RestingByCoin(orders []hl.FrontendOpenOrder) map[string][]RestingOrder {
	out := map[string][]RestingOrder{}
	for _, o := range orders {
		if o.IsTrigger || o.ReduceOnly || o.IsPositionTpSl {
			continue
		}
		cloid := ""
		if o.Cloid != nil {
			cloid = *o.Cloid
		}
		if !IsMMCloid(cloid) {
			continue // not an MM-placed quote — leave it untouched
		}
		side := core.Buy
		if o.Side == hl.OrderSideAsk {
			side = core.Sell
		}
		out[o.Coin] = append(out[o.Coin], RestingOrder{
			Oid:         o.Oid,
			Cloid:       cloid,
			Coin:        o.Coin,
			Side:        side,
			Px:          o.LimitPx,
			RemainingSz: int64(math.Round(o.Sz)),
		})
	}
	return out
}

// RestingForCoin returns the MM's own resting orders for ONE coin. Prefer
// RestingByCoin when iterating many coins (this rebuilds the map each call).
func RestingForCoin(orders []hl.FrontendOpenOrder, coin string) []RestingOrder {
	return RestingByCoin(orders)[coin]
}

// InventoryByCoin folds outcome positions into a coin -> share-count map. An outcome
// holding is a long token balance (Side "long", Szi the share count), so shares are
// non-negative; a non-outcome position is ignored.
func InventoryByCoin(positions []core.PositionView) map[string]int64 {
	inv := map[string]int64{}
	for _, p := range positions {
		if p.Class != "outcome" {
			continue
		}
		if v, ok := mm.ParseFloat(p.Szi); ok {
			inv[p.Coin] = int64(math.Round(math.Abs(v)))
		}
	}
	return inv
}

// PairInventory reads a market's two legs out of a coin->shares map into an
// mm.Inventory (Yes = the market's own Yes coin, No = its sibling).
func PairInventory(m core.Market, byCoin map[string]int64) mm.Inventory {
	return mm.Inventory{
		Yes: byCoin[mm.YesCoin(m.Outcome)],
		No:  byCoin[mm.NoCoin(m.Outcome)],
	}
}
