package hl

import (
	"context"
	"fmt"
	"math"
)

// ---- cancel ----

type CancelOrderRequest struct {
	Coin    string
	OrderID int64
}

type CancelOrderResponse struct {
	Statuses MixedArray `json:"statuses"`
}

func (e *Exchange) Cancel(ctx context.Context, coin string, oid int64) (*APIResponse[CancelOrderResponse], error) {
	return e.BulkCancel(ctx, []CancelOrderRequest{{Coin: coin, OrderID: oid}})
}

func (e *Exchange) BulkCancel(ctx context.Context, requests []CancelOrderRequest) (*APIResponse[CancelOrderResponse], error) {
	cancels := make([]CancelOrderWire, 0, len(requests))
	for _, req := range requests {
		asset, ok := e.info.CoinToAsset(req.Coin)
		if !ok {
			return nil, fmt.Errorf("coin %s not found in info", req.Coin)
		}
		cancels = append(cancels, CancelOrderWire{Asset: asset, OrderID: req.OrderID})
	}
	action := CancelAction{Type: "cancel", Cancels: cancels}
	return e.executeCancel(ctx, action)
}

type CancelOrderRequestByCloid struct {
	Coin  string
	Cloid string
}

func (e *Exchange) CancelByCloid(ctx context.Context, coin, cloid string) (*APIResponse[CancelOrderResponse], error) {
	return e.BulkCancelByCloids(ctx, []CancelOrderRequestByCloid{{Coin: coin, Cloid: cloid}})
}

func (e *Exchange) BulkCancelByCloids(ctx context.Context, requests []CancelOrderRequestByCloid) (*APIResponse[CancelOrderResponse], error) {
	cancels := make([]CancelByCloidWire, len(requests))
	for i, req := range requests {
		normalized, err := normalizeCloid(&req.Cloid)
		if err != nil {
			return nil, fmt.Errorf("invalid cloid for cancel request %d: %w", i, err)
		}
		if normalized == nil {
			return nil, fmt.Errorf("cloid is required for cancel by cloid request %d", i)
		}
		asset, ok := e.info.CoinToAsset(req.Coin)
		if !ok {
			return nil, fmt.Errorf("coin %s not found in info", req.Coin)
		}
		cancels[i] = CancelByCloidWire{Asset: asset, ClientID: *normalized}
	}
	action := CancelByCloidAction{Type: "cancelByCloid", Cancels: cancels}
	return e.executeCancel(ctx, action)
}

func (e *Exchange) executeCancel(ctx context.Context, action any) (*APIResponse[CancelOrderResponse], error) {
	var res *APIResponse[CancelOrderResponse]
	if err := e.executeAction(ctx, action, &res); err != nil {
		return nil, err
	}
	if res == nil || !res.Ok {
		if res != nil && res.Err != "" {
			return res, fmt.Errorf("%s", res.Err)
		}
		return res, fmt.Errorf("cancel failed")
	}
	if err := res.Data.Statuses.FirstError(); err != nil {
		return res, err
	}
	return res, nil
}

// ---- leverage / margin ----

func (e *Exchange) UpdateLeverage(ctx context.Context, leverage int, name string, isCross bool) (*UserState, error) {
	asset, ok := e.info.CoinToAsset(name)
	if !ok {
		return nil, fmt.Errorf("coin %s not found in info", name)
	}
	action := UpdateLeverageAction{Type: "updateLeverage", Asset: asset, IsCross: isCross, Leverage: leverage}
	// Success carries no data; executeChecked surfaces a status:"err" rejection
	// (e.g. "Insufficient margin") instead of silently reporting success.
	if err := e.executeChecked(ctx, action); err != nil {
		return nil, err
	}
	return &UserState{}, nil
}

// UpdateIsolatedMargin adds (amount>0) or removes (amount<0) isolated margin.
//
// MUST-VERIFY (parity landmine): this matches the reference Go SDK, which sends
// `ntli` as a RAW USD float. The official Python SDK instead sends an integer in
// 1e-6 units (int(usd*1e6)). We mirror the Go SDK so the differential signing
// tests stay byte-identical; before relying on this on mainnet, confirm on
// testnet that a $X margin add moves margin by exactly $X. If the exchange
// honors only the integer form, change Ntli to FloatToUsdInt(amount).
func (e *Exchange) UpdateIsolatedMargin(ctx context.Context, amount float64, name string) (*UserState, error) {
	asset, ok := e.info.CoinToAsset(name)
	if !ok {
		return nil, fmt.Errorf("coin %s not found in info", name)
	}
	// Match the official Python SDK exactly (both points verified live on mainnet,
	// where the reference Go SDK's encoding was wrong):
	//  1. ntli is a SIGNED INTEGER in USD*1e6 — a raw float makes the exchange
	//     recover a garbage signer ("User or API Wallet 0x... does not exist").
	//  2. isBuy is ALWAYS true; the sign of ntli decides add (+) vs remove (-).
	//     The Go SDK's isBuy=amount>0 + abs(ntli) silently ADDED on a remove.
	action := UpdateIsolatedMarginAction{
		Type:  "updateIsolatedMargin",
		Asset: asset,
		IsBuy: true,
		Ntli:  int(math.Round(amount * 1e6)),
	}
	if err := e.executeChecked(ctx, action); err != nil {
		return nil, err
	}
	return &UserState{}, nil
}

// ---- schedule cancel (dead man's switch) ----

type ScheduleCancelResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (e *Exchange) ScheduleCancel(ctx context.Context, scheduleTime *int64) (*ScheduleCancelResponse, error) {
	action := ScheduleCancelAction{Type: "scheduleCancel", Time: scheduleTime}
	// Success carries no data. A rejected arm (e.g. "Cannot set scheduled cancel
	// time until enough volume traded") MUST surface as an error — otherwise the
	// dead-man's switch is falsely reported as armed.
	if err := e.executeChecked(ctx, action); err != nil {
		return nil, err
	}
	return &ScheduleCancelResponse{Status: "ok"}, nil
}

// SetReferrer applies a referral code to the signing account (one-time). It is an
// L1 action so the agent/API wallet can sign it. A rejection (already referred,
// invalid/own code, not enough volume) surfaces as an error.
func (e *Exchange) SetReferrer(ctx context.Context, code string) error {
	return e.executeChecked(ctx, SetReferrerAction{Type: "setReferrer", Code: code})
}

// ---- TWAP ----

// TwapStatus is the inner status of a twapOrder response: either running with a
// twapId, or an error string.
type TwapStatus struct {
	Running *struct {
		TwapID int64 `json:"twapId"`
	} `json:"running,omitempty"`
	Error *string `json:"error,omitempty"`
}

type twapOrderResponseData struct {
	Status TwapStatus `json:"status"`
}

// TwapOrder submits a TWAP order sliced over `minutes`. Returns the inner
// status (running twapId or error).
//
// MUST-VERIFY: wire field letters (a,b,s,r,m,t), the "twap" wrapper key, and the
// response shape are ported from the Python SDK; there is no reference Go SDK
// oracle for TWAP. Confirm against a testnet round-trip before mainnet use.
func (e *Exchange) TwapOrder(
	ctx context.Context,
	coin string,
	isBuy bool,
	sz float64,
	reduceOnly bool,
	minutes int,
	randomize bool,
) (TwapStatus, error) {
	asset, ok := e.info.CoinToAsset(coin)
	if !ok {
		return TwapStatus{}, fmt.Errorf("coin %s not found in info", coin)
	}
	sizeWire, err := floatToWire(sz)
	if err != nil {
		return TwapStatus{}, fmt.Errorf("failed to wire twap size: %w", err)
	}
	action := TwapOrderAction{
		Type: "twapOrder",
		Twap: TwapWire{
			Asset:      asset,
			IsBuy:      isBuy,
			Size:       sizeWire,
			ReduceOnly: reduceOnly,
			Minutes:    minutes,
			Randomize:  randomize,
		},
	}
	var resp APIResponse[twapOrderResponseData]
	if err := e.executeAction(ctx, action, &resp); err != nil {
		return TwapStatus{}, err
	}
	if !resp.Ok {
		return TwapStatus{}, fmt.Errorf("twap order failed: %s", resp.Err)
	}
	st := resp.Data.Status
	if st.Error != nil {
		return st, fmt.Errorf("%s", *st.Error)
	}
	return st, nil
}

// TwapCancel cancels a running TWAP by its id.
func (e *Exchange) TwapCancel(ctx context.Context, coin string, twapID int64) (*APIResponse[MixedValue], error) {
	asset, ok := e.info.CoinToAsset(coin)
	if !ok {
		return nil, fmt.Errorf("coin %s not found in info", coin)
	}
	action := TwapCancelAction{Type: "twapCancel", Asset: asset, TwapID: twapID}
	var resp *APIResponse[MixedValue]
	if err := e.executeAction(ctx, action, &resp); err != nil {
		return nil, err
	}
	if resp == nil || !resp.Ok {
		if resp != nil && resp.Err != "" {
			return resp, fmt.Errorf("%s", resp.Err)
		}
		return resp, fmt.Errorf("twap cancel failed")
	}
	// A logically-failed cancel (e.g. unknown/finished twap id) can still return
	// envelope status:"ok" with the error nested in data — mirror the regular
	// cancel path and surface it instead of reporting a false success. The
	// success/failure shapes are pinned by TestTwapCancelRoundTrips and
	// TestTwapCancelInnerError; any other shape fails closed (treated as an error).
	if err := twapCancelInnerError(resp.Data); err != nil {
		return resp, err
	}
	return resp, nil
}

// twapCancelInnerError inspects a twapCancel response's data and returns an error
// unless it POSITIVELY confirms success. HL returns envelope status:"ok" even for
// a logically-failed cancel (an unknown/finished twap id), nesting the real
// outcome in data: {"status":"success"} on success, {"status":{"error":"..."}} on
// failure. Anything that is not the recognized success marker is treated as a
// failure (fail closed) — a still-running TWAP must never be reported as cancelled
// just because its response shape was unfamiliar (#41).
func twapCancelInnerError(data MixedValue) error {
	obj, ok := data.Object()
	if !ok {
		return fmt.Errorf("twap cancel: unrecognized response shape")
	}
	status, ok := obj["status"]
	if !ok {
		return fmt.Errorf("twap cancel: response missing status")
	}
	switch s := status.(type) {
	case string:
		if s == "success" {
			return nil
		}
		if s == "" {
			return fmt.Errorf("twap cancel: empty status")
		}
		return fmt.Errorf("%s", s)
	case map[string]any:
		if msg, ok := s["error"].(string); ok && msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("twap cancel: unrecognized status object")
	default:
		return fmt.Errorf("twap cancel: unrecognized status type")
	}
}
