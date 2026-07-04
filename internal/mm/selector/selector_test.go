package selector

import (
	"context"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// ---- fakes ----

type fakeMD struct {
	vol map[string]float64 // coin -> total 24h volume to report
	bbo map[string]mm.BookTop
}

func (f *fakeMD) Candles(_ context.Context, coin, _ string, _ *int64) ([]hl.Candle, error) {
	v := f.vol[coin]
	// One candle carrying the whole volume; the summer parses Volume.
	return []hl.Candle{{Volume: ftoa(v)}}, nil
}

func (f *fakeMD) Bbo(_ context.Context, coin string) (*core.BboView, error) {
	b, ok := f.bbo[coin]
	if !ok {
		return &core.BboView{Coin: coin}, nil
	}
	return &core.BboView{Coin: coin, Bid: ftoa(b.Bid), Ask: ftoa(b.Ask), BidSz: ftoa(b.BidSz), AskSz: ftoa(b.AskSz)}, nil
}

type fakeFV struct {
	fair map[string]mm.Fair
	def  mm.Fair
}

func (f *fakeFV) Estimate(_ context.Context, m core.Market) (mm.Fair, error) {
	if fr, ok := f.fair[m.Coin]; ok {
		return fr, nil
	}
	return f.def, nil
}

func ftoa(f float64) string {
	return strconvFormat(f)
}

// tiny local float formatter to avoid importing strconv in the test twice
func strconvFormat(f float64) string {
	return mm.ProbString(f) // not ideal for large volume, but selector only sums; fine for depths/probs
}

// ---- pure scoring ----

func defSel() config.MMSelection { return config.DefaultMM().Selection }

func TestSpreadScore(t *testing.T) {
	cfg := defSel() // HalfSpreadFloor 0.01, SpreadRef 0.05
	wide := mm.BookTop{Bid: 0.40, Ask: 0.60, HasBid: true, HasAsk: true}
	if s := spreadScore(wide, cfg); s <= 0.9 { // half=0.10, (0.10-0.01)/0.05=1.8 -> clamp 1
		t.Fatalf("wide book should score ~1, got %v", s)
	}
	tight := mm.BookTop{Bid: 0.499, Ask: 0.501, HasBid: true, HasAsk: true}
	if s := spreadScore(tight, cfg); s != 0 { // half=0.001 < floor 0.01
		t.Fatalf("book tighter than floor should score 0, got %v", s)
	}
	oneSided := mm.BookTop{Bid: 0.4, HasBid: true}
	if s := spreadScore(oneSided, cfg); s != 0 {
		t.Fatalf("one-sided book should score 0, got %v", s)
	}
}

func TestConfidenceScore(t *testing.T) {
	if c := confidenceScore(mm.Fair{P: 0.5, Conf: 0.8}); c != 0.8 {
		t.Fatalf("mid-prob conf passthrough, got %v", c)
	}
	if c := confidenceScore(mm.Fair{P: 0, Conf: 0.8}); c != 0 {
		t.Fatalf("out-of-band prob should score 0, got %v", c)
	}
}

func TestConcentrationFactor(t *testing.T) {
	if p := concentrationFactor(0, 100); p != 1 {
		t.Fatalf("empty bucket → P=1, got %v", p)
	}
	if p := concentrationFactor(50, 100); p != 0.5 {
		t.Fatalf("half-full bucket → P=0.5, got %v", p)
	}
	if p := concentrationFactor(200, 100); p != 0 {
		t.Fatalf("over-full bucket → P=0, got %v", p)
	}
	if p := concentrationFactor(50, 0); p != 1 {
		t.Fatalf("no cap → P=1, got %v", p)
	}
}

func TestScoreCandidateEligibility(t *testing.T) {
	cfg := defSel()
	good := &Candidate{
		Volume24h: 50000,
		Book:      mm.BookTop{Bid: 0.40, Ask: 0.60, BidSz: 20000, AskSz: 20000, HasBid: true, HasAsk: true},
		Fair:      mm.Fair{P: 0.5, Conf: 0.9},
	}
	scoreCandidate(good, cfg, 0, 0)
	if !good.Eligible || good.Score <= 0 {
		t.Fatalf("healthy market should be eligible with positive score: %+v", good)
	}
	// Confidence floored out (extreme probability with low conf).
	bad := &Candidate{
		Volume24h: 50000,
		Book:      mm.BookTop{Bid: 0.40, Ask: 0.60, BidSz: 20000, AskSz: 20000, HasBid: true, HasAsk: true},
		Fair:      mm.Fair{P: 0.99, Conf: 0.0},
	}
	scoreCandidate(bad, cfg, 0, 0)
	if bad.Eligible || bad.Score != 0 {
		t.Fatalf("low-confidence market should be excluded, got %+v", bad)
	}
	// Concentration penalty shrinks the score of an otherwise-good market.
	pen := &Candidate{Volume24h: 50000, Book: good.Book, Fair: good.Fair}
	scoreCandidate(pen, cfg, 90, 100) // bucket 90% full → P=0.1
	if pen.Score >= good.Score {
		t.Fatalf("concentration penalty should shrink score: pen=%v good=%v", pen.Score, good.Score)
	}
}

// ---- scan ----

func mkt(coin, und, target string, outcome int, ttl time.Duration, now time.Time) core.Market {
	return core.Market{
		Coin: coin, Class: "outcome", IsOutcome: true, Outcome: outcome, Side: "Yes",
		ResolutionStatus: "open", Underlying: und, TargetPrice: target,
		Expiry: now.Add(ttl).UTC().Format(mm.ExpiryLayout),
	}
}

func TestScanFiltersAndSelects(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cfg := defSel()
	cfg.MaxActiveMarkets = 2
	cfg.MaxPerUnderlying = 0 // no diversification cap for this test

	btcYes := mkt("#100", "BTC", "76000", 10, 2*time.Hour, now)
	ethYes := mkt("#110", "ETH", "4000", 11, 2*time.Hour, now)
	dogeYes := mkt("#120", "DOGE", "1", 12, 2*time.Hour, now) // underlying not priceable
	settled := mkt("#130", "BTC", "76000", 13, 2*time.Hour, now)
	settled.ResolutionStatus = "settled"
	nearExpiry := mkt("#140", "BTC", "76000", 14, 5*time.Minute, now) // inside MinTTLMins (30)
	noLeg := mkt("#101", "BTC", "76000", 10, 2*time.Hour, now)
	noLeg.Side = "No"

	universe := []core.Market{btcYes, ethYes, dogeYes, settled, nearExpiry, noLeg}

	md := &fakeMD{
		vol: map[string]float64{"#100": 0.9, "#110": 0.9}, // ProbString caps at 0.99999; volume magnitude small but nonzero
		bbo: map[string]mm.BookTop{
			"#100": {Bid: 0.40, Ask: 0.60, BidSz: 0.9, AskSz: 0.9, HasBid: true, HasAsk: true},
			"#110": {Bid: 0.40, Ask: 0.60, BidSz: 0.9, AskSz: 0.9, HasBid: true, HasAsk: true},
		},
	}
	// Relax floors/refs so the tiny fake volumes still clear eligibility.
	cfg.VolSat = 1
	cfg.DepthRef = 0.1
	cfg.MinL = 0.01
	cfg.MinS = 0.01
	cfg.MinC = 0.01
	fv := &fakeFV{def: mm.Fair{P: 0.5, Conf: 0.9}}

	s := New(md, fv, cfg)
	sel, err := s.Scan(context.Background(), universe, Inputs{Now: now})
	if err != nil {
		t.Fatal(err)
	}

	// Active set: exactly the two priceable, open, in-band YES markets.
	if len(sel.Active) != 2 {
		t.Fatalf("want 2 active, got %d: %+v", len(sel.Active), activeCoins(sel))
	}
	got := map[string]bool{}
	for _, c := range sel.Active {
		got[c.Market.Coin] = true
	}
	if !got["#100"] || !got["#110"] {
		t.Fatalf("active set should be #100 and #110, got %v", activeCoins(sel))
	}

	// Pool carries exclude reasons for the operator panel.
	reason := map[string]string{}
	for _, c := range sel.Pool {
		reason[c.Market.Coin] = c.Reason
	}
	if reason["#120"] == "" || reason["#130"] == "" || reason["#140"] == "" {
		t.Fatalf("excluded markets must carry reasons: %v", reason)
	}
	if _, ok := reason["#101"]; ok {
		t.Fatalf("the NO leg should be skipped entirely, not pooled")
	}
}

// Pure event/categorical markets (no underlying/target) are not v1-priceable, so they
// must be dropped from the pool entirely rather than listed as excluded noise.
func TestScanSkipsEventMarkets(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cfg := defSel()
	cfg.VolSat, cfg.DepthRef = 1, 0.1
	cfg.MinL, cfg.MinS, cfg.MinC = 0.01, 0.01, 0.01

	priceBin := mkt("#100", "BTC", "76000", 10, 2*time.Hour, now)
	event := core.Market{Coin: "#200", Class: "outcome", IsOutcome: true, Outcome: 20, Side: "Yes", ResolutionStatus: "open"} // no underlying/target
	book := mm.BookTop{Bid: 0.40, Ask: 0.60, BidSz: 0.9, AskSz: 0.9, HasBid: true, HasAsk: true}
	md := &fakeMD{vol: map[string]float64{"#100": 0.9}, bbo: map[string]mm.BookTop{"#100": book}}
	fv := &fakeFV{def: mm.Fair{P: 0.5, Conf: 0.9}}
	s := New(md, fv, cfg)
	sel, _ := s.Scan(context.Background(), []core.Market{priceBin, event}, Inputs{Now: now})

	if len(sel.Pool) != 1 {
		t.Fatalf("pool should contain only the priceBinary (event markets dropped), got %d: %+v", len(sel.Pool), sel.Pool)
	}
	for _, c := range sel.Pool {
		if c.Market.Coin == "#200" {
			t.Fatal("event market must not appear in the pool")
		}
	}
}

func TestScanDiversificationCap(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cfg := defSel()
	cfg.MaxActiveMarkets = 5
	cfg.MaxPerUnderlying = 1 // at most one BTC market
	cfg.VolSat, cfg.DepthRef = 1, 0.1
	cfg.MinL, cfg.MinS, cfg.MinC = 0.01, 0.01, 0.01

	btc1 := mkt("#100", "BTC", "76000", 10, 2*time.Hour, now)
	btc2 := mkt("#110", "BTC", "80000", 11, 3*time.Hour, now)
	eth := mkt("#120", "ETH", "4000", 12, 2*time.Hour, now)
	universe := []core.Market{btc1, btc2, eth}

	book := mm.BookTop{Bid: 0.40, Ask: 0.60, BidSz: 0.9, AskSz: 0.9, HasBid: true, HasAsk: true}
	md := &fakeMD{
		vol: map[string]float64{"#100": 0.9, "#110": 0.9, "#120": 0.9},
		bbo: map[string]mm.BookTop{"#100": book, "#110": book, "#120": book},
	}
	fv := &fakeFV{def: mm.Fair{P: 0.5, Conf: 0.9}}
	s := New(md, fv, cfg)
	sel, _ := s.Scan(context.Background(), universe, Inputs{Now: now})

	btcCount := 0
	for _, c := range sel.Active {
		if c.Market.Underlying == "BTC" {
			btcCount++
		}
	}
	if btcCount != 1 {
		t.Fatalf("per-underlying cap 1 should admit exactly one BTC market, got %d (%v)", btcCount, activeCoins(sel))
	}
}

// Pins are operator overrides: they must make the active set even when they'd exceed
// the per-underlying diversification cap.
func TestPinsBypassDiversification(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cfg := defSel()
	cfg.MaxActiveMarkets = 5
	cfg.MaxPerUnderlying = 1 // would admit only one BTC market...
	cfg.Pins = []string{"#100", "#110"}
	cfg.VolSat, cfg.DepthRef = 1, 0.1
	cfg.MinL, cfg.MinS, cfg.MinC = 0.01, 0.01, 0.01

	btc1 := mkt("#100", "BTC", "76000", 10, 2*time.Hour, now)
	btc2 := mkt("#110", "BTC", "80000", 11, 3*time.Hour, now)
	book := mm.BookTop{Bid: 0.40, Ask: 0.60, BidSz: 0.9, AskSz: 0.9, HasBid: true, HasAsk: true}
	md := &fakeMD{
		vol: map[string]float64{"#100": 0.9, "#110": 0.9},
		bbo: map[string]mm.BookTop{"#100": book, "#110": book},
	}
	fv := &fakeFV{def: mm.Fair{P: 0.5, Conf: 0.9}}
	s := New(md, fv, cfg)
	sel, _ := s.Scan(context.Background(), []core.Market{btc1, btc2}, Inputs{Now: now})

	if len(sel.Active) != 2 {
		t.Fatalf("both pinned BTC markets should be active despite MaxPerUnderlying=1, got %d (%v)", len(sel.Active), activeCoins(sel))
	}
}

func activeCoins(s Selection) []string {
	var out []string
	for _, c := range s.Active {
		out = append(out, c.Market.Coin)
	}
	return out
}
