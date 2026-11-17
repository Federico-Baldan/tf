package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func (m model) View() string {
	if m.quitting {
		return ""
	}
	switch m.stage {
	case stagePlanning:
		return m.viewProgress(false)
	case stagePrompt:
		return m.viewPrompt()
	case stageShowing:
		return m.viewShowing()
	case stageReview:
		return m.viewReview()
	case stageApplying:
		return m.viewProgress(true)
	}
	return ""
}

func (m model) spinner() string {
	return stCyan.Render(spinnerFrames[m.spin%len(spinnerFrames)])
}

// viewProgress renders the "active list" used by both plan-refresh and apply.
func (m model) viewProgress(applying bool) string {
	var b strings.Builder
	elapsed := time.Since(m.startedAt)

	title := "Planning"
	if m.cfg.sub == "destroy" {
		title = "Planning destroy"
	}
	if applying {
		title = "Applying"
		if m.cfg.sub == "destroy" {
			title = "Destroying"
		}
	}
	if m.interrupted {
		title += " (interrupting — press again to force)"
	}

	b.WriteString("\n " + m.spinner() + " " + stBold.Render(title) + "  " + stDim.Render(fmtDur(elapsed)) + "\n")

	if applying {
		b.WriteString(" " + m.progressLine(elapsed) + "\n")
	} else {
		info := []string{}
		if m.refreshed > 0 {
			info = append(info, fmt.Sprintf("%d refreshed", m.refreshed))
		}
		if m.plannedSeen > 0 {
			info = append(info, stYellow.Render(fmt.Sprintf("%d changes detected", m.plannedSeen)))
		}
		if len(info) > 0 {
			b.WriteString(" " + stDim.Render(strings.Join(info, " • ")) + "\n")
		} else {
			b.WriteString(" " + stDim.Render("refreshing state…") + "\n")
		}
	}
	b.WriteString("\n")

	// active / recently finished tasks
	maxRows := m.height - 8
	if maxRows < 3 {
		maxRows = 3
	}
	nameW := m.width - 24
	if nameW < 20 {
		nameW = 20
	}
	shown := 0
	hidden := 0
	for _, addr := range m.order {
		t := m.tasks[addr]
		if t.status == taskFailed {
			continue // failures render in the diagnostics block below
		}
		if shown >= maxRows {
			hidden++
			continue
		}
		shown++
		name := shorten(addr, nameW)
		glyph, gs := hookGlyph(t.action)
		g := gs.Render(glyph)
		switch t.status {
		case taskRunning:
			line := fmt.Sprintf("   %s %s %s %s", m.spinner(), g, stYellow.Render(name), stDim.Render(fmtDur(time.Since(t.start))))
			if t.note != "" {
				line += " " + stCyan.Render("("+t.note+")")
			}
			b.WriteString(line + "\n")
		case taskDone:
			dur := ""
			if t.elapsed > 0 {
				dur = stDim.Render(fmtDur(time.Duration(t.elapsed * float64(time.Second))))
			}
			b.WriteString(fmt.Sprintf("   %s %s %s %s\n", stGreen.Render("✓"), g, stGreen.Render(name), dur))
		}
	}
	if hidden > 0 {
		b.WriteString("   " + stDim.Render(fmt.Sprintf("… +%d more in flight", hidden)) + "\n")
	}
	if shown == 0 && hidden == 0 {
		b.WriteString("   " + stDim.Render("waiting…") + "\n")
	}

	// sticky failures + diagnostics
	b.WriteString(m.viewFailures(nameW))
	b.WriteString(m.viewDiags(4))

	return b.String()
}

func (m model) progressLine(elapsed time.Duration) string {
	if m.total == 0 {
		return stDim.Render("updating state…") // import/move only: terraform emits no per-resource events
	}
	total := m.total
	if total == 0 {
		total = 1
	}
	done := m.completed + m.failed
	barW := 24
	filled := done * barW / total
	if filled > barW {
		filled = barW
	}
	bar := stGreen.Render(strings.Repeat("█", filled)) + stDim.Render(strings.Repeat("░", barW-filled))

	pct := done * 100 / total
	parts := []string{
		bar,
		fmt.Sprintf("%d/%d", done, m.total),
		fmt.Sprintf("%d%%", pct),
	}
	running := 0
	for _, t := range m.tasks {
		if t.status == taskRunning {
			running++
		}
	}
	if running > 0 {
		parts = append(parts, stYellow.Render(fmt.Sprintf("%d active", running)))
	}
	if m.failed > 0 {
		parts = append(parts, stRed.Render(fmt.Sprintf("%d failed", m.failed)))
	}
	// naive ETA from observed completion rate
	if m.completed >= 2 && elapsed > 5*time.Second && done < total {
		rate := elapsed / time.Duration(done)
		eta := rate * time.Duration(total-done)
		parts = append(parts, stDim.Render("~"+fmtDur(eta)+" left"))
	}
	return strings.Join(parts, "  ")
}

