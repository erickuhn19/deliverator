package hl

// Info is the unsigned read client (POST /info). It also owns the coin->assetId
// and assetId->szDecimals maps the order encoder needs.

import (
	"context"
	"encoding/json"
	"fmt"
)

// spotAssetIndexOffset is added to a spot universe index to form its asset id.
const spotAssetIndexOffset = 10000

type Info struct {
	transport      *httpTransport
	coinToAsset    map[string]int
	assetToDecimal map[int]int
	clientOpts     []ClientOpt
}

// NewInfo builds a read client. If meta/spotMeta are nil they are fetched (and
// a fetch failure panics — deliverator wraps the constructor in a recover so a
// network error surfaces as a normal error). perpDexs is unused (deliverator
// only targets the default perp dex); it is kept for signature parity.
func NewInfo(
	ctx context.Context,
	baseURL string,
	skipWS bool,
	meta *Meta,
	spotMeta *SpotMeta,
	perpDexs *MixedArray,
	opts ...InfoOpt,
) *Info {
	_ = skipWS
	_ = perpDexs
	info := &Info{
		coinToAsset:    make(map[string]int),
		assetToDecimal: make(map[int]int),
	}
	for _, opt := range opts {
		opt(info)
	}
	info.transport = newTransport(baseURL, info.clientOpts...)

	if meta == nil {
		var err error
		meta, err = info.Meta(ctx)
		if err != nil {
			panic(err)
		}
	}
	if spotMeta == nil {
		var err error
		spotMeta, err = info.SpotMeta(ctx)
		if err != nil {
			panic(err)
		}
	}

	// Default perp dex: asset id is the index into the meta universe.
	for asset, assetInfo := range meta.Universe {
		info.coinToAsset[assetInfo.Name] = asset
		info.assetToDecimal[asset] = assetInfo.SzDecimals
	}

	// Spot assets start at 10000; szDecimals comes from the pair's base token.
	tokensByIndex := make(map[int]SpotTokenInfo, len(spotMeta.Tokens))
	for _, t := range spotMeta.Tokens {
		tokensByIndex[t.Index] = t
	}
	for _, spotInfo := range spotMeta.Universe {
		asset := spotInfo.Index + spotAssetIndexOffset
		info.coinToAsset[spotInfo.Name] = asset
		if len(spotInfo.Tokens) > 0 {
			if tokenInfo, ok := tokensByIndex[spotInfo.Tokens[0]]; ok {
				info.assetToDecimal[asset] = tokenInfo.SzDecimals
			}
		}
	}
	return info
}

// CoinToAsset resolves a coin name to its integer asset id.
func (i *Info) CoinToAsset(coin string) (int, bool) {
	a, ok := i.coinToAsset[coin]
	return a, ok
}

func (i *Info) postTimeRangeRequest(
	ctx context.Context,
	requestType, user string,
	startTime int64,
	endTime *int64,
	extraParams map[string]any,
) ([]byte, error) {
	payload := map[string]any{"type": requestType, "startTime": startTime}
	if user != "" {
		payload["user"] = user
	}
	if endTime != nil {
		payload["endTime"] = *endTime
	}
	for k, v := range extraParams {
		payload[k] = v
	}
	resp, err := i.transport.post(ctx, "/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", requestType, err)
	}
	return resp, nil
}

// parseMetaResponse decodes a perp meta payload, including the loosely-typed
// marginTables tuple array.
func parseMetaResponse(resp []byte) (*Meta, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal meta response: %w", err)
	}
	var universe []AssetInfo
	if err := json.Unmarshal(raw["universe"], &universe); err != nil {
		return nil, fmt.Errorf("failed to unmarshal universe: %w", err)
	}

	marginTablesResult := []MarginTable{}
	if rawMT, ok := raw["marginTables"]; ok {
		var marginTables [][]json.RawMessage
		if err := json.Unmarshal(rawMT, &marginTables); err == nil {
			for _, mt := range marginTables {
				if len(mt) < 2 {
					continue
				}
				var id int
				_ = json.Unmarshal(mt[0], &id)
				var table struct {
					Description string       `json:"description"`
					MarginTiers []MarginTier `json:"marginTiers"`
				}
				_ = json.Unmarshal(mt[1], &table)
				marginTablesResult = append(marginTablesResult, MarginTable{
					ID:          id,
					Description: table.Description,
					MarginTiers: table.MarginTiers,
				})
			}
		}
	}

	collateralToken := 0
	if rawCT, ok := raw["collateralToken"]; ok {
		_ = json.Unmarshal(rawCT, &collateralToken)
	}

	return &Meta{Universe: universe, MarginTables: marginTablesResult, CollateralToken: collateralToken}, nil
}

