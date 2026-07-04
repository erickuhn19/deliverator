// Package mmtui implements the outcome market maker's full-screen dashboard
// (spec §8) — the operator's "what am I making, why these markets, and what's my
// risk right now" surface for the running quoting engine.
//
// It is a SIBLING of `internal/tui` (the console/mission-control screen) and is
// deliberately shaped the same way — same bubbletea Model/New/Init/Update/View/Run
// skeleton, same lipgloss style palette, same key-handling + alt-screen full-screen
// pattern. Those styles are unexported and console-coupled over there, so they are
// COPIED here rather than imported (importing internal/tui would drag in the whole
// console client surface for six color vars). The two screens serve different
// engines and must not share mutable state.
//
// Like the console, this package holds NO trading/risk/correctness logic: it READS
// a consistent snapshot via Engine.View() on a tick and drives every mutation
// through injected, guarded closures (Panic/SetHalt/SetMMKey) that reuse the exact
// same safety paths the CLI uses. The dashboard never talks to the exchange itself.
package mmtui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/mm/engine"
	"github.com/erickuhn19/deliverator/internal/state"
)

const (
	// pollInterval drives the whole screen. Engine.View() is an in-memory snapshot
	// under a mutex (cheap) and the audit tail is a bounded incremental read, so a
	// half-second cadence keeps the dashboard live without any exchange I/O — the
	// engine goroutine, not the TUI, owns the per-IP weight budget.
	pollInterval = 500 * time.Millisecond
	// feedMax ring-buffers the audit tail so a long-running session can't grow the
	// activity feed without bound.
	feedMax = 400
)

// Deps wires the dashboard to the running engine + guarded control hooks. The
// closures are injected by cmd so this package never imports cmd (no cycle) and
// stays unit-testable with nil/fake hooks — a nil hook degrades to a status line,
// it never panics.
type Deps struct {
	Engine   *engine.Engine                  // live state via Engine.View(); pause via SetPaused/Paused; Live/SetLive
	Panic    func(ctx context.Context) error // emergency flatten (wraps core panic) — bound to a key with confirm
	SetHalt  func(on bool) error             // global halt on/off (wraps core.SetHalt)
	SetMMKey func(key, val string) error     // guarded [mm] config edit + save (e.g. "mm.selection.max_active_markets","8")
	// SetLive flips runtime signing on/off (the money switch) AND persists mm.enabled/
	// mm.dry_run so a restart keeps the state. Bound to the `L` key behind a confirm.
	SetLive   func(on bool) error
	Network   string
	AuditPath string
	// Selection / Strategy are the current config, used to SEED the local mirrors
	// (pins/blacklist/max-active/size) so an edit MERGES with what's on disk instead
	// of overwriting it.
	Selection config.MMSelection
	Strategy  config.MMStrategy
}

// ----- messages -----
//
// Every off-thread unit of work returns exactly one of these; the bubbletea
// runtime is the only goroutine that mutates the Model, so there is no shared
// mutable state to race (same discipline as the console).

type tickMsg time.Time

// dataMsg is one poll's result: a fresh engine snapshot plus any NEW audit lines
// since the high-water ts (so rows are never re-emitted).
type dataMsg struct {
	view engine.EngineView
	feed []string
	high int64
}

// editDoneMsg is the result of a guarded config/halt edit run off-thread. desc is
// the human summary to surface on success; err (if any) is shown, never fatal.
type editDoneMsg struct {
	desc string
	err  error
}

// panicDoneMsg is the result of the emergency-flatten hook.
type panicDoneMsg struct{ err error }

// ----- model -----

// Model is the bubbletea model. Only the runtime touches it; tea.Cmds run off the
// UI thread and hand results back as msgs.
type Model struct {
	deps Deps
	ctx  context.Context // program context, threaded into the panic hook (may be nil in tests)

	view     engine.EngineView // last good snapshot (zero value renders "starting…")
	haveView bool              // true once the first snapshot lands

	feed      []string // formatted audit lines, ring-buffered (oldest first)
	feedSince int64    // high-water ts (ms) for the incremental ReadSince tail

	sel int // cursor into view.Pool (the "why these markets" panel)

	// Locally tracked control intent. We deliberately do NOT read config back
	// (keeps the dashboard dependency-light and the edits idempotent-ish): we track
	// what WE last asked for and push absolute values through the guarded hooks.
	blacklist    []string // mm.selection.blacklist, seeded from config + session appends
	pins         []string // mm.selection.pins, seeded from config + session appends
	maxActive    int      // local mirror of mm.selection.max_active_markets (clamped >=0)
	size         int64    // local mirror of mm.strategy.base_size_shares (clamped >=1)
	haltOn       bool     // intended global-halt state (SetHalt is fire-and-forget)
	confirmPanic bool     // two-key emergency-flatten confirm is armed (press ! then y)
	confirmLive  bool     // two-key go-LIVE confirm is armed (press L when shadow, then y)

	status  string // transient success/info line (last guarded edit, pause, etc.)
	errLine string // last hook error (shown calmly in red, never crashes the UI)

	w, h  int
	ready bool
}

