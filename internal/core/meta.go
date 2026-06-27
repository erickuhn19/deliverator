package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/state"
)

// MaintenanceMarginFraction returns the maintenance-margin fraction for a coin at
// the given absolute position notional, derived from HL's margin tiers
// (mmf = 1/(2 * tierMaxLeverage)). It falls back to the asset's headline max
// leverage when no tier table applies (e.g. a sub-dex). Returns 0 for an unknown
// or zero-leverage coin.
func (m *MetaStore) MaintenanceMarginFraction(coin string, notional float64) float64 {
	mk, ok := m.Lookup(coin)
	if !ok {
		mk, ok = m.Lookup(bareCoin(coin)) // sub-dex coins are indexed bare
	}
	if !ok || mk.MaxLeverage <= 0 {
		return 0
	}
	maxLev := mk.MaxLeverage
	if m.meta != nil {
		bare := strings.ToUpper(bareCoin(coin))
		for _, a := range m.meta.Universe {
			if strings.ToUpper(a.Name) != bare {
				continue
			}
			for _, t := range m.meta.MarginTables {
				if t.ID == a.MarginTableId {
					maxLev = tierMaxLeverage(t.MarginTiers, notional, mk.MaxLeverage)
				}
			}
			break
		}
	}
	if maxLev <= 0 {
		return 0
	}
	return 1.0 / (2.0 * float64(maxLev))
}

// tierMaxLeverage returns the max leverage of the highest tier whose lowerBound
// does not exceed notional (tiers are ascending by lowerBound).
func tierMaxLeverage(tiers []hl.MarginTier, notional float64, dflt int) int {
	maxLev := dflt
	for _, t := range tiers {
		if lb, err := strconv.ParseFloat(t.LowerBound, 64); err == nil && notional >= lb && t.MaxLeverage > 0 {
			maxLev = t.MaxLeverage
		}
	}
	return maxLev
}

// Market is the precision + leverage profile of one tradable asset, surfaced by
// `deliverator markets` so an agent can self-format orders (§5.7).
type Market struct {
	Coin         string `json:"coin"`
	Class        string `json:"class"` // "perp" | "spot" | "outcome"
	AssetIndex   int    `json:"asset_index"`
	SzDecimals   int    `json:"sz_decimals"`
	PxDecimals   int    `json:"px_decimals"` // max price decimals = MAX_DECIMALS − szDecimals
	MaxLeverage  int    `json:"max_leverage,omitempty"`
	OnlyIsolated bool   `json:"only_isolated,omitempty"`
	IsSpot       bool   `json:"is_spot"`
	IsOutcome    bool   `json:"is_outcome,omitempty"` // HIP-4 outcome: price is a probability in (0,1)
	Delisted     bool   `json:"delisted,omitempty"`

	// HIP-4 outcome-only fields (set when IsOutcome). The tradable unit is a binary
	// Yes/No leaf; Outcome groups the two sides, Question groups related outcomes
	// (e.g. an N-team tournament) where exactly one resolves Yes.
	Outcome          int    `json:"outcome,omitempty"`
	Side             string `json:"side,omitempty"`              // "Yes" | "No"
	Title            string `json:"title,omitempty"`             // human-readable: what a Yes/No resolves on
	Question         int    `json:"question,omitempty"`          // grouping question id (0 = none)
	QuestionName     string `json:"question_name,omitempty"`     // e.g. "2026 World Cup Champion"
	Underlying       string `json:"underlying,omitempty"`        // priceBinary: BTC/ETH/...
	TargetPrice      string `json:"target_price,omitempty"`      // priceBinary target
	Expiry           string `json:"expiry,omitempty"`            // priceBinary expiry (YYYY-MM-DD HH:MMZ)
	ResolutionStatus string `json:"resolution_status,omitempty"` // "open" | "settled"
	PriceBound       string `json:"price_bound,omitempty"`       // "0..1" — price is a probability
	QuoteToken       string `json:"quote_token,omitempty"`       // collateral token, e.g. USDC
}

