package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/erickuhn19/deliverator/internal/core"
)

var (
	cTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	cHdr    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	cDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	cDanger = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	cSel    = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("63"))
)

func (m Model) View() string {
	if !m.ready {
		return "loading mission control…"
	}
	status := m.renderStatus()
	bodyH := m.h - lipgloss.Height(status)
	if bodyH < 8 {
		bodyH = 8
	}

	// Narrow terminals: stack everything (original single-column layout).
	if m.w < 110 {
		risk, acct := m.renderRisk(), m.renderAccount()
		feedH := bodyH - lipgloss.Height(risk) - lipgloss.Height(acct) - 2
		if feedH < 3 {
			feedH = 3
		}
		return lipgloss.JoinVertical(lipgloss.Left, risk, acct, m.renderFeed(feedH), status)
	}

	// Wide terminals: two panes — risk + account on the left, activity on the right.
	leftW := m.w * 48 / 100
	if leftW < 66 {
		leftW = 66
	}
	rightW := m.w - leftW - 2
	left := lipgloss.JoinVertical(lipgloss.Left, m.renderRisk(), "", m.renderAccount())
	right := m.renderFeedCol(bodyH, rightW)
	leftCol := lipgloss.NewStyle().Width(leftW).MaxHeight(bodyH).Render(left)
	rightCol := lipgloss.NewStyle().Width(rightW).MaxHeight(bodyH).Render(right)
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)
	return lipgloss.JoinVertical(lipgloss.Left, body, status)
}

