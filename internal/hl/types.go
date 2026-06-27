package hl

// Request/response types and enums. JSON tags mirror the Hyperliquid API (and
// the reference Go SDK) exactly — deliverator's on-disk meta cache marshals
// *Meta/*SpotMeta, so tag identity preserves cache compatibility.

import "encoding/json"

// Grouping selects TP/SL order grouping: "na" for an independent batch or
// "normalTpsl" for a linked [entry, tp, sl] OCO bracket.
type Grouping string

const (
	GroupingNA           Grouping = "na"
	GroupingNormalTpsl   Grouping = "normalTpsl"   // [entry, tp, sl] linked OCO bracket
	GroupingPositionTpsl Grouping = "positionTpsl" // tp/sl attached to the whole position
)

// Tif is an order time-in-force.
type Tif string

const (
	TifAlo Tif = "Alo" // add-liquidity-only (post-only)
	TifIoc Tif = "Ioc" // immediate-or-cancel
	TifGtc Tif = "Gtc" // good-till-cancel
)

// Tpsl distinguishes take-profit from stop-loss triggers.
type Tpsl string

const (
	TakeProfit Tpsl = "tp"
	StopLoss   Tpsl = "sl"
)

// OrderSide is the side of a resting/queried order ("A" ask/sell, "B" bid/buy).
type OrderSide string

const (
	OrderSideAsk OrderSide = "A"
	OrderSideBid OrderSide = "B"
)

// ---- perp metadata ----

type AssetInfo struct {
	Name          string `json:"name"`
	SzDecimals    int    `json:"szDecimals"`
	MaxLeverage   int    `json:"maxLeverage"`
	MarginTableId int    `json:"marginTableId"`
	OnlyIsolated  bool   `json:"onlyIsolated"`
	IsDelisted    bool   `json:"isDelisted"`
}

type MarginTier struct {
	LowerBound  string `json:"lowerBound"`
	MaxLeverage int    `json:"maxLeverage"`
}

type MarginTable struct {
	ID          int
	Description string       `json:"description"`
	MarginTiers []MarginTier `json:"marginTiers"`
}

type Meta struct {
	Universe        []AssetInfo   `json:"universe"`
	MarginTables    []MarginTable `json:"marginTables"`
	CollateralToken int           `json:"collateralToken"`
}

type AssetCtx struct {
	Funding      string   `json:"funding"`
	OpenInterest string   `json:"openInterest"`
	PrevDayPx    string   `json:"prevDayPx"`
	DayNtlVlm    string   `json:"dayNtlVlm"`
	Premium      string   `json:"premium"`
	OraclePx     string   `json:"oraclePx"`
	MarkPx       string   `json:"markPx"`
	MidPx        string   `json:"midPx,omitempty"`
	ImpactPxs    []string `json:"impactPxs"`
	DayBaseVlm   string   `json:"dayBaseVlm,omitempty"`
}

// SpotAssetCtx is one element of spotMetaAndAssetCtxs[1], keyed by a pair's
// universe INDEX (the slice is longer than the universe and self-identifies via
// Coin = "@<index>" / the canonical pair name) — NOT by universe array position.
// Spot has no funding/OI/oracle; it carries circulating/total supply instead.
type SpotAssetCtx struct {
	PrevDayPx         string `json:"prevDayPx"`
	DayNtlVlm         string `json:"dayNtlVlm"`
	DayBaseVlm        string `json:"dayBaseVlm,omitempty"`
	MarkPx            string `json:"markPx"`
	MidPx             string `json:"midPx,omitempty"`
	CirculatingSupply string `json:"circulatingSupply,omitempty"`
	TotalSupply       string `json:"totalSupply,omitempty"`
	Coin              string `json:"coin,omitempty"`
}

// MetaAndAssetCtxsParams optionally selects a non-default perp dex (unused here).
type MetaAndAssetCtxsParams struct {
	Dex *string
}

// MetaAndAssetCtxs embeds Meta and pairs each universe entry with its context.
type MetaAndAssetCtxs struct {
	Meta
	Ctxs []AssetCtx
}

