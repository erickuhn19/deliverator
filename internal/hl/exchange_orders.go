package hl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// CreateOrderRequest is a single order in float terms; the encoder converts to
// the wire string format.
type CreateOrderRequest struct {
	Coin          string
	IsBuy         bool
	Price         float64
	Size          float64
	ReduceOnly    bool
	OrderType     OrderType
	ClientOrderID *string
}

// ModifyOrderRequest identifies an order by exactly one of Oid or Cloid.
type ModifyOrderRequest struct {
	Oid   *int64
	Cloid *Cloid
	Order CreateOrderRequest
}

type OrderStatusResting struct {
	Oid      int64   `json:"oid"`
	ClientID *string `json:"cloid"`
	Status   string  `json:"status"`
}

type OrderStatusFilled struct {
	TotalSz string `json:"totalSz"`
	AvgPx   string `json:"avgPx"`
	Oid     int    `json:"oid"`
}

// OrderStatus is one element of an order/modify response's statuses array.
type OrderStatus struct {
	Resting *OrderStatusResting `json:"resting,omitempty"`
	Filled  *OrderStatusFilled  `json:"filled,omitempty"`
	Error   *string             `json:"error,omitempty"`
	// Status holds a bare-string status, e.g. "waitingForTrigger" returned for the
	// resting tp/sl legs of a grouped (normalTpsl) bracket.
	Status string `json:"-"`
}

// UnmarshalJSON accepts either an object ({resting|filled|error}) or a bare
// string status (e.g. "waitingForTrigger" / "success" from a grouped bracket).
func (s *OrderStatus) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &s.Status)
	}
	type alias OrderStatus
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = OrderStatus(a)
	return nil
}

type OrderResponse struct {
	Statuses []OrderStatus `json:"statuses"`
}

func newOrderTypeWire(o CreateOrderRequest) (OrderWireType, error) {
	if o.OrderType.Limit != nil {
		return OrderWireType{Limit: &OrderWireTypeLimit{Tif: o.OrderType.Limit.Tif}}, nil
	}
	if o.OrderType.Trigger != nil {
		// Propagate a wire-encoding failure instead of silently substituting "0":
		// a trigger px that loses precision must abort the build before a nonce is
		// consumed, exactly like the limit-px/size paths below (#40). Coercing to
		// "0" would sign — and the exchange reject — a trigger at price zero.
		triggerPxWire, err := floatToWire(o.OrderType.Trigger.TriggerPx)
		if err != nil {
			return OrderWireType{}, fmt.Errorf("failed to wire trigger px: %w", err)
		}
		return OrderWireType{Trigger: &OrderWireTypeTrigger{
			IsMarket:  o.OrderType.Trigger.IsMarket,
			TriggerPx: triggerPxWire,
			Tpsl:      o.OrderType.Trigger.Tpsl,
		}}, nil
	}
	return OrderWireType{}, nil
}

func newCreateOrderAction(e *Exchange, orders []CreateOrderRequest, builder *BuilderInfo, grouping Grouping, priority int) (OrderAction, error) {
	wires := make([]OrderWire, len(orders))
	for i, order := range orders {
		priceWire, err := floatToWire(order.Price)
		if err != nil {
			return OrderAction{}, fmt.Errorf("failed to wire price for order %d: %w", i, err)
		}
		sizeWire, err := floatToWire(order.Size)
		if err != nil {
			return OrderAction{}, fmt.Errorf("failed to wire size for order %d: %w", i, err)
		}
		asset, ok := e.info.CoinToAsset(order.Coin)
		if !ok {
			return OrderAction{}, fmt.Errorf("coin %s not found in info", order.Coin)
		}
		otWire, err := newOrderTypeWire(order)
		if err != nil {
			return OrderAction{}, fmt.Errorf("failed to wire order type for order %d: %w", i, err)
		}
		wire := OrderWire{
			Asset:      asset,
			IsBuy:      order.IsBuy,
			LimitPx:    priceWire,
			Size:       sizeWire,
			ReduceOnly: order.ReduceOnly,
			OrderType:  otWire,
		}
		cloid, err := normalizeCloid(order.ClientOrderID)
		if err != nil {
			return OrderAction{}, fmt.Errorf("invalid cloid for order %d: %w", i, err)
		}
		wire.Cloid = cloid
		wires[i] = wire
	}
	if grouping == "" {
		grouping = GroupingNA
	}
	var g any = string(grouping)
	if priority > 0 {
		// Priority and tp/sl grouping are mutually exclusive — the grouping field
		// carries one or the other (Hyperliquid order-priority semantics).
		if grouping != GroupingNA {
			return OrderAction{}, fmt.Errorf("order priority fee cannot be combined with %q grouping", grouping)
		}
		g = PriorityGrouping(priority)
	}
	return OrderAction{Type: "order", Orders: wires, Grouping: g, Builder: builder}, nil
}

