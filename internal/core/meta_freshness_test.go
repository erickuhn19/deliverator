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

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// Tests for #38: state.meta_ttl_secs was evaluated exactly once, in New. For a
// fork-per-command CLI that is the same question as "is it fresh now"; a `serve`
// process holds the answer for days, and a stale szDecimals does not fail loudly
// — it mis-rounds a SIGNED order.

func metaWith(coin string, szDecimals int) *hl.Meta {
	return &hl.Meta{Universe: []hl.AssetInfo{{Name: coin, SzDecimals: szDecimals, MaxLeverage: 20}}}
}

// A REFRESH MUST NOT DESTROY A WORKING UNIVERSE. This is the difference between
// construction (where nil-spot is simply the initial state and "spot is optional"
// is correct) and use (where nil-spot would delete every spot market on a
// transient /info hiccup and turn each into an unknown_coin).
func TestRefreshKeepsSpotWhenTheSpotFetchFails(t *testing.T) {
	spot := &hl.SpotMeta{
		Universe: []hl.SpotAssetInfo{{Name: "PURR/USDC", Index: 0, Tokens: []int{1, 0}}},
		Tokens:   []hl.SpotTokenInfo{{Name: "PURR", Index: 1, SzDecimals: 2}, {Name: "USDC", Index: 0, SzDecimals: 2}},
	}
	ms := NewMetaStore("mainnet", metaWith("BTC", 5), spot, time.Now())
	if _, ok := ms.Lookup("PURR/USDC"); !ok {
		t.Fatal("precondition: the spot pair should resolve")
	}

	// Perp meta refreshed fine; the separate spot fetch failed and returned nil.
	if err := ms.Refresh(metaWith("BTC", 4), nil, time.Now()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := ms.Lookup("PURR/USDC"); !ok {
		t.Error("a failed spot fetch DELETED the working spot universe — every spot coin becomes unknown_coin")
	}
	mk, _ := ms.Lookup("BTC")
	if mk.SzDecimals != 4 {
		t.Errorf("perp szDecimals = %d, want the refreshed 4", mk.SzDecimals)
	}
}

// A nil/empty perp meta is the whole universe; refuse it outright rather than
// installing nothing.
func TestRefreshRefusesAnEmptyPerpMeta(t *testing.T) {
	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now())
	for _, m := range []*hl.Meta{nil, {}} {
		if err := ms.Refresh(m, nil, time.Now()); err == nil {
			t.Errorf("Refresh(%v) should be refused", m)
		}
	}
	if _, ok := ms.Lookup("BTC"); !ok {
		t.Error("the previous universe must survive a refused refresh")
	}
}

// THE ACTUAL BUG: a changed szDecimals must reach the rounding path.
func TestRefreshPropagatesChangedSzDecimals(t *testing.T) {
	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now())
	if mk, _ := ms.Lookup("BTC"); mk.SzDecimals != 5 {
		t.Fatalf("precondition: szDecimals should start at 5, got %d", mk.SzDecimals)
	}
	if err := ms.Refresh(metaWith("BTC", 3), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	mk, _ := ms.Lookup("BTC")
	if mk.SzDecimals != 3 {
		t.Errorf("szDecimals = %d after refresh, want 3 — a stale value mis-rounds a SIGNED order", mk.SzDecimals)
	}
	if mk.PxDecimals != MaxDecimalsPerp-3 {
		t.Errorf("PxDecimals = %d, want it recomputed from the new szDecimals", mk.PxDecimals)
	}
}

