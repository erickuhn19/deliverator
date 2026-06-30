// Package tui implements `deliverator console` — a human-in-the-loop "mission
// control" screen. The agent drives execution through the normal JSON CLI; this
// screen is the small surface the operator owns: view the risk envelope + live
// utilization and edit it (the agent may not), watch a live activity feed, and
// glance at equity/positions. It holds NO risk/correctness logic — reads go through
// core.ClientAPI and the guarded cap-edit runs through an injected closure that
// reuses the exact `config set` safety path.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/state"
)

const (
	dataInterval = 5 * time.Second // portfolio refresh — gentle on the per-IP weight budget
	feedInterval = 700 * time.Millisecond
	feedMax      = 500 // ring-buffer cap on retained feed lines
)

// Deps is everything the TUI needs, injected by cmd so this package never imports
// cmd (no cycle / layering break) and stays unit-testable with fakes.
type Deps struct {
	Client     core.ClientAPI // reads: RiskStatus, Snapshot
	AuditPath  string         // signed-action trail JSONL (may be "")
	CommandLog string         // command-log JSONL (may be "")
	Network    string
	// SetCap runs the guarded config edit (load fresh → setConfigKey → Validate →
	// atomic Save) and returns the prior value + whether it was a risk cap. All the
	// safety/atomic-write logic lives behind this closure in cmd/config.
	SetCap func(key, val string) (old string, isRiskCap bool, err error)
	// DMSArmed reports the dead-man's-switch status for the status bar (read offline).
	DMSArmed func() (armed bool, secs int)
}

// ----- messages -----

type (
	tickMsg     time.Time
	feedTickMsg time.Time
)

type dataMsg struct {
	risk    *core.RiskView
	riskErr error
	pf      *core.PortfolioView
	pfErr   error
}

type feedMsg struct {
	lines []string
	high  int64
}

type editDoneMsg struct {
	key, val, old string
	isRiskCap     bool
	err           error
}

// ----- model -----

type editPhase int

const (
	browsing   editPhase = iota
	typing               // operator entering a new value
	confirming           // operator must confirm the operator-approved change
)

// Model is the bubbletea model. The bubbletea runtime is the only goroutine that
// touches it; tea.Cmds run off-thread and return msgs, so there is no shared
// mutable state to race.
type Model struct {
	deps Deps

	risk      *core.RiskView      // last good RiskStatus
	pf        *core.PortfolioView // last good Portfolio (equity / positions)
	feed      []string            // formatted feed lines, ring-buffered (oldest first)
	feedSince int64               // high-water ts (ms) for incremental ReadSince

	sel   int // selected cap row (only risk caps are selectable/editable)
	phase editPhase
	input textinput.Model

	status      string // last status / operator-approved warning line
	lastErr     string
	degraded    bool // last data refresh failed (keeping last-good data)
	rateLimited bool // last failure was a transient 429 (expected; shown calmly, not red)
	lastRefresh time.Time

	w, h  int
	ready bool
}

// New builds the initial model.
func New(d Deps) Model {
	ti := textinput.New()
	ti.Prompt = "» "
	ti.CharLimit = 24
	return Model{deps: d, input: ti}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.feedCmd(), dataTickCmd(), feedTickCmd())
}

