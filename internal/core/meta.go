package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	mk, ok := m.lookupLocked(coin)
	if !ok {
		mk, ok = m.lookupLocked(bareCoin(coin)) // sub-dex coins are indexed bare
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
//
// CONCURRENCY. Every field below is guarded by mu. This store was originally
// written for a process that lives for one command, where it is built once and
// only read afterwards — but `serve` handles each connection on its own
// goroutine (internal/serve/serve.go) and reloads the HIP-4 universe on the
// daily roll (RefreshOutcomes). That makes AddOutcomes a WRITER racing every
// Lookup, and a concurrent map read+write is a Go runtime FATAL ERROR, not a
// recoverable panic — it would kill a process holding the signing socket. See
// #43.
//
// The exported readers take mu.RLock. Methods that need a lookup while already
// holding the lock must call lookupLocked, never Lookup: sync.RWMutex is not
// reentrant, so a second RLock deadlocks the moment a writer is queued between
// the two.
type MetaStore struct {
	mu             sync.RWMutex
	network        string
	fetchedAt      time.Time
	meta           *hl.Meta
	spotMeta       *hl.SpotMeta
	byCoin         map[string]Market
	ordered        []Market
	perpDexs       []PerpDexEntry
	outcomeMeta    *hl.OutcomeMeta
	outcomeMarkets []Market // HIP-4 outcomes — discoverable via `markets --class outcome`, kept out of `ordered`
	// outcomeCoins is the set of byCoin keys the last AddOutcomes installed, so a
	// reload can retire the coins that rolled out instead of leaving them
	// resolvable forever.
	outcomeCoins []string
	// perpDexCoins is the same idea per sub-dex index.
	perpDexCoins map[int][]string
}

// AddPerpDex indexes a builder sub-dex's perps as "<dex>:<coin>" markets so they
// are tradable. The asset id matches hl.PerpDexAsset so signing is correct.
//
// Idempotent per dex index: re-adding a dex REPLACES its universe rather than
// appending a second copy. It is called once per dex at construction today, but
// an append-only writer is a latent duplication bug the moment anything reloads
// (exactly what bit the outcome universe — see AddOutcomes and #43).
func (m *MetaStore) AddPerpDex(dexIndex int, meta *hl.Meta) {
	if meta == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Retire the previous universe for this dex, if any.
	if old, ok := m.perpDexCoins[dexIndex]; ok {
		retired := make(map[string]bool, len(old))
		for _, key := range old {
			delete(m.byCoin, key)
			retired[key] = true
		}
		kept := m.ordered[:0]
		for _, mk := range m.ordered {
			if !retired[strings.ToUpper(mk.Coin)] {
				kept = append(kept, mk)
			}
		}
		m.ordered = kept
	}
	replaced := false
	for i, e := range m.perpDexs {
		if e.Index == dexIndex {
			m.perpDexs[i] = PerpDexEntry{Index: dexIndex, Meta: meta}
			replaced = true
			break
		}
	}
	if !replaced {
		m.perpDexs = append(m.perpDexs, PerpDexEntry{Index: dexIndex, Meta: meta})
	}

	if m.perpDexCoins == nil {
		m.perpDexCoins = make(map[int][]string)
	}
	keys := make([]string, 0, len(meta.Universe))
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
		key := strings.ToUpper(a.Name)
		m.byCoin[key] = mk
		m.ordered = append(m.ordered, mk)
		keys = append(keys, key)
	}
	m.perpDexCoins[dexIndex] = keys
}

// PerpDexEntries returns the loaded sub-dexes so the signing Exchange's Info can
// be registered with the same asset ids.
func (m *MetaStore) PerpDexEntries() []PerpDexEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]PerpDexEntry(nil), m.perpDexs...)
}

// OutcomeMeta returns the loaded HIP-4 outcome universe (nil if outcomes are not
// enabled/loaded), so the signing Exchange's Info can be registered with the same
// asset ids.
func (m *MetaStore) OutcomeMeta() *hl.OutcomeMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.outcomeMeta
}

// OutcomeMarkets returns the loaded HIP-4 outcome markets (Yes/No legs) in a stable
// order. They are surfaced via `markets --class outcome`, kept out of the default
// `markets` listing because they number in the hundreds and rotate daily.
func (m *MetaStore) OutcomeMarkets() []Market {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Market(nil), m.outcomeMarkets...)
}

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
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lookupLocked(coin)
}

// lookupLocked is Lookup's body without the lock. Callers must already hold
// m.mu (read or write) — see the concurrency note on MetaStore.
func (m *MetaStore) lookupLocked(coin string) (Market, bool) {
	mk, ok := m.byCoin[strings.ToUpper(strings.TrimSpace(coin))]
	return mk, ok
}

// SpotBaseToken returns the base token index (Tokens[0]) of a spot pair, used to
// find the sellable balance when closing a spot holding. SpotBalance.Token keys
// to the same index.
func (m *MetaStore) SpotBaseToken(coin string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
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

// SpotPairForToken resolves a token NAME (e.g. "HYPE", a fill's feeToken) to
// the Market of its <token>/USDC spot pair. A plain "<TOKEN>/USDC" name lookup
// only works for canonical pairs (on mainnet, essentially just PURR/USDC) — the
// rest carry an "@<index>" universe name, so the join must go token name →
// token index → the pair whose tokens are [token, USDC]. Used to value non-USDC
// fees at the pair's mid (pnl attribution): failing this join would wrongly
// EXCLUDE a fee whose mid is available, under-reporting costs.
func (m *MetaStore) SpotPairForToken(token string) (Market, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mk, ok := m.lookupLocked(token + "/USDC"); ok && mk.IsSpot {
		return mk, true // canonical pair name
	}
	if m.spotMeta == nil {
		return Market{}, false
	}
	up := strings.ToUpper(strings.TrimSpace(token))
	tokIdx, usdcIdx := -1, -1
	for _, tk := range m.spotMeta.Tokens {
		switch strings.ToUpper(tk.Name) {
		case up:
			tokIdx = tk.Index
		case "USDC":
			usdcIdx = tk.Index
		}
	}
	if tokIdx < 0 || usdcIdx < 0 || tokIdx == usdcIdx {
		return Market{}, false
	}
	for _, p := range m.spotMeta.Universe {
		if len(p.Tokens) == 2 && p.Tokens[0] == tokIdx && p.Tokens[1] == usdcIdx {
			return m.lookupLocked(p.Name)
		}
	}
	return Market{}, false
}

// Markets returns all markets in universe order (perps first, then spot). The
// slice is copied: the caller must not observe a later refresh mutating it.
func (m *MetaStore) Markets() []Market {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Market(nil), m.ordered...)
}

// Age reports how stale the cache is.
func (m *MetaStore) Age() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return time.Since(m.fetchedAt)
}

// FetchedAt reports when the metadata was fetched.
func (m *MetaStore) FetchedAt() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fetchedAt
}

// Meta / SpotMeta expose the raw API metas (to pass to NewExchange/NewInfo and
// avoid a refetch/panic).
func (m *MetaStore) Meta() *hl.Meta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.meta
}

func (m *MetaStore) SpotMeta() *hl.SpotMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.spotMeta
}

// Save writes the meta cache to path (0600).
func (m *MetaStore) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	m.mu.RLock()
	b, err := json.Marshal(metaCacheFile{
		Network:   m.network,
		FetchedAt: m.fetchedAt.UnixMilli(),
		Meta:      m.meta,
		SpotMeta:  m.spotMeta,
	})
	m.mu.RUnlock()
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