// ---- spot metadata ----

type SpotAssetInfo struct {
	Name        string `json:"name"`
	Tokens      []int  `json:"tokens"`
	Index       int    `json:"index"`
	IsCanonical bool   `json:"isCanonical"`
}

type EvmContract struct {
	Address             string `json:"address"`
	EvmExtraWeiDecimals int    `json:"evm_extra_wei_decimals"`
}

type SpotTokenInfo struct {
	Name        string       `json:"name"`
	SzDecimals  int          `json:"szDecimals"`
	WeiDecimals int          `json:"weiDecimals"`
	Index       int          `json:"index"`
	TokenID     string       `json:"tokenId"`
	IsCanonical bool         `json:"isCanonical"`
	EvmContract *EvmContract `json:"evmContract"`
	FullName    *string      `json:"fullName"`
}

type SpotMeta struct {
	Universe []SpotAssetInfo `json:"universe"`
	Tokens   []SpotTokenInfo `json:"tokens"`
}

// ---- outcome (HIP-4) metadata ----

// OutcomeSideSpec labels one side of an outcome market — side 0 is "Yes", side 1
// is "No". Only sides 0 and 1 are valid (markets are binary).
type OutcomeSideSpec struct {
	Name string `json:"name"`
}

// OutcomeInfo is one tradable outcome: a binary Yes/No leaf. Its side asset id is
// OutcomeAsset(Outcome, side) and the order/book coin is "#<10*Outcome+side>".
// Description is class-dependent: a pipe-delimited
// "class:priceBinary|underlying:..|expiry:YYYYMMDD-HHMM|targetPrice:..|period:.."
// for recurring price binaries, plain English for event markets, "index:N" for
// named legs — parse it defensively (richer modeling is deferred to #78).
type OutcomeInfo struct {
	Outcome     int               `json:"outcome"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	SideSpecs   []OutcomeSideSpec `json:"sideSpecs"`
	QuoteToken  string            `json:"quoteToken"`
}

// OutcomeQuestion groups related outcomes (a multi-way game or an N-entrant
// tournament) where exactly one resolves Yes; the remainder resolve No. Resolution
// surfaces as outcomes moving into SettledNamedOutcomes (and dropping out of a
// later outcomeMeta). Carried for the richer outcome model (#78).
type OutcomeQuestion struct {
	Question             int    `json:"question"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	FallbackOutcome      int    `json:"fallbackOutcome"`
	NamedOutcomes        []int  `json:"namedOutcomes"`
	SettledNamedOutcomes []int  `json:"settledNamedOutcomes"`
}

// OutcomeMeta is the live HIP-4 outcome universe (info "outcomeMeta"): the tradable
// binary outcomes plus their question groupings.
type OutcomeMeta struct {
	Outcomes  []OutcomeInfo     `json:"outcomes"`
	Questions []OutcomeQuestion `json:"questions"`
}

// ---- order request types ----

type OrderType struct {
	Limit   *LimitOrderType   `json:"limit,omitempty"`
	Trigger *TriggerOrderType `json:"trigger,omitempty"`
}

type LimitOrderType struct {
	Tif Tif `json:"tif"`
}

type TriggerOrderType struct {
	TriggerPx float64 `json:"triggerPx"`
	IsMarket  bool    `json:"isMarket"`
	Tpsl      Tpsl    `json:"tpsl"`
}

// Cloid is a client order id wrapper used by ModifyOrderRequest.
type Cloid struct {
	Value string
}

func (c Cloid) ToRaw() string { return c.Value }

// ---- user state ----

type AssetPosition struct {
	Position Position `json:"position"`
	Type     string   `json:"type"`
}

type Position struct {
	Coin           string      `json:"coin"`
	EntryPx        *string     `json:"entryPx"`
	Leverage       Leverage    `json:"leverage"`
	LiquidationPx  *string     `json:"liquidationPx"`
	MarginUsed     string      `json:"marginUsed"`
	PositionValue  string      `json:"positionValue"`
	ReturnOnEquity string      `json:"returnOnEquity"`
	Szi            string      `json:"szi"`
	UnrealizedPnl  string      `json:"unrealizedPnl"`
	CumFunding     *CumFunding `json:"cumFunding,omitempty"`
}

