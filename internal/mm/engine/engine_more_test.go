package engine

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
)

// ---- extra fakeClient methods (the base type lives in engine_test.go) ----

// ScheduleCancel is a no-op arm so armDMS can be exercised without a real venue.
func (f *fakeClient) ScheduleCancel(context.Context, *int64) error { return nil }

// fillsFake wraps the shared fakeClient and adds a Fills stub, so ingestFills can be
// driven with a scripted fill history without touching the shared struct.
type fillsFake struct {
	*fakeClient
	fills []hl.Fill
}

func (c *fillsFake) Fills(context.Context, *int64, int) ([]hl.Fill, core.ReadMeta, error) {
	return c.fills, core.ReadMeta{}, nil
}

// midStreamer emits a single allMids frame then returns, so a test can seed the feed's
// outcome mids (used for settlement inference) without a live WS.
type midStreamer struct{ data string }

func (m midStreamer) Stream(_ context.Context, _ []core.StreamSub, on func(core.StreamEvent)) error {
	on(core.StreamEvent{Channel: "allMids", Data: json.RawMessage(m.data)})
	return nil
}

// ---- helpers ----

func allPlaced(fc *fakeClient) []core.OrderReq {
	var out []core.OrderReq
	for _, b := range fc.placed {
		out = append(out, b...)
	}
	return out
}

func hasOrder(fc *fakeClient, coin string, side core.Side) bool {
	for _, o := range allPlaced(fc) {
		if o.Coin == coin && o.Side == side {
			return true
		}
	}
	return false
}

// liveTogglable builds a runtime-togglable engine: NOT forceDry (so SetLive works) and
// live=false at start (config dry_run=true), so the money switch can be flipped on/off.
func liveTogglable(fc *fakeClient) *Engine {
	cfg := config.Default()
	mmc := config.DefaultMM()
	mmc.Enabled = true
	mmc.DryRun = true // ⇒ live starts false, but forceDry stays false (Deps.DryRun=false)
	cfg.MM = mmc
	e := New(Deps{
		Client: fc,
		Cfg:    cfg,
		Feed:   oms.NewFeed([]string{"BTC"}, 0.94),
		DryRun: false,
		Fair:   stubFair{fair: mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)}},
	})
	e.active = []selector.Candidate{{Market: btcMarket()}}
	return e
}

// ---- (1) NO-side quoting ----

// With quote_no_side on and a fair near 0.5, the sibling NO coin is quoted: at least a
// NO bid (buying NO = shorting YES) must be placed on mm.NoCoin(outcome).
func TestTickQuotesNoSideNearMid(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	if !e.mmcfg.Strategy.QuoteNoSide {
		t.Fatal("precondition: DefaultMM should ship quote_no_side=true")
	}
	e.tick(context.Background())

	noCoin := mm.NoCoin(btcMarket().Outcome) // "#101"
	if !hasOrder(fc, noCoin, core.Buy) {
		t.Fatalf("quote_no_side near mid should place a NO-coin (%s) bid; placed=%+v", noCoin, allPlaced(fc))
	}
}

// Deep in-the-money (fair≈0.99) drives the NO price (1−fair≈0.01) below no_side_min_price
// while flat, so the NO side is gated OFF — no order on the NO coin — even though the YES
// coin is still quoted.
func TestTickGatesNoSideDeepITM(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.99, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	e.tick(context.Background())

	yesCoin := mm.YesCoin(btcMarket().Outcome) // "#100"
	noCoin := mm.NoCoin(btcMarket().Outcome)   // "#101"
	if !hasOrder(fc, yesCoin, core.Buy) {
		t.Fatalf("deep-ITM YES coin should still be quoted (a bid); placed=%+v", allPlaced(fc))
	}
	for _, o := range allPlaced(fc) {
		if o.Coin == noCoin {
			t.Fatalf("NO side must be gated off deep-ITM while flat, got order %+v", o)
		}
	}
}

// ---- (2) no-naked-short ----

// Flat inventory ⇒ the YES ask ladder is empty: you cannot sell YES shares you do not
// hold (HIP-4 has no naked short).
func TestTickNoNakedShortWhenFlat(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	e.tick(context.Background())

	yesCoin := mm.YesCoin(btcMarket().Outcome)
	if hasOrder(fc, yesCoin, core.Sell) {
		t.Fatalf("flat inventory must place no YES asks (no naked short); placed=%+v", allPlaced(fc))
	}
	// It should still quote the bid side (willing to buy).
	if !hasOrder(fc, yesCoin, core.Buy) {
		t.Fatalf("flat inventory should still quote a YES bid; placed=%+v", allPlaced(fc))
	}
}