// metaCacheFile is the on-disk meta cache (§8): the raw API metas + a stamp.
type metaCacheFile struct {
	Network   string       `json:"network"`
	FetchedAt int64        `json:"fetched_at_ms"`
	Meta      *hl.Meta     `json:"meta"`
	SpotMeta  *hl.SpotMeta `json:"spot_meta"`
}

// PerpDexEntry is a loaded builder sub-dex (HIP-3): its dex index + universe.
type PerpDexEntry struct {
	Index int
	Meta  *hl.Meta
}

// MetaStore holds the market universe and fast coin→Market lookups.
type MetaStore struct {
	network        string
	fetchedAt      time.Time
	meta           *hl.Meta
	spotMeta       *hl.SpotMeta
	byCoin         map[string]Market
	ordered        []Market
	perpDexs       []PerpDexEntry
	outcomeMeta    *hl.OutcomeMeta
	outcomeMarkets []Market // HIP-4 outcomes — discoverable via `markets --class outcome`, kept out of `ordered`
}

// AddPerpDex indexes a builder sub-dex's perps as "<dex>:<coin>" markets so they
// are tradable. The asset id matches hl.PerpDexAsset so signing is correct.
func (m *MetaStore) AddPerpDex(dexIndex int, meta *hl.Meta) {
	if meta == nil {
		return
	}
	m.perpDexs = append(m.perpDexs, PerpDexEntry{Index: dexIndex, Meta: meta})
	for j, a := range meta.Universe {
		mk := Market{
			Coin:         a.Name,
			Class:        "perp",
			AssetIndex:   hl.PerpDexAsset(dexIndex, j),
			SzDecimals:   a.SzDecimals,
			PxDecimals:   max(0, MaxDecimalsPerp-a.SzDecimals),
			MaxLeverage:  a.MaxLeverage,
			OnlyIsolated: a.OnlyIsolated,
			IsSpot:       false,
			Delisted:     a.IsDelisted,
		}
		m.byCoin[strings.ToUpper(a.Name)] = mk
		m.ordered = append(m.ordered, mk)
	}
}

// PerpDexEntries returns the loaded sub-dexes so the signing Exchange's Info can
// be registered with the same asset ids.
func (m *MetaStore) PerpDexEntries() []PerpDexEntry { return m.perpDexs }

// OutcomeMeta returns the loaded HIP-4 outcome universe (nil if outcomes are not
// enabled/loaded), so the signing Exchange's Info can be registered with the same
// asset ids.
func (m *MetaStore) OutcomeMeta() *hl.OutcomeMeta { return m.outcomeMeta }

// OutcomeMarkets returns the loaded HIP-4 outcome markets (Yes/No legs) in a stable
// order. They are surfaced via `markets --class outcome`, kept out of the default
// `markets` listing because they number in the hundreds and rotate daily.
func (m *MetaStore) OutcomeMarkets() []Market { return m.outcomeMarkets }

// NewMetaStore builds a store (and its lookup maps) from API metas.
func NewMetaStore(network string, meta *hl.Meta, spotMeta *hl.SpotMeta, fetchedAt time.Time) *MetaStore {
	ms := &MetaStore{
		network:   network,
		fetchedAt: fetchedAt,
		meta:      meta,
		spotMeta:  spotMeta,
		byCoin:    make(map[string]Market),
	}
	ms.build()
	return ms
}

func (m *MetaStore) build() {
	if m.meta != nil {
		for i, a := range m.meta.Universe {
			mk := Market{
				Coin:         a.Name,
				Class:        "perp",
				AssetIndex:   i,
				SzDecimals:   a.SzDecimals,
				PxDecimals:   max(0, MaxDecimalsPerp-a.SzDecimals),
				MaxLeverage:  a.MaxLeverage,
				OnlyIsolated: a.OnlyIsolated,
				IsSpot:       false,
				Delisted:     a.IsDelisted,
			}
			m.byCoin[strings.ToUpper(a.Name)] = mk
			m.ordered = append(m.ordered, mk)
		}
	}
	if m.spotMeta != nil {
		tokenSz := make(map[int]int, len(m.spotMeta.Tokens))
		for _, t := range m.spotMeta.Tokens {
			tokenSz[t.Index] = t.SzDecimals
		}
		for _, p := range m.spotMeta.Universe {
			szDec := 0
			if len(p.Tokens) > 0 {
				szDec = tokenSz[p.Tokens[0]]
			}
			mk := Market{
				Coin:       p.Name,
				Class:      "spot",
				AssetIndex: p.Index + 10000, // spot asset id offset (§ research)
				SzDecimals: szDec,
				PxDecimals: max(0, MaxDecimalsSpot-szDec),
				IsSpot:     true,
			}
			m.byCoin[strings.ToUpper(p.Name)] = mk
			m.ordered = append(m.ordered, mk)
		}
	}
}

