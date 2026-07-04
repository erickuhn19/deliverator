package engine

import (
	"context"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
)

// fakeClient embeds core.ClientAPI (nil), so any method the engine does NOT use
// panics if called — a tight guard that the tick path only touches what we stub.
type fakeClient struct {
	core.ClientAPI
	pf         *core.PortfolioView
	bbo        *core.BboView   // nil ⇒ empty book (no maker clamp)
	meta       *core.MetaStore // nil ok when there are no held positions to look up
	halted     bool
	placed     [][]core.OrderReq
	modified   [][]core.ModifyReq
	cancelAll  int
	cancelReqs []core.CancelReq
}

func (f *fakeClient) Portfolio(context.Context) (*core.PortfolioView, error) { return f.pf, nil }
func (f *fakeClient) Halted() bool                                           { return f.halted }
func (f *fakeClient) Bbo(context.Context, string) (*core.BboView, error)     { return f.bbo, nil }
func (f *fakeClient) Meta() *core.MetaStore                                  { return f.meta }
func (f *fakeClient) Fills(context.Context, *int64, int) ([]hl.Fill, core.ReadMeta, error) {
	return nil, core.ReadMeta{}, nil // no fills by default; fillsFake overrides for scripted history
}
func (f *fakeClient) PlaceBatch(_ context.Context, r []core.OrderReq) ([]*core.PlaceResult, []string, error) {
	f.placed = append(f.placed, r)
	return nil, nil, nil
}
func (f *fakeClient) ModifyBatch(_ context.Context, r []core.ModifyReq) ([]*core.PlaceResult, []string, error) {
	f.modified = append(f.modified, r)
	return nil, nil, nil
}
func (f *fakeClient) Cancel(_ context.Context, r core.CancelReq) (*core.CancelResult, error) {
	if r.All {
		f.cancelAll++
	}
	f.cancelReqs = append(f.cancelReqs, r)
	return &core.CancelResult{}, nil
}

// stubFair returns a fixed fair value regardless of market.
type stubFair struct {
	fair mm.Fair
	err  error
}

func (s stubFair) Estimate(context.Context, core.Market) (mm.Fair, error) { return s.fair, s.err }

func btcMarket() core.Market {
	return core.Market{
		Coin: "#100", Class: "outcome", IsOutcome: true, Outcome: 10, Side: "Yes",
		ResolutionStatus: "open", Underlying: "BTC", TargetPrice: "76000",
		Expiry: time.Now().Add(6 * time.Hour).UTC().Format(mm.ExpiryLayout),
		Title:  "BTC above 76000",
	}
}

func newEngine(t *testing.T, fc *fakeClient, dryRun bool, fair mm.Fair) *Engine {
	t.Helper()
	cfg := config.Default()
	// DefaultMM ships DryRun=true (safe default); set it explicitly so a live test
	// actually signs. e.dryRun = Deps.DryRun || cfg.MM.DryRun.
	mmc := config.DefaultMM()
	mmc.Enabled = true // opt in so !dryRun actually signs
	mmc.DryRun = dryRun
	cfg.MM = mmc
	e := New(Deps{
		Client: fc,
		Cfg:    cfg,
		Feed:   oms.NewFeed([]string{"BTC"}, 0.94),
		DryRun: dryRun,
		Fair:   stubFair{fair: fair},
	})
	// Seed the active set directly (bypass the slow scan).
	e.active = []selector.Candidate{{Market: btcMarket()}}
	return e
}

func emptyPortfolio() *core.PortfolioView {
	return &core.PortfolioView{AccountValue: "1000", Positions: nil, OpenOrders: nil}
}

func TestTickPlacesLadderWhenLive(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	e.tick(context.Background())

	if len(fc.placed) == 0 {
		t.Fatal("live tick with a valid fair value should place a ladder")
	}
	total := 0
	for _, b := range fc.placed {
		total += len(b)
	}
	if total == 0 {
		t.Fatal("placed batch was empty")
	}
	// Every quote must be a valid outcome limit (survives core's rounder) and post-only.
	for _, b := range fc.placed {
		for _, o := range b {
			if o.Tif != "Alo" {
				t.Fatalf("maker quote must be Alo, got %q", o.Tif)
			}
			if _, _, err := core.RoundOutcomePrice(o.Limit); err != nil {
				t.Fatalf("quote limit %q rejected by core rounder: %v", o.Limit, err)
			}
		}
	}
	v := e.View()
	if len(v.Active) != 1 || v.Active[0].Gate != "quoting" {
		t.Fatalf("view should show one quoting market, got %+v", v.Active)
	}
}

func TestTickDryRunSignsNothing(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, true, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	e.tick(context.Background())
	if len(fc.placed) != 0 || len(fc.modified) != 0 {
		t.Fatalf("dry-run must not sign: placed=%v modified=%v", fc.placed, fc.modified)
	}
	if !e.View().DryRun {
		t.Fatal("view should report dry-run")
	}
}

