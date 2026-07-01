package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/erickuhn19/deliverator/internal/core"
)

// fakeTUIClient stubs the two reads the TUI makes.
type fakeTUIClient struct{ core.ClientAPI }

func (fakeTUIClient) Portfolio(ctx context.Context) (*core.PortfolioView, error) {
	return &core.PortfolioView{AccountValue: "1000"}, nil
}

func (fakeTUIClient) RiskStatusFromPortfolio(pf *core.PortfolioView) *core.RiskView {
	return riskWithCaps()
}

func TestRefreshCmd(t *testing.T) {
	m := New(Deps{Client: fakeTUIClient{}})
	msg := m.refreshCmd()()
	d, ok := msg.(dataMsg)
	if !ok || d.risk == nil || d.pf == nil || d.riskErr != nil || d.pfErr != nil {
		t.Fatalf("refreshCmd msg=%+v ok=%v", msg, ok)
	}
}

func TestFeedCmd(t *testing.T) {
	dir := t.TempDir()
	cmdLog := filepath.Join(dir, "cmds.jsonl")
	audit := filepath.Join(dir, "audit.jsonl")
	_ = os.WriteFile(cmdLog, []byte(`{"ts":1000,"argv":["mids"],"exit":0,"ok":true}`+"\n"), 0o600)
	_ = os.WriteFile(audit, []byte(`{"ts":2000,"action":"order","coin":"BTC","status":"filled"}`+"\n"), 0o600)
	m := New(Deps{AuditPath: audit, CommandLog: cmdLog})
	fm, ok := m.feedCmd()().(feedMsg)
	if !ok || len(fm.lines) != 2 {
		t.Fatalf("feedCmd lines: %+v", fm)
	}
	if fm.high <= 2000 {
		t.Errorf("high should advance past 2000, got %d", fm.high)
	}
	// merged + sorted by ts: command (1000) before audit (2000)
	if !strings.Contains(fm.lines[0], "mids") || !strings.Contains(fm.lines[1], "order") {
		t.Errorf("feed not merged/sorted by ts: %v", fm.lines)
	}
}

func TestInitAndTicks(t *testing.T) {
	m := New(Deps{Client: fakeTUIClient{}})
	if m.Init() == nil {
		t.Error("Init should return a batch command")
	}
	if _, cmd := upd(t, m, tickMsg(time.Time{})); cmd == nil {
		t.Error("tickMsg should re-arm + refresh")
	}
	if _, cmd := upd(t, m, feedTickMsg(time.Time{})); cmd == nil {
		t.Error("feedTickMsg should re-arm + poll")
	}
}

func TestTypingForwardsToInput(t *testing.T) {
	m, _ := upd(t, New(Deps{}), dataMsg{risk: riskWithCaps()})
	m, _ = upd(t, m, rk('e')) // typing, input seeded with "10"
	m, _ = upd(t, m, rk('5')) // a digit forwards to the text input
	if !strings.Contains(m.input.Value(), "5") {
		t.Errorf("typed char should reach the input, got %q", m.input.Value())
	}
}

func upd(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	tm, cmd := m.Update(msg)
	return tm.(Model), cmd
}

