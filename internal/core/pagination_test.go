package core

// NEXT-2 item 7: time-bounded history reads (fills --since, funding, ledger)
// must page to completion — HL caps a single response at 2000 fills / 500
// funding-or-ledger rows and silently truncates. Pages advance startTime to the
// last-seen timestamp (INCLUSIVE — several rows can share one millisecond, e.g.
// one sweep's fills, and the exchange cuts a full page mid-millisecond), with
// the boundary-ms rows deferred to the next page so nothing is lost or
// double-counted. A hard safety cap stops a runaway read and is REPORTED
// (truncated:true + warning), never silent.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

// fillRowsFrom builds n fill rows with times start..start+n-1 (tid = time).
func fillRowsFrom(start int64, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		ts := start + int64(i)
		fmt.Fprintf(&b, `{"coin":"BTC","px":"1","sz":"1","side":"B","time":%d,"oid":%d,"hash":"0x","fee":"0","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":%d}`, ts, ts, ts)
	}
	b.WriteString("]")
	return b.String()
}

// fundingRowsFrom builds n funding rows with times start..start+n-1.
func fundingRowsFrom(start int64, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		ts := start + int64(i)
		fmt.Fprintf(&b, `{"delta":{"coin":"BTC","fundingRate":"0.0001","size":"1","type":"funding","usdc":"0.1"},"hash":"0x","time":%d}`, ts)
	}
	b.WriteString("]")
	return b.String()
}

// ledgerRowsFrom builds n ledger rows with times start..start+n-1.
func ledgerRowsFrom(start int64, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		ts := start + int64(i)
		fmt.Fprintf(&b, `{"delta":{"type":"deposit","usdc":"1"},"hash":"0x","time":%d}`, ts)
	}
	b.WriteString("]")
	return b.String()
}

