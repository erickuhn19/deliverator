package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// leaderboardURLFor maps a network to its public leaderboard source.
func leaderboardURLFor(network string) string {
	if network == config.NetworkMainnet {
		return hl.MainnetLeaderboardURL
	}
	return hl.TestnetLeaderboardURL
}

// Sort keys for the leaderboard.
const (
	LeaderSortPnl          = "pnl"
	LeaderSortRoi          = "roi"
	LeaderSortVlm          = "vlm"
	LeaderSortAccountValue = "account_value"
	LeaderSortPrize        = "prize"
)

// LeaderboardParams is the full filter/sort/drill-down spec for a leaderboard read.
// All numeric bounds are nil when unset. Window selects which trading window the
// sort and the pnl/roi/vlm filters operate on; every window is still returned per
// row so the agent sees day/week/month/allTime at once.
type LeaderboardParams struct {
	Window string // day | week | month | allTime (sort + metric-filter basis)
	SortBy string // pnl | roi | vlm | account_value | prize
	Order  string // desc | asc
	Limit  int    // max rows after filtering+sorting; 0 = all
	Offset int    // skip this many ranked rows (pagination)

	Addresses    []string // drill-down: keep only these addresses (case-insensitive)
	Named        bool     // keep only rows with a public display name
	Profitable   bool     // keep only rows with window pnl > 0 AND roi > 0
	ProfitableIn []string // keep only rows with pnl > 0 in EACH listed window (consistency)

	MinAccountValue, MaxAccountValue *float64
	MinPnl, MaxPnl                   *float64 // on the selected window (USD)
	MinRoi, MaxRoi                   *float64 // on the selected window (fraction: 0.1 = 10%)
	MinVlm, MaxVlm                   *float64 // on the selected window (USD)
	MinPrize                         *float64

	// Live enrichment: fetch each RETURNED row's CURRENT clearinghouse state (one
	// read per address, bounded by LiveScan). The board's window aggregates and its
	// account_value are lagged; this reflects what the trader holds right now. Any
	// live filter below implies Live.
	Live     bool
	LiveScan int  // max addresses to enrich; <=0 -> defaultLiveScan, capped at maxLiveScan
	InMarket bool // keep only addresses holding >=1 position now
	Flat     bool // keep only addresses currently in cash (no open positions)

	MinLiveEquity, MaxLiveEquity *float64 // on CURRENT equity (USD); fixes the stale board value
	MaxLiveLeverage              *float64 // drop addresses whose current max leverage exceeds this
}

// WindowPerfView is one window's performance, carrying the exchange's strings plus
// roi rendered as a percent for human-friendliness.
type WindowPerfView struct {
	Pnl    string `json:"pnl"`
	Roi    string `json:"roi"`     // fraction, as sent by HL (0.1234 = 12.34%)
	RoiPct string `json:"roi_pct"` // roi * 100, 2dp (e.g. "12.34")
	Vlm    string `json:"vlm"`
}

// LiveInfo is an address's CURRENT on-chain perp state, fetched on demand when
// --live is requested. Unlike the board's window aggregates and its lagged
// account_value, this reflects what the trader holds right now.
type LiveInfo struct {
	Equity        string   `json:"equity"`         // current perp account value (USD)
	OpenPositions int      `json:"open_positions"` // count of non-zero positions held now
	MaxLeverage   int      `json:"max_leverage"`   // highest leverage across open positions
	Coins         []string `json:"coins,omitempty"`
}

// LeaderEntry is one ranked address with every window's performance.
type LeaderEntry struct {
	Rank         int            `json:"rank"` // 1-based rank within the matched+sorted set (pre-offset)
	Address      string         `json:"address"`
	DisplayName  string         `json:"display_name,omitempty"`
	AccountValue string         `json:"account_value"`
	Prize        string         `json:"prize,omitempty"`
	Day          WindowPerfView `json:"day"`
	Week         WindowPerfView `json:"week"`
	Month        WindowPerfView `json:"month"`
	AllTime      WindowPerfView `json:"all_time"`
	Live         *LiveInfo      `json:"live,omitempty"` // present only when --live enriched this row
}