func (i *Info) Meta(ctx context.Context) (*Meta, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "meta"})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch meta: %w", err)
	}
	return parseMetaResponse(resp)
}

// HIP-3 builder sub-dex perp asset ids: base + dexIndex*stride + indexInUniverse.
const (
	perpDexAssetBase   = 100000
	perpDexAssetStride = 10000
)

// PerpDexAsset returns the HIP-3 asset id for a coin at indexInUniverse on the
// dex at dexIndex.
func PerpDexAsset(dexIndex, indexInUniverse int) int {
	return perpDexAssetBase + dexIndex*perpDexAssetStride + indexInUniverse
}

// MetaForDex fetches the perp universe for a named builder sub-dex (HIP-3).
func (i *Info) MetaForDex(ctx context.Context, dex string) (*Meta, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "meta", "dex": dex})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch meta for dex %q: %w", dex, err)
	}
	return parseMetaResponse(resp)
}

// PerpDexNames returns perp dex names indexed by dex index (index 0 = "" = the
// default/main perp dex).
func (i *Info) PerpDexNames(ctx context.Context) ([]string, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "perpDexs"})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch perpDexs: %w", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal perpDexs: %w", err)
	}
	names := make([]string, len(raw))
	for idx, r := range raw {
		var obj struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(r, &obj) // null (main) -> ""
		names[idx] = obj.Name
	}
	return names, nil
}

// RegisterPerpDex registers a sub-dex's coins into coin->asset resolution so an
// order on "<dex>:<coin>" signs with the correct HIP-3 asset id.
func (i *Info) RegisterPerpDex(dexIndex int, m *Meta) {
	for j, a := range m.Universe {
		asset := PerpDexAsset(dexIndex, j)
		i.coinToAsset[a.Name] = asset
		i.assetToDecimal[asset] = a.SzDecimals
	}
}

// HIP-4 outcome-market asset ids: outcomeAssetBase + encoding, where the per-market
// encoding = 10*outcome + side and only side 0 (Yes) / 1 (No) are valid. This is a
// 4th, disjoint id band, well above perps [0,10000), spot [10000,100000), and HIP-3
// sub-dex perps (perpDexAssetBase + dexIndex*stride + idx, which never reach
// outcomeAssetBase). The order/book coin string is "#<encoding>".
const (
	outcomeAssetBase = 100_000_000
	outcomeMaxSide   = 1 // binary: only sides 0 and 1 exist
	// Outcome (probability) price bounds for marketable-order slippage: a price
	// must stay in the open interval (0,1); these are the valid extremes (≤5 sig
	// figs / ≤5 decimals) used to clamp an aggressive slippage price.
	outcomePriceDecimals = 5
	outcomeMinPrice      = 0.00001
	outcomeMaxPrice      = 0.99999
)

// clampOutcomePrice clamps a probability price into the valid open-interval band.
func clampOutcomePrice(p float64) float64 {
	if p < outcomeMinPrice {
		return outcomeMinPrice
	}
	if p > outcomeMaxPrice {
		return outcomeMaxPrice
	}
	return p
}

// OutcomeEncoding returns the per-market encoding (10*outcome + side).
func OutcomeEncoding(outcome, side int) int { return 10*outcome + side }

// OutcomeAsset returns the on-chain asset id for an outcome's side (0=Yes, 1=No).
func OutcomeAsset(outcome, side int) int { return outcomeAssetBase + OutcomeEncoding(outcome, side) }

// OutcomeCoin returns the order/book coin string ("#<encoding>") for an outcome side.
func OutcomeCoin(outcome, side int) string {
	return fmt.Sprintf("#%d", OutcomeEncoding(outcome, side))
}