type Leverage struct {
	Type   string  `json:"type"`
	Value  int     `json:"value"`
	RawUsd *string `json:"rawUsd,omitempty"`
}

type CumFunding struct {
	AllTime     string `json:"allTime"`
	SinceChange string `json:"sinceChange"`
	SinceOpen   string `json:"sinceOpen"`
}

type MarginSummary struct {
	AccountValue    string `json:"accountValue"`
	TotalMarginUsed string `json:"totalMarginUsed"`
	TotalNtlPos     string `json:"totalNtlPos"`
	TotalRawUsd     string `json:"totalRawUsd"`
}

type UserState struct {
	AssetPositions     []AssetPosition `json:"assetPositions"`
	CrossMarginSummary MarginSummary   `json:"crossMarginSummary"`
	MarginSummary      MarginSummary   `json:"marginSummary"`
	Withdrawable       string          `json:"withdrawable"`
}

type SpotBalance struct {
	Coin     string `json:"coin"`
	Token    int    `json:"token"`
	Hold     string `json:"hold"`
	Total    string `json:"total"`
	EntryNtl string `json:"entryNtl"`
}

// TokenAvailable is one [tokenId, amount] entry of a unified/multi-collateral
// account's spendable collateral after maintenance margin.
type TokenAvailable struct {
	Token     int
	Available string
}

func (t *TokenAvailable) UnmarshalJSON(b []byte) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	if len(arr) >= 2 {
		_ = json.Unmarshal(arr[0], &t.Token)
		_ = json.Unmarshal(arr[1], &t.Available)
	}
	return nil
}

type SpotUserState struct {
	Balances []SpotBalance `json:"balances"`
	// TokenToAvailableAfterMaintenance reports each token's collateral available
	// to open positions. On a unified account the settlement asset (USDC, token
	// 0) here is the perp trading collateral, even though the perp
	// clearinghouseState accountValue reads 0 with no open positions.
	TokenToAvailableAfterMaintenance []TokenAvailable `json:"tokenToAvailableAfterMaintenance,omitempty"`
}

// ReferredBy identifies the account/code that referred this user (nil if none).
type ReferredBy struct {
	Referrer string `json:"referrer"`
	Code     string `json:"code"`
}

// ReferralInfo is the user's referral state (info "referral" query).
type ReferralInfo struct {
	ReferredBy *ReferredBy `json:"referredBy"`
	CumVlm     string      `json:"cumVlm"`
}

// IsReferred reports whether the user already has a referrer set (one-time).
func (r *ReferralInfo) IsReferred() bool { return r != nil && r.ReferredBy != nil }

// ---- orders ----

type FrontendOpenOrder struct {
	Coin             string    `json:"coin"`
	Cloid            *string   `json:"cloid,omitempty"`
	IsPositionTpSl   bool      `json:"isPositionTpsl"`
	IsTrigger        bool      `json:"isTrigger"`
	LimitPx          float64   `json:"limitPx,string"`
	Oid              int64     `json:"oid"`
	OrderType        string    `json:"orderType"`
	OrigSz           float64   `json:"origSz,string"`
	ReduceOnly       bool      `json:"reduceOnly"`
	Side             OrderSide `json:"side"`
	Sz               float64   `json:"sz,string"`
	Tif              Tif       `json:"tif"` // null for trigger orders -> ""
	Timestamp        int64     `json:"timestamp"`
	TriggerCondition string    `json:"triggerCondition"`
	TriggerPx        float64   `json:"triggerPx,string"`
}