func TestTickHaltedSignsNothing(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio(), halted: true}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	e.tick(context.Background())
	if len(fc.placed) != 0 {
		t.Fatal("halted engine must not place")
	}
	if !e.View().Halted {
		t.Fatal("view should report halted")
	}
}

func TestTickBlackoutCancelsAndPullsQuotes(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	// Two resting MM orders on the coin (MM-tagged cloids); market inside its blackout.
	c1, c2 := oms.NewMMCloid(), oms.NewMMCloid()
	fc.pf.OpenOrders = []hl.FrontendOpenOrder{
		{Coin: "#100", Oid: 1, Side: hl.OrderSideBid, LimitPx: 0.48, Sz: 10, Cloid: &c1},
		{Coin: "#100", Oid: 2, Side: hl.OrderSideAsk, LimitPx: 0.52, Sz: 10, Cloid: &c2},
	}
	cfg := config.Default()
	cfg.MM = config.DefaultMM()
	cfg.MM.Enabled = true
	cfg.MM.DryRun = false // exercise live cancels
	cfg.MM.Settle.BlackoutMins = 60
	e := New(Deps{Client: fc, Cfg: cfg, Feed: oms.NewFeed([]string{"BTC"}, 0.94),
		Fair: stubFair{fair: mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)}}})
	m := btcMarket()
	m.Expiry = time.Now().Add(30 * time.Minute).UTC().Format(mm.ExpiryLayout) // inside 60m blackout
	e.active = []selector.Candidate{{Market: m}}

	e.tick(context.Background())
	if len(fc.placed) != 0 {
		t.Fatal("blackout must place no new quotes")
	}
	// The two resting orders should be cancelled (desired is empty in blackout).
	found := 0
	for _, r := range fc.cancelReqs {
		found += len(r.Oids)
	}
	if found != 2 {
		t.Fatalf("blackout should cancel the 2 resting quotes, cancelled %d", found)
	}
	if g := e.View().Active[0].Gate; g == "quoting" {
		t.Fatalf("gate should reflect blackout, got %q", g)
	}
}

// A halt must still let the engine CANCEL its resting quotes (core.Cancel is
// halt-exempt) — otherwise a pull-down during a halt leaves quotes live to be
// adversely filled while the operator believes trading is stopped.
func TestTickHaltedStillCancels(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio(), halted: true}
	c1, c2 := oms.NewMMCloid(), oms.NewMMCloid()
	fc.pf.OpenOrders = []hl.FrontendOpenOrder{
		{Coin: "#100", Oid: 1, Side: hl.OrderSideBid, LimitPx: 0.48, Sz: 10, Cloid: &c1},
		{Coin: "#100", Oid: 2, Side: hl.OrderSideAsk, LimitPx: 0.52, Sz: 10, Cloid: &c2},
	}
	cfg := config.Default()
	cfg.MM = config.DefaultMM()
	cfg.MM.Enabled = true
	cfg.MM.DryRun = false
	cfg.MM.Settle.BlackoutMins = 60 // blackout ⇒ desired empty ⇒ diff cancels the quotes
	e := New(Deps{Client: fc, Cfg: cfg, Feed: oms.NewFeed([]string{"BTC"}, 0.94),
		Fair: stubFair{fair: mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)}}})
	m := btcMarket()
	m.Expiry = time.Now().Add(30 * time.Minute).UTC().Format(mm.ExpiryLayout)
	e.active = []selector.Candidate{{Market: m}}

	e.tick(context.Background())
	if len(fc.placed) != 0 {
		t.Fatal("halted engine must not place")
	}
	cancels := 0
	for _, r := range fc.cancelReqs {
		cancels += len(r.Oids)
	}
	if cancels != 2 {
		t.Fatalf("halted engine must still cancel its 2 resting quotes, got %d", cancels)
	}
}

func TestTeardownCancelsAll(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9})
	e.teardown()
	if fc.cancelAll != 1 {
		t.Fatalf("teardown should cancel-all once, got %d", fc.cancelAll)
	}
}

func TestTeardownDryRunNoCancel(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	e := newEngine(t, fc, true, mm.Fair{P: 0.5, Conf: 0.9})
	e.teardown()
	if fc.cancelAll != 0 {
		t.Fatal("dry-run teardown must not cancel (nothing was signed)")
	}
}

func TestPausePullsQuotes(t *testing.T) {
	fc := &fakeClient{pf: emptyPortfolio()}
	fc.pf.OpenOrders = []hl.FrontendOpenOrder{{Coin: "#100", Oid: 1, Side: hl.OrderSideBid, LimitPx: 0.48, Sz: 10}}
	e := newEngine(t, fc, false, mm.Fair{P: 0.5, Conf: 0.9, ValidUntil: time.Now().Add(time.Hour)})
	e.SetPaused(true)
	e.tick(context.Background())
	if len(fc.placed) != 0 {
		t.Fatal("paused engine must not place")
	}
	if e.View().Active[0].Gate != "paused" {
		t.Fatalf("gate should be paused, got %q", e.View().Active[0].Gate)
	}
}