// A delisted coin must actually disappear rather than linger from the old build.
func TestRefreshDropsCoinsThatLeftTheUniverse(t *testing.T) {
	ms := NewMetaStore("mainnet", &hl.Meta{Universe: []hl.AssetInfo{
		{Name: "BTC", SzDecimals: 5}, {Name: "DOOMED", SzDecimals: 2},
	}}, nil, time.Now())
	if _, ok := ms.Lookup("DOOMED"); !ok {
		t.Fatal("precondition")
	}
	if err := ms.Refresh(metaWith("BTC", 5), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ms.Lookup("DOOMED"); ok {
		t.Error("a coin that left the universe still resolves after a refresh")
	}
}

// Refresh must re-apply the HIP-4 and sub-dex universes layered on top, and must
// not double-count them (the #43 duplication shape).
func TestRefreshPreservesLayeredUniversesExactlyOnce(t *testing.T) {
	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now())
	ms.AddPerpDex(1, metaWith("xyz:BRENTOIL", 2))
	ms.AddOutcomes(rollUniverse(1000))

	beforeOutcomes := len(ms.OutcomeMarkets())
	beforeDexes := len(ms.PerpDexEntries())

	for i := 0; i < 3; i++ {
		if err := ms.Refresh(metaWith("BTC", 5), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := ms.Lookup("#10010"); !ok {
		t.Error("the HIP-4 universe did not survive a base-meta refresh")
	}
	if _, ok := ms.Lookup("xyz:BRENTOIL"); !ok {
		t.Error("the sub-dex universe did not survive a base-meta refresh")
	}
	if got := len(ms.OutcomeMarkets()); got != beforeOutcomes {
		t.Errorf("outcome markets = %d after 3 refreshes, want %d — re-application is duplicating", got, beforeOutcomes)
	}
	if got := len(ms.PerpDexEntries()); got != beforeDexes {
		t.Errorf("sub-dex entries = %d after 3 refreshes, want %d — re-application is duplicating", got, beforeDexes)
	}
}

// Refresh races the same readers AddOutcomes does. Run under -race.
func TestRefreshIsConcurrencySafe(t *testing.T) {
	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now())
	ms.AddOutcomes(rollUniverse(1000))
	done := make(chan struct{}, 3)
	go func() {
		for i := 0; i < 300; i++ {
			_ = ms.Refresh(metaWith("BTC", 5), nil, time.Now())
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 300; i++ {
			_, _ = ms.Lookup("BTC")
			_, _ = ms.Lookup("#10010")
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 300; i++ {
			_ = ms.Markets()
			_ = ms.MaintenanceMarginFraction("BTC", 1000)
		}
		done <- struct{}{}
	}()
	for i := 0; i < 3; i++ {
		<-done
	}
}

// REFRESH IS DISABLED FOR HAND-BUILT CLIENTS. Nearly every core test fixture is a
// struct literal that bypasses New, so metaTTL is 0 there. That must mean "never
// refresh" — a unit test silently reaching for a live endpoint would be a network
// test wearing a unit test's clothes.
func TestEnsureMetaFreshIsANoOpWithoutATTL(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now().Add(-72*time.Hour))
	c := &Client{meta: ms, info: hl.NewInfo(context.Background(), srv.URL, true, ms.Meta(), &hl.SpotMeta{}, nil)}
	// metaTTL is the zero value: refresh disabled.
	c.ensureMetaFresh(context.Background())
	if hits != 0 {
		t.Errorf("a client with no TTL made %d network calls; refresh must be disabled", hits)
	}
}

// Inside the TTL, nothing is fetched — this runs on every signing call.
func TestEnsureMetaFreshDoesNothingWhileFresh(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now())
	c := &Client{meta: ms, metaTTL: time.Hour,
		info: hl.NewInfo(context.Background(), srv.URL, true, ms.Meta(), &hl.SpotMeta{}, nil)}
	for i := 0; i < 5; i++ {
		c.ensureMetaFresh(context.Background())
	}
	if hits != 0 {
		t.Errorf("fresh meta triggered %d fetches, want 0", hits)
	}
}

// Past the TTL it refreshes, and the new szDecimals reaches the lookup.
func TestEnsureMetaFreshRefetchesWhenStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Type {
		case "meta":
			_ = json.NewEncoder(w).Encode(metaWith("BTC", 3))
		case "spotMeta":
			_ = json.NewEncoder(w).Encode(&hl.SpotMeta{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now().Add(-2*time.Hour))
	c := &Client{meta: ms, metaTTL: time.Hour,
		info: hl.NewInfo(context.Background(), srv.URL, true, ms.Meta(), &hl.SpotMeta{}, nil)}

	c.ensureMetaFresh(context.Background())

	mk, ok := ms.Lookup("BTC")
	if !ok {
		t.Fatal("BTC should still resolve")
	}
	if mk.SzDecimals != 3 {
		t.Errorf("szDecimals = %d, want the refreshed 3 — the TTL is still being honoured only at construction", mk.SzDecimals)
	}
	if ms.Age() > time.Minute {
		t.Errorf("fetchedAt was not advanced; age = %v", ms.Age())
	}
}

// A FAILING REFRESH MUST NOT BLOCK TRADING, must not hammer the endpoint, and
// must not be silent. Refusing to trade because metadata could not be re-fetched
// converts a read problem into an outage; being quiet about it lets a stale
// szDecimals round a signed order with nothing in the envelope to say so.
func TestFailingRefreshKeepsTradingButWarnsAndBacksOff(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now().Add(-2*time.Hour))
	c := &Client{meta: ms, metaTTL: time.Hour,
		info: hl.NewInfo(context.Background(), srv.URL, true, ms.Meta(), &hl.SpotMeta{}, nil)}

	for i := 0; i < 10; i++ {
		c.ensureMetaFresh(context.Background())
	}
	// The previous universe still works.
	if _, ok := ms.Lookup("BTC"); !ok {
		t.Error("a failed refresh must leave the working universe in place, not block trading")
	}
	// Backoff: one attempt, not ten.
	if hits > 2 {
		t.Errorf("a failing endpoint was hit %d times across 10 calls; the retry floor is not holding", hits)
	}
	// And it is visible in the write envelope.
	w := c.metaStaleWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "stale szDecimals") {
		t.Errorf("a stale universe with a failing refresh must warn on writes, got %q", w)
	}
	if got := c.writeWarnings(); len(got) == 0 {
		t.Error("writeWarnings must carry the stale-meta warning to the result envelope")
	}
}