func (m model) viewFailures(nameW int) string {
	var b strings.Builder
	for _, addr := range m.order {
		t := m.tasks[addr]
		if t.status != taskFailed {
			continue
		}
		b.WriteString(fmt.Sprintf("   %s %s %s\n", stRedBold.Render("✗"), stRed.Render(shorten(addr, nameW)), stDim.Render("failed")))
	}
	return b.String()
}

func (m model) viewDiags(max int) string {
	if len(m.diags) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	start := 0
	if len(m.diags) > max {
		start = len(m.diags) - max
		b.WriteString(" " + stDim.Render(fmt.Sprintf("(%d earlier diagnostics)", start)) + "\n")
	}
	for _, d := range m.diags[start:] {
		for _, line := range renderDiagLines(d, m.width) {
			b.WriteString(" " + line + "\n")
		}
	}
	return b.String()
}

func renderDiagLines(d *Diagnostic, width int) []string {
	mark := stRedBold.Render("✗ Error:")
	body := stRed
	if d.Severity == "warning" {
		mark = stYellow.Render("⚠ Warning:")
		body = stYellow
	}
	head := mark + " " + body.Render(d.Summary)
	if d.Address != "" {
		head += stDim.Render("  [" + d.Address + "]")
	}
	if d.Range != nil && d.Range.Filename != "" {
		head += stDim.Render(fmt.Sprintf("  %s:%d", d.Range.Filename, d.Range.Start.Line))
	}
	lines := []string{head}
	if d.Detail != "" {
		for _, dl := range strings.Split(strings.TrimSpace(d.Detail), "\n") {
			lines = append(lines, "    "+stDim.Render(truncRunes(dl, width-6)))
		}
	}
	return lines
}

func (m model) viewShowing() string {
	return "\n " + m.spinner() + " " + stBold.Render("Reading plan…") + "\n"
}

// ---- review tree ----

func (m model) viewReview() string {
	var b strings.Builder
	c := m.counts

	title := "Plan"
	if m.cfg.sub == "destroy" {
		title = "Destroy plan"
	}
	parts := []string{}
	if c.imports > 0 {
		parts = append(parts, stCyan.Render(fmt.Sprintf("↓%d import", c.imports)))
	}
	if c.add > 0 {
		parts = append(parts, stGreen.Render(fmt.Sprintf("+%d add", c.add)))
	}
	if c.change > 0 {
		parts = append(parts, stYellow.Render(fmt.Sprintf("~%d change", c.change)))
	}
	if c.replace > 0 {
		parts = append(parts, stMagenta.Render(fmt.Sprintf("±%d replace", c.replace)))
	}
	if c.destroy > 0 {
		parts = append(parts, stRed.Render(fmt.Sprintf("-%d destroy", c.destroy)))
	}
	if len(parts) == 0 {
		parts = append(parts, stDim.Render("state changes only"))
	}
	header := " " + stBold.Render(title+":") + " " + strings.Join(parts, "  ")
	if len(m.diags) > 0 {
		warns := 0
		for _, d := range m.diags {
			if d.Severity == "warning" {
				warns++
			}
		}
		if warns > 0 {
			header += "  " + stYellow.Render(fmt.Sprintf("⚠ %d", warns))
		}
	}
	b.WriteString("\n" + header + "\n " + stDim.Render(strings.Repeat("─", max(m.width-2, 1))) + "\n")

	h := m.bodyHeight()
	end := m.offset + h
	if end > len(m.rows) {
		end = len(m.rows)
	}
	for i := m.offset; i < end; i++ {
		b.WriteString(m.renderRow(i) + "\n")
	}
	for i := end - m.offset; i < h; i++ {
		b.WriteString("\n")
	}

	// footer
	var footer string
	if m.confirm {
		prompt := "Apply this plan?"
		style := stGreen
		if m.cfg.sub == "destroy" || c.destroy > 0 || c.replace > 0 {
			n := c.destroy + c.replace
			prompt = fmt.Sprintf("Apply this plan? (%s will be destroyed)", plural(n, "resource"))
			style = stRedBold
		}
		footer = " " + style.Render(prompt) + stBold.Render("  y") + stDim.Render("es / ") + stBold.Render("n") + stDim.Render("o")
	} else {
		scroll := ""
		if len(m.rows) > h {
			scroll = fmt.Sprintf(" %d/%d ", m.cursor+1, len(m.rows))
		}
		footer = " " + hint("↑↓", "move") + "  " + hint("⏎", "expand") + "  " + hint("e/c", "all") + "  " + hint("a", "apply") + "  " + hint("q", "quit") + stDim.Render(scroll)
	}
	b.WriteString(footer)
	return b.String()
}