// Lookup resolves a coin (perp ticker or spot pair name) to its Market.
func (m *MetaStore) Lookup(coin string) (Market, bool) {
	mk, ok := m.byCoin[strings.ToUpper(strings.TrimSpace(coin))]
	return mk, ok
}

// SpotBaseToken returns the base token index (Tokens[0]) of a spot pair, used to
// find the sellable balance when closing a spot holding. SpotBalance.Token keys
// to the same index.
func (m *MetaStore) SpotBaseToken(coin string) (int, bool) {
	if m.spotMeta == nil {
		return 0, false
	}
	up := strings.ToUpper(strings.TrimSpace(coin))
	for _, p := range m.spotMeta.Universe {
		if strings.ToUpper(p.Name) == up && len(p.Tokens) > 0 {
			return p.Tokens[0], true
		}
	}
	return 0, false
}

// Markets returns all markets in universe order (perps first, then spot).
func (m *MetaStore) Markets() []Market { return m.ordered }

// Age reports how stale the cache is.
func (m *MetaStore) Age() time.Duration { return time.Since(m.fetchedAt) }

// FetchedAt reports when the metadata was fetched.
func (m *MetaStore) FetchedAt() time.Time { return m.fetchedAt }

// Meta / SpotMeta expose the raw API metas (to pass to NewExchange/NewInfo and
// avoid a refetch/panic).
func (m *MetaStore) Meta() *hl.Meta         { return m.meta }
func (m *MetaStore) SpotMeta() *hl.SpotMeta { return m.spotMeta }

// Save writes the meta cache to path (0600).
func (m *MetaStore) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(metaCacheFile{
		Network:   m.network,
		FetchedAt: m.fetchedAt.UnixMilli(),
		Meta:      m.meta,
		SpotMeta:  m.spotMeta,
	})
	if err != nil {
		return err
	}
	// Atomic+fsync so a crash can't leave a torn coin→assetId cache (audit #91 / S12).
	return state.WriteFileAtomic(path, b, 0o600)
}

// LoadMetaCache reads a cached MetaStore from disk. It returns (nil,false) if the
// file is missing, unreadable, a symlink, for a different network, or carries a
// FetchedAt timestamp that isn't sane — the caller should then refetch.
//
// This cache maps coin→assetId, and signing is asset-agnostic: a poisoned id
// yields a valid signature for the WRONG market. So we (a) refuse a symlinked
// path, and (b) reject a future FetchedAt (which would make Age() negative and
// keep a stale/poisoned cache from ever expiring) and an absurdly old one
// (audit #91 / S11, T3-symlink).
func LoadMetaCache(path, network string) (*MetaStore, bool) {
	if err := state.ValidateStateFile(path); err != nil {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var f metaCacheFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, false
	}
	if f.Network != network || f.Meta == nil {
		return nil, false
	}
	fetchedAt := time.UnixMilli(f.FetchedAt)
	now := time.Now()
	// A small grace window absorbs benign clock skew without letting a future
	// stamp pin the cache forever.
	if fetchedAt.After(now.Add(time.Minute)) {
		return nil, false
	}
	if now.Sub(fetchedAt) >= 365*24*time.Hour {
		return nil, false
	}
	return NewMetaStore(network, f.Meta, f.SpotMeta, fetchedAt), true
}

// describeMarket is a small helper for error hints.
func (mk Market) priceHint() string {
	return fmt.Sprintf("%s: szDecimals=%d, max price decimals=%d", mk.Coin, mk.SzDecimals, mk.PxDecimals)
}
