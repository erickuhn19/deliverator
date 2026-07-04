package mmtui

// These tests exercise the pure render + key-handling paths WITHOUT a running
// bubbletea program: a Model renders View() and processes Update() as plain method
// calls, so we can assert "constructs + renders without panicking" against both a
// zero and a populated EngineView (the two cases the dashboard must survive).

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm/engine"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	"github.com/erickuhn19/deliverator/internal/mm/selector"
)

// TestViewZeroNoPanic: a freshly constructed model (no snapshot, no window size)
// must render the "starting…" screen without panicking.
func TestViewZeroNoPanic(t *testing.T) {
	m := New(Deps{Network: "mainnet"})
	out := m.View()
	if out == "" {
		t.Fatal("View() returned empty for zero model")
	}
	if !strings.Contains(out, "starting") {
		t.Fatalf("expected a starting screen before the first snapshot, got:\n%s", out)
	}
}

// sampleView is a representative, non-trivial snapshot touching every panel.
func sampleView() engine.EngineView {
	return engine.EngineView{
		Running: true,
		DryRun:  true,
		Network: "mainnet",
		Equity:  12500.42,
		Active: []engine.MarketView{{
			Coin: "BTCUP", Title: "BTC > 70k by Fri", Underlying: "BTC",
			TTL: 92 * time.Minute, FairP: 0.62, FairConf: 0.8, Mid: 0.60,
			BestBid: 0.59, BestAsk: 0.61, OurBid: 0.58, OurAsk: 0.64,
			InvYes: 12, InvNo: 0, Gate: "quoting",
		}, {
			Coin: "ETHUP", Title: "ETH > 4k", Underlying: "ETH",
			TTL: 8 * time.Minute, FairP: 0.41, Mid: 0.40, Gate: "blackout (8m to expiry)",
		}},
		Pool: []selector.Candidate{
			{Market: core.Market{Coin: "BTCUP"}, Score: 0.81, Eligible: true, Active: true, Reason: "active"},
			{Market: core.Market{Coin: "ETHUP"}, Score: 0.44, Eligible: true, Reason: "eligible"},
			{Market: core.Market{Coin: "DOGEUP"}, Score: 0, Reason: "excluded: settled"},
		},
		PnL:      oms.PnLView{Realized: 120.5, Fees: 4.25, Open: -12.0, Net: 104.25},
		LastScan: time.Now().Add(-30 * time.Second),
		LastTick: time.Now().Add(-1 * time.Second),
		Warmup:   false,
	}
}

// TestViewSampleNoPanic: with a window size + a populated snapshot, the full
// multi-panel layout must render (wide and narrow) and include recognizable content.
func TestViewSampleNoPanic(t *testing.T) {
	m := New(Deps{Network: "mainnet"})
	m.ready = true
	m.haveView = true
	m.view = sampleView()

	for _, sz := range []struct{ w, h int }{{160, 48}, {90, 30}} {
		m.w, m.h = sz.w, sz.h
		out := m.View()
		if !strings.Contains(out, "MARKET MAKER") {
			t.Fatalf("[%dx%d] missing banner title:\n%s", sz.w, sz.h, out)
		}
		if !strings.Contains(out, "BTCUP") {
			t.Fatalf("[%dx%d] missing active market row:\n%s", sz.w, sz.h, out)
		}
		if !strings.Contains(out, "PnL") {
			t.Fatalf("[%dx%d] missing PnL panel:\n%s", sz.w, sz.h, out)
		}
	}
}

// TestUpdateKeysNoPanic: key handling with nil hooks must never panic and must move
// the cursor / arm the panic confirm as specified.
func TestUpdateKeysNoPanic(t *testing.T) {
	m := New(Deps{Network: "mainnet"}) // all hooks nil, engine nil
	m.ready, m.haveView, m.w, m.h = true, true, 160, 48
	m.view = sampleView()

	key := func(s string) tea.Msg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

	// Cursor down then up.
	mm, _ := m.Update(key("j"))
	got := mm.(Model)
	if got.sel != 1 {
		t.Fatalf("expected cursor at 1 after 'j', got %d", got.sel)
	}
	mm, _ = got.Update(key("k"))
	got = mm.(Model)
	if got.sel != 0 {
		t.Fatalf("expected cursor back at 0 after 'k', got %d", got.sel)
	}

	// Guarded edits with nil hooks return a command; running it must not panic and
	// should surface a wiring error rather than crashing.
	mm, cmd := got.Update(key("b"))
	got = mm.(Model)
	if len(got.blacklist) != 1 || got.blacklist[0] != "BTCUP" {
		t.Fatalf("expected BTCUP appended to blacklist, got %v", got.blacklist)
	}
	if cmd != nil {
		if _, ok := cmd().(editDoneMsg); !ok {
			t.Fatal("expected editDoneMsg from a guarded edit command")
		}
	}

	// Panic requires two keys: '!' arms, 'y' fires (nil hook → panicDoneMsg error).
	mm, _ = got.Update(key("!"))
	got = mm.(Model)
	if !got.confirmPanic {
		t.Fatal("expected panic confirm to arm after '!'")
	}
	mm, cmd = got.Update(key("y"))
	got = mm.(Model)
	if got.confirmPanic {
		t.Fatal("expected panic confirm to disarm after 'y'")
	}
	if cmd != nil {
		if _, ok := cmd().(panicDoneMsg); !ok {
			t.Fatal("expected panicDoneMsg from the panic command")
		}
	}

	// Rendering after all that must still be clean.
	if got.View() == "" {
		t.Fatal("View() empty after key handling")
	}
}