// LeaderboardView is the result envelope payload for `leaderboard`.
type LeaderboardView struct {
	Network     string        `json:"network"`
	Source      string        `json:"source"`      // the URL the data came from
	SortWindow  string        `json:"sort_window"` // normalized window used for sort/filters
	SortBy      string        `json:"sort_by"`
	Order       string        `json:"order"`
	TotalRows   int           `json:"total_rows"` // rows the endpoint returned
	Matched     int           `json:"matched"`    // rows surviving the cheap filters
	Returned    int           `json:"returned"`   // rows in this page (after offset+limit and any live filter)
	Offset      int           `json:"offset"`
	LiveScanned int           `json:"live_scanned,omitempty"` // rows enriched with live state (when --live)
	Rows        []LeaderEntry `json:"rows"`
}

// normalizeWindow accepts the wire name plus common aliases (alltime, all_time,
// all-time, d/w/m, daily/weekly/monthly) and returns the canonical wire name.
func normalizeWindow(w string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(w)) {
	case "", "day", "d", "daily", "1d", "24h":
		return hl.LeaderboardWindowDay, true
	case "week", "w", "weekly", "7d":
		return hl.LeaderboardWindowWeek, true
	case "month", "m", "monthly", "30d":
		return hl.LeaderboardWindowMonth, true
	case "alltime", "all_time", "all-time", "all", "a":
		return hl.LeaderboardWindowAllTime, true
	}
	return "", false
}

func normalizeSort(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "pnl":
		return LeaderSortPnl, true
	case "roi":
		return LeaderSortRoi, true
	case "vlm", "volume":
		return LeaderSortVlm, true
	case "account_value", "account-value", "equity", "av":
		return LeaderSortAccountValue, true
	case "prize":
		return LeaderSortPrize, true
	}
	return "", false
}

func normalizeOrder(o string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(o)) {
	case "", "desc", "descending", "high", "top":
		return "desc", true
	case "asc", "ascending", "low", "bottom":
		return "asc", true
	}
	return "", false
}

const (
	// maxLeaderboardBytes caps the leaderboard response body so a hostile, buggy,
	// or MITM'd stats host cannot OOM this one-shot process. The live mainnet board
	// is already ~31 MiB and growing, so the cap has real headroom at 64 MiB.
	maxLeaderboardBytes = 64 << 20 // 64 MiB
	// defaultLiveScan / maxLiveScan bound how many returned rows get enriched with a
	// live clearinghouseState read (one network call each) — enrichment is never a
	// fan-out over the whole ~39k-row board.
	defaultLiveScan = 25
	maxLiveScan     = 100
)