// OutcomeMeta fetches the live HIP-4 outcome universe (binary Yes/No markets and
// their question groupings). Settled outcomes drop out of subsequent responses, so
// it is fetched fresh rather than cached.
func (i *Info) OutcomeMeta(ctx context.Context) (*OutcomeMeta, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "outcomeMeta"})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch outcomeMeta: %w", err)
	}
	var om OutcomeMeta
	if err := json.Unmarshal(resp, &om); err != nil {
		return nil, fmt.Errorf("failed to unmarshal outcomeMeta: %w", err)
	}
	return &om, nil
}

// RegisterOutcomes registers each outcome's Yes/No sides into coin->asset resolution
// so an order on "#<encoding>" signs with the correct HIP-4 asset id. Sizes are
// integer (szDecimals 0). Only sides 0 and 1 are registered.
func (i *Info) RegisterOutcomes(m *OutcomeMeta) {
	if m == nil {
		return
	}
	for _, o := range m.Outcomes {
		for side := 0; side < len(o.SideSpecs) && side <= outcomeMaxSide; side++ {
			asset := OutcomeAsset(o.Outcome, side)
			i.coinToAsset[OutcomeCoin(o.Outcome, side)] = asset
			i.assetToDecimal[asset] = 0
		}
	}
}

func (i *Info) SpotMeta(ctx context.Context) (*SpotMeta, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "spotMeta"})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch spot meta: %w", err)
	}
	var spotMeta SpotMeta
	if err := json.Unmarshal(resp, &spotMeta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spot meta response: %w", err)
	}
	return &spotMeta, nil
}

func (i *Info) UserState(ctx context.Context, address string) (*UserState, error) {
	return i.UserStateForDex(ctx, address, "")
}

// UserStateForDex returns the clearinghouse state for a specific perp dex. dex=""
// is the main perp dex; a sub-dex name (e.g. "xyz") returns that HIP-3 dex's
// positions/margin.
func (i *Info) UserStateForDex(ctx context.Context, address, dex string) (*UserState, error) {
	body := map[string]any{"type": "clearinghouseState", "user": address}
	if dex != "" {
		body["dex"] = dex
	}
	resp, err := i.transport.post(ctx, "/info", body)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user state: %w", err)
	}
	var result UserState
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user state: %w", err)
	}
	return &result, nil
}

// Referral returns a user's referral state (who referred them, volume, rewards).
func (i *Info) Referral(ctx context.Context, address string) (*ReferralInfo, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "referral", "user": address})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch referral: %w", err)
	}
	var ri ReferralInfo
	if err := json.Unmarshal(resp, &ri); err != nil {
		return nil, fmt.Errorf("failed to unmarshal referral: %w", err)
	}
	return &ri, nil
}

func (i *Info) SpotUserState(ctx context.Context, address string) (*SpotUserState, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "spotClearinghouseState", "user": address})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch spot user state: %w", err)
	}
	var result SpotUserState
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spot user state: %w", err)
	}
	return &result, nil
}

func (i *Info) FrontendOpenOrders(ctx context.Context, address string) ([]FrontendOpenOrder, error) {
	return i.FrontendOpenOrdersForDex(ctx, address, "")
}

// FrontendOpenOrdersForDex returns resting orders for a perp dex. dex="" is the
// main dex (+spot); a HIP-3 sub-dex name returns that dex's orders (coins keyed
// "<dex>:<coin>"). Like positions, open orders are per-dex — without the dex
// field, sub-dex resting orders never come back.
func (i *Info) FrontendOpenOrdersForDex(ctx context.Context, address, dex string) ([]FrontendOpenOrder, error) {
	body := map[string]any{"type": "frontendOpenOrders", "user": address}
	if dex != "" {
		body["dex"] = dex
	}
	resp, err := i.transport.post(ctx, "/info", body)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch frontend open orders: %w", err)
	}
	var result []FrontendOpenOrder
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal frontend open orders: %w", err)
	}
	return result, nil
}

// RunningTwap is a live TWAP (cancellable via twapCancel by coin+id). A running
// TWAP is NOT a frontendOpenOrder, so it must be enumerated separately.
type RunningTwap struct {
	ID   int64
	Coin string
}

