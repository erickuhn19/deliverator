package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// newSignerInfo builds an hl.Info the way the signing Exchange does, with metas
// supplied so nothing is fetched.
func newSignerInfo() *hl.Info {
	return hl.NewInfo(context.Background(), "", true, &hl.Meta{}, &hl.SpotMeta{}, nil)
}

// Regression tests for #43: the reactive outcome refresh added by #37 duplicated
// the universe on every reload, left rolled-out coins resolvable, and raced every
// concurrent Lookup under `serve`.

// rollUniverse builds a one-question priceBucket universe like mainnet's daily
// BTC range market, shaped after a live outcomeMeta response.
func rollUniverse(base int) *hl.OutcomeMeta {
	sides := []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}
	return &hl.OutcomeMeta{
		Outcomes: []hl.OutcomeInfo{
			{Outcome: base, Name: "Recurring Fallback", Description: "other", SideSpecs: sides, QuoteToken: "USDC"},
			{Outcome: base + 1, Name: "Recurring Named Outcome", Description: "index:0", SideSpecs: sides, QuoteToken: "USDC"},
			{Outcome: base + 2, Name: "Recurring Named Outcome", Description: "index:1", SideSpecs: sides, QuoteToken: "USDC"},
		},
		Questions: []hl.OutcomeQuestion{{
			Question: base, Name: "Recurring",
			Description:     "class:priceBucket|underlying:BTC|expiry:20260805-0600|priceThresholds:62506,65057|period:1d",
			FallbackOutcome: base,
			NamedOutcomes:   []int{base + 1, base + 2},
		}},
	}
}

// A reload must INSTALL the universe, not append it. #37 made RefreshOutcomes
// fire on every unknown coin, and each call re-appended every row: a server
// observed 2, 4, 6, 8, 10 markets over five refreshes of a 1-outcome universe.
func TestAddOutcomesIsIdempotent(t *testing.T) {
	ms := testMeta()
	const wantMarkets = 6 // 3 outcomes x (Yes, No)
	for i := 1; i <= 5; i++ {
		ms.AddOutcomes(rollUniverse(1000))
		if got := len(ms.OutcomeMarkets()); got != wantMarkets {
			t.Fatalf("refresh #%d: OutcomeMarkets() = %d, want %d — the universe is being appended, not replaced", i, got, wantMarkets)
		}
	}
}

// A coin that rolled out must stop resolving. Leaving it in byCoin lets an order
// price against a market that no longer trades, and the stale asset id would sign
// for the wrong leaf.
func TestAddOutcomesRetiresRolledOutCoins(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))
	if _, ok := ms.Lookup("#10010"); !ok {
		t.Fatalf("day-1 coin #10010 should resolve after the first load")
	}

	ms.AddOutcomes(rollUniverse(2000)) // the next day's universe
	if _, ok := ms.Lookup("#10010"); ok {
		t.Errorf("#10010 rolled out but still resolves — a retired coin must not be tradable")
	}
	if _, ok := ms.Lookup("#20010"); !ok {
		t.Errorf("#20010 is the rolled-to coin and must resolve")
	}
	if got := len(ms.OutcomeMarkets()); got != 6 {
		t.Errorf("OutcomeMarkets() = %d, want 6 after the roll", got)
	}
}

// The shape that crashes a server: serve runs one goroutine per connection and
// reloads the universe on the daily roll, so AddOutcomes writes byCoin while
// other connections read it. In Go a concurrent map read+write is a FATAL error,
// not a recoverable panic — it kills the process holding the signing socket.
// Run under -race (CI always does).
func TestMetaStoreConcurrentRefreshAndLookup(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))

	var wg sync.WaitGroup
	const iters = 500
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ms.AddOutcomes(rollUniverse(1000))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = ms.Lookup("#10010")
			_, _ = ms.Lookup("BTC")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = ms.OutcomeMarkets()
			_ = ms.Markets()
			_ = ms.OutcomeMeta()
		}
	}()
	wg.Wait()
}