// parseLeaderFloat parses an exchange numeric string; missing/garbage/non-finite
// (NaN/Inf, which strconv.ParseFloat accepts) -> 0, so a poisoned row can never
// slip a NaN past the min/max filters (NaN comparisons are always false) or
// render "NaN"/"+Inf" into the JSON envelope.
func parseLeaderFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// Leaderboard fetches the public trader leaderboard and applies the filter/sort/
// pagination spec. It is a pure read: side-effect free and safe to retry.
func (c *Client) Leaderboard(ctx context.Context, p LeaderboardParams) (*LeaderboardView, error) {
	window, ok := normalizeWindow(p.Window)
	if !ok {
		return nil, output.Validation("bad_window", fmt.Sprintf("window must be day|week|month|allTime, got %q", p.Window))
	}
	sortBy, ok := normalizeSort(p.SortBy)
	if !ok {
		return nil, output.Validation("bad_sort", fmt.Sprintf("sort must be pnl|roi|vlm|account_value|prize, got %q", p.SortBy))
	}
	order, ok := normalizeOrder(p.Order)
	if !ok {
		return nil, output.Validation("bad_order", fmt.Sprintf("order must be desc|asc, got %q", p.Order))
	}
	profitableIn := make([]string, 0, len(p.ProfitableIn))
	for _, w := range p.ProfitableIn {
		nw, ok := normalizeWindow(w)
		if !ok {
			return nil, output.Validation("bad_window", fmt.Sprintf("profitable-in window must be day|week|month|allTime, got %q", w))
		}
		profitableIn = append(profitableIn, nw)
	}
	if p.Limit < 0 {
		return nil, output.Validation("bad_limit", "limit must be >= 0 (0 = all)")
	}
	if p.Offset < 0 {
		return nil, output.Validation("bad_offset", "offset must be >= 0")
	}

	// Live enrichment: a live filter implies it. It must be bounded (one read per
	// returned row), so it requires a finite --limit.
	liveFilter := p.InMarket || p.Flat || p.MinLiveEquity != nil || p.MaxLiveEquity != nil || p.MaxLiveLeverage != nil
	live := p.Live || liveFilter
	if p.InMarket && p.Flat {
		return nil, output.Validation("bad_live", "--in-market and --flat are mutually exclusive")
	}
	if live && p.Limit <= 0 {
		return nil, output.Validation("bad_live", "live enrichment fetches one read per returned row, so it requires a bounded --limit (not 0/all)")
	}
	liveScan := p.LiveScan
	if liveScan <= 0 {
		liveScan = defaultLiveScan
	}
	if liveScan > maxLiveScan {
		liveScan = maxLiveScan
	}

	lb, err := c.fetchLeaderboard(ctx)
	if err != nil {
		return nil, err
	}

	addrSet := map[string]bool{}
	for _, a := range p.Addresses {
		if a = strings.TrimSpace(a); a != "" {
			addrSet[strings.ToLower(a)] = true
		}
	}

	// Build entries, applying every filter as we go.
	entries := make([]LeaderEntry, 0, len(lb.Rows))
	for _, row := range lb.Rows {
		if len(addrSet) > 0 && !addrSet[strings.ToLower(row.EthAddress)] {
			continue
		}
		if p.Named && row.DisplayName == "" {
			continue
		}
		wp := row.Windows[window]
		av := parseLeaderFloat(row.AccountValue)
		pnl := parseLeaderFloat(wp.Pnl)
		roi := parseLeaderFloat(wp.Roi)
		vlm := parseLeaderFloat(wp.Vlm)
		prize := parseLeaderFloat(row.Prize)

		if !inRange(av, p.MinAccountValue, p.MaxAccountValue) ||
			!inRange(pnl, p.MinPnl, p.MaxPnl) ||
			!inRange(roi, p.MinRoi, p.MaxRoi) ||
			!inRange(vlm, p.MinVlm, p.MaxVlm) ||
			!atLeast(prize, p.MinPrize) {
			continue
		}
		if p.Profitable && !(pnl > 0 && roi > 0) {
			continue
		}
		if !profitableInAll(row, profitableIn) {
			continue
		}
		entries = append(entries, leaderEntry(row))
	}

	matched := len(entries)
	sortLeaderEntries(entries, window, sortBy, order)
	for i := range entries {
		entries[i].Rank = i + 1
	}
	entries = paginate(entries, p.Offset, p.Limit)

	// Live enrichment runs only on the returned page, capped at liveScan reads.
	// Live filters then drop rows that fail (including any left un-enriched).
	liveScanned := 0
	if live {
		for i := range entries {
			if liveScanned >= liveScan {
				break
			}
			c.enrichLive(ctx, &entries[i])
			liveScanned++
		}
		if liveFilter {
			kept := entries[:0]
			for _, e := range entries {
				if passLiveFilters(e, p) {
					kept = append(kept, e)
				}
			}
			entries = kept
		}
	}

	return &LeaderboardView{
		Network:     c.network,
		Source:      c.lbURL,
		SortWindow:  window,
		SortBy:      sortBy,
		Order:       order,
		TotalRows:   len(lb.Rows),
		Matched:     matched,
		Returned:    len(entries),
		Offset:      p.Offset,
		LiveScanned: liveScanned,
		Rows:        entries,
	}, nil
}

// enrichLive attaches the address's CURRENT clearinghouse state. Best-effort: on a
// read error (or nil info client) the entry's Live stays nil, and a live filter
// then drops it (we won't show an un-confirmed row as if it passed).
func (c *Client) enrichLive(ctx context.Context, e *LeaderEntry) {
	if c.info == nil {
		return
	}
	st, err := c.info.UserState(ctx, e.Address)
	if err != nil || st == nil {
		return
	}
	li := &LiveInfo{Equity: st.MarginSummary.AccountValue}
	for _, ap := range st.AssetPositions {
		if parseLeaderFloat(ap.Position.Szi) == 0 {
			continue
		}
		li.OpenPositions++
		li.Coins = append(li.Coins, ap.Position.Coin)
		if ap.Position.Leverage.Value > li.MaxLeverage {
			li.MaxLeverage = ap.Position.Leverage.Value
		}
	}
	e.Live = li
}

// passLiveFilters reports whether an enriched entry satisfies the live filters. An
// un-enriched entry (Live nil — read error or beyond the scan cap) fails, since we
// cannot confirm it meets the filter.
func passLiveFilters(e LeaderEntry, p LeaderboardParams) bool {
	if e.Live == nil {
		return false
	}
	if p.InMarket && e.Live.OpenPositions == 0 {
		return false
	}
	if p.Flat && e.Live.OpenPositions > 0 {
		return false
	}
	if !inRange(parseLeaderFloat(e.Live.Equity), p.MinLiveEquity, p.MaxLiveEquity) {
		return false
	}
	if p.MaxLiveLeverage != nil && float64(e.Live.MaxLeverage) > *p.MaxLiveLeverage {
		return false
	}
	return true
}