// RunningTwaps returns the user's live TWAPs. They live in webData2.twapStates
// (an array of [id, state] tuples) — there is no lighter dedicated endpoint
// (userTwapStates/twapStates 422). Used so `panic` can stop them before they
// keep slicing and rebuild a position after the flatten.
func (i *Info) RunningTwaps(ctx context.Context, address string) ([]RunningTwap, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "webData2", "user": address})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch webData2: %w", err)
	}
	var wd struct {
		TwapStates []json.RawMessage `json:"twapStates"`
	}
	if err := json.Unmarshal(resp, &wd); err != nil {
		return nil, fmt.Errorf("failed to unmarshal twapStates: %w", err)
	}
	out := []RunningTwap{}
	for _, raw := range wd.TwapStates {
		var tuple []json.RawMessage
		if json.Unmarshal(raw, &tuple) != nil || len(tuple) < 2 {
			continue
		}
		var id int64
		var st struct {
			Coin string `json:"coin"`
		}
		if json.Unmarshal(tuple[0], &id) == nil && json.Unmarshal(tuple[1], &st) == nil {
			out = append(out, RunningTwap{ID: id, Coin: st.Coin})
		}
	}
	return out, nil
}

// TwapState is one running TWAP's progress, parsed from webData2.twapStates. The
// exchange drops a TWAP from this list once it finishes, so a status query that
// returns nothing for a known id means that TWAP is done (or never started).
type TwapState struct {
	ID          int64  `json:"-"`
	Coin        string `json:"coin"`
	Side        string `json:"side"` // HL encodes "B"/"A"
	Sz          string `json:"sz"`
	ExecutedSz  string `json:"executedSz"`
	ExecutedNtl string `json:"executedNtl"`
	Minutes     int    `json:"minutes"`
	ReduceOnly  bool   `json:"reduceOnly"`
	Randomize   bool   `json:"randomize"`
	Timestamp   int64  `json:"timestamp"`
}

// TwapStates returns the user's live TWAPs with progress (size, executed size and
// notional, minutes, start time). Like RunningTwaps it reads webData2.twapStates,
// but parses the full per-TWAP state rather than just the coin — this backs
// `twap status`. RunningTwaps is left untouched (the panic path depends on it).
func (i *Info) TwapStates(ctx context.Context, address string) ([]TwapState, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "webData2", "user": address})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch webData2: %w", err)
	}
	var wd struct {
		TwapStates []json.RawMessage `json:"twapStates"`
	}
	if err := json.Unmarshal(resp, &wd); err != nil {
		return nil, fmt.Errorf("failed to unmarshal twapStates: %w", err)
	}
	out := []TwapState{}
	for _, raw := range wd.TwapStates {
		var tuple []json.RawMessage
		if json.Unmarshal(raw, &tuple) != nil || len(tuple) < 2 {
			continue
		}
		var id int64
		var st TwapState
		if json.Unmarshal(tuple[0], &id) != nil || json.Unmarshal(tuple[1], &st) != nil {
			continue
		}
		st.ID = id
		out = append(out, st)
	}
	return out, nil
}

// TwapSliceFill is one fill produced by a TWAP slice (the fill plus the twap id it
// belongs to), from the userTwapSliceFills endpoint.
type TwapSliceFill struct {
	Fill   Fill  `json:"fill"`
	TwapID int64 `json:"twapId"`
}

// UserTwapSliceFills returns the per-slice fills of the user's TWAPs (avg px, size,
// fees per slice). Used by `twap status` for executed detail.
func (i *Info) UserTwapSliceFills(ctx context.Context, address string) ([]TwapSliceFill, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "userTwapSliceFills", "user": address})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch userTwapSliceFills: %w", err)
	}
	out := []TwapSliceFill{}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("failed to unmarshal userTwapSliceFills: %w", err)
	}
	return out, nil
}

// HistoricalOrders returns the user's closed-order history (filled, canceled,
// rejected, expired) — the lifecycle reads orderStatus shows for one order, but
// across all recent orders. Used for reconciliation and post-mortems. Same shape
// as an orderStatus entry: {order, status, statusTimestamp}.
func (i *Info) HistoricalOrders(ctx context.Context, address string) ([]OrderQueryResponse, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "historicalOrders", "user": address})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch historicalOrders: %w", err)
	}
	out := []OrderQueryResponse{}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("failed to unmarshal historicalOrders: %w", err)
	}
	return out, nil
}