func dataTickCmd() tea.Cmd {
	return tea.Tick(dataInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func feedTickCmd() tea.Cmd {
	return tea.Tick(feedInterval, func(t time.Time) tea.Msg { return feedTickMsg(t) })
}

// refreshCmd fetches the portfolio ONCE off the UI thread and derives both the
// account panel and the risk view from it (RiskStatusFromPortfolio does no I/O) —
// one API round-trip per tick, not two, to stay well under the per-IP weight budget.
func (m Model) refreshCmd() tea.Cmd {
	c := m.deps.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		pf, perr := c.Portfolio(ctx)
		if perr != nil {
			return dataMsg{riskErr: perr, pfErr: perr}
		}
		return dataMsg{risk: c.RiskStatusFromPortfolio(pf), pf: pf}
	}
}

// feedCmd polls the command-log + audit JSONL since the high-water ts, merges by
// ts, and returns formatted lines. Uses state.ReadSince (public) and advances the
// high-water to max+1 so rows are never re-emitted (a row written at the exact same
// ms as the prior poll's last row across a poll boundary is negligibly rare).
func (m Model) feedCmd() tea.Cmd {
	since := m.feedSince
	paths := []string{m.deps.AuditPath, m.deps.CommandLog}
	return func() tea.Msg {
		type row struct {
			ts   int64
			line string
		}
		var rows []row
		high := since
		for _, p := range paths {
			if p == "" {
				continue
			}
			rs, err := state.ReadSince(p, since)
			if err != nil {
				continue
			}
			for _, r := range rs {
				ts := int64(0)
				if v, ok := r["ts"].(float64); ok {
					ts = int64(v)
				}
				rows = append(rows, row{ts: ts, line: state.FormatLogEntry(r)})
				if ts >= high {
					high = ts + 1
				}
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ts < rows[j].ts })
		lines := make([]string, len(rows))
		for i, r := range rows {
			lines[i] = r.line
		}
		return feedMsg{lines: lines, high: high}
	}
}

// setCapCmd runs the guarded edit off-thread.
func (m Model) setCapCmd(key, val string) tea.Cmd {
	set := m.deps.SetCap
	return func() tea.Msg {
		old, isRisk, err := set(key, val)
		return editDoneMsg{key: key, val: val, old: old, isRiskCap: isRisk, err: err}
	}
}

func (m *Model) selectableCaps() []core.RiskCap {
	if m.risk == nil {
		return nil
	}
	return m.risk.Caps
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ready = true
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.refreshCmd(), dataTickCmd())
	case feedTickMsg:
		return m, tea.Batch(m.feedCmd(), feedTickCmd())

	case dataMsg:
		m.lastRefresh = time.Now()
		if msg.risk != nil {
			m.risk = msg.risk
			if m.sel >= len(m.risk.Caps) {
				m.sel = len(m.risk.Caps) - 1
			}
		}
		if msg.pf != nil {
			m.pf = msg.pf
		}
		err := msg.pfErr
		if err == nil {
			err = msg.riskErr
		}
		switch {
		case err == nil:
			m.lastErr, m.rateLimited, m.degraded = "", false, false
		case isRateLimit(err):
			// Transient + expected (shared per-IP weight budget): keep last-good
			// data and show it calmly, don't flash a red error.
			m.lastErr, m.rateLimited, m.degraded = "", true, true
		default:
			m.lastErr, m.rateLimited, m.degraded = "data: "+err.Error(), false, true
		}
		return m, nil

	case feedMsg:
		if msg.high > m.feedSince {
			m.feedSince = msg.high
		}
		if len(msg.lines) > 0 {
			m.feed = append(m.feed, msg.lines...)
			if len(m.feed) > feedMax {
				m.feed = m.feed[len(m.feed)-feedMax:]
			}
		}
		return m, nil

	case editDoneMsg:
		m.phase = browsing
		m.input.Blur()
		if msg.err != nil {
			m.lastErr = "edit rejected: " + msg.err.Error()
			m.status = ""
		} else {
			m.lastErr = ""
			if msg.isRiskCap {
				m.status = fmt.Sprintf("risk cap changed: %s %s → %s — confirm the account operator approved this safety-limit change.", msg.key, msg.old, msg.val)
			} else {
				m.status = fmt.Sprintf("set %s = %s", msg.key, msg.val)
			}
			return m, m.refreshCmd() // reflect the persisted value
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward other messages to the text input while editing (cursor blink, etc.).
	if m.phase == typing {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.phase {
	case typing:
		switch msg.String() {
		case "esc":
			m.phase = browsing
			m.input.Blur()
			return m, nil
		case "enter":
			val := strings.TrimSpace(m.input.Value())
			if val == "" {
				m.phase = browsing
				m.input.Blur()
				return m, nil
			}
			m.phase = confirming
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case confirming:
		switch msg.String() {
		case "y", "Y":
			caps := m.selectableCaps()
			if m.sel < 0 || m.sel >= len(caps) {
				m.phase = browsing
				return m, nil
			}
			key := caps[m.sel].Key
			val := strings.TrimSpace(m.input.Value())
			m.phase = browsing
			m.input.Blur()
			m.status = "applying…"
			return m, m.setCapCmd(key, val)
		default: // any other key cancels
			m.phase = browsing
			m.input.Blur()
			m.status = "edit cancelled"
			return m, nil
		}

	default: // browsing
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			return m, m.refreshCmd()
		case "up", "k":
			if m.sel > 0 {
				m.sel--
			}
			return m, nil
		case "down", "j":
			if n := len(m.selectableCaps()); m.sel < n-1 {
				m.sel++
			}
			return m, nil
		case "e", "enter":
			caps := m.selectableCaps()
			if m.sel >= 0 && m.sel < len(caps) {
				// Empty input + the current value as placeholder, so typing REPLACES
				// (rather than appending to a pre-filled value). Empty + enter = cancel.
				m.input.SetValue("")
				m.input.Placeholder = "new value (current " + caps[m.sel].Value + ")"
				m.input.Focus()
				m.phase = typing
				m.lastErr = ""
				m.status = ""
			}
			return m, nil
		}
	}
	return m, nil
}

// isRateLimit reports whether err is a transient Hyperliquid rate-limit (HTTP 429 /
// per-IP weight) — an expected, self-recovering condition the console shows calmly
// rather than as a red failure.
func isRateLimit(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") || strings.Contains(s, "per-ip") ||
		strings.Contains(s, "rate limit") || strings.Contains(s, "rate-limit")
}

// Run builds and runs the console TUI program until the operator quits.
func Run(ctx context.Context, d Deps) error {
	p := tea.NewProgram(New(d), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}