// leaderboardCacheMeta is the small sidecar stored next to the cached board body
// so a later call can revalidate cheaply (ETag/Last-Modified) instead of re-pulling
// the full ~32 MB blob.
type leaderboardCacheMeta struct {
	Network   string `json:"network"`
	FetchedAt int64  `json:"fetched_at_ms"`
	ETag      string `json:"etag,omitempty"`
	LastMod   string `json:"last_modified,omitempty"`
}

func leaderboardCachePaths(network string) (body, meta string) {
	base := filepath.Join(config.Dir(), "leaderboard_"+network)
	return base + ".json", base + ".meta.json"
}

// loadLeaderboardCache returns the cached body + metadata for the network, or
// (nil, _, false) when absent/unreadable/for a different network.
func loadLeaderboardCache(bodyPath, metaPath, network string) ([]byte, leaderboardCacheMeta, bool) {
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, leaderboardCacheMeta{}, false
	}
	var m leaderboardCacheMeta
	if err := json.Unmarshal(mb, &m); err != nil || m.Network != network {
		return nil, leaderboardCacheMeta{}, false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return nil, leaderboardCacheMeta{}, false
	}
	return body, m, true
}

// saveLeaderboardCache atomically writes the board body + sidecar metadata (0600,
// best-effort — a cache write never fails the read).
func saveLeaderboardCache(bodyPath, metaPath string, body []byte, m leaderboardCacheMeta) {
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o700); err != nil {
		return
	}
	if err := atomicWrite(bodyPath, body); err != nil {
		return
	}
	if mb, err := json.Marshal(m); err == nil {
		_ = atomicWrite(metaPath, mb)
	}
}