// Once the refresh succeeds the warning clears.
func TestStaleWarningClearsAfterASuccessfulRefresh(t *testing.T) {
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Type {
		case "meta":
			_ = json.NewEncoder(w).Encode(metaWith("BTC", 4))
		default:
			_ = json.NewEncoder(w).Encode(&hl.SpotMeta{})
		}
	}))
	defer srv.Close()

	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now().Add(-2*time.Hour))
	c := &Client{meta: ms, metaTTL: time.Hour,
		info: hl.NewInfo(context.Background(), srv.URL, true, ms.Meta(), &hl.SpotMeta{}, nil)}

	c.ensureMetaFresh(context.Background())
	if len(c.metaStaleWarnings()) != 1 {
		t.Fatal("precondition: the failing refresh should warn")
	}
	fail = false
	c.metaAttempt = time.Time{} // clear the backoff, as elapsed time would
	c.ensureMetaFresh(context.Background())
	if got := c.metaStaleWarnings(); len(got) != 0 {
		t.Errorf("warning persisted after a successful refresh: %q", got)
	}
}

// Several connection goroutines hitting the same expiry must produce ONE fetch,
// not one per caller — serve handles connections concurrently.
func TestConcurrentEnsureMetaFreshFetchesOnce(t *testing.T) {
	hits := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		var body struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Type {
		case "meta":
			_ = json.NewEncoder(w).Encode(metaWith("BTC", 3))
		default:
			_ = json.NewEncoder(w).Encode(&hl.SpotMeta{})
		}
	}))
	defer srv.Close()

	ms := NewMetaStore("mainnet", metaWith("BTC", 5), nil, time.Now().Add(-2*time.Hour))
	c := &Client{meta: ms, metaTTL: time.Hour,
		info: hl.NewInfo(context.Background(), srv.URL, true, ms.Meta(), &hl.SpotMeta{}, nil)}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.ensureMetaFresh(context.Background()) }()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// meta + spotMeta for the single winning refresh.
	if hits > 2 {
		t.Errorf("%d fetches for one expiry; concurrent callers are stampeding the endpoint", hits)
	}
}
