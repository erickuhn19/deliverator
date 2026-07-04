// Package engine is the outcome market maker's hot loop. Each fast tick it reads
// authoritative account + book state, prices every active market with the fair-value
// model, builds an inventory-skewed two-sided ladder, diffs it against the resting
// book, and applies the minimal place/modify/cancel through core.Client — which runs
// every guardrail (preTradeChecks, portfolio gates, the outcome settlement gate)
// before signing. A slower scan re-ranks the universe into the active set. Direction
// is handled by inventory skew + small size + the settlement gate (no perp hedge in
// v1). --dry-run computes and renders everything but never signs.
package engine

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
	"github.com/erickuhn19/deliverator/internal/mm/fairvalue"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
	"github.com/erickuhn19/deliverator/internal/mm/settle"
)

// pxTol is the price tolerance under which a resting rung is left untouched (half a
// 5-dp outcome tick) — avoids re-quoting on sub-tick fair-value noise.
const pxTol = 5e-6

// teardownTimeout bounds the shutdown cancel-all, which runs in a fresh context
// because the run context is already cancelled at that point.
const teardownTimeout = 10 * time.Second

// dmsRearmInterval is the minimum spacing between dead-man-switch re-arms (HL
// rate-limits scheduleCancel); the arm window is always set comfortably longer.
const dmsRearmInterval = 90 * time.Second

// Deps are the engine's collaborators.
type Deps struct {
	Client core.ClientAPI // the guarded exchange client (reads, streams, signs)
	Cfg    *config.Config // full config (risk caps + [mm] table)
	Feed   *oms.Feed      // live underlying marks + realized vol (allMids stream)
	DryRun bool           // shadow: compute + render, never sign
	// ConfigPath is the config file to RELOAD each scan so live [mm] edits (pins,
	// blacklist, caps, spreads) from the TUI take effect. "" disables reload. Note:
	// mm.enabled / mm.dry_run are NOT hot-reloaded — going live requires a restart.
	ConfigPath string
	// Fair overrides the fair-value model. Normally nil — the engine builds the
	// deterministic PriceBinaryModel from Feed. A test injects a stub here.
	Fair mm.FairValuer
}

// Engine ties the fair-value model, selector, strategy, arb scanner, settlement
// policy, and OMS together behind one loop.
type Engine struct {
	client     core.ClientAPI
	cfg        *config.Config
	mmcfg      *config.MM
	feed       *oms.Feed       // WS allMids: outcome mids for settlement inference
	candleFeed *oms.CandleFeed // perp candles: underlying mark + vol for the model
	fair       mm.FairValuer
	sel        *selector.Selector
	pnl        *oms.PnLAccountant
	policy     settle.Policy

	// forceDry is set by the --dry-run flag: shadow ALWAYS, un-toggleable at runtime.
	// live is the runtime signing switch (from config at start; flipped by SetLive from
	// the TUI). Effective signing = !forceDry && live. Both guarded by mu.
	forceDry bool
	live     bool

	configPath string
	quoteEvery time.Duration
	scanEvery  time.Duration
	lastDMSArm time.Time // throttles DMS re-arming (loop goroutine only)

	mu          sync.Mutex
	paused      bool
	active      []selector.Candidate
	view        EngineView
	lastFillTs  int64     // ms; high-water for incremental fill reads
	startedAt   time.Time // session start (for the dashboard uptime)
	questionNtl map[string]float64
}

// New builds an engine from its dependencies. The fair-value model reads marks + vol
// from the feed; the selector and quote loop share it.
func New(d Deps) *Engine {
	mmcfg := d.Cfg.MMOrDefault()
	// The model prices off the perp's candles (fresh mark + realized vol, no warm-up),
	// not the slow WS allMids stream. A test may inject d.Fair to bypass both.
	candleFeed := oms.NewCandleFeed()
	fair := d.Fair
	if fair == nil {
		fair = fairvalue.NewPriceBinaryModel(candleFeed)
	}
	e := &Engine{
		client:     d.Client,
		cfg:        d.Cfg,
		mmcfg:      mmcfg,
		feed:       d.Feed,
		candleFeed: candleFeed,
		fair:       fair,
		sel:        selector.New(d.Client, fair, mmcfg.Selection),
		pnl:        oms.NewPnLAccountant(),
		policy:     settle.Policy{BlackoutMins: mmcfg.Settle.BlackoutMins},
		// Sign ONLY when the operator has explicitly opted in: mm.enabled = true AND
		// mm.dry_run = false (and no --dry-run). A fresh/default config never signs; the
		// TUI can flip `live` at runtime (unless --dry-run forced shadow).
		forceDry:    d.DryRun,
		live:        mmcfg.Enabled && !mmcfg.DryRun,
		configPath:  d.ConfigPath,
		quoteEvery:  durationMs(mmcfg.QuoteIntervalMs, time.Second),
		scanEvery:   durationSecs(mmcfg.Selection.ScanIntervalSecs, 5*time.Minute),
		questionNtl: map[string]float64{},
	}
	e.view.DryRun = !e.signing()
	e.view.Network = d.Cfg.Network
	return e
}