type QueriedOrder struct {
	Coin             string         `json:"coin"`
	Side             OrderSide      `json:"side"`
	LimitPx          string         `json:"limitPx"`
	Sz               string         `json:"sz"`
	Oid              int64          `json:"oid"`
	Timestamp        int64          `json:"timestamp"`
	TriggerCondition string         `json:"triggerCondition"`
	IsTrigger        bool           `json:"isTrigger"`
	TriggerPx        string         `json:"triggerPx"`
	Children         []QueriedOrder `json:"children"`
	IsPositionTpsl   bool           `json:"isPositionTpsl"`
	ReduceOnly       bool           `json:"reduceOnly"`
	OrderType        string         `json:"orderType"`
	OrigSz           string         `json:"origSz"`
	Tif              Tif            `json:"tif"`
	Cloid            *string        `json:"cloid"`
}

// OrderStatusValue is the per-order lifecycle status (e.g. "open", "filled").
type OrderStatusValue string

type OrderQueryResponse struct {
	Order           QueriedOrder     `json:"order"`
	Status          OrderStatusValue `json:"status"`
	StatusTimestamp int64            `json:"statusTimestamp"`
}

// OrderQueryStatus is the top-level result of an orderStatus query.
type OrderQueryStatus string

const (
	OrderQueryStatusSuccess OrderQueryStatus = "order"
	OrderQueryStatusError   OrderQueryStatus = "unknownOid"
)

type OrderQueryResult struct {
	Status OrderQueryStatus   `json:"status"`
	Order  OrderQueryResponse `json:"order,omitempty"`
}

// ---- fills / funding / ledger ----

type Fill struct {
	ClosedPnl     string `json:"closedPnl"`
	Coin          string `json:"coin"`
	Crossed       bool   `json:"crossed"`
	Dir           string `json:"dir"`
	Hash          string `json:"hash"`
	Oid           int64  `json:"oid"`
	Price         string `json:"px"`
	Side          string `json:"side"`
	StartPosition string `json:"startPosition"`
	Size          string `json:"sz"`
	Time          int64  `json:"time"`
	Fee           string `json:"fee"`
	FeeToken      string `json:"feeToken"`
	BuilderFee    string `json:"builderFee,omitempty"`
	Tid           int64  `json:"tid"`
}

type UserFillsParams struct {
	Address         string
	AggregateByTime *bool
}

type UserFundingHistory struct {
	Delta Delta  `json:"delta"`
	Hash  string `json:"hash"`
	Time  int64  `json:"time"`
}

type Delta struct {
	Coin        string `json:"coin"`
	FundingRate string `json:"fundingRate"`
	Size        string `json:"size"`
	Type        string `json:"type"`
	USDC        string `json:"usdc"`
}

type UserNonFundingLedgerUpdates struct {
	Delta LedgerDelta `json:"delta"`
	Hash  string      `json:"hash"`
	Time  int64       `json:"time"`
}

type LedgerDelta struct {
	Type           string `json:"type"`
	USDC           string `json:"usdc"`
	User           string `json:"user"`
	Destination    string `json:"destination"`
	Fee            string `json:"fee"`
	Token          string `json:"token"`
	UsdcValue      string `json:"usdcValue"`
	SourceDex      string `json:"sourceDex"`
	DestinationDex string `json:"destinationDex"`
}

// ---- market data ----

type Level struct {
	N  int     `json:"n"`
	Px float64 `json:"px,string"`
	Sz float64 `json:"sz,string"`
}

type L2Book struct {
	Coin   string    `json:"coin"`
	Levels [][]Level `json:"levels"`
	Time   int64     `json:"time"`
}

type Candle struct {
	TimeOpen    int64  `json:"t"`
	TimeClose   int64  `json:"T"`
	Interval    string `json:"i"`
	TradesCount int    `json:"n"`
	Open        string `json:"o"`
	High        string `json:"h"`
	Low         string `json:"l"`
	Close       string `json:"c"`
	Symbol      string `json:"s"`
	Volume      string `json:"v"`
}

// ---- portfolio time series ----

// AccountHistory is one window of the portfolio time series.
type AccountHistory struct {
	AccountValueHistory []MixedArray `json:"accountValueHistory"`
	PnlHistory          []MixedArray `json:"pnlHistory"`
	Vlm                 string       `json:"vlm"`
}

// Portfolio is one [label, AccountHistory] tuple from the portfolio endpoint.
type Portfolio []MixedValue