// renderFeedCol renders the activity feed sized for the right pane: the last
// (height-1) lines, each truncated to the column width (feed lines carry no ANSI,
// so rune-length truncation is exact).
func (m Model) renderFeedCol(height, width int) string {
	var b strings.Builder
	b.WriteString(cTitle.Render("ACTIVITY") + "\n")
	if len(m.feed) == 0 {
		b.WriteString(cDim.Render("  (waiting for activity — set state.command_log to also see commands)"))
		return b.String()
	}
	maxLines := height - 1
	if maxLines < 1 {
		maxLines = 1
	}
	lines := m.feed
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for _, ln := range lines {
		b.WriteString("  " + truncate(ln, width-2) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncate(s string, w int) string {
	if w < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

func (m Model) renderRisk() string {
	var b strings.Builder
	b.WriteString(cTitle.Render("RISK ENVELOPE") + cDim.Render("   ↑↓ select · e edit · r refresh · q quit") + "\n")
	if m.risk == nil {
		b.WriteString(cDim.Render("  loading…"))
		return b.String()
	}
	for i, rc := range m.risk.Caps {
		name := fmt.Sprintf("%-22s", rc.Label)
		if i == m.sel {
			name = cSel.Render(name)
		}
		val := fmt.Sprintf("%9s %-4s", rc.Value, rc.Unit)
		cur, util := "", ""
		if rc.Current != nil {
			cur = "cur " + fmtNum(*rc.Current, rc.Unit)
			if rc.UtilPct != nil {
				util = utilCell(*rc.UtilPct)
			}
		} else if !rc.Active {
			cur = cDim.Render("off")
		}
		prefix := "  "
		if i == m.sel {
			prefix = "› "
		}
		b.WriteString(fmt.Sprintf("%s%s %s  %-18s %s\n", prefix, name, val, cur, util))
	}
	switch m.phase {
	case typing:
		b.WriteString("\n" + cWarn.Render("edit "+m.curKey()+" = ") + m.input.View() + cDim.Render("   (enter to review · esc cancel)"))
	case confirming:
		b.WriteString("\n" + cDanger.Render(fmt.Sprintf("risk cap changed: %s %s → %s", m.curKey(), m.curVal(), strings.TrimSpace(m.input.Value()))))
		b.WriteString("\n" + cWarn.Render("Deliverator does NOT block this. Press y to confirm the account operator approved this change; any other key cancels."))
	}
	return b.String()
}

func (m Model) renderAccount() string {
	var b strings.Builder
	b.WriteString("\n" + cTitle.Render("ACCOUNT") + "\n")
	if m.pf == nil {
		b.WriteString(cDim.Render("  loading…"))
		return b.String()
	}
	equity := ""
	if m.risk != nil {
		equity = m.risk.Equity
	}
	b.WriteString(fmt.Sprintf("  equity %s   margin ratio %s   maint %s\n",
		dollars(equity), pctRatio(m.pf.MarginRatio), dollars(m.pf.MaintenanceMargin)))
	var open []core.PositionView
	for _, p := range m.pf.Positions {
		if p.Side == "flat" || p.Side == "" {
			continue
		}
		open = append(open, p)
	}
	if len(open) == 0 {
		b.WriteString(cDim.Render("  (flat)"))
		return b.String()
	}
	b.WriteString(cHdr.Render(fmt.Sprintf("  %-12s %-5s %13s %13s %9s", "coin", "side", "notional", "uPnL", "dist-liq")) + "\n")
	for _, p := range open {
		dl := "—"
		if p.DistanceToLiqPct != "" {
			dl = p.DistanceToLiqPct + "%"
		}
		b.WriteString(fmt.Sprintf("  %-12s %-5s %13s %13s %9s\n",
			p.Coin, p.Side, dollars(p.PositionValue), p.UnrealizedPnl, dl))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) renderFeed(maxLines int) string {
	var b strings.Builder
	b.WriteString("\n" + cTitle.Render("ACTIVITY") + "\n")
	lines := m.feed
	if len(lines) == 0 {
		b.WriteString(cDim.Render("  (waiting for activity — set state.command_log to also see commands)"))
		return b.String()
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for _, ln := range lines {
		b.WriteString("  " + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) renderStatus() string {
	parts := []string{"net:" + m.deps.Network}
	if m.risk != nil {
		if m.risk.Halted {
			parts = append(parts, cDanger.Render("HALTED"))
		} else {
			parts = append(parts, cOK.Render("live"))
		}
		if m.risk.DrawdownPct > 0 {
			parts = append(parts, fmt.Sprintf("dd %.1f%%", m.risk.DrawdownPct))
		}
		if m.risk.DailyLossUSD > 0 {
			parts = append(parts, fmt.Sprintf("day -$%.0f", m.risk.DailyLossUSD))
		}
	}
	if m.deps.DMSArmed != nil {
		if armed, secs := m.deps.DMSArmed(); armed {
			parts = append(parts, fmt.Sprintf("DMS %ds", secs))
		} else {
			parts = append(parts, cDim.Render("DMS off"))
		}
	}
	if !m.lastRefresh.IsZero() {
		parts = append(parts, cDim.Render(fmt.Sprintf("refreshed %ds ago", int(time.Since(m.lastRefresh).Seconds()))))
	}
	if m.degraded {
		parts = append(parts, cWarn.Render("degraded"))
	}
	bar := strings.Join(parts, "  ·  ")
	msgLine := ""
	if m.lastErr != "" {
		msgLine = "\n" + cDanger.Render(m.lastErr)
	} else if m.status != "" {
		msgLine = "\n" + cWarn.Render(m.status)
	}
	return "\n" + cDim.Render(strings.Repeat("─", divWidth(m.w))) + "\n" + bar + msgLine
}

// ----- small formatters -----

func (m Model) curKey() string {
	if m.risk != nil && m.sel >= 0 && m.sel < len(m.risk.Caps) {
		return m.risk.Caps[m.sel].Key
	}
	return ""
}

func (m Model) curVal() string {
	if m.risk != nil && m.sel >= 0 && m.sel < len(m.risk.Caps) {
		return m.risk.Caps[m.sel].Value
	}
	return ""
}

func fmtNum(v float64, unit string) string {
	switch unit {
	case "x":
		return fmt.Sprintf("%.2fx", v)
	case "pct":
		return fmt.Sprintf("%.1f%%", v)
	case "count":
		return fmt.Sprintf("%.0f", v)
	default: // usd
		return fmt.Sprintf("$%.0f", v)
	}
}

func utilCell(pct float64) string {
	const w = 12
	if pct < 0 {
		pct = 0
	}
	filled := int(pct/100*float64(w) + 0.5)
	if filled > w {
		filled = w
	}
	label := strings.Repeat("█", filled) + strings.Repeat("░", w-filled) + fmt.Sprintf(" %3.0f%%", pct)
	switch {
	case pct >= 100:
		return cDanger.Render(label)
	case pct >= 80:
		return cWarn.Render(label)
	default:
		return cOK.Render(label)
	}
}

func dollars(s string) string {
	if s == "" {
		return "—"
	}
	return "$" + s
}

func pctRatio(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func divWidth(w int) int {
	if w < 8 || w > 200 {
		return 80
	}
	return w
}