// A held YES position lets an ask appear (reduce-only-ish) AND surfaces the holding in
// the view.
func TestTickHeldYesPlacesAskAndHoldings(t *testing.T) {
	fc := &fakeClient{pf: &core.PortfolioView{
		AccountValue: "1000",
		Positions: []core.PositionView{{
			Coin:          "#100",
			Class:         "outcome",
			Szi:           "100",
			Side:          "long",
			OutcomeSide:   "Yes",
			Title:         "BTC above 76000",
			EntryPx:       "0.45",
			MarkPx:        "0.50",
			PositionValue: "50",
			UnrealizedPnl: "5",
		}},
	}}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	started := time.Now().Add(-3 * time.Minute)
	e.view.StartedAt = started // set once at Run() in prod; assert tick preserves it
	e.tick(context.Background())

	yesCoin := mm.YesCoin(btcMarket().Outcome)
	if !hasOrder(fc, yesCoin, core.Sell) {
		t.Fatalf("holding YES should let an ask appear; placed=%+v", allPlaced(fc))
	}

	v := e.View()
	if len(v.Holdings) != 1 {
		t.Fatalf("view should carry exactly one holding, got %+v", v.Holdings)
	}
	h := v.Holdings[0]
	if h.Coin != "#100" || h.Shares != 100 || !h.Active {
		t.Fatalf("holding row wrong: %+v", h)
	}
	// Open PnL is taken from HL's authoritative unrealized figure summed over holdings.
	if math.Abs(v.PnL.Open-5) > 1e-9 {
		t.Fatalf("PnL.Open should equal the held position's unrealized 5, got %v", v.PnL.Open)
	}
	if math.Abs(v.PnL.Net-5) > 1e-9 {
		t.Fatalf("PnL.Net = realized(0) − fees(0) + open(5) should be 5, got %v", v.PnL.Net)
	}
	if !v.StartedAt.Equal(started) {
		t.Fatalf("tick must not clobber StartedAt: want %v got %v", started, v.StartedAt)
	}
}

// ---- (3) SetLive / Live / SetPaused ----

func TestSetLiveForceDryRefuses(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, true, mm.Fair{P: 0.5, Conf: 0.9}) // DryRun ⇒ forceDry
	if e.Live() {
		t.Fatal("a --dry-run engine must not report live")
	}
	if err := e.SetLive(true); err != errForceDry {
		t.Fatalf("SetLive on a forceDry engine must return errForceDry, got %v", err)
	}
	if e.Live() {
		t.Fatal("SetLive must not have flipped live under forceDry")
	}
}

func TestSetLiveTogglesSigning(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := liveTogglable(fc)
	if e.Live() {
		t.Fatal("engine should start not-live (config dry_run=true)")
	}
	if err := e.SetLive(true); err != nil {
		t.Fatalf("SetLive(true) should succeed, got %v", err)
	}
	if !e.Live() {
		t.Fatal("Live() should be true after SetLive(true)")
	}
	// Turning live OFF must pull the book (cancel-all).
	if err := e.SetLive(false); err != nil {
		t.Fatalf("SetLive(false) should succeed, got %v", err)
	}
	if e.Live() {
		t.Fatal("Live() should be false after SetLive(false)")
	}
	if fc.cancelAll != 1 {
		t.Fatalf("going live→off must cancel-all once to clear the book, got %d", fc.cancelAll)
	}
}

func TestPausedAccessor(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9})
	if e.Paused() {
		t.Fatal("a fresh engine must not be paused")
	}
	e.SetPaused(true)
	if !e.Paused() {
		t.Fatal("Paused() should reflect SetPaused(true)")
	}
	if !e.View().Paused {
		t.Fatal("view should reflect the pause")
	}
	e.SetPaused(false)
	if e.Paused() {
		t.Fatal("Paused() should reflect SetPaused(false)")
	}
}

// ---- (5) strategyParams ----