// Order places a single order and returns its status (statuses[0]). On a
// per-order error the status carries the error AND a non-nil error is returned.
func (e *Exchange) Order(ctx context.Context, req CreateOrderRequest, builder *BuilderInfo, priority int) (result OrderStatus, err error) {
	resp, err := e.BulkOrders(ctx, []CreateOrderRequest{req}, builder, priority)
	if err != nil && (resp == nil || len(resp.Data.Statuses) == 0) {
		return
	}
	if resp == nil || !resp.Ok {
		if err == nil {
			err = fmt.Errorf("failed to create order: %s", errOf(resp))
		}
		return
	}
	if len(resp.Data.Statuses) == 0 {
		if err == nil {
			err = fmt.Errorf("no status for order")
		}
		return
	}
	return resp.Data.Statuses[0], err
}

func (e *Exchange) BulkOrders(ctx context.Context, orders []CreateOrderRequest, builder *BuilderInfo, priority int) (*APIResponse[OrderResponse], error) {
	return e.BulkOrdersGrouped(ctx, orders, builder, GroupingNA, priority)
}

// BulkOrdersGrouped submits N orders in one signed action with the given grouping
// — GroupingNA for an independent batch, GroupingNormalTpsl for a linked
// [entry, tp, sl] OCO bracket.
func (e *Exchange) BulkOrdersGrouped(ctx context.Context, orders []CreateOrderRequest, builder *BuilderInfo, grouping Grouping, priority int) (*APIResponse[OrderResponse], error) {
	action, err := newCreateOrderAction(e, orders, builder, grouping, priority)
	if err != nil {
		return nil, err
	}
	var result *APIResponse[OrderResponse]
	if err := e.executeAction(ctx, action, &result); err != nil {
		return nil, err
	}
	if result != nil {
		for _, s := range result.Data.Statuses {
			if s.Error != nil {
				return result, fmt.Errorf("%s", *s.Error)
			}
		}
	}
	return result, nil
}

// newModifyWire builds one (oid, replacement-order) pair. Shared by the single
// modify action and batchModify so the per-order wire never drifts between them.
func newModifyWire(e *Exchange, req ModifyOrderRequest) (ModifyWire, error) {
	var wireOid any
	switch {
	case req.Oid != nil && req.Cloid != nil:
		return ModifyWire{}, fmt.Errorf("modify request must specify only one of Oid or Cloid")
	case req.Oid != nil:
		wireOid = *req.Oid
	case req.Cloid != nil:
		raw := req.Cloid.ToRaw()
		normalized, err := normalizeCloid(&raw)
		if err != nil {
			return ModifyWire{}, fmt.Errorf("invalid cloid for modify request: %w", err)
		}
		wireOid = *normalized
	default:
		return ModifyWire{}, fmt.Errorf("modify request must specify either Oid or Cloid")
	}

	priceWire, err := floatToWire(req.Order.Price)
	if err != nil {
		return ModifyWire{}, fmt.Errorf("failed to wire price: %w", err)
	}
	sizeWire, err := floatToWire(req.Order.Size)
	if err != nil {
		return ModifyWire{}, fmt.Errorf("failed to wire size: %w", err)
	}
	asset, ok := e.info.CoinToAsset(req.Order.Coin)
	if !ok {
		return ModifyWire{}, fmt.Errorf("coin %s not found in info", req.Order.Coin)
	}
	otWire, err := newOrderTypeWire(req.Order)
	if err != nil {
		return ModifyWire{}, fmt.Errorf("failed to wire order type: %w", err)
	}
	order := OrderWire{
		Asset:      asset,
		IsBuy:      req.Order.IsBuy,
		LimitPx:    priceWire,
		Size:       sizeWire,
		ReduceOnly: req.Order.ReduceOnly,
		OrderType:  otWire,
	}
	cloid, err := normalizeCloid(req.Order.ClientOrderID)
	if err != nil {
		return ModifyWire{}, fmt.Errorf("invalid cloid: %w", err)
	}
	order.Cloid = cloid
	return ModifyWire{Oid: wireOid, Order: order}, nil
}