func (m model) renderRow(i int) string {
	r := m.rows[i]
	w := m.width - 6
	if w < 20 {
		w = 20
	}
	cursorMark := "  "
	isCursor := i == m.cursor
	if isCursor {
		cursorMark = stCyan.Render("│ ")
	}

	switch r.kind {
	case rowSection:
		style := stSection
		if r.action != "" {
			_, s := actionGlyph(r.action)
			style = s.Bold(true) // section colour follows the action
		}
		return " " + style.Render(r.text)
	case rowNode:
		glyph, style := actionGlyph(r.action)
		text := truncRunes(r.text, w)
		note := ""
		if rest := w - len([]rune(text)); r.note != "" && rest >= 12 {
			note = "  (" + truncRunes(r.note, rest-5) + ")"
		}
		if isCursor {
			// highlight the whole row, not just the address
			pad := strings.Repeat(" ", max(w-len([]rune(text))-len([]rune(note)), 0))
			return " " + cursorMark + style.Render(glyph) + " " + stCursor.Render(text) + stCursorNote.Render(note+pad)
		}
		return " " + cursorMark + style.Render(glyph) + " " + text + stDim.Render(note)
	case rowDetail:
		var style lipgloss.Style
		switch r.sym {
		case '+':
			style = stGreen
		case '-':
			style = stRed
		case '~':
			style = stYellow
		default:
			style = stDim
		}
		prefix := " " + cursorMark + "    "
		if r.key == "" {
			return prefix + style.Render(string(r.sym)+" "+truncRunes(r.text, w-6))
		}
		out := prefix + style.Render(string(r.sym)) + " " + m.renderAttr(r, w-6)
		if r.forces {
			out += " " + stMagenta.Render("# forces replacement")
		}
		return out
	}
	return ""
}

// renderAttr draws "key: old → new" with the key column aligned and the old
// and new values coloured, fitting avail cells.
func (m model) renderAttr(r row, avail int) string {
	kw := max(r.keyW, len([]rune(r.key)))
	// colon hugs the key; the padding after it aligns the values
	key := r.key + stDim.Render(":") + strings.Repeat(" ", kw-len([]rune(r.key))+1)
	budget := max(avail-kw-2, 8)
	if r.sym == '~' {
		budget = max(budget-3, 8)
		// shrink only what doesn't fit, and never starve the shorter side
		ol, nl := len([]rune(r.old)), len([]rune(r.new))
		ol = min(ol, max(budget/2, budget-nl))
		nl = min(nl, budget-ol)
		return key + valStyle(r.old, stRed).Render(truncRunes(r.old, ol)) + stDim.Render(" → ") + valStyle(r.new, stGreen).Render(truncRunes(r.new, max(nl, 4)))
	}
	if r.sym == '-' {
		return key + valStyle(r.old, stRed).Render(truncRunes(r.old, budget))
	}
	return key + valStyle(r.new, stGreen).Render(truncRunes(r.new, budget))
}

// valStyle dims placeholders so real values stand out.
func valStyle(v string, s lipgloss.Style) lipgloss.Style {
	switch v {
	case "null", "(known after apply)", "(sensitive)":
		return stDim
	}
	return s
}

// ---- variable prompt ----

func (m model) viewPrompt() string {
	v := m.queue[0]
	title := "Variable required"
	if n := len(m.vars) + len(m.queue); n > 1 {
		title += fmt.Sprintf("  %d/%d", len(m.vars)+1, n)
	}
	shown := m.input
	help := "text as-is · lists and maps as HCL, e.g. [\"a\",\"b\"]"
	if v.masked() {
		shown = strings.Repeat("•", len([]rune(m.input)))
		help = "input hidden"
	}
	var b strings.Builder
	b.WriteString("\n " + stCyan.Render("?") + " " + stBold.Render(title) + "\n")
	b.WriteString("   " + stBold.Render("var."+v.name))
	if v.where != "" {
		b.WriteString("  " + stDim.Render(v.where))
	}
	b.WriteString("\n\n   " + stCyan.Render("›") + " " + shown + stCyan.Render("▏") + "\n")
	b.WriteString("   " + stDim.Render(help) + "\n\n")
	b.WriteString(" " + hint("⏎", "set") + "  " + hint("esc", "cancel"))
	return b.String()
}