// VenueFunding is one venue's predicted next-interval funding for a coin.
type VenueFunding struct {
	Venue                string `json:"venue"`
	FundingRate          string `json:"funding_rate"`
	NextFundingTime      int64  `json:"next_funding_time,omitempty"`
	FundingIntervalHours int    `json:"funding_interval_hours,omitempty"`
}

// PredictedFunding is the cross-venue predicted funding for one coin.
type PredictedFunding struct {
	Coin   string         `json:"coin"`
	Venues []VenueFunding `json:"venues"`
}

// PredictedFundings returns the forecast next-interval funding rate per coin and
// venue — the forward-looking signal for funding-carry strategies (userFunding
// only reports funding already paid/collected). The wire is an array of
// [coin, [[venue, {fundingRate, nextFundingTime, fundingIntervalHours}], ...]].
func (i *Info) PredictedFundings(ctx context.Context) ([]PredictedFunding, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "predictedFundings"})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch predictedFundings: %w", err)
	}
	var raw [][]json.RawMessage
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal predictedFundings: %w", err)
	}
	out := []PredictedFunding{}
	for _, entry := range raw {
		if len(entry) < 2 {
			continue
		}
		var coin string
		if json.Unmarshal(entry[0], &coin) != nil {
			continue
		}
		var venues [][]json.RawMessage
		if json.Unmarshal(entry[1], &venues) != nil {
			continue
		}
		pf := PredictedFunding{Coin: coin, Venues: []VenueFunding{}}
		for _, v := range venues {
			if len(v) < 2 {
				continue
			}
			var name string
			var data struct {
				FundingRate          string `json:"fundingRate"`
				NextFundingTime      int64  `json:"nextFundingTime"`
				FundingIntervalHours int    `json:"fundingIntervalHours"`
			}
			if json.Unmarshal(v[0], &name) == nil && json.Unmarshal(v[1], &data) == nil {
				pf.Venues = append(pf.Venues, VenueFunding{
					Venue: name, FundingRate: data.FundingRate,
					NextFundingTime: data.NextFundingTime, FundingIntervalHours: data.FundingIntervalHours,
				})
			}
		}
		out = append(out, pf)
	}
	return out, nil
}

func (i *Info) AllMids(ctx context.Context) (map[string]string, error) {
	return i.AllMidsForDex(ctx, "")
}

// AllMidsForDex returns mid prices for a perp dex. dex="" is the main dex (+spot);
// a sub-dex name returns that HIP-3 dex's mids, keyed by "<dex>:<coin>".
func (i *Info) AllMidsForDex(ctx context.Context, dex string) (map[string]string, error) {
	body := map[string]any{"type": "allMids"}
	if dex != "" {
		body["dex"] = dex
	}
	resp, err := i.transport.post(ctx, "/info", body)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch all mids: %w", err)
	}
	var result map[string]string
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal all mids: %w", err)
	}
	return result, nil
}

func (i *Info) UserFills(ctx context.Context, params UserFillsParams) ([]Fill, error) {
	payload := map[string]any{"type": "userFills", "user": params.Address}
	if params.AggregateByTime != nil {
		payload["aggregateByTime"] = *params.AggregateByTime
	}
	resp, err := i.transport.post(ctx, "/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user fills: %w", err)
	}
	var result []Fill
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user fills: %w", err)
	}
	return result, nil
}

func (i *Info) UserFillsByTime(
	ctx context.Context,
	address string,
	startTime int64,
	endTime *int64,
	aggregateByTime *bool,
) ([]Fill, error) {
	extra := map[string]any{}
	if aggregateByTime != nil {
		extra["aggregateByTime"] = *aggregateByTime
	}
	resp, err := i.postTimeRangeRequest(ctx, "userFillsByTime", address, startTime, endTime, extra)
	if err != nil {
		return nil, err
	}
	var result []Fill
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user fills by time: %w", err)
	}
	return result, nil
}

func (i *Info) UserFundingHistory(ctx context.Context, user string, startTime int64, endTime *int64) ([]UserFundingHistory, error) {
	resp, err := i.postTimeRangeRequest(ctx, "userFunding", user, startTime, endTime, nil)
	if err != nil {
		return nil, err
	}
	var result []UserFundingHistory
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user funding history: %w", err)
	}
	return result, nil
}