func newModifyOrderAction(e *Exchange, req ModifyOrderRequest) (ModifyAction, error) {
	mw, err := newModifyWire(e, req)
	if err != nil {
		return ModifyAction{}, err
	}
	return ModifyAction{Type: "modify", Oid: mw.Oid, Order: mw.Order}, nil
}

// BatchModify modifies N resting orders in one signed action. Like BulkOrders it
// returns the full per-leg statuses with a non-nil error if any leg errored, so
// callers can surface partial outcomes.
func (e *Exchange) BatchModify(ctx context.Context, reqs []ModifyOrderRequest) (*APIResponse[OrderResponse], error) {
	modifies := make([]ModifyWire, 0, len(reqs))
	for i, req := range reqs {
		mw, err := newModifyWire(e, req)
		if err != nil {
			return nil, fmt.Errorf("modify %d: %w", i, err)
		}
		modifies = append(modifies, mw)
	}
	action := BatchModifyAction{Type: "batchModify", Modifies: modifies}
	var result *APIResponse[OrderResponse]
	if err := e.executeAction(ctx, action, &result); err != nil {
		return nil, err
	}
	if result != nil {
		for _, s := range result.Data.Statuses {
			if s.Error != nil {
				return result, fmt.Errorf("%s", *s.Error)
			}
		}
	}
	return result, nil
}

// ModifyOrder modifies an existing order and returns its new status.
//
// A successful single modify returns {"status":"ok","response":{"type":"default"}}
// with NO data — unlike order/cancel. So we parse the envelope leniently: a
// non-ok envelope or a per-order error is a failure; an ok response with no
// statuses is a success (the order is modified/resting, discoverable via a
// follow-up orders/status query).
func (e *Exchange) ModifyOrder(ctx context.Context, req ModifyOrderRequest) (result OrderStatus, err error) {
	action, err := newModifyOrderAction(e, req)
	if err != nil {
		return result, fmt.Errorf("failed to create modify action: %w", err)
	}
	nonce := e.nextNonce()
	sig, err := e.signL1Action(action, nonce)
	if err != nil {
		return result, err
	}
	raw, err := e.postAction(ctx, action, sig, nonce)
	if err != nil {
		return result, fmt.Errorf("failed to modify order: %w", err)
	}
	var env struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return result, fmt.Errorf("failed to parse modify response: %w", err)
	}
	if env.Status != "ok" {
		var msg string
		_ = json.Unmarshal(env.Response, &msg) // failure response is a plain string
		if msg == "" {
			msg = env.Status
		}
		return result, fmt.Errorf("failed to modify order: %s", msg)
	}
	// Some modify paths (e.g. a crossing modify that fills) may carry order
	// statuses; surface a per-order error or the resulting status when present.
	var inner struct {
		Data *struct {
			Statuses []OrderStatus `json:"statuses"`
		} `json:"data"`
	}
	_ = json.Unmarshal(env.Response, &inner)
	if inner.Data != nil && len(inner.Data.Statuses) > 0 {
		st := inner.Data.Statuses[0]
		if st.Error != nil {
			return st, fmt.Errorf("%s", *st.Error)
		}
		return st, nil
	}
	return result, nil // ok, no data ("type":"default") — modify accepted
}

// MarketOpen places an aggressive IOC order at the slippage-adjusted price.
func (e *Exchange) MarketOpen(
	ctx context.Context,
	name string,
	isBuy bool,
	sz float64,
	px *float64,
	slippage float64,
	cloid *string,
	builder *BuilderInfo,
) (OrderStatus, error) {
	slippagePrice, err := e.SlippagePrice(ctx, name, isBuy, slippage, px)
	if err != nil {
		return OrderStatus{}, err
	}
	return e.Order(ctx, CreateOrderRequest{
		Coin:          name,
		IsBuy:         isBuy,
		Size:          sz,
		Price:         slippagePrice,
		OrderType:     OrderType{Limit: &LimitOrderType{Tif: TifIoc}},
		ReduceOnly:    false,
		ClientOrderID: cloid,
	}, builder, 0)
}