func TestStrategyParamsMapsConfig(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9})

	p := e.strategyParams(0.9, 42)
	s := e.mmcfg.Strategy
	if p.HeldShares != 42 {
		t.Fatalf("HeldShares should pass through, got %d", p.HeldShares)
	}
	if p.MidAnchorWeight != s.MidAnchorWeight || p.MidAnchorWeight != 0.70 {
		t.Fatalf("MidAnchorWeight should map from config (0.70), got %v", p.MidAnchorWeight)
	}
	if p.BaseSpread != s.BaseSpread || p.Levels != s.Levels || p.LevelStep != s.LevelStep {
		t.Fatalf("spread/level params mismapped: %+v", p)
	}
	if p.BaseSizeShares != s.BaseSizeShares || p.SizeStepShares != s.SizeStepShares {
		t.Fatalf("size params mismapped: %+v", p)
	}
	if p.MaxInventoryShares != s.MaxInventoryShares || p.InventorySkew != s.InventorySkew || p.MinEdge != s.MinEdge {
		t.Fatalf("inventory/edge params mismapped: %+v", p)
	}
	if p.MinNotionalUSD != e.cfg.Risk.MinOrderNotionalUSD {
		t.Fatalf("MinNotionalUSD should come from risk config, got %v", p.MinNotionalUSD)
	}
	// Confidence widens the spread: 1/max(conf,0.34), capped at 3.
	if want := 1 / 0.9; math.Abs(p.SpreadMult-want) > 1e-9 {
		t.Fatalf("SpreadMult at conf 0.9 should be %v, got %v", want, p.SpreadMult)
	}
	if lo := e.strategyParams(0.1, 0).SpreadMult; math.Abs(lo-1/0.34) > 1e-9 {
		t.Fatalf("low conf should clamp the divisor at 0.34 (mult %v), got %v", 1/0.34, lo)
	}
	if zero := e.strategyParams(0, 0).SpreadMult; zero != 1.0 {
		t.Fatalf("zero conf should leave the spread unmultiplied (1.0), got %v", zero)
	}
}

// ---- (6) ingestFills ----

func TestIngestFillsBooksRealizedAndVolume(t *testing.T) {
	base := &fakeClient{pf: emptyPortfolio()}
	cli := &fillsFake{fakeClient: base, fills: []hl.Fill{
		{Tid: 1, Coin: "#100", Side: "B", Price: "0.40", Size: "100", Time: 10},
		{Tid: 2, Coin: "#100", Side: "A", Price: "0.50", Size: "100", Time: 20},
	}}
	cfg := config.Default()
	mmc := config.DefaultMM()
	mmc.Enabled = true
	cfg.MM = mmc
	e := New(Deps{
		Client: cli,
		Cfg:    cfg,
		Feed:   oms.NewFeed([]string{"BTC"}, 0.94),
		Fair:   stubFair{fair: mm.Fair{P: 0.5, Conf: 0.9}},
	})

	e.ingestFills(context.Background())

	pnl := e.pnl.View(nil)
	if pnl.Fills != 2 {
		t.Fatalf("both fills should book, got %d", pnl.Fills)
	}
	// Buy 100@0.40 then sell 100@0.50 ⇒ realized (0.50−0.40)*100 = 10.
	if math.Abs(pnl.Realized-10) > 1e-6 {
		t.Fatalf("realized should be 10 on the round-trip, got %v", pnl.Realized)
	}
	// Volume = |0.40*100| + |0.50*100| = 90.
	if math.Abs(pnl.Volume-90) > 1e-6 {
		t.Fatalf("volume should be 90, got %v", pnl.Volume)
	}
	// The high-water fill timestamp advances so the next read is incremental.
	if e.lastFillTs != 20 {
		t.Fatalf("lastFillTs should advance to the newest fill (20), got %d", e.lastFillTs)
	}
}

// ---- (4)+ reconcileSettlements / marketResolved ----

