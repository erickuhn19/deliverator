package mmtui

// render_more_test.go — additional coverage for the render + key-handling paths that
// the first test file leaves largely untouched: every posture of renderBanner, the
// holdings/PnL/activity/pool panels, all the small formatters, the off-thread Cmd
// closures (poll/setLive/setHalt), and the full key-handling matrix driven through
// Update with recording stub Deps hooks and a real (client-less) engine for the
// pause/live gates. Assertions check exact expected values/states, not mere non-panic.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm/engine"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
)

// ----- small helpers -----

func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// stepKey feeds one key through Update and returns the resulting Model plus the Cmd.
func stepKey(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// modelReady builds a New model in the "first snapshot arrived, window sized" state.
func modelReady(v engine.EngineView) Model {
	m := New(Deps{})
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = v
	return m
}

// keyRec records the last guarded [mm] config edit and lets a test inject an error.
type keyRec struct {
	key, val string
	calls    int
	err      error
}

// boolRec records a single-bool guarded hook (SetLive / SetHalt).
type boolRec struct {
	on    bool
	calls int
	err   error
}

// clientlessEngine builds a real engine with a nil client — enough for Live()/Paused()/
// SetPaused()/View(), which never touch the exchange. live=true => signing is on.
func clientlessEngine(t *testing.T, live bool) *engine.Engine {
	t.Helper()
	cfg := config.Default()
	mmc := config.DefaultMM()
	mmc.Enabled = live
	mmc.DryRun = !live
	cfg.MM = mmc
	e := engine.New(engine.Deps{Cfg: cfg})
	if e.Live() != live {
		t.Fatalf("clientlessEngine: Live()=%v want %v", e.Live(), live)
	}
	return e
}

// richView is a fuller snapshot than sampleView: it also populates Holdings (active +
// settling, winners + losers), a session start, and PnL fills/volume so the holdings /
// PnL / session paths render real content.
func richView() engine.EngineView {
	return engine.EngineView{
		Running: true, DryRun: true, Network: "mainnet", Equity: 9800.50,
		Active: []engine.MarketView{{
			Coin: "BTCUP", Title: "BTC > 70k", Underlying: "BTC",
			TTL: 45 * time.Minute, FairP: 0.62, Mid: 0.60, OurBid: 0.58, OurAsk: 0.64,
			InvYes: 12, InvNo: 0, Gate: "quoting",
		}},
		Holdings: []engine.HoldingView{
			{Coin: "BTCUP", Title: "BTC > 70k", Side: "Yes", Shares: 40,
				Value: 24.0, Entry: 0.55, Mark: 0.60, PnL: 2.0, Active: true},
			{Coin: "OLDNO", Title: "ETH < 3k", Side: "No", Shares: 10,
				Value: 3.0, Entry: 0.40, Mark: 0.30, PnL: -1.0, Active: false},
		},
		Pool: []selector.Candidate{
			{Market: core.Market{Coin: "BTCUP"}, Score: 0.81, Eligible: true, Active: true, Reason: "active"},
			{Market: core.Market{Coin: "ETHUP"}, Score: 0.44, Eligible: true, Reason: "eligible"},
		},
		PnL:       oms.PnLView{Realized: 12.0, Fees: 1.25, Open: -3.0, Net: -50.0, Fills: 7, Volume: 1500},
		StartedAt: time.Now().Add(-90 * time.Minute),
		LastScan:  time.Now().Add(-30 * time.Second),
		LastTick:  time.Now().Add(-1 * time.Second),
	}
}

// ============================ pure formatters ============================

func TestFmtUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{5 * time.Minute, "5m 0s"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
	}
	for _, c := range cases {
		if got := fmtUptime(c.d); got != c.want {
			t.Errorf("fmtUptime(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestFmtCompactUSD(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{2_500_000, "$2.50M"},
		{1500, "$1.5k"},
		{950, "$950"},
		{0, "$0"},
	}
	for _, c := range cases {
		if got := fmtCompactUSD(c.v); got != c.want {
			t.Errorf("fmtCompactUSD(%v)=%q want %q", c.v, got, c.want)
		}
	}
}

func TestFmtTTL(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-1 * time.Second, "expired"},
		{0, "expired"},
		{45 * time.Second, "45s"},
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h30m"},
	}
	for _, c := range cases {
		if got := fmtTTL(c.d); got != c.want {
			t.Errorf("fmtTTL(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestFmtProb(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "—"},
		{-0.5, "—"},
		{0.62, "0.62"},
		{1, "1.00"},
	}
	for _, c := range cases {
		if got := fmtProb(c.v); got != c.want {
			t.Errorf("fmtProb(%v)=%q want %q", c.v, got, c.want)
		}
	}
}

func TestFmtAge(t *testing.T) {
	if got := fmtAge(time.Time{}); got != "never" {
		t.Errorf("fmtAge(zero)=%q want never", got)
	}
	if got := fmtAge(time.Now().Add(-5 * time.Second)); got != "5s" {
		t.Errorf("fmtAge(-5s)=%q want 5s", got)
	}
	if got := fmtAge(time.Now().Add(-90 * time.Second)); got != "1m" {
		t.Errorf("fmtAge(-90s)=%q want 1m", got)
	}
	if got := fmtAge(time.Now().Add(-2 * time.Hour)); got != "2h" {
		t.Errorf("fmtAge(-2h)=%q want 2h", got)
	}
}

func TestTruncAndTrunc2(t *testing.T) {
	if got := trunc("hello", 10); got != "hello" {
		t.Errorf("trunc no-cut=%q", got)
	}
	if got := trunc("abcdef", 4); got != "abcd" {
		t.Errorf("trunc cut=%q want abcd", got)
	}
	if got := trunc2("abc", 0); got != "" {
		t.Errorf("trunc2 w=0 =%q want empty", got)
	}
	if got := trunc2("abc", 5); got != "abc" {
		t.Errorf("trunc2 no-cut=%q", got)
	}
	if got := trunc2("abcdef", 1); got != "…" {
		t.Errorf("trunc2 w=1 =%q want ellipsis", got)
	}
	if got := trunc2("abcdef", 4); got != "abc…" {
		t.Errorf("trunc2 cut=%q want abc…", got)
	}
}

func TestClampWidth(t *testing.T) {
	cases := []struct{ in, want int }{
		{5, 100}, {250, 100}, {8, 8}, {220, 220}, {80, 80},
	}
	for _, c := range cases {
		if got := clampWidth(c.in); got != c.want {
			t.Errorf("clampWidth(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestAppendUnique(t *testing.T) {
	if got := appendUnique(nil, "x"); len(got) != 1 || got[0] != "x" {
		t.Errorf("appendUnique(nil,x)=%v", got)
	}
	got := appendUnique([]string{"a"}, "b")
	if len(got) != 2 || got[1] != "b" {
		t.Errorf("append new=%v want [a b]", got)
	}
	// Case-insensitive duplicate is a no-op (returns the same slice, unchanged).
	got = appendUnique([]string{"a"}, "A")
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("append dup=%v want [a]", got)
	}
}

// colorGate/netStyle route to a specific style by severity — assert the EXACT styled
// string (robust to the active color profile since both sides render identically).
func TestColorGate(t *testing.T) {
	if got := colorGate("quoting"); got != cOK.Render("quoting") {
		t.Errorf("colorGate quoting mis-styled: %q", got)
	}
	if got := colorGate("settled"); got != cDim.Render("settled") {
		t.Errorf("colorGate settled mis-styled: %q", got)
	}
	if got := colorGate("stale fair"); got != cWarn.Render("stale fair") {
		t.Errorf("colorGate other mis-styled: %q", got)
	}
	if got := colorGate("   "); got != cDim.Render("—") {
		t.Errorf("colorGate blank=%q want dim em-dash", got)
	}
}

func TestNetStyle(t *testing.T) {
	if got := netStyle("mainnet"); got != cOK.Render("mainnet") {
		t.Errorf("netStyle mainnet=%q", got)
	}
	if got := netStyle("  mainnet "); got != cOK.Render("mainnet") {
		t.Errorf("netStyle trims: %q", got)
	}
	if got := netStyle("testnet"); got != cWarn.Render("testnet (test)") {
		t.Errorf("netStyle testnet=%q", got)
	}
	if got := netStyle(""); got != cDim.Render("?") {
		t.Errorf("netStyle empty=%q", got)
	}
}

func TestRowWithTag(t *testing.T) {
	// Normal: prefix fits, tag appended after a single space.
	if got := rowWithTag(40, "abc", "X"); got != "abc X" {
		t.Errorf("rowWithTag normal=%q want 'abc X'", got)
	}
	// Pathologically narrow: available width goes negative and is clamped to 0, so the
	// prefix is fully dropped and only the tag survives.
	if got := rowWithTag(2, "longprefix", "TAG"); got != " TAG" {
		t.Errorf("rowWithTag narrow=%q want ' TAG'", got)
	}
}

// ============================ banner postures ============================

func TestRenderBannerPostures(t *testing.T) {
	banner := func(v engine.EngineView, net string) string {
		m := Model{deps: Deps{Network: net}, view: v}
		return m.renderBanner(140)
	}

	// DRY-RUN (shadow) shows the dry-run badge and never the LIVE badge.
	out := banner(engine.EngineView{Running: true, DryRun: true}, "mainnet")
	if !strings.Contains(out, "DRY-RUN") {
		t.Errorf("dry-run banner missing DRY-RUN: %q", out)
	}
	if strings.Contains(out, "LIVE") {
		t.Errorf("dry-run banner must not claim LIVE: %q", out)
	}
	if !strings.Contains(out, "mainnet") {
		t.Errorf("banner should show Deps.Network mainnet: %q", out)
	}

	// Running && !DryRun => the loud LIVE badge, and no DRY-RUN.
	out = banner(engine.EngineView{Running: true, DryRun: false}, "mainnet")
	if !strings.Contains(out, "LIVE") {
		t.Errorf("live banner missing LIVE: %q", out)
	}
	if strings.Contains(out, "DRY-RUN") {
		t.Errorf("live banner must not show DRY-RUN: %q", out)
	}

	// Paused / Halted / Warmup badges each appear when set.
	out = banner(engine.EngineView{Running: true, DryRun: true, Paused: true}, "mainnet")
	if !strings.Contains(out, "PAUSED") {
		t.Errorf("banner missing PAUSED: %q", out)
	}
	out = banner(engine.EngineView{Running: true, Halted: true}, "mainnet")
	if !strings.Contains(out, "HALTED") {
		t.Errorf("banner missing HALTED: %q", out)
	}
	out = banner(engine.EngineView{Warmup: true}, "mainnet")
	if !strings.Contains(out, "WARMUP") {
		t.Errorf("banner missing WARMUP: %q", out)
	}

	// Empty Deps.Network falls back to the view's Network.
	out = banner(engine.EngineView{Running: true, DryRun: true, Network: "testnet"}, "")
	if !strings.Contains(out, "testnet") {
		t.Errorf("banner should fall back to view.Network testnet: %q", out)
	}
}

// ============================ panels ============================

func TestRenderHoldings(t *testing.T) {
	m := modelReady(richView())

	empty := Model{view: engine.EngineView{}}
	if got := empty.renderHoldings(120); !strings.Contains(got, "(flat — no outcome positions)") {
		t.Errorf("empty holdings=%q", got)
	}

	out := m.renderHoldings(120)
	if !strings.Contains(out, "POSITIONS (2)") {
		t.Errorf("holdings header wrong: %q", out)
	}
	if !strings.Contains(out, "BTCUP") || !strings.Contains(out, "OLDNO") {
		t.Errorf("holdings missing coin rows: %q", out)
	}
	// The active winner is "managing"; the dropped loser is "held→settle".
	if !strings.Contains(out, "managing") {
		t.Errorf("active holding should read 'managing': %q", out)
	}
	if !strings.Contains(out, "held→settle") {
		t.Errorf("inactive holding should read 'held→settle': %q", out)
	}
	// Losing position renders a signed-negative PnL.
	if !strings.Contains(out, "-$1.00") {
		t.Errorf("loser should show -$1.00: %q", out)
	}
}

func TestRenderPnLSession(t *testing.T) {
	m := modelReady(richView())
	out := m.renderPnL(120)
	if !strings.Contains(out, "realized") {
		t.Errorf("pnl missing realized line: %q", out)
	}
	// StartedAt 90m ago => session begins with "1h".
	if !strings.Contains(out, "session 1h") {
		t.Errorf("session should read 1h..: %q", out)
	}
	if !strings.Contains(out, "7 fills") {
		t.Errorf("pnl missing fill count: %q", out)
	}
	if !strings.Contains(out, "$1.5k") {
		t.Errorf("pnl missing compact volume: %q", out)
	}
	// Net -50 renders as a signed-negative figure.
	if !strings.Contains(out, "-$50.00") {
		t.Errorf("net should be -$50.00: %q", out)
	}

	// Zero StartedAt => the session shows an em-dash, not a duration.
	z := modelReady(richView())
	z.view.StartedAt = time.Time{}
	if got := z.renderPnL(120); !strings.Contains(got, "session —") {
		t.Errorf("zero StartedAt should show 'session —': %q", got)
	}
}

func TestRenderActivity(t *testing.T) {
	// No feed, no error => the placeholder line.
	m := modelReady(richView())
	if got := m.renderActivity(120, 6); !strings.Contains(got, "(no audit activity yet)") {
		t.Errorf("empty activity=%q", got)
	}

	// LastError pins at the top; only the last `rows` feed lines show.
	m.view.LastError = "feed stalled"
	m.feed = []string{"l1", "l2", "l3", "l4", "l5"}
	out := m.renderActivity(120, 2)
	if !strings.Contains(out, "last error: feed stalled") {
		t.Errorf("activity missing pinned error: %q", out)
	}
	if !strings.Contains(out, "l5") || !strings.Contains(out, "l4") {
		t.Errorf("activity should show the last 2 lines: %q", out)
	}
	if strings.Contains(out, "l1") {
		t.Errorf("activity should have scrolled l1 off: %q", out)
	}
}

func TestRenderPoolScroll(t *testing.T) {
	empty := Model{view: engine.EngineView{}}
	if got := empty.renderPool(80, 6); !strings.Contains(got, "(empty — waiting for the first scan)") {
		t.Errorf("empty pool=%q", got)
	}

	var pool []selector.Candidate
	for i := 0; i < 10; i++ {
		pool = append(pool, selector.Candidate{
			Market: core.Market{Coin: "C" + string(rune('0'+i))}, Score: float64(i) / 10, Reason: "eligible",
		})
	}
	m := modelReady(engine.EngineView{Pool: pool})
	m.sel = 2
	out := m.renderPool(80, 3)
	// Cursor row is marked and 7 rows remain below the 3-row window.
	if !strings.Contains(out, "▸") {
		t.Errorf("pool missing cursor marker: %q", out)
	}
	if !strings.Contains(out, "…7 more") {
		t.Errorf("pool should note 7 more rows: %q", out)
	}
}

func TestRenderActiveEmpty(t *testing.T) {
	m := Model{view: engine.EngineView{}}
	if got := m.renderActive(120); !strings.Contains(got, "(none quoting)") {
		t.Errorf("empty active=%q", got)
	}
}

// ============================ View() at both breakpoints ============================

func TestViewBreakpoints(t *testing.T) {
	m := modelReady(richView())
	m.feed = []string{"12:00:00  place         coin=BTCUP"}
	for _, w := range []int{90, 160} { // narrow (<120 stacked) and wide (>=120 two-col)
		m.w = w
		out := m.View()
		for _, want := range []string{"OUTCOME MARKET MAKER", "POSITIONS", "session", "DRY-RUN", "BTCUP"} {
			if !strings.Contains(out, want) {
				t.Errorf("[w=%d] View missing %q", w, want)
			}
		}
	}

	// A running, non-dry view shows the LIVE badge through View().
	live := modelReady(richView())
	live.view.DryRun = false
	if out := live.View(); !strings.Contains(out, "LIVE") {
		t.Errorf("live View missing LIVE badge")
	}
}

// View() with a window size but before the first snapshot keeps the banner and shows
// the starting line.
func TestViewStartingWithBanner(t *testing.T) {
	m := New(Deps{Network: "mainnet"})
	m.ready, m.w, m.h = true, 160, 48 // sized, but haveView=false
	out := m.View()
	if !strings.Contains(out, "starting") {
		t.Errorf("expected starting line: %q", out)
	}
	if !strings.Contains(out, "OUTCOME MARKET MAKER") {
		t.Errorf("starting screen should still show the banner: %q", out)
	}
}

// ============================ Update: non-key messages ============================

func TestUpdateWindowAndData(t *testing.T) {
	m := New(Deps{})

	// WindowSizeMsg marks ready and stores geometry.
	m, _ = stepKey(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if !m.ready || m.w != 100 || m.h != 40 {
		t.Fatalf("window size not applied: ready=%v w=%d h=%d", m.ready, m.w, m.h)
	}

	// dataMsg installs the snapshot, advances the high-water ts, appends feed, and
	// clamps an out-of-range cursor down to the last pool row.
	m.sel = 10
	m, _ = stepKey(t, m, dataMsg{view: sampleView(), feed: []string{"a"}, high: 42})
	if !m.haveView {
		t.Fatal("haveView should be set after dataMsg")
	}
	if m.feedSince != 42 {
		t.Fatalf("feedSince=%d want 42", m.feedSince)
	}
	if len(m.feed) != 1 || m.feed[0] != "a" {
		t.Fatalf("feed=%v want [a]", m.feed)
	}
	if m.sel != 2 { // sampleView pool has 3 rows => clamp 10 -> 2
		t.Fatalf("cursor should clamp to 2, got %d", m.sel)
	}

	// An empty-pool snapshot drives the cursor down through -1 to 0.
	m.sel = 10
	m, _ = stepKey(t, m, dataMsg{view: engine.EngineView{}, high: 10})
	if m.sel != 0 {
		t.Fatalf("cursor should clamp to 0 for empty pool, got %d", m.sel)
	}
	// high went backwards (10 < 42) so feedSince must not regress.
	if m.feedSince != 42 {
		t.Fatalf("feedSince regressed to %d", m.feedSince)
	}
}

func TestUpdateEditAndPanicMsgs(t *testing.T) {
	m := New(Deps{})

	// Successful edit => status set, error cleared.
	m, _ = stepKey(t, m, editDoneMsg{desc: "done thing"})
	if m.status != "done thing" || m.errLine != "" {
		t.Fatalf("ok edit: status=%q err=%q", m.status, m.errLine)
	}
	// Failed edit => error line "desc: err", status cleared.
	m, _ = stepKey(t, m, editDoneMsg{desc: "bad thing", err: errors.New("boom")})
	if m.errLine != "bad thing: boom" || m.status != "" {
		t.Fatalf("err edit: status=%q err=%q", m.status, m.errLine)
	}
	// Panic success.
	m, _ = stepKey(t, m, panicDoneMsg{})
	if !strings.Contains(m.status, "PANIC complete") || m.errLine != "" {
		t.Fatalf("panic ok: status=%q err=%q", m.status, m.errLine)
	}
	// Panic failure.
	m, _ = stepKey(t, m, panicDoneMsg{err: errors.New("nope")})
	if !strings.Contains(m.errLine, "PANIC FAILED: nope") {
		t.Fatalf("panic fail err=%q", m.errLine)
	}
}

func TestUpdateTickReschedules(t *testing.T) {
	m := New(Deps{})
	_, cmd := stepKey(t, m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tickMsg should return a batch cmd (poll + re-tick)")
	}
}

// ============================ Init / pollCmd ============================

func TestInitReturnsCmd(t *testing.T) {
	if New(Deps{}).Init() == nil {
		t.Fatal("Init should return a non-nil batch cmd")
	}
}

func TestPollCmdZero(t *testing.T) {
	// Nil engine + empty audit path => a zero snapshot, no feed, unchanged high-water.
	m := New(Deps{})
	msg, ok := m.pollCmd()().(dataMsg)
	if !ok {
		t.Fatal("pollCmd should produce a dataMsg")
	}
	if msg.view.Running || len(msg.feed) != 0 || msg.high != 0 {
		t.Fatalf("zero poll not zero: %+v", msg)
	}
}

func TestPollCmdReadsAudit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	content := "" +
		`{"ts":1000,"action":"place","coin":"BTCUP","side":"buy","status":"ok"}` + "\n" +
		`{"ts":2000,"argv":["buy","BTCUP","0.1"],"exit":0}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// since=0 => both lines, high-water = max ts + 1.
	m := New(Deps{})
	m.deps.AuditPath = path
	msg := m.pollCmd()().(dataMsg)
	if len(msg.feed) != 2 {
		t.Fatalf("want 2 feed lines, got %d: %v", len(msg.feed), msg.feed)
	}
	if msg.high != 2001 {
		t.Fatalf("high=%d want 2001", msg.high)
	}
	if !strings.Contains(msg.feed[0], "place") || !strings.Contains(msg.feed[0], "coin=BTCUP") {
		t.Fatalf("audit line not formatted: %q", msg.feed[0])
	}
	if !strings.Contains(msg.feed[1], "deliverator buy BTCUP") {
		t.Fatalf("command line not formatted: %q", msg.feed[1])
	}

	// A high-water above the first ts skips it (rows are never re-emitted).
	m.feedSince = 1500
	msg = m.pollCmd()().(dataMsg)
	if len(msg.feed) != 1 || !strings.Contains(msg.feed[0], "deliverator") {
		t.Fatalf("since filter wrong: %v", msg.feed)
	}
	if msg.high != 2001 {
		t.Fatalf("filtered high=%d want 2001", msg.high)
	}
}

// ============================ key handling: config edits ============================

func TestKeyMarketAndSizeEdits(t *testing.T) {
	rec := &keyRec{}
	deps := Deps{SetMMKey: func(k, v string) error { rec.calls++; rec.key, rec.val = k, v; return rec.err }}

	// '+' grows the active-market target from the default 6 to 7 and pushes it.
	m := New(deps)
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()
	m2, cmd := stepKey(t, m, runeKey("+"))
	if m2.maxActive != 7 {
		t.Fatalf("'+' maxActive=%d want 7", m2.maxActive)
	}
	done := cmd().(editDoneMsg)
	if rec.key != "mm.selection.max_active_markets" || rec.val != "7" {
		t.Fatalf("'+' pushed %s=%s", rec.key, rec.val)
	}
	if done.err != nil {
		t.Fatalf("'+' unexpected err: %v", done.err)
	}

	// '-' shrinks it; never below 0. The pushed value only lands when the returned
	// Cmd closure runs, so we execute it and check what was pushed.
	m3, cmd := stepKey(t, m, runeKey("-"))
	if m3.maxActive != 5 {
		t.Fatalf("'-' maxActive=%d want 5", m3.maxActive)
	}
	cmd()
	if rec.val != "5" {
		t.Fatalf("'-' should push 5, got %s", rec.val)
	}
	m.maxActive = 0
	m4, cmd := stepKey(t, m, runeKey("-"))
	if m4.maxActive != 0 {
		t.Fatalf("'-' floor breached: %d", m4.maxActive)
	}
	cmd()
	if rec.val != "0" {
		t.Fatalf("'-' at floor should still push 0, got %s", rec.val)
	}

	// 'S' grows base size from the default 1 to 2; 's' shrinks; never below 1.
	m5, cmd := stepKey(t, m, runeKey("S"))
	cmd()
	if m5.size != 2 || rec.key != "mm.strategy.base_size_shares" || rec.val != "2" {
		t.Fatalf("'S' size=%d push=%s=%s", m5.size, rec.key, rec.val)
	}
	m.size = 1
	m6, cmd := stepKey(t, m, runeKey("s"))
	cmd()
	if m6.size != 1 || rec.val != "1" {
		t.Fatalf("'s' floor: size=%d push=%s", m6.size, rec.val)
	}
}

func TestKeyPinAndBlacklistPushCSV(t *testing.T) {
	rec := &keyRec{}
	deps := Deps{SetMMKey: func(k, v string) error { rec.calls++; rec.key, rec.val = k, v; return rec.err }}
	m := New(deps)
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	// 'P' pins the selected (row 0 = BTCUP) coin and pushes the full pins csv.
	mp, cmd := stepKey(t, m, runeKey("P"))
	if len(mp.pins) != 1 || mp.pins[0] != "BTCUP" {
		t.Fatalf("pins=%v want [BTCUP]", mp.pins)
	}
	cmd()
	if rec.key != "mm.selection.pins" || rec.val != "BTCUP" {
		t.Fatalf("pin pushed %s=%s", rec.key, rec.val)
	}

	// A guarded edit whose hook errors surfaces on the error line via Update.
	rec.err = errors.New("disk full")
	mb, cmd := stepKey(t, m, runeKey("b"))
	if len(mb.blacklist) != 1 || mb.blacklist[0] != "BTCUP" {
		t.Fatalf("blacklist=%v", mb.blacklist)
	}
	msg := cmd()
	mb, _ = stepKey(t, mb, msg)
	if !strings.Contains(mb.errLine, "blacklisted BTCUP: disk full") {
		t.Fatalf("blacklist error not surfaced: %q", mb.errLine)
	}
}

// selCoin with no selectable row returns ok=false, so b/P report "no market selected".
func TestKeyEditNoSelection(t *testing.T) {
	m := New(Deps{SetMMKey: func(string, string) error { return nil }})
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = engine.EngineView{} // empty pool
	m.sel = 5                    // out of range
	if _, ok := m.selCoin(); ok {
		t.Fatal("selCoin should fail on an empty pool")
	}
	mb, cmd := stepKey(t, m, runeKey("b"))
	if mb.status != "no market selected" {
		t.Fatalf("b with no selection: status=%q", mb.status)
	}
	if cmd != nil {
		t.Fatal("b with no selection should not issue a command")
	}
}

// ============================ key handling: engine gates ============================

func TestKeyPauseToggles(t *testing.T) {
	eng := clientlessEngine(t, false)
	m := New(Deps{})
	m.deps.Engine = eng
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	m2, _ := stepKey(t, m, runeKey("p"))
	if !eng.Paused() {
		t.Fatal("'p' should pause the engine")
	}
	if !strings.Contains(m2.status, "PAUSED") {
		t.Fatalf("pause status=%q", m2.status)
	}
	m3, _ := stepKey(t, m2, runeKey("p"))
	if eng.Paused() {
		t.Fatal("second 'p' should resume")
	}
	if !strings.Contains(m3.status, "resumed") {
		t.Fatalf("resume status=%q", m3.status)
	}
}

func TestKeyPauseAndLiveUnwired(t *testing.T) {
	m := New(Deps{}) // engine nil
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()
	if mp, _ := stepKey(t, m, runeKey("p")); mp.status != "engine not wired" {
		t.Fatalf("p unwired status=%q", mp.status)
	}
	if ml, _ := stepKey(t, m, runeKey("L")); ml.status != "engine not wired" {
		t.Fatalf("L unwired status=%q", ml.status)
	}
}

func TestKeyGoLiveConfirmFlow(t *testing.T) {
	live := &boolRec{}
	deps := Deps{SetLive: func(on bool) error { live.calls++; live.on = on; return live.err }}
	m := New(deps)
	m.deps.Engine = clientlessEngine(t, false) // shadow => L arms the confirm
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	// 'L' from shadow arms the two-key confirm (does NOT flip yet).
	m2, cmd := stepKey(t, m, runeKey("L"))
	if !m2.confirmLive {
		t.Fatal("'L' from shadow should arm the go-live confirm")
	}
	if cmd != nil {
		t.Fatal("arming go-live must not run a command yet")
	}
	if !strings.Contains(m2.status, "GO LIVE") {
		t.Fatalf("arm status=%q", m2.status)
	}

	// 'y' confirms => setLiveCmd(true) runs and drives SetLive(on=true).
	m3, cmd := stepKey(t, m2, runeKey("y"))
	if m3.confirmLive {
		t.Fatal("'y' should disarm the confirm")
	}
	done := cmd().(editDoneMsg)
	if live.calls != 1 || live.on != true {
		t.Fatalf("SetLive not called with on=true: %+v", live)
	}
	if !strings.Contains(done.desc, "LIVE") || done.err != nil {
		t.Fatalf("go-live editDoneMsg=%+v", done)
	}

	// Cancel path: arm again, press a non-y key.
	m4, _ := stepKey(t, m, runeKey("L"))
	m5, cmd := stepKey(t, m4, runeKey("n"))
	if m5.confirmLive {
		t.Fatal("non-y should cancel go-live")
	}
	if !strings.Contains(m5.status, "still shadow") {
		t.Fatalf("cancel status=%q", m5.status)
	}
	if cmd != nil {
		t.Fatal("cancelled go-live should not run a command")
	}
}

func TestKeyLiveToShadowImmediate(t *testing.T) {
	live := &boolRec{}
	deps := Deps{SetLive: func(on bool) error { live.calls++; live.on = on; return live.err }}
	m := New(deps)
	m.deps.Engine = clientlessEngine(t, true) // already live => L drops to shadow, no confirm
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	m2, cmd := stepKey(t, m, runeKey("L"))
	if m2.confirmLive {
		t.Fatal("dropping to shadow needs no confirm")
	}
	done := cmd().(editDoneMsg)
	if live.calls != 1 || live.on != false {
		t.Fatalf("SetLive should be called with on=false: %+v", live)
	}
	if !strings.Contains(done.desc, "SHADOW") {
		t.Fatalf("shadow desc=%q", done.desc)
	}
}

// ============================ key handling: halt / panic / misc ============================

func TestKeyHaltToggles(t *testing.T) {
	halt := &boolRec{}
	deps := Deps{SetHalt: func(on bool) error { halt.calls++; halt.on = on; return halt.err }}
	m := New(deps)
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	m2, cmd := stepKey(t, m, runeKey("h"))
	if !m2.haltOn {
		t.Fatal("'h' should set intended halt on")
	}
	done := cmd().(editDoneMsg)
	if halt.calls != 1 || halt.on != true || !strings.Contains(done.desc, "HALT ON") {
		t.Fatalf("halt-on: rec=%+v desc=%q", halt, done.desc)
	}

	m3, cmd := stepKey(t, m2, runeKey("h"))
	if m3.haltOn {
		t.Fatal("second 'h' should clear intended halt")
	}
	done = cmd().(editDoneMsg)
	if halt.on != false || !strings.Contains(done.desc, "halt OFF") {
		t.Fatalf("halt-off: rec=%+v desc=%q", halt, done.desc)
	}
}

func TestKeyHaltUnwired(t *testing.T) {
	m := New(Deps{}) // SetHalt nil
	_, cmd := stepKey(t, m, runeKey("h"))
	done := cmd().(editDoneMsg)
	if done.err == nil || !strings.Contains(done.err.Error(), "halt not wired") {
		t.Fatalf("unwired halt should error: %+v", done)
	}
}

func TestKeyPanicConfirmFires(t *testing.T) {
	panicRec := &boolRec{}
	deps := Deps{Panic: func(context.Context) error { panicRec.calls++; return panicRec.err }}
	m := New(deps)
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	// '!' arms and clears status/error.
	m.status, m.errLine = "old", "old"
	m2, _ := stepKey(t, m, runeKey("!"))
	if !m2.confirmPanic || m2.status != "" || m2.errLine != "" {
		t.Fatalf("'!' arm: confirm=%v status=%q err=%q", m2.confirmPanic, m2.status, m2.errLine)
	}

	// 'y' fires the panic hook.
	m3, cmd := stepKey(t, m2, runeKey("y"))
	if m3.confirmPanic {
		t.Fatal("'y' should disarm panic")
	}
	if !strings.Contains(m3.status, "flattening") {
		t.Fatalf("panic firing status=%q", m3.status)
	}
	if _, ok := cmd().(panicDoneMsg); !ok {
		t.Fatal("panic should run panicCmd")
	}
	if panicRec.calls != 1 {
		t.Fatalf("Panic hook calls=%d want 1", panicRec.calls)
	}

	// Cancel path: arm, then a non-y key aborts without calling the hook.
	m4, _ := stepKey(t, m, runeKey("!"))
	m5, cmd := stepKey(t, m4, runeKey("x"))
	if m5.confirmPanic || m5.status != "panic cancelled" {
		t.Fatalf("panic cancel: confirm=%v status=%q", m5.confirmPanic, m5.status)
	}
	if cmd != nil {
		t.Fatal("cancelled panic should not run a command")
	}
}

func TestKeyCursorAndQuitAndRefresh(t *testing.T) {
	m := New(Deps{})
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView() // pool of 3

	// Arrow keys move and clamp at both ends.
	m, _ = stepKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.sel != 1 {
		t.Fatalf("down sel=%d want 1", m.sel)
	}
	for i := 0; i < 5; i++ { // walk past the end
		m, _ = stepKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.sel != 2 {
		t.Fatalf("down should clamp at 2, got %d", m.sel)
	}
	for i := 0; i < 5; i++ { // walk past the top
		m, _ = stepKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	}
	if m.sel != 0 {
		t.Fatalf("up should clamp at 0, got %d", m.sel)
	}

	// 'r' forces an immediate re-poll.
	if _, cmd := stepKey(t, m, runeKey("r")); cmd == nil {
		t.Fatal("'r' should return a poll cmd")
	} else if _, ok := cmd().(dataMsg); !ok {
		t.Fatal("'r' cmd should produce a dataMsg")
	}

	// 'q' and ctrl+c both quit.
	if _, cmd := stepKey(t, m, runeKey("q")); cmd == nil {
		t.Fatal("'q' should return a quit cmd")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("'q' cmd should be QuitMsg")
	}
	if _, cmd := stepKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("ctrl+c should return a quit cmd")
	}
}

// The footer renders its transient lines: panic-confirm banner, error, then status.
func TestRenderFooterStates(t *testing.T) {
	m := New(Deps{})
	m.w, m.h = 120, 40

	m.confirmPanic = true
	if out := m.renderFooter(120); !strings.Contains(out, "EMERGENCY FLATTEN") {
		t.Errorf("confirmPanic footer=%q", out)
	}

	m.confirmPanic = false
	m.errLine = "hook exploded"
	if out := m.renderFooter(120); !strings.Contains(out, "hook exploded") {
		t.Errorf("error footer=%q", out)
	}

	m.errLine = ""
	m.status = "all good"
	if out := m.renderFooter(120); !strings.Contains(out, "all good") {
		t.Errorf("status footer=%q", out)
	}
}
