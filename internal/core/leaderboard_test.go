package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

const lbBody = `{"leaderboardRows":[
  {"ethAddress":"0xWHALE","accountValue":"1000000","displayName":"whale","prize":5,
   "windowPerformances":[
     ["day",{"pnl":"200","roi":"0.002","vlm":"5000000"}],
     ["week",{"pnl":"9000","roi":"0.05","vlm":"20000000"}],
     ["month",{"pnl":"40000","roi":"0.2","vlm":"90000000"}],
     ["allTime",{"pnl":"500000","roi":"2.0","vlm":"900000000"}]]},
  {"ethAddress":"0xGRINDER","accountValue":"50000","displayName":null,"prize":0,
   "windowPerformances":[
     ["day",{"pnl":"800","roi":"0.016","vlm":"300000"}],
     ["week",{"pnl":"3000","roi":"0.06","vlm":"1200000"}],
     ["month",{"pnl":"5000","roi":"0.1","vlm":"4000000"}],
     ["allTime",{"pnl":"20000","roi":"0.4","vlm":"30000000"}]]},
  {"ethAddress":"0xLOSER","accountValue":"3000","displayName":"rekt","prize":0,
   "windowPerformances":[
     ["day",{"pnl":"-500","roi":"-0.14","vlm":"100000"}],
     ["week",{"pnl":"-1200","roi":"-0.3","vlm":"500000"}],
     ["month",{"pnl":"900","roi":"0.3","vlm":"800000"}],
     ["allTime",{"pnl":"-4000","roi":"-0.5","vlm":"2000000"}]]}
]}`

// newLeaderboardClient wires a Client to an httptest server serving the leaderboard
// blob, with no real network / keychain.
func newLeaderboardClient(t *testing.T, body string) (*Client, context.Context) {
	t.Helper()
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := &Client{
		cfg:     config.Default(),
		network: "testnet",
		lbURL:   srv.URL,
		httpc:   &http.Client{},
		meta:    testMeta(),
	}
	return c, context.Background()
}