func (i *Info) UserNonFundingLedgerUpdates(ctx context.Context, user string, startTime int64, endTime *int64) ([]UserNonFundingLedgerUpdates, error) {
	resp, err := i.postTimeRangeRequest(ctx, "userNonFundingLedgerUpdates", user, startTime, endTime, nil)
	if err != nil {
		return nil, err
	}
	var result []UserNonFundingLedgerUpdates
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user non-funding ledger updates: %w", err)
	}
	return result, nil
}

func (i *Info) L2Snapshot(ctx context.Context, name string) (*L2Book, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "l2Book", "coin": name})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch L2 snapshot: %w", err)
	}
	var result L2Book
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal L2 snapshot: %w", err)
	}
	return &result, nil
}

func (i *Info) CandlesSnapshot(ctx context.Context, name, interval string, startTime, endTime int64) ([]Candle, error) {
	req := map[string]any{"coin": name, "interval": interval, "startTime": startTime, "endTime": endTime}
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "candleSnapshot", "req": req})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch candles snapshot: %w", err)
	}
	var result []Candle
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal candles snapshot: %w", err)
	}
	return result, nil
}

func (i *Info) MetaAndAssetCtxs(ctx context.Context, params MetaAndAssetCtxsParams) (*MetaAndAssetCtxs, error) {
	payload := struct {
		Type string  `json:"type"`
		Dex  *string `json:"dex,omitempty"`
	}{Type: "metaAndAssetCtxs", Dex: params.Dex}
	resp, err := i.transport.post(ctx, "/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch meta and asset contexts: %w", err)
	}
	var result []json.RawMessage
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal meta and asset contexts: %w", err)
	}
	if len(result) < 2 {
		return nil, fmt.Errorf("expected at least 2 elements in response, got %d", len(result))
	}
	meta, err := parseMetaResponse(result[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse meta: %w", err)
	}
	var ctxs []AssetCtx
	if err := json.Unmarshal(result[1], &ctxs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ctxs: %w", err)
	}
	return &MetaAndAssetCtxs{Meta: *meta, Ctxs: ctxs}, nil
}

// SpotMetaAndAssetCtxs returns the spot universe and the per-pair market context
// slice (mark/mid/prevDay/volume/supply). The ctx slice is keyed by a pair's
// universe index, not its position in the universe — index it by SpotAssetInfo.Index.
func (i *Info) SpotMetaAndAssetCtxs(ctx context.Context) (*SpotMeta, []SpotAssetCtx, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "spotMetaAndAssetCtxs"})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch spot meta and asset contexts: %w", err)
	}
	var result []json.RawMessage
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal spot meta and asset contexts: %w", err)
	}
	if len(result) < 2 {
		return nil, nil, fmt.Errorf("expected 2 elements in spot ctx response, got %d", len(result))
	}
	var meta SpotMeta
	if err := json.Unmarshal(result[0], &meta); err != nil {
		return nil, nil, fmt.Errorf("failed to parse spot meta: %w", err)
	}
	var ctxs []SpotAssetCtx
	if err := json.Unmarshal(result[1], &ctxs); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal spot ctxs: %w", err)
	}
	return &meta, ctxs, nil
}

func (i *Info) QueryOrderByOid(ctx context.Context, user string, oid int64) (*OrderQueryResult, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "orderStatus", "user": user, "oid": oid})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order status: %w", err)
	}
	var result OrderQueryResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order status: %w", err)
	}
	return &result, nil
}

func (i *Info) QueryOrderByCloid(ctx context.Context, user, cloid string) (*OrderQueryResult, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "orderStatus", "user": user, "oid": cloid})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order status by cloid: %w", err)
	}
	var result OrderQueryResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order status: %w", err)
	}
	return &result, nil
}

func (i *Info) Portfolio(ctx context.Context, user string) ([]Portfolio, error) {
	resp, err := i.transport.post(ctx, "/info", map[string]any{"type": "portfolio", "user": user})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch portfolio: %w", err)
	}
	var result []Portfolio
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal portfolio: %w", err)
	}
	return result, nil
}