// New builds the initial model, seeding the pin/blacklist/max-active mirrors from the
// current config so edits merge with (never overwrite) the on-disk selection lists.
func New(d Deps) Model {
	max := d.Selection.MaxActiveMarkets
	if max <= 0 {
		max = 6
	}
	size := d.Strategy.BaseSizeShares
	if size < 1 {
		size = 1
	}
	return Model{
		deps:      d,
		maxActive: max,
		size:      size,
		blacklist: append([]string(nil), d.Selection.Blacklist...),
		pins:      append([]string(nil), d.Selection.Pins...),
	}
}

func (m Model) Init() tea.Cmd {
	// Poll immediately AND start the tick, so the first frame has data fast.
	return tea.Batch(m.pollCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// pollCmd grabs a fresh engine snapshot and the incremental audit tail OFF the UI
// thread. Engine.View() copies under a lock (never races the quoting loop) and the
// tail advances a high-water ts so each row is formatted exactly once. A nil engine
// (tests) yields a zero view; a read error drops that tick's lines rather than
// failing — the dashboard must degrade quietly, never hard-fail on a bad log line.
func (m Model) pollCmd() tea.Cmd {
	eng := m.deps.Engine
	path := m.deps.AuditPath
	since := m.feedSince
	return func() tea.Msg {
		var view engine.EngineView
		if eng != nil {
			view = eng.View()
		}
		var lines []string
		high := since
		if path != "" {
			if rows, err := state.ReadSince(path, since); err == nil {
				for _, r := range rows {
					ts := int64(0)
					if v, ok := r["ts"].(float64); ok {
						ts = int64(v)
					}
					lines = append(lines, state.FormatLogEntry(r))
					// Advance to max+1 so the same row is never re-emitted next tick.
					if ts >= high {
						high = ts + 1
					}
				}
			}
		}
		return dataMsg{view: view, feed: lines, high: high}
	}
}

// setKeyCmd runs a guarded [mm] config edit off-thread. desc is the summary shown on
// success. A nil hook (unwired/tests) is reported, not fatal.
func (m Model) setKeyCmd(key, val, desc string) tea.Cmd {
	set := m.deps.SetMMKey
	return func() tea.Msg {
		if set == nil {
			return editDoneMsg{desc: desc, err: errors.New("config edit not wired")}
		}
		return editDoneMsg{desc: desc, err: set(key, val)}
	}
}

// setLiveCmd flips runtime signing on/off off-thread via the SetLive hook (which also
// persists mm.enabled/mm.dry_run and, when going shadow, cancels resting quotes).
func (m Model) setLiveCmd(on bool) tea.Cmd {
	set := m.deps.SetLive
	return func() tea.Msg {
		if set == nil {
			return editDoneMsg{err: errors.New("live toggle not wired")}
		}
		desc := "→ SHADOW (dry-run) — resting quotes cancelled"
		if on {
			desc = "● LIVE — signing enabled"
		}
		return editDoneMsg{desc: desc, err: set(on)}
	}
}

// setHaltCmd flips global halt off-thread and reports the outcome.
func (m Model) setHaltCmd(on bool) tea.Cmd {
	set := m.deps.SetHalt
	desc := "global halt OFF — engine may quote again"
	if on {
		desc = "global HALT ON — all trading gated"
	}
	return func() tea.Msg {
		if set == nil {
			return editDoneMsg{desc: desc, err: errors.New("halt not wired")}
		}
		return editDoneMsg{desc: desc, err: set(on)}
	}
}

// panicCmd runs the emergency flatten off-thread using the program context (falling
// back to Background if the model was built without one, e.g. in a test).
func (m Model) panicCmd() tea.Cmd {
	fn := m.deps.Panic
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		if fn == nil {
			return panicDoneMsg{err: errors.New("panic not wired")}
		}
		return panicDoneMsg{err: fn(ctx)}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ready = true
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.pollCmd(), tickCmd())

	case dataMsg:
		m.view = msg.view
		m.haveView = true
		if msg.high > m.feedSince {
			m.feedSince = msg.high
		}
		if len(msg.feed) > 0 {
			m.feed = append(m.feed, msg.feed...)
			if len(m.feed) > feedMax {
				m.feed = m.feed[len(m.feed)-feedMax:]
			}
		}
		// Keep the cursor in-bounds as the pool churns each scan.
		if n := len(m.view.Pool); m.sel >= n {
			m.sel = n - 1
		}
		if m.sel < 0 {
			m.sel = 0
		}
		return m, nil

	case editDoneMsg:
		if msg.err != nil {
			m.errLine = msg.desc + ": " + msg.err.Error()
			m.status = ""
		} else {
			m.status = msg.desc
			m.errLine = ""
		}
		return m, nil

	case panicDoneMsg:
		if msg.err != nil {
			m.errLine = "PANIC FAILED: " + msg.err.Error()
		} else {
			m.status = "PANIC complete — positions flattened"
			m.errLine = ""
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Two-key emergency-flatten confirm takes priority over everything: once armed
	// (`!`), the NEXT key is either `y` (fire) or a cancel. This guarantees the
	// destructive action can never happen on a single fat-fingered key.
	if m.confirmPanic {
		m.confirmPanic = false
		if s := msg.String(); s == "y" || s == "Y" {
			m.status = "flattening — sending emergency panic…"
			return m, m.panicCmd()
		}
		m.status = "panic cancelled"
		return m, nil
	}
	// Two-key GO-LIVE confirm: arming happens on `L` (from shadow); the next key is
	// `y` to actually enable real signing, or a cancel.
	if m.confirmLive {
		m.confirmLive = false
		if s := msg.String(); s == "y" || s == "Y" {
			m.status = "GOING LIVE — real orders will be placed"
			return m, m.setLiveCmd(true)
		}
		m.status = "go-live cancelled — still shadow"
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "r": // force an immediate re-poll
		return m, m.pollCmd()

	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
		return m, nil
	case "down", "j":
		if m.sel < len(m.view.Pool)-1 {
			m.sel++
		}
		return m, nil

	case "p": // pause / resume quoting (in-memory, cheap — do it inline)
		if m.deps.Engine == nil {
			m.status = "engine not wired"
			return m, nil
		}
		np := !m.deps.Engine.Paused()
		m.deps.Engine.SetPaused(np)
		if np {
			m.status = "PAUSED — resting quotes cancel on the next tick"
		} else {
			m.status = "resumed — quotes rebuild on the next tick"
		}
		return m, nil

	case "b": // blacklist the selected pool market (append + push the full csv)
		coin, ok := m.selCoin()
		if !ok {
			m.status = "no market selected"
			return m, nil
		}
		m.blacklist = appendUnique(m.blacklist, coin)
		csv := strings.Join(m.blacklist, ",")
		return m, m.setKeyCmd("mm.selection.blacklist", csv, "blacklisted "+coin)

	case "P": // pin the selected market (append + push the full csv, so earlier pins survive)
		coin, ok := m.selCoin()
		if !ok {
			m.status = "no market selected"
			return m, nil
		}
		m.pins = appendUnique(m.pins, coin)
		return m, m.setKeyCmd("mm.selection.pins", strings.Join(m.pins, ","), "pinned "+coin)

	case "+", "=": // grow the active set
		m.maxActive++
		return m, m.setKeyCmd("mm.selection.max_active_markets", strconv.Itoa(m.maxActive),
			fmt.Sprintf("max active markets → %d", m.maxActive))
	case "-", "_": // shrink the active set (never below 0)
		if m.maxActive > 0 {
			m.maxActive--
		}
		return m, m.setKeyCmd("mm.selection.max_active_markets", strconv.Itoa(m.maxActive),
			fmt.Sprintf("max active markets → %d", m.maxActive))

	case "S": // grow the base order size (shares at ladder level 0)
		m.size++
		return m, m.setKeyCmd("mm.strategy.base_size_shares", strconv.FormatInt(m.size, 10),
			fmt.Sprintf("base size → %d shares", m.size))
	case "s": // shrink the base order size (never below 1)
		if m.size > 1 {
			m.size--
		}
		return m, m.setKeyCmd("mm.strategy.base_size_shares", strconv.FormatInt(m.size, 10),
			fmt.Sprintf("base size → %d shares", m.size))

	case "L": // toggle live signing (the money switch)
		if m.deps.Engine == nil {
			m.status = "engine not wired"
			return m, nil
		}
		if m.deps.Engine.Live() {
			// Going back to shadow is the SAFE direction — no confirm; it also cancels
			// resting quotes inside SetLive.
			return m, m.setLiveCmd(false)
		}
		m.confirmLive = true
		m.status = "GO LIVE and place REAL orders? press y to confirm · any other key cancels"
		m.errLine = ""
		return m, nil

	case "h": // toggle global halt (guarded hook; local flag tracks intent)
		m.haltOn = !m.haltOn
		return m, m.setHaltCmd(m.haltOn)

	case "!": // arm the emergency-flatten confirm
		m.confirmPanic = true
		m.status = ""
		m.errLine = ""
		return m, nil
	}
	return m, nil
}

// selCoin returns the coin of the currently selected pool candidate.
func (m Model) selCoin() (string, bool) {
	if m.sel < 0 || m.sel >= len(m.view.Pool) {
		return "", false
	}
	return m.view.Pool[m.sel].Market.Coin, true
}

// appendUnique appends x to xs unless a case-insensitive match is already present
// (so re-blacklisting the same coin doesn't bloat the csv).
func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return xs
		}
	}
	return append(xs, x)
}

// Run renders the dashboard full-screen until ctx is cancelled or the user quits.
// Mirrors the console: alt-screen + a program bound to ctx, so a SIGINT-derived
// context cancel (wired by cmd) tears the program down cleanly. The program's
// context is threaded into the model for the panic hook.
func Run(ctx context.Context, d Deps) error {
	m := New(d)
	m.ctx = ctx
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}