// MaintenanceMarginFraction and SpotPairForToken both need a lookup while already
// holding the read lock. sync.RWMutex is not reentrant, so calling the exported
// Lookup from inside them deadlocks the moment a writer queues between the two
// RLocks. These must complete rather than hang.
func TestMetaStoreNoReentrantLockDeadlock(t *testing.T) {
	ms := testMeta()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				ms.AddOutcomes(rollUniverse(1000)) // queue writers between the RLocks
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				_ = ms.MaintenanceMarginFraction("BTC", 10_000)
				_, _ = ms.SpotPairForToken("HYPE")
			}
		}()
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: a reentrant RLock in MaintenanceMarginFraction/SpotPairForToken")
	}
}

// THE REGRESSION THAT REOPENED THE OUTAGE. RefreshOutcomes must also correct the
// SIGNER's asset ids, not just the read paths.
//
// The signer carries its own hl.Info, seeded exactly once in exchange() on the
// process's first write. #37 refreshed c.meta and c.info but never c.ex.Info(),
// so after a roll a placement resolved through the gates, got SIGNED, and then
// died in newCreateOrderAction with "coin #<enc> not found in info" — mapped to
// exit 50 (exchange-rejected, NON-retryable), which serve's unknown_coin retry
// cannot recover. That reproduced the very outage #37 was filed to fix.
//
// This drives the real RefreshOutcomes path against a server that rolls the
// universe between calls. It FAILS on the unfixed tree.
func TestRefreshOutcomesTeachesTheSignerTheRolledToCoin(t *testing.T) {
	day := 1000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Type != "outcomeMeta" {
			serveInfo(w, body.Type)
			return
		}
		_ = json.NewEncoder(w).Encode(rollUniverse(day))
	}))
	defer srv.Close()

	c, ctx := newClientAt(t, &config.Config{}, Options{}, srv.URL)

	// Day 1: the signer learns the universe exactly as exchange() would.
	if err := c.RefreshOutcomes(ctx); err != nil {
		t.Fatalf("day-1 RefreshOutcomes: %v", err)
	}
	c.ex.Info().RegisterOutcomes(c.meta.OutcomeMeta()) // what exchange() does on the first write
	if _, ok := c.ex.Info().CoinToAsset("#10010"); !ok {
		t.Fatalf("day-1 coin must resolve on the signer")
	}

	// The daily roll: yesterday's coins retire, today's are listed.
	day = 2000
	if err := c.RefreshOutcomes(ctx); err != nil {
		t.Fatalf("post-roll RefreshOutcomes: %v", err)
	}

	if _, ok := c.meta.Lookup("#20010"); !ok {
		t.Fatalf("the rolled-to coin must resolve in the meta store (the gate path)")
	}
	asset, ok := c.ex.Info().CoinToAsset("#20010")
	if !ok {
		t.Fatal("rolled-to coin does not resolve on the SIGNER after a refresh: " +
			"the order would pass the gates, get signed, then be rejected exit 50 (non-retryable)")
	}
	if want := hl.OutcomeAsset(2001, 0); asset != want {
		t.Errorf("signer asset id = %d, want %d — a wrong id signs for the wrong market", asset, want)
	}
}

// The signer's coin->asset maps are written by RegisterOutcomes on the roll while
// order construction reads them to stamp the asset id. Same fatal-crash class as
// MetaStore, but on the signing path.
func TestInfoConcurrentRegisterAndResolve(t *testing.T) {
	info := newSignerInfo()
	info.RegisterOutcomes(rollUniverse(1000))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			info.RegisterOutcomes(rollUniverse(1000))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = info.CoinToAsset("#10010")
			_ = info.AssetDecimals(hl.OutcomeAsset(1001, 0))
		}
	}()
	wg.Wait()
}

// A priceBucket leg still renders through the legacy path today (#42 covers the
// title synthesis); this pins that the reload rework did not change it.
func TestRolledUniverseStillResolvesBothSides(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))
	yes, ok := ms.Lookup("#10010")
	if !ok || yes.Side != "Yes" {
		t.Fatalf("#10010 should be the Yes leg, got %+v ok=%v", yes, ok)
	}
	no, ok := ms.Lookup("#10011")
	if !ok || no.Side != "No" {
		t.Fatalf("#10011 should be the No leg, got %+v ok=%v", no, ok)
	}
	if !strings.EqualFold(yes.Class, "outcome") {
		t.Errorf("class = %q, want outcome", yes.Class)
	}
}