// MarketClose flattens (or reduces) the position in coin with an aggressive IOC
// reduce-only order. With sz nil it closes the full position size.
func (e *Exchange) MarketClose(
	ctx context.Context,
	coin string,
	sz *float64,
	px *float64,
	slippage float64,
	cloid *string,
	builder *BuilderInfo,
) (OrderStatus, error) {
	address := e.accountAddr
	if address == "" {
		address = e.vault
	}
	// A "<dex>:<coin>" position lives in that HIP-3 sub-dex's clearinghouse.
	dex := ""
	if i := strings.IndexByte(coin, ':'); i > 0 {
		dex = coin[:i]
	}
	userState, err := e.info.UserStateForDex(ctx, address, dex)
	if err != nil {
		return OrderStatus{}, err
	}
	for _, assetPos := range userState.AssetPositions {
		pos := assetPos.Position
		if coin != pos.Coin {
			continue
		}
		szi := parseFloat(pos.Szi)
		size := absFloat(szi)
		if sz != nil {
			size = *sz
		}
		isBuy := szi < 0
		slippagePrice, err := e.SlippagePrice(ctx, coin, isBuy, slippage, px)
		if err != nil {
			return OrderStatus{}, err
		}
		return e.Order(ctx, CreateOrderRequest{
			Coin:          coin,
			IsBuy:         isBuy,
			Size:          size,
			Price:         slippagePrice,
			OrderType:     OrderType{Limit: &LimitOrderType{Tif: TifIoc}},
			ReduceOnly:    true,
			ClientOrderID: cloid,
		}, builder, 0)
	}
	return OrderStatus{}, fmt.Errorf("position not found for coin: %s", coin)
}

// SlippagePrice converts a mid (or supplied px) into an aggressive limit price
// rounded to 5 significant figures then to the asset's allowed decimals. The
// float64 path matches the exchange exactly — do not substitute decimal math.
func (e *Exchange) SlippagePrice(ctx context.Context, name string, isBuy bool, slippage float64, px *float64) (float64, error) {
	asset := e.info.coinToAsset[name]
	// Spot ids are [10000, 100000); sub-dex (HIP-3) ids are >= 100000 and are NOT
	// spot. A "<dex>:<coin>" name draws its mid from that dex's allMids. HIP-4
	// outcome ids are >= 100_000_000 and price in (0,1).
	isSpot := asset >= spotAssetIndexOffset && asset < perpDexAssetBase
	isOutcome := asset >= outcomeAssetBase

	var price float64
	if px != nil {
		price = *px
	} else {
		dex := ""
		if i := strings.IndexByte(name, ':'); i > 0 {
			dex = name[:i]
		}
		mids, err := e.info.AllMidsForDex(ctx, dex)
		if err != nil {
			return 0, err
		}
		midPriceStr, ok := mids[name]
		if !ok && isSpot {
			// Non-canonical spot pairs are keyed by "@<index>", not the pair name.
			midPriceStr, ok = mids["@"+strconv.Itoa(asset-spotAssetIndexOffset)]
		}
		if !ok {
			return 0, fmt.Errorf("could not get mid price for coin: %s", name)
		}
		price = parseFloat(midPriceStr)
	}

	// Outcome prices are probabilities in (0,1): a multiplicative slippage (mid *
	// 1.05) would push a 0.97 mid past 1.0 (an invalid order). Use ADDITIVE slippage
	// clamped into the valid band, then round to the outcome tick.
	if isOutcome {
		if isBuy {
			price += slippage
		} else {
			price -= slippage
		}
		price = clampOutcomePrice(price)
		price = roundToSignificantFigures(price, 5)
		price = roundToDecimals(price, outcomePriceDecimals)
		return clampOutcomePrice(price), nil
	}

	if isBuy {
		price *= 1 + slippage
	} else {
		price *= 1 - slippage
	}
	price = roundToSignificantFigures(price, 5)

	decimals := 6
	if isSpot {
		decimals = 8
	}
	szDecimals := e.info.assetToDecimal[asset]
	return roundToDecimals(price, decimals-szDecimals), nil
}

func errOf[T any](resp *APIResponse[T]) string {
	if resp == nil {
		return ""
	}
	return resp.Err
}