// signing reports whether the engine may place real orders right now.
func (e *Engine) signing() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.forceDry && e.live
}

// Live reports whether live signing is currently on.
func (e *Engine) Live() bool { return e.signing() }

// SetLive turns live signing on/off at runtime (the TUI's money switch). It refuses
// when the process was launched with --dry-run (restart without it to trade). Turning
// live OFF cancels all resting quotes so the book is left clean.
func (e *Engine) SetLive(on bool) error {
	e.mu.Lock()
	if e.forceDry {
		e.mu.Unlock()
		return errForceDry
	}
	wasLive := e.live
	e.live = on
	e.mu.Unlock()
	if wasLive && !on {
		tctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
		defer cancel()
		_, _ = e.client.Cancel(tctx, core.CancelReq{All: true})
	}
	return nil
}

var errForceDry = errShim("launched with --dry-run — restart without it to trade live")

// errShim is a tiny error type so engine.go needs no fmt import just for one message.
type errShim string

func (e errShim) Error() string { return string(e) }

// Run drives the engine until ctx is cancelled: it starts the market-data feed,
// takes an initial scan, then interleaves the fast quote tick and the slow candidate
// scan. On shutdown it cancels every resting quote (fresh context) so the book is
// never left quoted. Run blocks; call it in the foreground of the daemon.
func (e *Engine) Run(ctx context.Context) error {
	// Book PnL only from fills SINCE startup: the dashboard shows this MM session's
	// P&L, not the account's lifetime history, and it avoids a multi-thousand-fill
	// backfill that would stall the first scan.
	now := time.Now()
	e.mu.Lock()
	e.lastFillTs = now.UnixMilli()
	e.startedAt = now
	e.view.StartedAt = now
	e.mu.Unlock()

	// Seed the PnL cost basis from positions carried in from a prior session, using the
	// venue's entry price, so selling them down this session books realized against the
	// true cost (not as pure profit). Best-effort — a read failure just means those
	// legs mark from a zero basis until a fill re-establishes it.
	if pf, err := e.client.Portfolio(ctx); err == nil && pf != nil {
		for _, pos := range pf.Positions {
			if pos.Class != "outcome" {
				continue
			}
			if sh, ok := mm.ParseFloat(pos.Szi); ok && sh != 0 {
				if entry, ok := mm.ParseFloat(pos.EntryPx); ok {
					e.pnl.SeedPosition(pos.Coin, int64(math.Round(math.Abs(sh))), entry)
				}
			}
		}
	}

	// Market-data feed (allMids → marks + vol) runs for the engine's lifetime. A
	// terminal return before shutdown is surfaced; marks then go stale (Feed.Mark),
	// which stops quoting — the fail-safe against pricing off a dead stream.
	go func() {
		if ferr := e.feed.Run(ctx, e.client); ferr != nil && ctx.Err() == nil {
			e.setError("feed stream ended: " + ferr.Error())
		}
	}()

	e.setRunning(true)
	defer e.setRunning(false)

	// Arm the dead-man's switch as a crash backstop: if this process dies uncleanly,
	// Hyperliquid cancels the resting quotes at the deadline. Re-armed each scan.
	e.armDMS(ctx)

	// Give the feed a moment, then take the first scan so the first quote tick has an
	// active set. A scan failure is non-fatal (retried on the scan cadence).
	e.scan(ctx)

	quote := time.NewTicker(e.quoteEvery)
	scan := time.NewTicker(e.scanEvery)
	defer quote.Stop()
	defer scan.Stop()

	for {
		select {
		case <-ctx.Done():
			e.teardown()
			return nil
		case <-scan.C:
			e.scan(ctx)
		case <-quote.C:
			e.tick(ctx)
		}
	}
}

// scan re-ranks the outcome universe into the active set (slow cadence). It also
// refreshes PnL from new fills. Read failures are recorded but non-fatal.
func (e *Engine) scan(ctx context.Context) {
	e.reloadConfig() // pick up live [mm] edits (pins/blacklist/caps/spreads)
	// Refresh the underlying mark + vol from perp candles BEFORE ranking, so the very
	// first scan can price markets (no WS warm-up wait).
	e.candleFeed.Refresh(ctx, e.client, e.mmcfg.Selection.PriceableUnderlyings)
	if err := e.client.EnsureOutcomes(ctx); err != nil {
		e.setError("ensure outcomes: " + err.Error())
		return
	}
	universe := e.client.Meta().OutcomeMarkets()

	e.mu.Lock()
	prevActive := map[string]bool{}
	for _, c := range e.active {
		prevActive[c.Market.Coin] = true
	}
	qn := copyFloatMap(e.questionNtl)
	e.mu.Unlock()

	in := selector.Inputs{
		Now:              time.Now(),
		QuestionNotional: qn,
		BucketCap:        e.cfg.Risk.MaxOutcomeQuestionNotionalUSD,
		PrevActive:       prevActive,
	}
	sel, err := e.sel.Scan(ctx, universe, in)
	if err != nil {
		e.setError("scan: " + err.Error())
		return
	}

	e.ingestFills(ctx)

	e.mu.Lock()
	e.active = sel.Active
	e.view.Pool = sel.Pool
	e.view.LastScan = time.Now()
	e.mu.Unlock()

	// Cross-book arb runs on the scan cadence (best-effort, guarded, dry-run-aware).
	// Passes the candidates so it reuses the YES books the selector already fetched.
	e.arbScan(ctx, sel.Active)
	// Heartbeat the dead-man's switch so it stays ahead of the next scan.
	e.armDMS(ctx)
}