func TestLeaderboardSortAndShape(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)
	v, err := c.Leaderboard(ctx, LeaderboardParams{Window: "day", SortBy: "pnl", Order: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if v.TotalRows != 3 || v.Matched != 3 || v.Returned != 3 {
		t.Fatalf("counts wrong: %+v", v)
	}
	// Sort by day pnl desc: WHALE(200) ... wait GRINDER(800) > WHALE(200) > LOSER(-500).
	if v.Rows[0].Address != "0xGRINDER" || v.Rows[1].Address != "0xWHALE" || v.Rows[2].Address != "0xLOSER" {
		t.Fatalf("day-pnl-desc order wrong: %v", addrs(v.Rows))
	}
	if v.Rows[0].Rank != 1 || v.Rows[2].Rank != 3 {
		t.Fatalf("ranks wrong: %+v", v.Rows)
	}
	// All four windows present; roi_pct derived.
	if v.Rows[0].Month.Pnl != "5000" || v.Rows[0].Week.RoiPct != "6.00" {
		t.Fatalf("windows/roi_pct wrong: %+v", v.Rows[0])
	}
	if v.SortWindow != "day" || v.SortBy != "pnl" || v.Order != "desc" {
		t.Fatalf("echoed params wrong: %+v", v)
	}
}

func TestLeaderboardSortByAccountValueAsc(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)
	v, err := c.Leaderboard(ctx, LeaderboardParams{SortBy: "account_value", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Rows[0].Address != "0xLOSER" || v.Rows[2].Address != "0xWHALE" {
		t.Fatalf("account_value-asc order wrong: %v", addrs(v.Rows))
	}
}

func TestLeaderboardFilters(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)

	// --profitable on the week window drops 0xLOSER (week pnl -1200).
	v, err := c.Leaderboard(ctx, LeaderboardParams{Window: "week", Profitable: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 2 {
		t.Fatalf("week-profitable should match 2, got %d (%v)", v.Matched, addrs(v.Rows))
	}

	// --profitable-in day,week,month -> only GRINDER & WHALE are positive in all three.
	v, err = c.Leaderboard(ctx, LeaderboardParams{ProfitableIn: []string{"day", "week", "month"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 2 || !contains(addrs(v.Rows), "0xGRINDER") || contains(addrs(v.Rows), "0xLOSER") {
		t.Fatalf("profitable-in wrong: %v", addrs(v.Rows))
	}

	// --min-account-value 100000 keeps only the whale.
	min := 100000.0
	v, err = c.Leaderboard(ctx, LeaderboardParams{MinAccountValue: &min})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 1 || v.Rows[0].Address != "0xWHALE" {
		t.Fatalf("min-account-value wrong: %v", addrs(v.Rows))
	}

	// --named drops the anonymous grinder.
	v, err = c.Leaderboard(ctx, LeaderboardParams{Named: true})
	if err != nil {
		t.Fatal(err)
	}
	if contains(addrs(v.Rows), "0xGRINDER") {
		t.Fatalf("named should drop anonymous row: %v", addrs(v.Rows))
	}

	// min-vlm on day window as an activity floor.
	mv := 1000000.0
	v, err = c.Leaderboard(ctx, LeaderboardParams{Window: "day", MinVlm: &mv})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 1 || v.Rows[0].Address != "0xWHALE" {
		t.Fatalf("min-vlm wrong: %v", addrs(v.Rows))
	}
}

func TestLeaderboardDrilldownAndPaging(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)

	// Drill-down by address (case-insensitive).
	v, err := c.Leaderboard(ctx, LeaderboardParams{Addresses: []string{"0xwhale", "0xGRINDER"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 2 {
		t.Fatalf("address drill-down should match 2, got %d", v.Matched)
	}

	// Pagination: limit 1, offset 1 over day-pnl-desc -> the 2nd ranked (WHALE).
	v, err = c.Leaderboard(ctx, LeaderboardParams{Window: "day", SortBy: "pnl", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 3 || v.Returned != 1 || v.Rows[0].Address != "0xWHALE" || v.Rows[0].Rank != 2 {
		t.Fatalf("paging wrong: matched=%d returned=%d rows=%v rank=%d", v.Matched, v.Returned, addrs(v.Rows), rank0(v.Rows))
	}

	// Offset past the end yields an empty page, not an error.
	v, err = c.Leaderboard(ctx, LeaderboardParams{Offset: 99})
	if err != nil || v.Returned != 0 {
		t.Fatalf("over-offset should be empty: returned=%d err=%v", v.Returned, err)
	}
}

func TestLeaderboardBadParams(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)
	for _, p := range []LeaderboardParams{
		{Window: "fortnight"},
		{SortBy: "sharpe"},
		{Order: "sideways"},
		{Limit: -1},
		{Offset: -1},
		{ProfitableIn: []string{"day", "decade"}},
	} {
		if _, err := c.Leaderboard(ctx, p); err == nil {
			t.Fatalf("expected validation error for %+v", p)
		} else if oe := asOutputErr(err); oe == nil || oe.Category != output.CatValidation {
			t.Fatalf("want validation error for %+v, got %v", p, err)
		}
	}
}

func TestLeaderboardHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	testHome(t)
	c := &Client{cfg: config.Default(), network: "testnet", lbURL: srv.URL, httpc: &http.Client{}, meta: testMeta()}
	if _, err := c.Leaderboard(context.Background(), LeaderboardParams{}); err == nil {
		t.Fatal("503 should surface as an error")
	}
}

// --- helpers ---

func addrs(rows []LeaderEntry) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Address
	}
	return out
}

func rank0(rows []LeaderEntry) int {
	if len(rows) == 0 {
		return 0
	}
	return rows[0].Rank
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func asOutputErr(err error) *output.Error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	return nil
}

// Sort keys beyond the default (roi/vlm/prize) and a non-day window, to lock the
// entryMetric branches. Computed from lbBody month figures.
func TestLeaderboardSortKeys(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)
	// month ROI desc: LOSER(0.3) > WHALE(0.2) > GRINDER(0.1)
	v, err := c.Leaderboard(ctx, LeaderboardParams{Window: "month", SortBy: "roi", Order: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := addrs(v.Rows); got[0] != "0xLOSER" || got[2] != "0xGRINDER" {
		t.Fatalf("month-roi-desc wrong: %v", got)
	}
	// month VLM desc: WHALE(90M) > GRINDER(4M) > LOSER(800k)
	v, err = c.Leaderboard(ctx, LeaderboardParams{Window: "month", SortBy: "vlm", Order: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := addrs(v.Rows); got[0] != "0xWHALE" || got[2] != "0xLOSER" {
		t.Fatalf("month-vlm-desc wrong: %v", got)
	}
	// prize desc: WHALE(5) first; the two zero-prize rows tie-break by address.
	v, err = c.Leaderboard(ctx, LeaderboardParams{SortBy: "prize", Order: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := addrs(v.Rows); got[0] != "0xWHALE" || got[1] != "0xGRINDER" {
		t.Fatalf("prize-desc wrong: %v", got)
	}
}

// A row whose window metrics are non-finite ("NaN"/"Inf" — which ParseFloat
// accepts) must be normalized to 0: it can't slip past a numeric filter (NaN
// comparisons are always false) and its derived roi_pct must not render "NaN".
func TestLeaderboardNonFiniteNormalized(t *testing.T) {
	const body = `{"leaderboardRows":[
	  {"ethAddress":"0xNAN","accountValue":"NaN","prize":0,
	   "windowPerformances":[["day",{"pnl":"NaN","roi":"Inf","vlm":"NaN"}]]},
	  {"ethAddress":"0xOK","accountValue":"20000","prize":0,
	   "windowPerformances":[["day",{"pnl":"100","roi":"0.05","vlm":"5000"}]]}]}`
	c, ctx := newLeaderboardClient(t, body)

	v, err := c.Leaderboard(ctx, LeaderboardParams{Window: "day"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range v.Rows {
		if r.Address == "0xNAN" && r.Day.RoiPct != "0.00" {
			t.Fatalf("non-finite roi must derive roi_pct 0.00, got %q", r.Day.RoiPct)
		}
	}
	// A min-pnl floor must EXCLUDE the non-finite row (pnl treated as 0, not a
	// value that sneaks past the comparison).
	min := 50.0
	v, err = c.Leaderboard(ctx, LeaderboardParams{Window: "day", MinPnl: &min})
	if err != nil {
		t.Fatal(err)
	}
	if v.Matched != 1 || v.Rows[0].Address != "0xOK" {
		t.Fatalf("non-finite pnl row must not pass min-pnl: %v", addrs(v.Rows))
	}
}

// A 429 from the stats host maps to a retryable rate-limit error.
func TestLeaderboard429(t *testing.T) {
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{cfg: config.Default(), network: "testnet", lbURL: srv.URL, httpc: &http.Client{}, meta: testMeta()}
	_, err := c.Leaderboard(context.Background(), LeaderboardParams{})
	if oe := asOutputErr(err); oe == nil || oe.Category != output.CatRateLimit {
		t.Fatalf("429 should map to rate_limit, got %v", err)
	}
}

// A response body over the cap is rejected before parsing, so a hostile/buggy
// host cannot OOM the process.
func TestLeaderboardBodyCap(t *testing.T) {
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20) // 1 MiB
		for i := 0; i < (maxLeaderboardBytes>>20)+1; i++ {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()
	c := &Client{cfg: config.Default(), network: "testnet", lbURL: srv.URL, httpc: &http.Client{}, meta: testMeta()}
	if _, err := c.Leaderboard(context.Background(), LeaderboardParams{}); err == nil {
		t.Fatal("oversized leaderboard body should be rejected")
	}
}

// --- Phase 1: caching + conditional GET ---

// Within the TTL the board is served from the on-disk cache with zero network hits.
func TestLeaderboardCacheTTLHit(t *testing.T) {
	testHome(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, lbBody)
	}))
	defer srv.Close()
	c := &Client{cfg: config.Default(), network: "testnet", lbURL: srv.URL, httpc: &http.Client{}, meta: testMeta()}
	for i := 0; i < 3; i++ {
		if _, err := c.Leaderboard(context.Background(), LeaderboardParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("TTL cache should fetch the board once, got %d server hits", got)
	}
}

// With the TTL disabled, each call revalidates; an unchanged board returns a 304 and
// reuses the cached body instead of re-downloading it, and the data stays correct.
func TestLeaderboardConditionalGET(t *testing.T) {
	testHome(t)
	var full, notModified int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&notModified, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		atomic.AddInt32(&full, 1)
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, lbBody)
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.State.LeaderboardTTLSecs = 0 // always revalidate
	c := &Client{cfg: cfg, network: "testnet", lbURL: srv.URL, httpc: &http.Client{}, meta: testMeta()}
	for i := 0; i < 3; i++ {
		v, err := c.Leaderboard(context.Background(), LeaderboardParams{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if v.TotalRows != 3 {
			t.Fatalf("call %d: want 3 rows from cache, got %d", i, v.TotalRows)
		}
	}
	if full != 1 || notModified != 2 {
		t.Fatalf("want 1 full download + 2 conditional 304s, got full=%d notModified=%d", full, notModified)
	}
}

// --- Phase 2: live enrichment + filters ---

var liveStates = map[string]string{
	"0xWHALE":   `{"marginSummary":{"accountValue":"30000"},"assetPositions":[{"position":{"coin":"BTC","szi":"0.5","positionValue":"30000","leverage":{"type":"isolated","value":40}}}]}`,
	"0xGRINDER": `{"marginSummary":{"accountValue":"8000"},"assetPositions":[{"position":{"coin":"ETH","szi":"2","positionValue":"8000","leverage":{"type":"cross","value":5}}}]}`,
	"0xLOSER":   `{"marginSummary":{"accountValue":"5000"},"assetPositions":[]}`,
}

// newLiveLeaderboardClient serves the board on GET and per-address clearinghouse
// state on POST /info, with c.info wired so enrichment works. No real network.
func newLiveLeaderboardClient(t *testing.T, board string, states map[string]string) (*Client, context.Context) {
	t.Helper()
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, board)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		user, _ := body["user"].(string)
		if s, ok := states[user]; ok {
			_, _ = io.WriteString(w, s)
			return
		}
		_, _ = io.WriteString(w, `{"marginSummary":{"accountValue":"0"},"assetPositions":[]}`)
	}))
	t.Cleanup(srv.Close)
	meta := testMeta()
	ctx := context.Background()
	c := &Client{
		cfg: config.Default(), network: "testnet",
		lbURL: srv.URL, infoURL: srv.URL, httpc: &http.Client{},
		meta: meta, info: hl.NewInfo(ctx, srv.URL, true, meta.Meta(), meta.SpotMeta(), nil),
	}
	return c, ctx
}

func TestLeaderboardLiveAnnotate(t *testing.T) {
	c, ctx := newLiveLeaderboardClient(t, lbBody, liveStates)
	v, err := c.Leaderboard(ctx, LeaderboardParams{Live: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if v.LiveScanned != 3 {
		t.Fatalf("want 3 rows enriched, got %d", v.LiveScanned)
	}
	by := map[string]*LiveInfo{}
	for _, r := range v.Rows {
		by[r.Address] = r.Live
	}
	if by["0xWHALE"] == nil || by["0xWHALE"].MaxLeverage != 40 || by["0xWHALE"].OpenPositions != 1 {
		t.Fatalf("whale live wrong: %+v", by["0xWHALE"])
	}
	if by["0xLOSER"] == nil || by["0xLOSER"].OpenPositions != 0 {
		t.Fatalf("loser should be live-flat: %+v", by["0xLOSER"])
	}
}

func TestLeaderboardLiveFilters(t *testing.T) {
	c, ctx := newLiveLeaderboardClient(t, lbBody, liveStates)

	// --in-market drops the flat LOSER.
	v, err := c.Leaderboard(ctx, LeaderboardParams{InMarket: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if v.Returned != 2 || contains(addrs(v.Rows), "0xLOSER") {
		t.Fatalf("in-market wrong: %v", addrs(v.Rows))
	}

	// --flat keeps only the one in cash.
	v, err = c.Leaderboard(ctx, LeaderboardParams{Flat: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if v.Returned != 1 || v.Rows[0].Address != "0xLOSER" {
		t.Fatalf("flat wrong: %v", addrs(v.Rows))
	}

	// --max-live-leverage 10 drops the 40x whale.
	ml := 10.0
	v, err = c.Leaderboard(ctx, LeaderboardParams{MaxLiveLeverage: &ml, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if contains(addrs(v.Rows), "0xWHALE") {
		t.Fatalf("max-live-leverage should drop the 40x whale: %v", addrs(v.Rows))
	}

	// --min-live-equity uses LIVE equity: GRINDER's board account_value is 50000 but
	// its live equity is 8000, so a 10000 floor drops it and keeps only the whale.
	min := 10000.0
	v, err = c.Leaderboard(ctx, LeaderboardParams{MinLiveEquity: &min, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if v.Returned != 1 || v.Rows[0].Address != "0xWHALE" {
		t.Fatalf("min-live-equity (live, not stale board value) wrong: %v", addrs(v.Rows))
	}
}

func TestLeaderboardLiveScanCap(t *testing.T) {
	c, ctx := newLiveLeaderboardClient(t, lbBody, liveStates)
	v, err := c.Leaderboard(ctx, LeaderboardParams{Live: true, LiveScan: 2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if v.LiveScanned != 2 {
		t.Fatalf("live-scan cap should enrich 2, got %d", v.LiveScanned)
	}
	enriched := 0
	for _, r := range v.Rows {
		if r.Live != nil {
			enriched++
		}
	}
	if enriched != 2 {
		t.Fatalf("want exactly 2 enriched rows, got %d", enriched)
	}
}

func TestLeaderboardLiveValidation(t *testing.T) {
	c, ctx := newLeaderboardClient(t, lbBody)
	if _, err := c.Leaderboard(ctx, LeaderboardParams{Live: true, Limit: 0}); err == nil {
		t.Fatal("--live with --limit 0 should error (enrichment must be bounded)")
	}
	if _, err := c.Leaderboard(ctx, LeaderboardParams{InMarket: true, Flat: true, Limit: 5}); err == nil {
		t.Fatal("--in-market + --flat should error (mutually exclusive)")
	}
}
