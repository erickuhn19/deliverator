package mmtui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Style palette — COPIED from internal/tui/render.go (those vars are unexported and
// coupled to the console package, so we duplicate the six colors rather than import
// the whole console surface). Keeping the exact same 256-color codes makes the two
// operator screens read as one family.
var (
	cTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	cHdr    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	cDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	cDanger = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	cSel    = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("63"))

	// Banner badges: background-filled + bold so posture reads from across the room.
	// A DRY-RUN / PAUSED / HALTED session must be impossible to miss at a glance.
	bDry    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("39"))
	bPaused = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("214"))
	bHalted = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("196"))
	bLive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("196"))
	bWarmup = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("245"))
)

func (m Model) View() string {
	w, h := m.w, m.h
	if !m.ready || w < 40 || h < 12 {
		// Pre-size render (a test constructs the model and renders once before any
		// WindowSizeMsg, or a pathologically tiny terminal): fall back to a sane
		// geometry so the real layout paths still run and never panic.
		w, h = 140, 42
	}

	banner := m.renderBanner(w)
	footer := m.renderFooter(w)
	bodyH := h - lipgloss.Height(banner) - lipgloss.Height(footer)
	if bodyH < 6 {
		bodyH = 6
	}

	// No snapshot yet: keep the banner (so posture/net still show) and say so.
	if !m.haveView {
		body := cDim.Render("starting… waiting for the first engine snapshot")
		return lipgloss.JoinVertical(lipgloss.Left, banner, "", body, footer)
	}

	// Narrow terminals: stack every panel in one column (rows self-truncate to w).
	if w < 120 {
		body := lipgloss.JoinVertical(lipgloss.Left,
			m.renderActive(w), "",
			m.renderHoldings(w), "",
			m.renderPool(w, 8), "",
			m.renderAccount(w), "",
			m.renderPnL(w), "",
			m.renderActivity(w, 6),
		)
		return lipgloss.JoinVertical(lipgloss.Left, banner, body, footer)
	}

	// Wide terminals: two columns. Left = what I'm making (active) + PnL + risk;
	// right = why these markets (pool) + the activity tail.
	leftW := w * 58 / 100
	rightW := w - leftW - 2
	left := lipgloss.JoinVertical(lipgloss.Left,
		m.renderActive(leftW), "",
		m.renderHoldings(leftW), "",
		m.renderPnL(leftW), "",
		m.renderAccount(leftW),
	)
	poolH := bodyH * 55 / 100
	actH := bodyH - poolH - 2
	if poolH < 4 {
		poolH = 4
	}
	if actH < 3 {
		actH = 3
	}
	right := lipgloss.JoinVertical(lipgloss.Left,
		m.renderPool(rightW, poolH-1), "",
		m.renderActivity(rightW, actH-1),
	)
	leftCol := lipgloss.NewStyle().Width(leftW).MaxHeight(bodyH).Render(left)
	rightCol := lipgloss.NewStyle().Width(rightW).MaxHeight(bodyH).Render(right)
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)
	return lipgloss.JoinVertical(lipgloss.Left, banner, body, footer)
}

// renderBanner is the top posture strip: the title, any active flag badges (or a
// LIVE badge when clean), and the network.
func (m Model) renderBanner(width int) string {
	v := m.view
	var badges []string
	// Posture reads from across the room: a loud red ● LIVE ● when signing, a blue
	// DRY-RUN when in shadow. Never ambiguous about whether real orders can be placed.
	if v.DryRun {
		badges = append(badges, bDry.Render(" DRY-RUN "))
	} else if v.Running {
		badges = append(badges, bLive.Render(" ● LIVE ● "))
	}
	if v.Paused {
		badges = append(badges, bPaused.Render(" PAUSED "))
	}
	if v.Halted {
		badges = append(badges, bHalted.Render(" HALTED "))
	}
	if v.Warmup {
		badges = append(badges, bWarmup.Render(" WARMUP "))
	}
	net := m.deps.Network
	if net == "" {
		net = v.Network
	}
	line := cTitle.Render("OUTCOME MARKET MAKER") + "   " + strings.Join(badges, " ") +
		"   " + cDim.Render("net ") + netStyle(net)
	_ = width // width reserved for future right-alignment; kept for a stable signature
	return line
}

