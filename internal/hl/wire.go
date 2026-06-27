package hl

// Action wire structs. Field declaration order IS the msgpack serialization
// order, which is part of the signature — do not reorder. Tags mirror the
// Hyperliquid Python SDK insertion order.

// BuilderInfo attaches a builder address + fee (tenths of a basis point).
type BuilderInfo struct {
	Builder string `json:"b" msgpack:"b"`
	Fee     int    `json:"f" msgpack:"f"`
}

// OrderWire is one order in an order action. Order: a,b,p,s,r,t,c.
type OrderWire struct {
	Asset      int           `json:"a"           msgpack:"a"`
	IsBuy      bool          `json:"b"           msgpack:"b"`
	LimitPx    string        `json:"p"           msgpack:"p"`
	Size       string        `json:"s"           msgpack:"s"`
	ReduceOnly bool          `json:"r"           msgpack:"r"`
	OrderType  OrderWireType `json:"t"           msgpack:"t"`
	Cloid      *string       `json:"c,omitempty" msgpack:"c,omitempty"`
}

type OrderWireType struct {
	Limit   *OrderWireTypeLimit   `json:"limit,omitempty"   msgpack:"limit,omitempty"`
	Trigger *OrderWireTypeTrigger `json:"trigger,omitempty" msgpack:"trigger,omitempty"`
}

type OrderWireTypeLimit struct {
	Tif Tif `json:"tif" msgpack:"tif"`
}

// OrderWireTypeTrigger order: isMarket, triggerPx, tpsl (matches Python SDK).
type OrderWireTypeTrigger struct {
	IsMarket  bool   `json:"isMarket"  msgpack:"isMarket"`
	TriggerPx string `json:"triggerPx" msgpack:"triggerPx"`
	Tpsl      Tpsl   `json:"tpsl"      msgpack:"tpsl"`
}

// OrderAction order: type, orders, grouping, builder. Grouping is `any` because
// Hyperliquid overloads it: normally a string ("na"/"normalTpsl"/"positionTpsl"),
// but an object {"p": rate} when an ORDER PRIORITY fee is attached (the two are
// mutually exclusive — priority can't combine with a tp/sl grouping). A plain
// string value encodes byte-identically to the old `string` field, so existing
// signed vectors are unchanged.
type OrderAction struct {
	Type     string       `json:"type"              msgpack:"type"`
	Orders   []OrderWire  `json:"orders"            msgpack:"orders"`
	Grouping any          `json:"grouping"          msgpack:"grouping"`
	Builder  *BuilderInfo `json:"builder,omitempty" msgpack:"builder,omitempty"`
}

// priorityGrouping is the {"p": rate} form of an order action's grouping when an
// order-priority fee is set. rate = p/1e8 of filled notional, paid in HYPE from
// the undelegated staking balance and burned; HL caps it at 8 bps (p=80000).
type priorityGrouping struct {
	P int `json:"p" msgpack:"p"`
}

// PriorityGrouping returns the grouping value for an order action carrying an
// order-priority fee of rate p/1e8. Pass it where a Grouping string would go.
func PriorityGrouping(p int) any { return priorityGrouping{P: p} }

// ModifyAction modifies one order. Oid is int64 (by oid) or string (by cloid).
type ModifyAction struct {
	Type  string    `json:"type" msgpack:"type"`
	Oid   any       `json:"oid"  msgpack:"oid"`
	Order OrderWire `json:"order" msgpack:"order"`
}

// ModifyWire is one (oid, replacement order) pair — the body of a single modify
// and the element of a batchModify's `modifies` array. Field name/order match
// the Hyperliquid action schema (load-bearing for the msgpack action hash).
type ModifyWire struct {
	Oid   any       `json:"oid"   msgpack:"oid"`
	Order OrderWire `json:"order" msgpack:"order"`
}

// BatchModifyAction modifies N resting orders in one signed action.
type BatchModifyAction struct {
	Type     string       `json:"type"     msgpack:"type"`
	Modifies []ModifyWire `json:"modifies" msgpack:"modifies"`
}

// CancelOrderWire cancels one order by (asset, oid).
type CancelOrderWire struct {
	Asset   int   `json:"a" msgpack:"a"`
	OrderID int64 `json:"o" msgpack:"o"`
}

type CancelAction struct {
	Type    string            `json:"type"    msgpack:"type"`
	Cancels []CancelOrderWire `json:"cancels" msgpack:"cancels"`
}

// CancelByCloidWire uses keys "asset"/"cloid" (NOT "a"/"o") per the API.
type CancelByCloidWire struct {
	Asset    int    `json:"asset" msgpack:"asset"`
	ClientID string `json:"cloid" msgpack:"cloid"`
}

type CancelByCloidAction struct {
	Type    string              `json:"type"    msgpack:"type"`
	Cancels []CancelByCloidWire `json:"cancels" msgpack:"cancels"`
}

type UpdateLeverageAction struct {
	Type     string `json:"type"     msgpack:"type"`
	Asset    int    `json:"asset"    msgpack:"asset"`
	IsCross  bool   `json:"isCross"  msgpack:"isCross"`
	Leverage int    `json:"leverage" msgpack:"leverage"`
}

// UpdateIsolatedMarginAction adds (isBuy true) or removes (false) margin.
// Ntli is an INTEGER in USD*1e6 — HL types this field as an int; a float here
// breaks signature recovery (see Exchange.UpdateIsolatedMargin).
type UpdateIsolatedMarginAction struct {
	Type  string `json:"type"  msgpack:"type"`
	Asset int    `json:"asset" msgpack:"asset"`
	IsBuy bool   `json:"isBuy" msgpack:"isBuy"`
	Ntli  int    `json:"ntli"  msgpack:"ntli"`
}

type ScheduleCancelAction struct {
	Type string `json:"type"           msgpack:"type"`
	Time *int64 `json:"time,omitempty" msgpack:"time,omitempty"`
}

// SetReferrerAction applies a referral code to the signing account (one-time).
// It is an L1 action, so the agent/API wallet can sign it.
type SetReferrerAction struct {
	Type string `json:"type" msgpack:"type"`
	Code string `json:"code" msgpack:"code"`
}

// ---- TWAP ----

// TwapWire is the inner twap payload. Order: a,b,s,r,m,t (asset, isBuy, size,
// reduceOnly, minutes, randomize) per the Python SDK twap_order wire.
type TwapWire struct {
	Asset      int    `json:"a" msgpack:"a"`
	IsBuy      bool   `json:"b" msgpack:"b"`
	Size       string `json:"s" msgpack:"s"`
	ReduceOnly bool   `json:"r" msgpack:"r"`
	Minutes    int    `json:"m" msgpack:"m"`
	Randomize  bool   `json:"t" msgpack:"t"`
}

type TwapOrderAction struct {
	Type string   `json:"type" msgpack:"type"`
	Twap TwapWire `json:"twap" msgpack:"twap"`
}

type TwapCancelAction struct {
	Type   string `json:"type" msgpack:"type"`
	Asset  int    `json:"a"    msgpack:"a"`
	TwapID int64  `json:"t"    msgpack:"t"`
}