// atomicWrite writes data to a 0600 temp file in path's dir, fsyncs it, and renames
// over path (atomic on POSIX) — so a crash mid-write can't leave a torn cache
// (audit #91/S12). The temp file is removed if the rename never happens.
func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".lb-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed away
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fetchLeaderboard returns the leaderboard, served from a local cache when fresh or
// unchanged. The board is GET-only on the stats host (a different origin than
// /info), so it does not use the shared POST transport. Strategy:
//  1. cache fresh within LeaderboardTTLSecs -> use it, no network at all.
//  2. else conditional GET (If-None-Match / If-Modified-Since); a 304 reuses the
//     cached body (0-byte response instead of ~32 MB).
//  3. else a 200 is parsed, capped, and cached for next time.
func (c *Client) fetchLeaderboard(ctx context.Context) (*hl.Leaderboard, error) {
	bodyPath, metaPath := leaderboardCachePaths(c.network)
	cachedBody, cachedMeta, haveCache := loadLeaderboardCache(bodyPath, metaPath, c.network)

	ttl := time.Duration(c.cfg.State.LeaderboardTTLSecs) * time.Second
	if haveCache && ttl > 0 && time.Since(time.UnixMilli(cachedMeta.FetchedAt)) < ttl {
		if lb, err := hl.ParseLeaderboard(cachedBody); err == nil {
			return lb, nil
		}
		// corrupt cache: fall through and refetch.
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.lbURL, nil)
	if err != nil {
		return nil, output.Network("leaderboard_request", err.Error())
	}
	if haveCache {
		if cachedMeta.ETag != "" {
			req.Header.Set("If-None-Match", cachedMeta.ETag)
		}
		if cachedMeta.LastMod != "" {
			req.Header.Set("If-Modified-Since", cachedMeta.LastMod)
		}
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, mapNetwork("leaderboard", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && haveCache {
		lb, perr := hl.ParseLeaderboard(cachedBody)
		if perr != nil {
			return nil, output.Network("leaderboard_parse", perr.Error())
		}
		// Unchanged upstream: reuse the cached body and reset the TTL clock.
		cachedMeta.FetchedAt = time.Now().UnixMilli()
		if mb, merr := json.Marshal(cachedMeta); merr == nil {
			_ = atomicWrite(metaPath, mb)
		}
		return lb, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, output.RateLimit("ip_rate_limited", "Hyperliquid stats host returned 429").WithRetryAfter(2000)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, output.Network("leaderboard_http",
			fmt.Sprintf("leaderboard fetch failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))).Retry()
	}
	// Reject an over-cap body up front when the host declares Content-Length.
	if resp.ContentLength > maxLeaderboardBytes {
		return nil, output.Network("leaderboard_too_large",
			fmt.Sprintf("leaderboard Content-Length %d exceeds cap %d bytes", resp.ContentLength, maxLeaderboardBytes))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLeaderboardBytes+1))
	if err != nil {
		return nil, mapNetwork("leaderboard", err)
	}
	if int64(len(body)) > maxLeaderboardBytes {
		return nil, output.Network("leaderboard_too_large",
			fmt.Sprintf("leaderboard response exceeded %d bytes (capped)", maxLeaderboardBytes))
	}
	lb, err := hl.ParseLeaderboard(body)
	if err != nil {
		return nil, output.Network("leaderboard_parse", err.Error())
	}
	saveLeaderboardCache(bodyPath, metaPath, body, leaderboardCacheMeta{
		Network:   c.network,
		FetchedAt: time.Now().UnixMilli(),
		ETag:      resp.Header.Get("ETag"),
		LastMod:   resp.Header.Get("Last-Modified"),
	})
	return lb, nil
}

// leaderEntry maps a raw row to the view, filling every window.
func leaderEntry(row hl.LeaderboardRow) LeaderEntry {
	return LeaderEntry{
		Address:      row.EthAddress,
		DisplayName:  row.DisplayName,
		AccountValue: row.AccountValue,
		Prize:        row.Prize,
		Day:          windowView(row.Windows[hl.LeaderboardWindowDay]),
		Week:         windowView(row.Windows[hl.LeaderboardWindowWeek]),
		Month:        windowView(row.Windows[hl.LeaderboardWindowMonth]),
		AllTime:      windowView(row.Windows[hl.LeaderboardWindowAllTime]),
	}
}

func windowView(wp hl.WindowPerformance) WindowPerfView {
	v := WindowPerfView{Pnl: wp.Pnl, Roi: wp.Roi, Vlm: wp.Vlm}
	if strings.TrimSpace(wp.Roi) != "" {
		v.RoiPct = strconv.FormatFloat(parseLeaderFloat(wp.Roi)*100, 'f', 2, 64)
	}
	return v
}

// entryMetric pulls the sort value for an entry's selected window + key.
func entryMetric(e LeaderEntry, window, sortBy string) float64 {
	switch sortBy {
	case LeaderSortAccountValue:
		return parseLeaderFloat(e.AccountValue)
	case LeaderSortPrize:
		return parseLeaderFloat(e.Prize)
	}
	var wp WindowPerfView
	switch window {
	case hl.LeaderboardWindowDay:
		wp = e.Day
	case hl.LeaderboardWindowWeek:
		wp = e.Week
	case hl.LeaderboardWindowMonth:
		wp = e.Month
	default:
		wp = e.AllTime
	}
	switch sortBy {
	case LeaderSortRoi:
		return parseLeaderFloat(wp.Roi)
	case LeaderSortVlm:
		return parseLeaderFloat(wp.Vlm)
	default: // pnl
		return parseLeaderFloat(wp.Pnl)
	}
}

func sortLeaderEntries(entries []LeaderEntry, window, sortBy, order string) {
	sort.SliceStable(entries, func(i, j int) bool {
		a := entryMetric(entries[i], window, sortBy)
		b := entryMetric(entries[j], window, sortBy)
		if a == b {
			// Stable tie-break by address so output is deterministic.
			return entries[i].Address < entries[j].Address
		}
		if order == "asc" {
			return a < b
		}
		return a > b
	})
}

// profitableInAll reports whether the row had pnl > 0 in every listed window.
func profitableInAll(row hl.LeaderboardRow, windows []string) bool {
	for _, w := range windows {
		if parseLeaderFloat(row.Windows[w].Pnl) <= 0 {
			return false
		}
	}
	return true
}

// inRange reports whether v satisfies the optional [min,max] bounds.
func inRange(v float64, min, max *float64) bool {
	if min != nil && v < *min {
		return false
	}
	if max != nil && v > *max {
		return false
	}
	return true
}

func atLeast(v float64, min *float64) bool { return min == nil || v >= *min }

// paginate applies offset then limit (0 = no limit) to a ranked slice.
func paginate(entries []LeaderEntry, offset, limit int) []LeaderEntry {
	if offset >= len(entries) {
		return []LeaderEntry{}
	}
	entries = entries[offset:]
	if limit > 0 && limit < len(entries) {
		entries = entries[:limit]
	}
	return entries
}