// renderActive is panel §1 — "what am I making and how": one row per active market
// with fair vs mid, our two-sided quote + spread, inventory, and the gate status.
func (m Model) renderActive(width int) string {
	var b strings.Builder
	b.WriteString(cTitle.Render(fmt.Sprintf("ACTIVE MARKETS (%d)", len(m.view.Active))) + "\n")
	if len(m.view.Active) == 0 {
		b.WriteString(cDim.Render("  (none quoting)"))
		return b.String()
	}
	b.WriteString(cHdr.Render(fmt.Sprintf("  %-9s %-16s %-4s %7s  %4s %4s  %9s %5s  %9s  %s",
		"coin", "title", "undl", "expires", "fair", "mid", "bid/ask", "sprd", "inv Y/N", "gate")) + "\n")
	for _, mk := range m.view.Active {
		// Build the plain, fixed-width prefix, then append the COLORED gate via
		// rowWithTag so truncation math stays on ANSI-free text (coloring a string
		// we then rune-truncate would slice escape codes).
		left := fmt.Sprintf("  %-9s %-16s %-4s %7s  %4s %4s  %4s/%-4s %5s  %4d/%-4d",
			trunc(mk.Coin, 9), trunc(mk.Title, 16), trunc(mk.Underlying, 4),
			fmtTTL(mk.TTL), fmtProb(mk.FairP), fmtProb(mk.Mid),
			fmtProb(mk.OurBid), fmtProb(mk.OurAsk), fmtProb(mk.OurAsk-mk.OurBid),
			mk.InvYes, mk.InvNo)
		b.WriteString(rowWithTag(width, left, colorGate(mk.Gate)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPool is panel §2 — "why did it pick these": the ranked candidate pool with
// each score and include/exclude reason. A ▸ cursor marks the selected row; the
// window scrolls to keep the cursor visible. Operator edits (b/P) act on the cursor.
func (m Model) renderPool(width, maxRows int) string {
	var b strings.Builder
	b.WriteString(cTitle.Render(fmt.Sprintf("SELECTION POOL (%d)", len(m.view.Pool))) +
		cDim.Render("   ↑↓ select · b blacklist · P pin") + "\n")
	if len(m.view.Pool) == 0 {
		b.WriteString(cDim.Render("  (empty — waiting for the first scan)"))
		return b.String()
	}
	if maxRows < 1 {
		maxRows = 1
	}
	// Scroll so the cursor is always on screen.
	start := 0
	if m.sel >= maxRows {
		start = m.sel - maxRows + 1
	}
	end := start + maxRows
	if end > len(m.view.Pool) {
		end = len(m.view.Pool)
	}
	for i := start; i < end; i++ {
		c := m.view.Pool[i]
		// Truncate the plain text FIRST, then style the whole (already-sized) line —
		// so the selection highlight / eligibility color never wraps a sliced escape.
		line := trunc2(fmt.Sprintf("%-11s %6.3f  %s", c.Market.Coin, c.Score, c.Reason), width-2)
		switch {
		case i == m.sel:
			b.WriteString(cSel.Render("▸ "+line) + "\n")
		case c.Active:
			b.WriteString("  " + cOK.Render(line) + "\n")
		case strings.HasPrefix(c.Reason, "excluded"):
			b.WriteString("  " + cDim.Render(line) + "\n")
		default:
			b.WriteString("  " + line + "\n")
		}
	}
	if end < len(m.view.Pool) {
		b.WriteString(cDim.Render(fmt.Sprintf("  …%d more", len(m.view.Pool)-end)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderAccount is panel §3 — equity, the posture flags, network, and scan/tick
// freshness. It also surfaces the LOCAL control intent (target market count, halt)
// so the operator sees what they last asked for even before the next snapshot.
// renderHoldings shows EVERY outcome position the MM holds — even markets no longer in
// the active set — so exposure is never invisible. "managing" = still quoted; "held→
// settle" = dropped from selection / settling, just carried to resolution.
func (m Model) renderHoldings(width int) string {
	var b strings.Builder
	b.WriteString(cTitle.Render(fmt.Sprintf("POSITIONS (%d)", len(m.view.Holdings))) + "\n")
	if len(m.view.Holdings) == 0 {
		b.WriteString(cDim.Render("  (flat — no outcome positions)"))
		return b.String()
	}
	b.WriteString(cHdr.Render(fmt.Sprintf("  %-9s %-16s %-3s %6s %8s  %5s %5s  %s",
		"coin", "title", "sd", "shares", "value", "entry", "mark", "uPnL")) + "\n")
	for _, h := range m.view.Holdings {
		left := fmt.Sprintf("  %-9s %-16s %-3s %6d %8s  %5s %5s",
			trunc(h.Coin, 9), trunc(h.Title, 16), trunc(h.Side, 3), h.Shares,
			fmtUSD(h.Value), fmtProb(h.Entry), fmtProb(h.Mark))
		pnlc := cOK
		if h.PnL < 0 {
			pnlc = cDanger
		}
		status, sc := "managing", cOK
		if !h.Active {
			status, sc = "held→settle", cWarn
		}
		tag := pnlc.Render(fmtSignedUSD(h.PnL)) + "  " + sc.Render(status)
		b.WriteString(rowWithTag(width, left, tag) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) renderAccount(width int) string {
	v := m.view
	var b strings.Builder
	b.WriteString(cTitle.Render("ACCOUNT / RISK") + "\n")
	b.WriteString(fmt.Sprintf("  equity %s    network %s\n", fmtUSD(v.Equity), netStyle(v.Network)))
	flags := strings.Join([]string{
		flagBadge("DRY-RUN", v.DryRun, bDry),
		flagBadge("PAUSED", v.Paused, bPaused),
		flagBadge("HALTED", v.Halted, bHalted),
		flagBadge("WARMUP", v.Warmup, bWarmup),
	}, " ")
	b.WriteString("  " + flags + "\n")
	b.WriteString(cDim.Render(fmt.Sprintf("  scan %s ago · tick %s ago · target markets %d · base size %d sh · halt(intended) %v",
		fmtAge(v.LastScan), fmtAge(v.LastTick), m.maxActive, m.size, m.haltOn)))
	_ = width
	return b.String()
}

// renderPnL is panel §5 — the realized/fees/open/net split; net is colored by sign.
func (m Model) renderPnL(width int) string {
	p := m.view.PnL
	netc := cOK
	if p.Net < 0 {
		netc = cDanger
	}
	_ = width
	sess := "—"
	if !m.view.StartedAt.IsZero() {
		sess = fmtUptime(time.Since(m.view.StartedAt))
	}
	return cTitle.Render("PnL") + "\n" +
		fmt.Sprintf("  realized %s    fees %s    open %s    net %s",
			fmtSignedUSD(p.Realized), fmtUSD(p.Fees), fmtSignedUSD(p.Open),
			netc.Render(fmtSignedUSD(p.Net))) + "\n" +
		cDim.Render(fmt.Sprintf("  session %s · %d fills · %s volume", sess, p.Fills, fmtCompactUSD(p.Volume)))
}

// fmtUptime renders a session duration at a resolution that stays useful whether the
// bot has run for seconds or hours.
func fmtUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// fmtCompactUSD renders a USD notional compactly (k/M) so large session volume fits.
func fmtCompactUSD(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.1fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// renderActivity is panel §4 — the tail of the audit log, with the engine's
// LastError pinned at the top in red when present (the one thing an operator most
// needs to see immediately).
func (m Model) renderActivity(width, rows int) string {
	var b strings.Builder
	b.WriteString(cTitle.Render("ACTIVITY") + "\n")
	if m.view.LastError != "" {
		b.WriteString("  " + cDanger.Render(trunc2("last error: "+m.view.LastError, width-2)) + "\n")
	}
	if len(m.feed) == 0 {
		b.WriteString(cDim.Render("  (no audit activity yet)"))
		return strings.TrimRight(b.String(), "\n")
	}
	if rows < 1 {
		rows = 1
	}
	lines := m.feed
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	for _, ln := range lines {
		b.WriteString("  " + trunc2(ln, width-2) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderFooter is the help strip + the transient status / error / panic-confirm line.
func (m Model) renderFooter(width int) string {
	help := cDim.Render("↑↓ move · L live · p pause · s/S size · +/- markets · b blacklist · P pin · h halt · ! panic · r refresh · q quit")
	var msg string
	switch {
	case m.confirmPanic:
		msg = "\n" + bHalted.Render(" EMERGENCY FLATTEN ") + " " +
			cDanger.Render("press y to confirm · any other key cancels")
	case m.errLine != "":
		msg = "\n" + cDanger.Render(trunc2(m.errLine, width))
	case m.status != "":
		msg = "\n" + cWarn.Render(trunc2(m.status, width))
	}
	div := cDim.Render(strings.Repeat("─", clampWidth(width)))
	return "\n" + div + "\n" + help + msg
}

// ----- small formatters / helpers -----

// flagBadge renders a posture flag: a filled badge when on, dim text when off.
func flagBadge(label string, on bool, style lipgloss.Style) string {
	if on {
		return style.Render(" " + label + " ")
	}
	return cDim.Render(label)
}

// colorGate colors the per-market gate string by severity: quoting = go (green),
// settled = done (dim), anything gating quotes (blackout / stale / inv cap / paused)
// = attention (amber).
func colorGate(g string) string {
	if strings.TrimSpace(g) == "" {
		return cDim.Render("—")
	}
	switch lg := strings.ToLower(g); {
	case strings.Contains(lg, "quoting"):
		return cOK.Render(g)
	case strings.Contains(lg, "settled"):
		return cDim.Render(g)
	default: // blackout / stale / inv cap / paused / anything else
		return cWarn.Render(g)
	}
}

// netStyle colors the network: mainnet reads as live/real, testnet as a warning.
func netStyle(net string) string {
	net = strings.TrimSpace(net)
	if net == "" {
		return cDim.Render("?")
	}
	if strings.EqualFold(net, "mainnet") {
		return cOK.Render("mainnet")
	}
	return cWarn.Render(net + " (test)")
}

// rowWithTag appends a (possibly ANSI-colored) tag to a plain prefix, truncating the
// prefix so the visible line fits width. It uses lipgloss.Width for the tag so a
// colored tag still measures by its visible cells, not its escape bytes.
func rowWithTag(width int, left, tag string) string {
	avail := width - lipgloss.Width(tag) - 1
	if avail < 0 {
		avail = 0
	}
	return trunc2(left, avail) + " " + tag
}

// fmtProb renders a probability in (0,1) to 2 dp; a non-positive value (no
// quote/mid) shows an em-dash so absence reads distinctly from a real 0.
func fmtProb(v float64) string {
	if v <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

// fmtTTL renders a countdown compactly (h/m/s), flagging a past expiry.
func fmtTTL(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// fmtAge renders how long ago a timestamp was, or "never" for a zero time.
func fmtAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func fmtUSD(v float64) string { return fmt.Sprintf("$%.2f", v) }

// fmtSignedUSD keeps the sign OUTSIDE the dollar sign (-$12.34), the convention a
// trader expects for a PnL figure.
func fmtSignedUSD(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// trunc hard-cuts a string to n runes (no ellipsis) — for fixed-width table cells
// where fmt then pads the field.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// trunc2 truncates to w runes with an ellipsis — for free-form lines (feed, reasons)
// where the cut should read as "there's more". Copied from the console's truncate.
func trunc2(s string, w int) string {
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

// clampWidth bounds the divider width to a sane range (mirrors the console's
// divWidth) so a bogus terminal size can't blow up the rule.
func clampWidth(w int) int {
	if w < 8 || w > 220 {
		return 100
	}
	return w
}
