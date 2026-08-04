package serve

// The wire types for anything that signs.
//
// THESE EXIST FOR A SECURITY REASON, not for tidiness.
//
// core.OrderReq carries `Closing bool`, and its own comment says so plainly:
//
//	Closing marks a reductive exit (spot close ...). It exempts the order from
//	the NEW-exposure guards (allowlist, limit_only, max caps) ... Set only by
//	closeSpot — never from CLI/JSON input.
//
// Decoding a socket request straight into that struct would let any caller set
// `"Closing": true` and walk an ordinary opening order past the coin allowlist,
// limit_only and the notional caps. That is precisely the thing this server must
// never become: a way to launder around the risk envelope. The CLI does not
// expose the field, so serving it would make the socket strictly more powerful
// than the front-end it mirrors.
//
// So the wire has its OWN types, listing only what a caller may set, and the
// mapping into core is explicit. A field added to core.OrderReq in future is
// invisible here until someone deliberately exposes it — which is the correct
// default for a struct that reaches a signing path.

import (
	"fmt"
	"strings"

	"github.com/erickuhn19/deliverator/internal/core"
)

// OrderParams is an order as a caller may express it.
type OrderParams struct {
	Coin       string   `json:"coin"`
	Side       string   `json:"side"` // buy | sell
	Size       string   `json:"size"`
	Notional   float64  `json:"notional"`
	Limit      string   `json:"limit"` // "" => market
	Tif        string   `json:"tif"`   // Gtc | Ioc | Alo
	ReduceOnly bool     `json:"reduce_only"`
	Cloid      string   `json:"cloid"`
	BuilderFee *int     `json:"builder_fee"`
	Priority   *int     `json:"priority"`
	Slippage   float64  `json:"slippage"`
	Trigger    *Trigger `json:"trigger"`
}

// Trigger is a stop/take-profit attachment.
type Trigger struct {
	TriggerPx string `json:"trigger_px"`
	IsMarket  bool   `json:"is_market"`
	Tpsl      string `json:"tpsl"` // tp | sl
}

// toCore maps the wire order onto the engine's input. Note what is NOT set:
// Closing and panicFlatten stay at their zero values, unreachable from here.
func (p OrderParams) toCore() (core.OrderReq, error) {
	side, err := parseSide(p.Side)
	if err != nil {
		return core.OrderReq{}, err
	}
	if strings.TrimSpace(p.Coin) == "" {
		return core.OrderReq{}, fmt.Errorf("coin is required")
	}
	req := core.OrderReq{
		Coin:       p.Coin,
		Side:       side,
		Size:       p.Size,
		Notional:   p.Notional,
		Limit:      p.Limit,
		Tif:        p.Tif,
		ReduceOnly: p.ReduceOnly,
		Cloid:      p.Cloid,
		BuilderFee: p.BuilderFee,
		Priority:   p.Priority,
		Slippage:   p.Slippage,
	}
	if p.Trigger != nil {
		req.Trigger = &core.TriggerReq{
			TriggerPx: p.Trigger.TriggerPx,
			IsMarket:  p.Trigger.IsMarket,
			Tpsl:      p.Trigger.Tpsl,
		}
	}
	return req, nil
}

// parseSide is strict. core.Side is an int enum whose zero value is Buy, so a
// missing or misspelled side would silently become a BUY — the worst possible
// default for a field that says which way money moves.
func parseSide(s string) (core.Side, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "buy", "b", "bid":
		return core.Buy, nil
	case "sell", "s", "ask", "a":
		return core.Sell, nil
	case "":
		return 0, fmt.Errorf("side is required (buy|sell); it has no safe default")
	default:
		return 0, fmt.Errorf("side %q is not buy or sell", s)
	}
}

// CancelParams is a cancellation as a caller may express it. Every field here is
// already reachable from the CLI, so this is a straight rename rather than a
// narrowing — it exists so the wire contract does not silently inherit future
// additions to core.CancelReq.
type CancelParams struct {
	Oid    *int64   `json:"oid"`
	Cloid  string   `json:"cloid"`
	Oids   []int64  `json:"oids"`
	Cloids []string `json:"cloids"`
	All    bool     `json:"all"`
	Coin   string   `json:"coin"`
}

func (p CancelParams) toCore() core.CancelReq {
	return core.CancelReq{
		Oid: p.Oid, Cloid: p.Cloid, Oids: p.Oids,
		Cloids: p.Cloids, All: p.All, Coin: p.Coin,
	}
}
