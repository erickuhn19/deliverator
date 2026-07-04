package oms

import (
	"math"
	"strings"
	"sync"

	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// PnLView is the market maker's profit split (spec §11).
type PnLView struct {
	Realized float64 // realized PnL booked against the cost basis on every sell/settle
	Fees     float64 // Σ fill fees (charged on close/burn/settle — HIP-4 has none on open)
	Open     float64 // mark-to-fair value of the current inventory vs its cost basis
	Net      float64 // Realized − Fees + Open
	Fills    int     // number of distinct fills booked this session
	Volume   float64 // Σ |px·sz| traded this session (USD notional)
}

// coinPos tracks the current inventory and its cost basis for one coin, so open
// inventory can be marked to the live fair probability.
type coinPos struct {
	shares int64
	cost   float64 // total USDC still invested in the held shares (avg cost = cost/shares)
}

// PnLAccountant books realized/fee PnL from fills and values open inventory. Fills
// are deduplicated by trade id (Tid), so a re-read after a stream reconnect (or an
// overlapping REST poll) never double-counts. Concurrency-safe.
type PnLAccountant struct {
	mu       sync.Mutex
	seenTid  map[int64]bool
	realized float64
	fees     float64
	fills    int     // distinct fills booked this session
	volume   float64 // Σ |px·sz| traded this session
	pos      map[string]*coinPos
}

// NewPnLAccountant builds an empty accountant.
func NewPnLAccountant() *PnLAccountant {
	return &PnLAccountant{seenTid: map[int64]bool{}, pos: map[string]*coinPos{}}
}

// IngestFills books a batch of fills, ignoring any Tid already seen. Realized PnL is
// taken from the exchange's own closedPnl; fees are summed; and the per-coin cost
// basis is maintained so open inventory can later be marked.
func (a *PnLAccountant) IngestFills(fills []hl.Fill) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range fills {
		if a.seenTid[f.Tid] {
			continue
		}
		a.seenTid[f.Tid] = true
		a.fees += parseF(f.Fee)

		px, sz := parseF(f.Price), int64(math.Round(parseF(f.Size)))
		if sz <= 0 {
			continue
		}
		a.fills++
		a.volume += px * float64(sz)
		p := a.pos[f.Coin]
		if p == nil {
			p = &coinPos{}
			a.pos[f.Coin] = p
		}
		if isBuyFill(f) {
			p.shares += sz
			p.cost += px * float64(sz)
		} else {
			// Spot-style outcome fill: book the realized gain against the average cost
			// basis ourselves. HL's closedPnl is a perp concept and reads 0 on outcome
			// fills, so relying on it (as this once did) silently dropped every round-
			// trip's spread from realized PnL.
			avg := 0.0
			if p.shares > 0 {
				avg = p.cost / float64(p.shares)
			}
			if sz > p.shares {
				sz = p.shares // never book a sell beyond the tracked position
			}
			a.realized += (px - avg) * float64(sz)
			p.shares -= sz
			p.cost -= avg * float64(sz)
			if p.shares <= 0 {
				p.shares, p.cost = 0, 0
			}
		}
	}
}

// SeedPosition initializes the cost basis for a position carried in from a prior
// session (from the venue's entry price), so a later sell books realized PnL against
// the TRUE average cost rather than treating it as pure profit. Called once at startup
// for each held outcome position, before session fills are ingested.
func (a *PnLAccountant) SeedPosition(coin string, shares int64, entryPx float64) {
	if shares <= 0 || entryPx <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.pos[coin]; ok {
		return // a fill already established a basis; don't clobber it
	}
	a.pos[coin] = &coinPos{shares: shares, cost: entryPx * float64(shares)}
}

// RealizeSettlement books a settled market: each held share of coin pays out
// payoutPerShare (1.0 for the winning side, 0.0 for the loser), the cost basis is
// released to realized PnL, and the position is cleared. Called by the engine when
// it detects a market's ResolutionStatus flip (there is no settled-price endpoint).
func (a *PnLAccountant) RealizeSettlement(coin string, payoutPerShare float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pos[coin]
	if p == nil || p.shares == 0 {
		return
	}
	a.realized += payoutPerShare*float64(p.shares) - p.cost
	p.shares, p.cost = 0, 0
}

// HeldCoins returns the coins the accountant currently holds a non-zero position in
// (cost basis still open). The engine reconciles this against live positions to
// detect a silent HIP-4 settlement (a held coin that has left the position set).
func (a *PnLAccountant) HeldCoins() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for coin, p := range a.pos {
		if p.shares > 0 {
			out = append(out, coin)
		}
	}
	return out
}

// OpenPnL marks the current inventory against the supplied fair/mark probability per
// coin: Σ shares·mark − cost. Coins with no supplied mark contribute 0.
func (a *PnLAccountant) OpenPnL(markByCoin map[string]float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.openLocked(markByCoin)
}

// OpenForCoin marks one coin's held inventory against a supplied probability:
// mark·shares − cost. Returns 0 if the coin isn't tracked. Used as the open-PnL fallback
// for a holding HL couldn't mark (no live mid) but the model still prices.
func (a *PnLAccountant) OpenForCoin(coin string, mark float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pos[coin]
	if p == nil || p.shares == 0 {
		return 0
	}
	return mark*float64(p.shares) - p.cost
}

func (a *PnLAccountant) openLocked(markByCoin map[string]float64) float64 {
	var open float64
	for coin, p := range a.pos {
		if p.shares == 0 {
			continue
		}
		if mark, ok := markByCoin[coin]; ok {
			open += mark*float64(p.shares) - p.cost
		}
	}
	return open
}

// View returns the current realized/fee/open/net split, with open valued against
// markByCoin.
func (a *PnLAccountant) View(markByCoin map[string]float64) PnLView {
	a.mu.Lock()
	defer a.mu.Unlock()
	open := a.openLocked(markByCoin)
	return PnLView{
		Realized: a.realized,
		Fees:     a.fees,
		Open:     open,
		Net:      a.realized - a.fees + open,
		Fills:    a.fills,
		Volume:   a.volume,
	}
}

func isBuyFill(f hl.Fill) bool {
	switch strings.ToUpper(strings.TrimSpace(f.Side)) {
	case "B", "BUY", "BID":
		return true
	case "A", "S", "SELL", "ASK":
		return false
	}
	// Fall back to the human direction string ("Open Long"/"Buy" ⇒ buy).
	return strings.Contains(strings.ToLower(f.Dir), "buy") || strings.Contains(strings.ToLower(f.Dir), "long")
}

func parseF(s string) float64 {
	v, _ := mm.ParseFloat(s)
	return v
}