// A held coin that has left the on-chain position set with the market resolved and a
// final mark ≥ 0.5 is realized at a payout of 1 per share against its cost basis.
func TestReconcileSettlementsRealizesWinner(t *testing.T) {
	fc := &fakeClient{
		pf:   emptyPortfolio(),
		meta: core.NewMetaStore("mainnet", nil, nil, time.Now()), // empty ⇒ Lookup miss ⇒ resolved
	}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	// Seed a final mark for the settled coin via one allMids frame.
	if err := e.feed.Run(context.Background(), midStreamer{data: `{"mids":{"#100":"0.80"}}`}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	// Carry a prior-session position: 100 shares at cost 0.40.
	e.pnl.SeedPosition("#100", 100, 0.40)

	// Reconcile with an on-chain inventory that no longer holds #100 (silently settled).
	e.reconcileSettlements(context.Background(), map[string]int64{})

	got := e.pnl.View(nil).Realized
	// payout 1*100 − cost 40 = 60.
	if math.Abs(got-60) > 1e-6 {
		t.Fatalf("winner settlement should realize 60 (payout 100 − cost 40), got %v", got)
	}
}

func TestMarketResolved(t *testing.T) {
	meta := core.NewMetaStore("mainnet", &hl.Meta{Universe: []hl.AssetInfo{{Name: "FOO", MaxLeverage: 1}}}, nil, time.Now())
	fc := &fakeClient{pf: emptyPortfolio(), meta: meta}
	e := newEngine(t, fc, true, mm.Fair{P: 0.5, Conf: 0.9})

	if !e.marketResolved("#404") {
		t.Fatal("a coin absent from meta must be treated as resolved/gone")
	}
	if e.marketResolved("FOO") {
		t.Fatal("an open, non-settled market with no past expiry must not be resolved")
	}
}

// ---- armDMS ----

func TestArmDMS(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	if !e.signing() {
		t.Fatal("precondition: a live engine should be signing")
	}
	e.armDMS(context.Background())
	first := e.lastDMSArm
	if first.IsZero() {
		t.Fatal("armDMS should stamp lastDMSArm while signing")
	}
	e.armDMS(context.Background()) // immediate re-arm is throttled
	if !e.lastDMSArm.Equal(first) {
		t.Fatal("armDMS should throttle a re-arm inside the rearm interval")
	}

	// A shadow engine never arms (nothing is signed).
	de := newEngine(t, fc, true, mm.Fair{P: 0.5, Conf: 0.9})
	de.armDMS(context.Background())
	if !de.lastDMSArm.IsZero() {
		t.Fatal("a dry-run engine must not arm the dead-man switch")
	}
}

// ---- reloadConfig ----

func TestReloadConfigPicksUpEdits(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9})
	if e.mmcfg.Strategy.BaseSpread == 0.055 {
		t.Fatal("precondition: default base_spread should not already be 0.055")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[mm]\n[mm.strategy]\nbase_spread = 0.055\n"), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	e.configPath = path
	e.reloadConfig()

	if e.mmcfg.Strategy.BaseSpread != 0.055 {
		t.Fatalf("reloadConfig should pick up the edited base_spread, got %v", e.mmcfg.Strategy.BaseSpread)
	}
	if got := e.View().LastError; got != "" {
		t.Fatalf("a clean reload should record no error, got %q", got)
	}

	// No path ⇒ reload is a no-op (does not error).
	e.configPath = ""
	e.reloadConfig()
}

// ---- stale fair gate + small accessors ----

func TestTickStaleFairPlacesNothing(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	// ValidUntil in the past ⇒ Fair.Valid(now) is false ⇒ "stale fair" gate, no quotes.
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(-time.Hour)})
	e.tick(context.Background())
	if len(fc.placed) != 0 {
		t.Fatalf("stale fair must place nothing, placed=%+v", allPlaced(fc))
	}
	if g := e.View().Active[0].Gate; g != "stale fair" {
		t.Fatalf("gate should be 'stale fair', got %q", g)
	}
}

func TestSmallHelpers(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9})

	e.setRunning(true)
	if !e.View().Running {
		t.Fatal("setRunning(true) should show in the view")
	}
	e.setError("boom")
	if e.View().LastError != "boom" {
		t.Fatal("setError should surface in the view")
	}

	src := map[string]float64{"a": 1, "b": 2}
	cp := copyFloatMap(src)
	if len(cp) != 2 || cp["a"] != 1 || cp["b"] != 2 {
		t.Fatalf("copyFloatMap should duplicate contents, got %+v", cp)
	}
	cp["a"] = 99
	if src["a"] != 1 {
		t.Fatal("copyFloatMap must return an independent copy")
	}

	if p := ptrInt64(7); p == nil || *p != 7 {
		t.Fatal("ptrInt64 should box its argument")
	}
	if errForceDry.Error() == "" {
		t.Fatal("errShim.Error should return its message")
	}
}

// ---- arbScan gating ----

func TestArbScanDisabledNoOp(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9})
	// Arb is off by default ⇒ early return, nothing placed.
	e.arbScan(context.Background(), []selector.Candidate{{Market: btcMarket()}})
	if len(fc.placed) != 0 {
		t.Fatalf("disabled arb must place nothing, got %+v", allPlaced(fc))
	}

	// Enabled but with empty books ⇒ no crossable edge ⇒ still nothing placed, but the
	// scan body (params + per-candidate loop) is exercised.
	e.mmcfg.Arb.Enabled = true
	e.arbScan(context.Background(), []selector.Candidate{{Market: btcMarket()}})
	if len(fc.placed) != 0 {
		t.Fatalf("no-edge arb must place nothing, got %+v", allPlaced(fc))
	}
}