// fillsWorldResp serves a FIXED world of fills — `times` ascending, tid = the
// row's position in the world (unique even inside a same-millisecond cluster) —
// with the real userFillsByTime contract: up to fillsPageSize rows with
// time >= startTime, ascending. This models the exchange exactly, so a test
// asserts completeness (every world row fetched once) rather than the cursor
// arithmetic.
func fillsWorldResp(times []int64, calls *int, starts *[]int64) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path != "/info" || typ != "userFillsByTime" {
			return 200, "{}"
		}
		if calls != nil {
			*calls++
		}
		start := int64(body["startTime"].(float64))
		if starts != nil {
			*starts = append(*starts, start)
		}
		var b strings.Builder
		b.WriteString("[")
		n := 0
		for i, ts := range times {
			if ts < start {
				continue
			}
			if n == fillsPageSize {
				break
			}
			if n > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"coin":"BTC","px":"1","sz":"1","side":"B","time":%d,"oid":%d,"hash":"0x","fee":"0","feeToken":"USDC","closedPnl":"0","startPosition":"0","dir":"Open Long","crossed":true,"tid":%d}`, ts, i+1, i+1)
			n++
		}
		b.WriteString("]")
		return 200, b.String()
	}
}

// A window holding more than one exchange page (2000 fills) must be paged to
// completion: the second request re-starts AT the boundary timestamp (HL's
// startTime is inclusive; the boundary-ms rows were deferred), and every world
// row arrives exactly once.
func TestFillsSincePaginatesToCompletion(t *testing.T) {
	times := make([]int64, 0, 2004)
	for ts := int64(1); ts <= 2004; ts++ {
		times = append(times, ts)
	}
	var starts []int64
	c, ctx := newTestClient(t, config.Default(), Options{}, fillsWorldResp(times, nil, &starts))
	since := int64(1)
	fills, rm, err := c.Fills(ctx, &since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 2004 {
		t.Fatalf("must merge all pages without loss or double-count: got %d fills, want 2004", len(fills))
	}
	if len(starts) != 2 || starts[0] != 1 || starts[1] != 2000 {
		t.Fatalf("second page must re-start AT the first page's boundary timestamp (inclusive): starts=%v, want [1 2000]", starts)
	}
	if rm.Truncated {
		t.Fatal("a completed paged read must not be marked truncated")
	}
	if fills[0].Time != 2004 {
		t.Fatalf("fills must stay newest-first across pages, got first time %d", fills[0].Time)
	}
	seen := map[int64]bool{}
	for _, f := range fills {
		if seen[f.Tid] {
			t.Fatalf("fill tid %d returned twice — double-counted at the page boundary", f.Tid)
		}
		seen[f.Tid] = true
	}
}

// THE silent-shortfall case this fix-up exists for: a same-millisecond cluster
// (one aggressive order sweeping several book levels yields fills with an
// identical `time`) straddles the 2000-row page cap. The old last+1 cursor
// permanently skipped the cluster's remainder — no truncated flag, no warning,
// under-reported fees/PnL. Every fill must be fetched exactly once.
func TestFillsSameMsClusterAtPageBoundaryNotDropped(t *testing.T) {
	// 1998 fills at t=1..1998, then FIVE fills at t=1999: page 1 (cap 2000) cuts
	// after the second t=1999 fill; the other three must still arrive.
	times := make([]int64, 0, 2003)
	for ts := int64(1); ts <= 1998; ts++ {
		times = append(times, ts)
	}
	for i := 0; i < 5; i++ {
		times = append(times, 1999)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, fillsWorldResp(times, nil, nil))
	since := int64(1)
	fills, rm, err := c.Fills(ctx, &since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rm.Truncated {
		t.Fatalf("a completable window must not be reported truncated: %+v", rm)
	}
	seen := map[int64]bool{}
	atBoundary := 0
	for _, f := range fills {
		if seen[f.Tid] {
			t.Fatalf("fill tid %d fetched twice (double-count corrupts pnl attribution)", f.Tid)
		}
		seen[f.Tid] = true
		if f.Time == 1999 {
			atBoundary++
		}
	}
	if len(fills) != 2003 || atBoundary != 5 {
		t.Fatalf("got %d fills (%d at the boundary ms), want 2003 (5 at t=1999) — same-ms rows straddling a page boundary must never be dropped silently", len(fills), atBoundary)
	}
}

// A same-millisecond cluster BIGGER than one page cannot be paged past (the API
// has no within-ms cursor): the read must keep what it got and REPORT the
// shortfall (truncated + warning) — never return a silently smaller history.
func TestFillsOversizedSameMsClusterReportsTruncated(t *testing.T) {
	times := make([]int64, 2500)
	for i := range times {
		times[i] = 1000 // 2500 fills all in one millisecond
	}
	calls := 0
	c, ctx := newTestClient(t, config.Default(), Options{}, fillsWorldResp(times, &calls, nil))
	since := int64(1)
	fills, rm, err := c.Fills(ctx, &since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !rm.Truncated {
		t.Fatal("an unpageable same-ms cluster MUST report truncated — a silent shortfall is a money bug")
	}
	if len(fills) != fillsPageSize {
		t.Fatalf("the fetched page must still be returned: got %d fills, want %d", len(fills), fillsPageSize)
	}
	if calls != 1 {
		t.Fatalf("an unpageable cluster must bail without hammering the API: %d calls", calls)
	}
	joined := strings.Join(rm.EnvelopeWarnings(), " | ")
	if !strings.Contains(joined, "incomplete") {
		t.Fatalf("the shortfall must be warned, got %v", rm.EnvelopeWarnings())
	}
}

// When every page comes back full, the hard safety cap stops the read and
// REPORTS truncation (truncated:true + a continue-from hint) — never silent.
func TestFillsHardCapReportsTruncation(t *testing.T) {
	calls := 0
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "userFillsByTime" {
			calls++
			start := int64(body["startTime"].(float64))
			return 200, fillRowsFrom(start, fillsPageSize)
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	since := int64(1)
	fills, rm, err := c.Fills(ctx, &since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if calls != maxHistoryPages {
		t.Fatalf("hard cap must stop after %d sequential pages, made %d calls", maxHistoryPages, calls)
	}
	// Each full page keeps pageSize−1 rows: its boundary-ms rows are deferred to
	// the next page (inclusive re-start), and the final page's boundary is
	// covered by the continue-from --since hint.
	if len(fills) != maxHistoryPages*(fillsPageSize-1) {
		t.Fatalf("got %d fills, want %d", len(fills), maxHistoryPages*(fillsPageSize-1))
	}
	if !rm.Truncated {
		t.Fatal("hitting the safety cap MUST set truncated")
	}
	warned := false
	for _, w := range rm.EnvelopeWarnings() {
		if strings.Contains(w, "--since") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("truncation must be warned with a continue-from hint, got %v", rm.EnvelopeWarnings())
	}
}

// Funding pages past HL's 500-row cap the same way (inclusive boundary re-start).
func TestFundingPaginatesToCompletion(t *testing.T) {
	var starts []int64
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "userFunding" {
			start := int64(body["startTime"].(float64))
			starts = append(starts, start)
			if start == 1 {
				return 200, fundingRowsFrom(1, ledgerPageSize) // full page → more to fetch
			}
			return 200, fundingRowsFrom(start, 3) // short page (times start..start+2) → done
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	since := int64(1)
	f, rm, err := c.Funding(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	// Page 1 keeps rows 1..499 (t=500 deferred); page 2 re-starts at 500 and
	// returns 500..502 → 502 distinct rows.
	if len(f) != ledgerPageSize+2 {
		t.Fatalf("funding must page to completion: got %d rows, want %d", len(f), ledgerPageSize+2)
	}
	if len(starts) != 2 || starts[1] != int64(ledgerPageSize) {
		t.Fatalf("funding page 2 must re-start at the boundary timestamp (inclusive): %v", starts)
	}
	if rm.Truncated {
		t.Fatal("completed funding read must not be truncated")
	}
}

// Ledger pages past the 500-row cap too.
func TestLedgerPaginatesToCompletion(t *testing.T) {
	var starts []int64
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "userNonFundingLedgerUpdates" {
			start := int64(body["startTime"].(float64))
			starts = append(starts, start)
			if start == 1 {
				return 200, ledgerRowsFrom(1, ledgerPageSize)
			}
			return 200, ledgerRowsFrom(start, 2)
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	since := int64(1)
	l, rm, err := c.Ledger(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != ledgerPageSize+1 {
		t.Fatalf("ledger must page to completion: got %d rows, want %d", len(l), ledgerPageSize+1)
	}
	if len(starts) != 2 || starts[1] != int64(ledgerPageSize) {
		t.Fatalf("ledger pagination starts wrong: %v", starts)
	}
	if rm.Truncated {
		t.Fatal("completed ledger read must not be truncated")
	}
}

// A server that returns the identical full page forever (stuck cursor) must not
// loop or double-count: the second page's only fresh row (the deferred boundary
// row) is kept, the stale duplicates are discarded, and the read reports
// truncation.
func TestPaginationStuckCursorBailsTruncated(t *testing.T) {
	calls := 0
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "userFunding" {
			calls++
			return 200, fundingRowsFrom(1, ledgerPageSize) // same full page forever
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	since := int64(1)
	f, rm, err := c.Funding(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("a non-advancing cursor must bail on the second page, made %d calls", calls)
	}
	if len(f) != ledgerPageSize {
		t.Fatalf("stale duplicate rows must be discarded: got %d rows, want %d", len(f), ledgerPageSize)
	}
	if !rm.Truncated {
		t.Fatal("a bailed pagination must report truncated")
	}
}

// A full page whose every row predates the requested start (contract violation)
// is discarded entirely and the read bails loudly rather than double-counting
// or looping.
func TestPaginationRowsOlderThanStartBails(t *testing.T) {
	calls := 0
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "userFunding" {
			calls++
			return 200, fundingRowsFrom(1, ledgerPageSize) // rows 1..500, all < since
		}
		return 200, "{}"
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	since := int64(1000)
	f, rm, err := c.Funding(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("an all-stale page must bail immediately, made %d calls", calls)
	}
	if len(f) != 0 {
		t.Fatalf("out-of-window rows must be discarded, got %d", len(f))
	}
	if !rm.Truncated {
		t.Fatal("the bail must be reported as truncated, never silent")
	}
}