// reloadConfig re-reads the config file (if a path was provided) so live [mm] edits —
// pins, blacklist, selection caps, strategy spreads, blackout window — take effect on
// the next scan. It updates the selection config, strategy/arb knobs, risk caps, and
// the settlement policy, but deliberately NOT the dry-run/enabled gate: flipping to
// live signing requires a restart, so a stray config edit can't silently start
// trading. Runs on the scan goroutine, the only reader of these fields (bar the view).
func (e *Engine) reloadConfig() {
	if e.configPath == "" {
		return
	}
	fresh, err := config.Load(e.configPath)
	if err != nil {
		e.setError("config reload: " + err.Error())
		return
	}
	e.cfg = fresh
	e.mmcfg = fresh.MMOrDefault()
	e.sel.SetSelection(e.mmcfg.Selection)
	e.policy = settle.Policy{BlackoutMins: e.mmcfg.Settle.BlackoutMins}
}

// armDMS (re)arms the dead-man's switch to cancel resting orders if the daemon dies.
// The window is set comfortably past the scan cadence so the periodic re-arm keeps it
// alive; a crash lets it fire. Skipped in dry-run (nothing is signed).
func (e *Engine) armDMS(ctx context.Context) {
	if !e.signing() {
		return
	}
	// Hyperliquid rate-limits scheduleCancel (and requires some traded volume), so
	// re-arm on a slow cadence, not every scan. Throttle regardless of outcome so a
	// persistent failure doesn't spam the activity feed; the window is set well past
	// the re-arm interval so the switch never lapses between arms.
	if !e.lastDMSArm.IsZero() && time.Since(e.lastDMSArm) < dmsRearmInterval {
		return
	}
	e.lastDMSArm = time.Now()
	secs := e.cfg.Risk.DeadManSwitchSecs
	if minWindow := int(dmsRearmInterval/time.Second) + 90; secs < minWindow {
		secs = minWindow
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
	// Best-effort: HL gates scheduleCancel behind an eligibility rule some accounts
	// don't meet. A failure is NOT surfaced as the prominent last-error (it would look
	// alarming) — the crash backstop is simply unavailable; exit-cancel + panic remain.
	_ = e.client.ScheduleCancel(ctx, &deadline)
}

// ingestFills reads new fills since the high-water timestamp and books them into PnL.
func (e *Engine) ingestFills(ctx context.Context) {
	e.mu.Lock()
	since := e.lastFillTs
	e.mu.Unlock()
	var sincePtr *int64
	if since > 0 {
		sincePtr = &since
	}
	// Since lastFillTs starts at session start and advances, each read covers only a
	// short window (few fills); 1000 is ample headroom against core.Fills' newest-N
	// truncation. Dedup-by-Tid makes the overlap on the next read harmless.
	fills, _, err := e.client.Fills(ctx, sincePtr, 1000)
	if err != nil {
		return
	}
	e.pnl.IngestFills(fills)
	var maxTs int64 = since
	for _, f := range fills {
		if f.Time > maxTs {
			maxTs = f.Time
		}
	}
	e.mu.Lock()
	e.lastFillTs = maxTs
	e.mu.Unlock()
}

// teardown cancels every resting order on shutdown, in a fresh (non-cancelled)
// context. Cancel-all is deliberate: the MM must never leave the book quoted, and the
// daemon owns the agent key in practice. Skipped in dry-run (nothing was signed).
func (e *Engine) teardown() {
	if !e.signing() {
		return
	}
	tctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	_, _ = e.client.Cancel(tctx, core.CancelReq{All: true})
}

// ---- small helpers / accessors ----

func (e *Engine) setRunning(r bool) {
	e.mu.Lock()
	e.view.Running = r
	e.mu.Unlock()
}

func (e *Engine) setError(msg string) {
	e.mu.Lock()
	e.view.LastError = msg
	e.mu.Unlock()
}

func durationMs(ms int, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

func durationSecs(s int, def time.Duration) time.Duration {
	if s <= 0 {
		return def
	}
	return time.Duration(s) * time.Second
}

func copyFloatMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
