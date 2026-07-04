package engine

import (
	"time"

	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
)

// MarketView is the live quoting state for one active market, rendered by the TUI.
type MarketView struct {
	Coin       string
	Title      string
	Underlying string
	Expiry     string
	TTL        time.Duration
	FairP      float64 // model fair YES probability
	FairConf   float64
	Mid        float64 // book mid (0 if one-sided/absent)
	BestBid    float64 // book best bid/ask
	BestAsk    float64
	OurBid     float64 // our best resting/desired bid (0 if none)
	OurAsk     float64
	InvYes     int64
	InvNo      int64
	Gate       string // "quoting" | "blackout Nm" | "settled" | "stale fair" | "paused" | "inv cap"
}

// HoldingView is one outcome position the MM currently holds, shown regardless of
// whether the market is still in the active quote set — so a position is never
// invisible after a market drops out of selection.
type HoldingView struct {
	Coin   string
	Title  string
	Side   string // "Yes" | "No"
	Shares int64
	Value  float64 // current position value (USD)
	Entry  float64 // avg entry probability
	Mark   float64 // current mark probability
	PnL    float64 // unrealized PnL (USD), from HL when it marked the coin
	HasPnL bool    // HL provided an unrealized figure (coin present in the allMids frame)
	Active bool    // still in the active quote set (being managed)
}

// EngineView is a consistent snapshot of the engine for the dashboard. It is copied
// out under lock, so the TUI never races the quoting loop.
type EngineView struct {
	Running   bool
	Paused    bool
	DryRun    bool
	Halted    bool
	Network   string
	Equity    float64
	Active    []MarketView
	Holdings  []HoldingView // ALL held outcome positions (active or not)
	Pool      []selector.Candidate
	PnL       oms.PnLView
	LastScan  time.Time
	LastTick  time.Time
	StartedAt time.Time // session start, for the dashboard uptime
	LastError string
	Warmup    bool // fair-value feed not yet warmed up (no quotes will place)
}

// View returns a snapshot of the engine state for rendering. Safe for concurrent use.
func (e *Engine) View() EngineView {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.view // shallow copy
	// Deep-copy the slices so the caller can't observe a torn write next tick.
	v.Active = append([]MarketView(nil), e.view.Active...)
	v.Holdings = append([]HoldingView(nil), e.view.Holdings...)
	v.Pool = append([]selector.Candidate(nil), e.view.Pool...)
	return v
}

// Paused reports whether quoting is currently paused.
func (e *Engine) Paused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused
}

// SetPaused pauses or resumes quoting. While paused the engine drives every active
// market's desired quotes to empty, so the next tick cancels all resting MM orders
// and places none; resuming rebuilds them.
func (e *Engine) SetPaused(p bool) {
	e.mu.Lock()
	e.paused = p
	e.view.Paused = p
	e.mu.Unlock()
}
