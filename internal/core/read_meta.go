package core

import (
	"fmt"
	"strings"
	"time"
)

// ReadMeta is the read-health metadata a TOLERANT read returns alongside its
// data (NEXT-2 item 1): which inputs it could NOT include (degraded coverage)
// and whether a paged history read hit its safety cap (truncation). The agent
// ACTS on read data, so a partial answer must never be silent — the command
// layer turns this into envelope warnings plus the additive machine-readable
// `degraded_dexs` / `truncated` envelope fields. The zero value means a clean,
// complete read (no warnings, no fields). Reads carrying a non-zero ReadMeta
// still exit 0 — only the risk gates FAIL CLOSED on degraded state (see
// portfolioEquitySnapshot).
type ReadMeta struct {
	// Degraded lists the unreadable inputs as human/agent-readable descriptors,
	// e.g. "sub-dex state (xyz)", "open orders (xyz)", "spot state".
	Degraded []string
	// DegradedDexs lists just the HIP-3 sub-dex names whose clearinghouse or
	// open-orders read failed — the machine-readable envelope field.
	DegradedDexs []string
	// Truncated reports that a paged history read stopped at the hard safety
	// cap with more rows available (see pageByTime).
	Truncated bool
	// Notes are surface-specific warnings to emit verbatim (e.g. an excluded
	// non-USDC fee token, or the truncation continue-from hint).
	Notes []string
}

// addDegraded records one unreadable input; dex, when non-empty, is also
// deduplicated into DegradedDexs.
func (m *ReadMeta) addDegraded(descriptor, dex string) {
	m.Degraded = append(m.Degraded, descriptor)
	if dex == "" {
		return
	}
	for _, d := range m.DegradedDexs {
		if strings.EqualFold(d, dex) {
			return
		}
	}
	m.DegradedDexs = append(m.DegradedDexs, dex)
}

// merge folds another read's health into this one (e.g. PnlAttribution merges
// its fills and funding reads).
func (m *ReadMeta) merge(o ReadMeta) {
	m.Degraded = append(m.Degraded, o.Degraded...)
	for _, dex := range o.DegradedDexs {
		dup := false
		for _, have := range m.DegradedDexs {
			if strings.EqualFold(have, dex) {
				dup = true
				break
			}
		}
		if !dup {
			m.DegradedDexs = append(m.DegradedDexs, dex)
		}
	}
	m.Truncated = m.Truncated || o.Truncated
	m.Notes = append(m.Notes, o.Notes...)
}

// EnvelopeWarnings renders the health as envelope warnings. Empty for a clean
// read — degradation must be loud, a clean read must be quiet.
func (m ReadMeta) EnvelopeWarnings() []string {
	w := append([]string{}, m.Notes...)
	if len(m.Degraded) > 0 {
		w = append(w, "PARTIAL DATA — could not read: "+strings.Join(m.Degraded, ", ")+
			". Anything held there is MISSING from this response, not gone; retry before acting on absence (degraded_dexs names the affected sub-dexs).")
	}
	return w
}

// utcMidnightMs is the start of the current UTC day in unix ms — the default
// window anchor for `pnl attribution` (matching the daily-loss gate's UTC-day
// anchoring).
func utcMidnightMs() int64 {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
}

// ---- pagination for time-bounded history reads (NEXT-2 item 7) ----

const (
	// fillsPageSize / ledgerPageSize are Hyperliquid's per-response caps: one
	// userFillsByTime response returns at most 2000 fills; userFunding and
	// userNonFundingLedgerUpdates at most 500 rows. A full page means MORE rows
	// exist in the window — a single fetch silently under-reports history/P&L.
	fillsPageSize  = 2000
	ledgerPageSize = 500
	// maxHistoryPages is the hard safety cap per read: pages are fetched
	// SEQUENTIALLY (each is a weight-20 info call — no parallel hammering of
	// the per-IP budget) and a read that would exceed the cap stops and REPORTS
	// truncation (truncated:true + a continue-from --since hint) instead of
	// running away or, worse, stopping silently.
	maxHistoryPages = 10
)