func rk(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func riskWithCaps() *core.RiskView {
	return &core.RiskView{Equity: "1000", Caps: []core.RiskCap{
		{Key: "risk.max_leverage", Label: "Max leverage", Unit: "x", Value: "10", Active: true},
		{Key: "risk.max_net_exposure_usd", Label: "Net exposure", Unit: "usd", Value: "5000", Active: true},
	}}
}

func TestModelWindowSize(t *testing.T) {
	m, _ := upd(t, New(Deps{}), tea.WindowSizeMsg{Width: 100, Height: 40})
	if !m.ready || m.w != 100 || m.h != 40 {
		t.Fatalf("window size not applied: %+v", m)
	}
}

func TestModelDataMsg(t *testing.T) {
	m, _ := upd(t, New(Deps{}), dataMsg{risk: riskWithCaps(), pf: &core.PortfolioView{AccountValue: "1000"}})
	if m.risk == nil || m.pf == nil || m.degraded {
		t.Fatalf("data not stored / wrongly degraded: %+v", m)
	}
	// a partial failure flags degraded but keeps last-good data
	m, _ = upd(t, m, dataMsg{riskErr: errors.New("boom"), pf: &core.PortfolioView{AccountValue: "2000"}})
	if !m.degraded || m.risk == nil || m.lastErr == "" {
		t.Errorf("partial failure: want degraded + last-good kept + error: %+v", m)
	}
}

func TestModelFeedMerge(t *testing.T) {
	m, _ := upd(t, New(Deps{}), feedMsg{lines: []string{"a", "b"}, high: 100})
	if len(m.feed) != 2 || m.feedSince != 100 {
		t.Fatalf("feed=%v since=%d", m.feed, m.feedSince)
	}
	big := make([]string, feedMax+50)
	m, _ = upd(t, m, feedMsg{lines: big, high: 200})
	if len(m.feed) != feedMax {
		t.Errorf("feed ring should cap at %d, got %d", feedMax, len(m.feed))
	}
	if m.feedSince != 200 {
		t.Errorf("feedSince should advance to 200, got %d", m.feedSince)
	}
}

func TestModelEditConfirmCallsSetCapOnce(t *testing.T) {
	calls := 0
	var gotKey, gotVal string
	deps := Deps{SetCap: func(key, val string) (string, bool, error) {
		calls++
		gotKey, gotVal = key, val
		return "10", true, nil
	}}
	m, _ := upd(t, New(deps), dataMsg{risk: riskWithCaps()}) // sel=0 -> max_leverage
	m, _ = upd(t, m, rk('e'))                                // browsing -> typing
	if m.phase != typing {
		t.Fatalf("phase=%v want typing", m.phase)
	}
	m.input.SetValue("7")
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // typing -> confirming
	if m.phase != confirming {
		t.Fatalf("phase=%v want confirming", m.phase)
	}
	m, cmd := upd(t, m, rk('y')) // confirm -> setCap command
	if m.phase != browsing {
		t.Errorf("phase=%v want browsing", m.phase)
	}
	if cmd == nil {
		t.Fatal("confirm should return a setCap command")
	}
	msg := cmd() // run it: calls SetCap, returns editDoneMsg
	if calls != 1 || gotKey != "risk.max_leverage" || gotVal != "7" {
		t.Fatalf("SetCap calls=%d key=%q val=%q", calls, gotKey, gotVal)
	}
	done, ok := msg.(editDoneMsg)
	if !ok {
		t.Fatalf("want editDoneMsg, got %T", msg)
	}
	m, _ = upd(t, m, done)
	if m.status == "" {
		t.Error("a successful risk-cap edit should set the operator-approved status line")
	}
}

func TestModelEditCancelDoesNotCallSetCap(t *testing.T) {
	calls := 0
	deps := Deps{SetCap: func(key, val string) (string, bool, error) { calls++; return "", false, nil }}
	m, _ := upd(t, New(deps), dataMsg{risk: riskWithCaps()})
	m, _ = upd(t, m, rk('e'))
	m.input.SetValue("7")
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // confirming
	m, _ = upd(t, m, rk('n'))                        // any non-y cancels
	if m.phase != browsing {
		t.Errorf("phase=%v want browsing after cancel", m.phase)
	}
	if calls != 0 {
		t.Errorf("cancel must not call SetCap, got %d", calls)
	}
}

func TestModelEditEscFromTyping(t *testing.T) {
	m, _ := upd(t, New(Deps{}), dataMsg{risk: riskWithCaps()})
	m, _ = upd(t, m, rk('e'))
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.phase != browsing {
		t.Errorf("esc should cancel typing, phase=%v", m.phase)
	}
}

func TestModelEditErrorSurfaces(t *testing.T) {
	m, _ := upd(t, New(Deps{}), dataMsg{risk: riskWithCaps()})
	m, _ = upd(t, m, editDoneMsg{key: "risk.max_leverage", val: "-1", err: errors.New("must be >= 0")})
	if m.lastErr == "" || m.phase != browsing {
		t.Errorf("edit error should surface + return to browsing: %+v", m)
	}
}

func TestModelNavAndQuit(t *testing.T) {
	m, _ := upd(t, New(Deps{}), dataMsg{risk: riskWithCaps()})
	m, _ = upd(t, m, rk('j')) // down
	if m.sel != 1 {
		t.Errorf("down should select row 1, got %d", m.sel)
	}
	m, _ = upd(t, m, rk('k')) // up
	if m.sel != 0 {
		t.Errorf("up should select row 0, got %d", m.sel)
	}
	_, cmd := upd(t, m, rk('q'))
	if cmd == nil {
		t.Fatal("q should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q should map to tea.Quit")
	}
}

func TestModelView(t *testing.T) {
	m, _ := upd(t, New(Deps{Network: "mainnet"}), tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = upd(t, m, dataMsg{
		risk: riskWithCaps(),
		pf: &core.PortfolioView{Positions: []core.PositionView{
			{Coin: "BTC", Side: "short", PositionValue: "640", UnrealizedPnl: "-5", DistanceToLiqPct: "12.3"},
		}},
	})
	m, _ = upd(t, m, feedMsg{lines: []string{"15:04:05  $ deliverator positions  → ok"}, high: 1})
	v := m.View()
	for _, want := range []string{"RISK ENVELOPE", "ACCOUNT", "ACTIVITY", "Max leverage", "BTC", "deliverator positions"} {
		if !strings.Contains(v, want) {
			t.Errorf("View missing %q\n---\n%s", want, v)
		}
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Errorf("short string should be unchanged: %q", truncate("hello", 10))
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("truncate to 5 = %q want hell…", got)
	}
	if truncate("x", 0) != "" {
		t.Error("zero width should be empty")
	}
}

func TestViewLayouts(t *testing.T) {
	risk := riskWithCaps()
	pf := &core.PortfolioView{Positions: []core.PositionView{
		{Coin: "BTC", Side: "short", PositionValue: "640", UnrealizedPnl: "-5", DistanceToLiqPct: "12.3"},
	}}
	longFeed := []string{"15:04:05  $ deliverator " + strings.Repeat("x", 200) + " positions"}

	// Wide (>=110): two-pane layout exercises renderFeedCol + long-line truncation.
	mw, _ := upd(t, New(Deps{Network: "mainnet"}), tea.WindowSizeMsg{Width: 160, Height: 40})
	mw, _ = upd(t, mw, dataMsg{risk: risk, pf: pf})
	mw, _ = upd(t, mw, feedMsg{lines: longFeed, high: 1})
	vw := mw.View()
	if !strings.Contains(vw, "RISK ENVELOPE") || !strings.Contains(vw, "ACTIVITY") || !strings.Contains(vw, "ACCOUNT") {
		t.Error("wide view missing a panel")
	}
	if !strings.Contains(vw, "…") {
		t.Error("a long feed line should be truncated with an ellipsis in the right pane")
	}

	// Narrow (<110): stacked layout exercises the renderFeed path.
	mn, _ := upd(t, New(Deps{Network: "mainnet"}), tea.WindowSizeMsg{Width: 90, Height: 40})
	mn, _ = upd(t, mn, dataMsg{risk: risk, pf: pf})
	mn, _ = upd(t, mn, feedMsg{lines: []string{"a line"}, high: 1})
	vn := mn.View()
	if !strings.Contains(vn, "RISK ENVELOPE") || !strings.Contains(vn, "ACTIVITY") {
		t.Error("narrow view missing a panel")
	}
}

func TestModelRateLimitSoftError(t *testing.T) {
	// seed last-good data
	m, _ := upd(t, New(Deps{}), dataMsg{risk: riskWithCaps(), pf: &core.PortfolioView{AccountValue: "1"}})
	// a 429 must be a calm, transient rate-limit: no red error, last-good kept
	m, _ = upd(t, m, dataMsg{pfErr: errors.New("Hyperliquid returned 429 (per-IP weight exceeded)"), riskErr: errors.New("429")})
	if !m.rateLimited || m.lastErr != "" {
		t.Errorf("429 should be a soft rate-limit (rateLimited, no lastErr): rateLimited=%v lastErr=%q", m.rateLimited, m.lastErr)
	}
	if m.risk == nil || m.pf == nil {
		t.Error("rate-limit must keep last-good data")
	}
	// a genuine error is shown as a red error, not a rate-limit
	m, _ = upd(t, m, dataMsg{pfErr: errors.New("connection refused")})
	if m.rateLimited || m.lastErr == "" {
		t.Errorf("a non-429 error should surface as a red error: rateLimited=%v lastErr=%q", m.rateLimited, m.lastErr)
	}
	// recovery clears both
	m, _ = upd(t, m, dataMsg{risk: riskWithCaps(), pf: &core.PortfolioView{}})
	if m.rateLimited || m.lastErr != "" || m.degraded {
		t.Errorf("a good refresh should clear error state: %+v", m)
	}
}

// riskWithPosture adds the operator posture rows after the two caps, so the flat
// row order is: [0]cap max_leverage, [1]cap net_exposure, [2]outcomes(bool),
// [3]allowed_coins(list,empty), [4]perp_dexs(list,"xyz").
func riskWithPosture() *core.RiskView {
	rv := riskWithCaps()
	rv.Posture = []core.PostureSetting{
		{Key: "outcomes", Label: "Outcome markets", Type: "bool", Value: "false"},
		{Key: "automation.allowed_coins", Label: "Allowed coins", Type: "list", Value: ""},
		{Key: "perp_dexs", Label: "Sub-dexes (HIP-3)", Type: "list", Value: "xyz"},
	}
	return rv
}

func navDown(t *testing.T, m Model, n int) Model {
	t.Helper()
	for i := 0; i < n; i++ {
		m, _ = upd(t, m, rk('j'))
	}
	return m
}

// A boolean posture row skips the text-entry step: `e` flips the value and goes
// straight to confirm; `y` applies the flipped value exactly once.
func TestPostureBoolTogglesAndConfirms(t *testing.T) {
	var calls int
	var gotKey, gotVal string
	deps := Deps{SetCap: func(key, val string) (string, bool, error) {
		calls++
		gotKey, gotVal = key, val
		return "false", false, nil // not a risk cap
	}}
	m, _ := upd(t, New(deps), dataMsg{risk: riskWithPosture()})
	m = navDown(t, m, 2) // -> outcomes (index 2)
	m, _ = upd(t, m, rk('e'))
	if m.phase != confirming {
		t.Fatalf("bool edit should jump to confirming, phase=%d", m.phase)
	}
	if m.editKey != "outcomes" || m.input.Value() != "true" {
		t.Fatalf("expected outcomes flipped to true, key=%q val=%q", m.editKey, m.input.Value())
	}
	_, cmd := upd(t, m, rk('y'))
	if cmd == nil {
		t.Fatal("confirm should return a setCap command")
	}
	cmd()
	if calls != 1 || gotKey != "outcomes" || gotVal != "true" {
		t.Fatalf("SetCap calls=%d key=%q val=%q", calls, gotKey, gotVal)
	}
}

// A list posture row takes free text; a non-empty value applies as typed.
func TestPostureListEditApplies(t *testing.T) {
	var gotKey, gotVal string
	deps := Deps{SetCap: func(key, val string) (string, bool, error) { gotKey, gotVal = key, val; return "", false, nil }}
	m, _ := upd(t, New(deps), dataMsg{risk: riskWithPosture()})
	m = navDown(t, m, 4) // -> perp_dexs (index 4)
	m, _ = upd(t, m, rk('e'))
	if m.phase != typing {
		t.Fatalf("list edit should enter typing, phase=%d", m.phase)
	}
	for _, r := range "ab" {
		m, _ = upd(t, m, rk(r))
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // -> confirming
	if m.phase != confirming {
		t.Fatalf("non-empty list edit should confirm, phase=%d", m.phase)
	}
	_, cmd := upd(t, m, rk('y'))
	cmd()
	if gotKey != "perp_dexs" || gotVal != "ab" {
		t.Fatalf("SetCap key=%q val=%q, want perp_dexs=ab", gotKey, gotVal)
	}
}

// Empty input on a LIST row is a valid clear (proceed to confirm), unlike a numeric
// cap where empty cancels.
func TestPostureListEmptyClears(t *testing.T) {
	var gotVal string
	called := false
	deps := Deps{SetCap: func(key, val string) (string, bool, error) { called = true; gotVal = val; return "xyz", false, nil }}
	m, _ := upd(t, New(deps), dataMsg{risk: riskWithPosture()})
	m = navDown(t, m, 4) // perp_dexs (has "xyz")
	m, _ = upd(t, m, rk('e'))
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // empty + enter
	if m.phase != confirming {
		t.Fatalf("empty list edit should confirm (clear), phase=%d", m.phase)
	}
	_, cmd := upd(t, m, rk('y'))
	cmd()
	if !called || gotVal != "" {
		t.Fatalf("clear should call SetCap with empty, called=%v val=%q", called, gotVal)
	}
}

// A numeric cap still cancels on empty+enter (regression guard for the split path).
func TestNumericCapEmptyCancels(t *testing.T) {
	called := false
	deps := Deps{SetCap: func(key, val string) (string, bool, error) { called = true; return "", true, nil }}
	m, _ := upd(t, New(deps), dataMsg{risk: riskWithPosture()}) // sel=0 = a cap
	m, _ = upd(t, m, rk('e'))
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // empty + enter
	if m.phase != browsing {
		t.Fatalf("empty numeric edit should cancel to browsing, phase=%d", m.phase)
	}
	if called {
		t.Error("cancel must not call SetCap")
	}
}

// The posture section renders (on/off + list) and its confirm is a plain change,
// not the loud risk-cap warning.
func TestPostureRenders(t *testing.T) {
	m, _ := upd(t, New(Deps{}), tea.WindowSizeMsg{Width: 90, Height: 40})
	m, _ = upd(t, m, dataMsg{risk: riskWithPosture(), pf: &core.PortfolioView{AccountValue: "1000"}})
	v := m.View()
	if !strings.Contains(v, "POSTURE") || !strings.Contains(v, "Outcome markets") {
		t.Fatalf("posture section not rendered:\n%s", v)
	}
	// Confirm on a posture bool must NOT say "risk cap changed".
	m = navDown(t, m, 2)
	m, _ = upd(t, m, rk('e')) // -> confirming
	cv := m.View()
	if strings.Contains(cv, "risk cap changed") || !strings.Contains(cv, "change posture") {
		t.Fatalf("posture confirm should be plain, got:\n%s", cv)
	}
}