// pageByTime pages a time-bounded HL info read to completion: fetch(start)
// returns one ascending-time page; a page shorter than pageSize is the last.
//
// Timestamps are milliseconds and NOT unique — one aggressive order sweeping
// several book levels yields multiple fills with an identical `time`, and the
// exchange cuts a full page at exactly pageSize rows, possibly MID-cluster. So
// the cursor may never skip past the boundary millisecond: a full page keeps
// only its rows BEFORE the boundary timestamp and re-starts the next page AT it
// (HL's startTime is inclusive) — the deferred boundary rows return at the head
// of the next page, so nothing is lost and nothing double-counts. The old
// (last+1) cursor silently dropped whatever part of a same-ms cluster didn't
// fit the page — an invisible under-count of fills/fees/funding.
//
// Returns the merged rows plus a ReadMeta whose Truncated/Notes report an
// incomplete read: safety cap hit, a same-ms cluster larger than one page (the
// API cannot page within a millisecond), or a stuck cursor whose stale rows are
// discarded rather than double-counted. what names the rows for the notes.
func pageByTime[T any](start int64, pageSize int, what string, timeOf func(T) int64, fetch func(start int64) ([]T, error)) ([]T, ReadMeta, error) {
	var all []T
	var rm ReadMeta
	for page := 0; page < maxHistoryPages; page++ {
		rows, err := fetch(start)
		if err != nil {
			return nil, rm, err
		}
		if len(rows) < pageSize {
			if page == 0 {
				// A single short page: the pre-pagination fast path — one fetch,
				// returned as-is (the server applies the window).
				return append(all, rows...), rm, nil
			}
			// Short continuation page: the window is complete. The inclusive
			// re-start legitimately repeats the boundary millisecond (time ==
			// start — those rows were deferred, keep them); anything OLDER is a
			// stale duplicate of a previous page — drop, don't double-count.
			for _, r := range rows {
				if timeOf(r) >= start {
					all = append(all, r)
				}
			}
			return all, rm, nil
		}
		last := int64(-1)
		for _, r := range rows {
			if t := timeOf(r); t > last {
				last = t
			}
		}
		if last < start {
			// A FULL page whose every row predates the requested start: contract
			// violation / stuck cursor. Appending would double-count (or return
			// out-of-window rows) and looping would never terminate — bail loudly.
			rm.Truncated = true
			rm.Notes = append(rm.Notes, fmt.Sprintf(
				"%s pagination stopped: the exchange returned rows older than the requested start (%d) — results may be incomplete; retry, or narrow the window with --since", what, start))
			return all, rm, nil
		}
		// Full page: fresh = this page's window (a continuation page's rows
		// before the cursor are stale duplicates); the cap may have cut the
		// boundary timestamp `last` mid-cluster.
		fresh := rows
		if page > 0 {
			fresh = make([]T, 0, len(rows))
			for _, r := range rows {
				if timeOf(r) >= start {
					fresh = append(fresh, r)
				}
			}
		}
		kept := make([]T, 0, len(fresh))
		for _, r := range fresh {
			if timeOf(r) != last {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			// Every fresh row of a FULL page shares ONE timestamp: more rows than
			// a page exist at that millisecond and the API cannot page within it.
			// Keep what arrived and REPORT the shortfall — never a silent under-count.
			all = append(all, fresh...)
			rm.Truncated = true
			rm.Notes = append(rm.Notes, fmt.Sprintf(
				"%s pagination stopped: more than one page of rows share timestamp %d (the exchange cannot page within a millisecond) — rows at that timestamp are incomplete; totals may under-report", what, last))
			return all, rm, nil
		}
		all = append(all, kept...)
		start = last
	}
	rm.Truncated = true
	rm.Notes = append(rm.Notes, fmt.Sprintf(
		"%s truncated at the %d-page safety cap (%d rows fetched) — rows at/after --since %d were NOT fetched; continue from there", what, maxHistoryPages, len(all), start))
	return all, rm, nil
}
