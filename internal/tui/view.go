package tui

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/validate"
)

var (
	purple = lipgloss.Color("#6C5CE7")
	lilac  = lipgloss.Color("#A78BFA")
	green  = lipgloss.Color("#04B575")
	red    = lipgloss.Color("#E53E3E")
	yellow = lipgloss.Color("#FFEAA7")
	cyan   = lipgloss.Color("#48D1CC")
	dim    = lipgloss.Color("#696969")

	sTitle    = lipgloss.NewStyle().Bold(true).Foreground(lilac)
	sDim      = lipgloss.NewStyle().Foreground(dim)
	sGreen    = lipgloss.NewStyle().Foreground(green)
	sRed      = lipgloss.NewStyle().Foreground(red)
	sYellow   = lipgloss.NewStyle().Foreground(yellow)
	sCyan     = lipgloss.NewStyle().Foreground(cyan)
	sBold     = lipgloss.NewStyle().Bold(true)
	sSelected = lipgloss.NewStyle().Background(lipgloss.Color("#2D3748"))
	sStatus   = lipgloss.NewStyle().Foreground(lipgloss.Color("#DDD6FE")).Background(lipgloss.Color("#1A202C"))
)

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	bodyH := m.height - 1
	leftW := m.width * 45 / 100
	rightW := m.width - leftW

	inputsH := len(m.inputs) + len(m.issues) + len(m.activeResults()) + 5
	switch m.editing {
	case editValue:
		inputsH += m.editor.Height()
	case editKey, editCaseName:
		inputsH++
	case editPick:
		inputsH += len(m.pickOptions)
	}
	inputsH = max(min(inputsH, bodyH*2/3), 5)
	nodeH := bodyH - inputsH

	testsH := max(min(len(m.cases)+3, bodyH/3), 4)
	flowH := bodyH - testsH
	left := lipgloss.JoinVertical(lipgloss.Left,
		pane(m.flowTitle(), m.flowLines(leftW-4, flowH-3), leftW, flowH, m.focus == focusFlow),
		pane("[2] Tests", m.caseLines(leftW-4, testsH-3), leftW, testsH, m.focus == focusTests),
	)
	right := lipgloss.JoinVertical(lipgloss.Left,
		pane("[3] "+m.nodeTitle(), m.scrolledNodeLines(rightW-4, nodeH-3), rightW, nodeH, m.focus == focusNode),
		pane("[4] "+m.inputsTitle(), m.inputLines(rightW-4), rightW, inputsH, m.focus == focusInputs),
	)
	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, left, right),
		m.statusBar(),
	)
}

// pane draws a bordered box of exactly w×h cells.
func pane(title string, lines []string, w, h int, active bool) string {
	border := dim
	if active {
		border = purple
	}
	innerW, innerH := w-4, h-3
	if len(lines) > innerH {
		lines = lines[:max(innerH, 0)]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, innerW, "…")
	}
	body := sTitle.Render(title) + "\n" + strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(w - 2).
		Height(h - 2).
		Render(body)
}

// flowLines draws the flow as a tree from the start node, one row per line.
func (m *Model) flowLines(w, h int) []string {
	if m.flow == nil {
		return []string{sRed.Render(fmt.Sprint(m.loadErr))}
	}
	if m.flowView == viewPath && m.run != nil && len(m.run.path) > 0 {
		lines, rowLine := m.pathLines()
		if c := min(m.cursor, len(rowLine)-1); c >= 0 {
			if top := rowLine[c] - h + 3; top > 0 {
				lines = lines[top:]
			}
		}
		return lines
	}
	rows := m.layout()
	var lines []string
	for i, r := range rows {
		lines = append(lines, m.rowLine(r, i == m.cursor))
	}
	if r := m.run; r != nil && r.done && r.err == nil && len(r.path) > 0 {
		lines = append(lines, "", m.outcomeLine()+sDim.Render("   v path view"))
	}
	if m.loadErr != nil {
		lines = append([]string{sRed.Render("reload failed: " + m.loadErr.Error()), ""}, lines...)
	}
	// Keep the cursor in view.
	if top := m.cursor - h + 3; top > 0 {
		lines = lines[top:]
	}
	return lines
}

// outcomeLine names the node the run ended on and what it produced.
func (m *Model) outcomeLine() string {
	last := m.run.path[len(m.run.path)-1]
	s := sBold.Render("Outcome: ") + last
	if n := m.flow.Node(last); n != nil {
		ev, _ := m.eventFor(last)
		s += "  " + nodeDetail(n, ev, true)
	}
	return s
}

func (m *Model) eventFor(node string) (runner.Event, bool) {
	if m.run == nil {
		return runner.Event{}, false
	}
	ev, ok := m.run.events[node]
	return ev, ok
}

func (m *Model) statusIcon(node string) string {
	r := m.run
	switch {
	case r == nil:
		return sDim.Render("○")
	case r.errNode == node:
		return sRed.Render("✗")
	case r.paused == node:
		return sYellow.Render("⏸")
	case r.current == node:
		return sYellow.Render("◐")
	case slices.Contains(r.path, node):
		return sGreen.Render("●")
	case r.done:
		return sDim.Render("·")
	}
	return sDim.Render("○")
}

func answer(d backend.Decision) string {
	switch d.Kind {
	case flow.Bool:
		if yes, _ := d.Answer.(bool); yes {
			return fmt.Sprintf("yes %.2f", d.Probs["yes"])
		}
		return fmt.Sprintf("no %.2f", d.Probs["no"])
	case flow.Score:
		v, _ := d.Answer.(float64)
		return fmt.Sprintf("%.2f", v)
	}
	return tmpl.Format(d.Answer)
}

// selected is the node on the cursor's row of the Flow pane.
func (m *Model) selected() *flow.Node {
	rows := m.rows()
	if len(rows) == 0 {
		return nil
	}
	return m.flow.Node(rows[min(m.cursor, len(rows)-1)].node)
}

func (m *Model) nodeTitle() string {
	n := m.selected()
	if n == nil {
		return "Node"
	}
	t := badge(n) + " " + n.ID
	if n.Kind.IsDecision() {
		b := n.Backend
		if b == "" {
			b = m.backend
		}
		t += fmt.Sprintf(" · %s · threshold %.2f", b, m.flow.ThresholdFor(n))
	}
	return t
}

// scrolledNodeLines applies the Node pane's scroll, keeping a marker when
// there is more above or below.
func (m *Model) scrolledNodeLines(w, h int) []string {
	lines := m.nodeLines(w)
	m.nodeScroll = max(min(m.nodeScroll, len(lines)-h), 0)
	lines = lines[m.nodeScroll:]
	if m.nodeScroll > 0 {
		lines = append([]string{sDim.Render(fmt.Sprintf("↑ %d more", m.nodeScroll))}, lines...)
	}
	if len(lines) > h && h > 0 {
		more := len(lines) - h + 1
		lines = append(lines[:h-1], sDim.Render(fmt.Sprintf("↓ %d more", more)))
	}
	return lines
}

func (m *Model) nodeLines(w int) []string {
	n := m.selected()
	if n == nil {
		return nil
	}
	ev, ran := m.eventFor(n.ID)
	var lines []string

	if m.run != nil && m.run.errNode == n.ID {
		lines = append(lines, sRed.Render("✗ "+m.run.err.Error()), "")
	}

	if n.Kind == flow.Action {
		return append(lines, m.actionLines(n, ev, ran, w)...)
	}

	var prevEv runner.Event
	var hasPrev bool
	if m.prev != nil {
		prevEv, hasPrev = m.prev.events[n.ID]
	}
	for i, q := range n.Questions {
		if len(n.Questions) > 1 {
			lines = append(lines, sBold.Render(q.Name))
		}
		if ran && i < len(ev.Asked) {
			lines = append(lines, wrap(ev.Asked[i], w)...)
		} else {
			for _, l := range wrap(q.Text, w) {
				lines = append(lines, sDim.Render(l))
			}
		}
		if !ran || i >= len(ev.Decisions) {
			lines = append(lines, sDim.Render("not run"), "")
			continue
		}
		d := ev.Decisions[i]
		lines = append(lines, "")
		lines = append(lines, bars(q, d, w)...)
		lines = append(lines, m.confidenceLine(n, d))
		if hasPrev && i < len(prevEv.Decisions) {
			p := prevEv.Decisions[i]
			line := fmt.Sprintf("previous run: %s  conf %.2f", answer(p), p.Confidence)
			if changed(p, d) {
				lines = append(lines, sYellow.Render(line+"  ← changed"))
			} else {
				lines = append(lines, sDim.Render(line))
			}
		}
		lines = append(lines, "")
	}
	if ran && ev.Next != "" {
		via := ev.Branch
		if via == "" {
			via = "then"
		}
		lines = append(lines, sGreen.Render(fmt.Sprintf("%s → %s", via, ev.Next)))
	}
	if ran && ev.CostUSD > 0 {
		lines = append(lines, sDim.Render(fmt.Sprintf("$%.6f · %s", ev.CostUSD, ev.Latency.Round(1e6))))
	}
	return lines
}

// actionLines shows what an action does and, once it ran, everything it
// produced, with long text wrapped.
func (m *Model) actionLines(n *flow.Node, ev runner.Event, ran bool, w int) []string {
	a := n.Action
	var lines []string
	src := func(label, v string) {
		if v != "" {
			lines = append(lines, sDim.Render(label)+" "+slots(v))
		}
	}
	switch a.Type {
	case flow.ActLog:
		src("say", a.Message)
	case flow.ActShell:
		src("$", a.Run)
	case flow.ActLLM:
		src("system", a.System)
		src("prompt", a.Prompt)
	case flow.ActHTTP:
		src(a.Method, a.URL)
	case flow.ActOutput:
		for _, kv := range a.Set {
			src(kv.Key+" =", kv.Value)
		}
	}
	lines = append(lines, "")
	if !ran {
		return append(lines, sDim.Render("not run"))
	}
	out, _ := ev.Output.(map[string]any)
	if out["dry_run"] == true {
		lines = append(lines, sYellow.Render("dry run: nothing was sent"), "")
	}
	for _, k := range slices.Sorted(maps.Keys(out)) {
		if k == "dry_run" {
			continue
		}
		v := tmpl.Format(out[k])
		if len(v) > w-len(k)-2 || strings.Contains(v, "\n") {
			lines = append(lines, sBold.Render(k))
			for _, para := range strings.Split(v, "\n") {
				lines = append(lines, wrap(para, w)...)
			}
			continue
		}
		lines = append(lines, sBold.Render(k)+"  "+v)
	}
	if ev.CostUSD > 0 {
		lines = append(lines, "", sDim.Render(fmt.Sprintf("$%.6f · %s", ev.CostUSD, ev.Latency.Round(1e6))))
	}
	return lines
}

func (m *Model) confidenceLine(n *flow.Node, d backend.Decision) string {
	th := m.flow.ThresholdFor(n)
	s := fmt.Sprintf("confidence %.2f  (threshold %.2f)", d.Confidence, th)
	if d.Confidence >= th {
		return sDim.Render(s)
	}
	if _, ok := n.Then.Get(flow.Unsure); ok {
		return sYellow.Render(s + "  below → unsure")
	}
	return sYellow.Render(s + "  below, but no unsure route")
}

func changed(a, b backend.Decision) bool {
	if a.Kind == flow.Score {
		x, _ := a.Answer.(float64)
		y, _ := b.Answer.(float64)
		return math.Round(x) != math.Round(y)
	}
	return tmpl.Format(a.Answer) != tmpl.Format(b.Answer)
}

// bars draws one probability bar per answer, in the question's own order.
func bars(q flow.Question, d backend.Decision, w int) []string {
	var keys []string
	switch q.Kind {
	case flow.Bool:
		keys = []string{"yes", "no"}
	case flow.Choice:
		keys = q.Options
	case flow.Score:
		for l := q.Scale.Min; l <= q.Scale.Max; l++ {
			keys = append(keys, strconv.Itoa(l))
		}
	}
	labelW := 0
	for _, k := range keys {
		labelW = max(labelW, len(k))
	}
	barW := max(w-labelW-10, 5)
	chosen := tmpl.Format(d.Answer)
	if q.Kind == flow.Bool {
		chosen = "no"
		if yes, _ := d.Answer.(bool); yes {
			chosen = "yes"
		}
	}

	var lines []string
	for _, k := range keys {
		p := d.Probs[k]
		filled := int(p*float64(barW) + 0.5)
		bar := strings.Repeat("█", filled) + sDim.Render(strings.Repeat("░", barW-filled))
		label := fmt.Sprintf("%-*s", labelW, k)
		if k == chosen {
			label = sCyan.Render(label)
			bar = sCyan.Render(strings.Repeat("█", filled)) + sDim.Render(strings.Repeat("░", barW-filled))
		}
		lines = append(lines, fmt.Sprintf("%s %s %3.0f%%", label, bar, p*100))
	}
	if q.Kind == flow.Score {
		lines = append(lines, sDim.Render(fmt.Sprintf("expected value %s", answer(d))))
	}
	return lines
}

func (m *Model) inputLines(w int) []string {
	var lines []string
	if len(m.inputs) == 0 && m.editing != editKey {
		lines = append(lines, sDim.Render("no inputs · a to add one"))
	}
	keyW := 0
	for _, in := range m.inputs {
		keyW = max(keyW, len(in.key))
	}
	for i, in := range m.inputs {
		spec := m.spec(in.key)
		editingThis := i == m.inputCursor && (m.editing == editValue || m.editing == editPick)
		if editingThis {
			head := sTitle.Render(in.key)
			if spec.Description != "" {
				head += "  " + sDim.Render(spec.Description)
			}
			lines = append(lines, head)
			if m.editing == editValue {
				lines = append(lines, strings.Split(m.editor.View(), "\n")...)
			} else {
				lines = append(lines, m.pickerLines(spec, in.value)...)
			}
			continue
		}
		marker, key := " ", fmt.Sprintf("%-*s", keyW, in.key)
		if m.focus == focusInputs && i == m.inputCursor && m.editing == editNone {
			marker, key = sTitle.Render("▸"), sBold.Render(key)
		}
		lines = append(lines, fmt.Sprintf("%s%s  %s", marker, key, m.valueView(spec, in)))
	}
	if m.editing == editKey || m.editing == editCaseName {
		lines = append(lines, m.keyEditor.View())
	}
	if rs := m.activeResults(); len(rs) > 0 {
		lines = append(lines, "")
		for _, r := range rs {
			if r.OK {
				lines = append(lines, sGreen.Render("✓ ")+fmt.Sprintf("%s = %s", r.Target, r.Want))
			} else {
				lines = append(lines, sRed.Render("✗ ")+fmt.Sprintf("%s = %s ", r.Target, r.Want)+sRed.Render("got "+r.Got))
			}
		}
	}
	if len(m.issues) > 0 {
		lines = append(lines, "")
		for _, is := range m.issues {
			style := sYellow
			if is.Severity == validate.Error {
				style = sRed
			}
			lines = append(lines, style.Render(is.String()))
		}
	}
	return lines
}

// valueView renders an input's value the way its type reads best.
func (m *Model) valueView(s flow.InputSpec, in input) string {
	if in.value == "" {
		return sYellow.Render("(empty)") + sDim.Render(typeHint(s))
	}
	if err := m.inputError(in); err != nil {
		return sRed.Render(in.value) + sRed.Render("  ✗ "+err.Error())
	}
	switch s.Type {
	case flow.InputChoice:
		return lipgloss.NewStyle().Foreground(pink).Bold(true).Render(in.value) + sDim.Render(" ▾")
	case flow.InputBool:
		v, _ := s.Parse(in.value)
		if v == true {
			return sGreen.Bold(true).Render("YES")
		}
		return sRed.Bold(true).Render("NO")
	case flow.InputNumber:
		return lipgloss.NewStyle().Foreground(orange).Render(in.value) + sDim.Render(typeHint(s))
	}
	return preview(in.value)
}

// typeHint is a short, dim reminder of what an input accepts.
func typeHint(s flow.InputSpec) string {
	switch s.Type {
	case flow.InputChoice:
		return "  " + strings.Join(s.Options, " / ")
	case flow.InputBool:
		return "  yes / no"
	case flow.InputNumber:
		switch {
		case s.Min != nil && s.Max != nil:
			return fmt.Sprintf("  %g–%g", *s.Min, *s.Max)
		case s.Min != nil:
			return fmt.Sprintf("  ≥ %g", *s.Min)
		case s.Max != nil:
			return fmt.Sprintf("  ≤ %g", *s.Max)
		}
		return "  number"
	}
	return ""
}

func (m *Model) pickerLines(s flow.InputSpec, current string) []string {
	current = displayValue(s, current)
	lines := make([]string, len(m.pickOptions))
	for i, o := range m.pickOptions {
		cursor, dot := "  ", sDim.Render("○")
		if o == current {
			dot = lipgloss.NewStyle().Foreground(pink).Render("●")
		}
		label := o
		if i == m.pickCursor {
			cursor, label = sTitle.Render("▸ "), sBold.Render(o)
		}
		lines[i] = "  " + cursor + dot + " " + label
	}
	return lines
}

// preview shows the first line of a value and how much more there is.
func preview(v string) string {
	if v == "" {
		return sYellow.Render("(empty)")
	}
	first, rest, multi := strings.Cut(v, "\n")
	if !multi {
		return v
	}
	return first + sDim.Render(fmt.Sprintf(" ⏎ +%d lines", strings.Count(rest, "\n")+1))
}

// modeLabel says whether http nodes would really send.
func (m *Model) modeLabel() string {
	if m.live {
		return sRed.Bold(true).Render("LIVE")
	}
	return sYellow.Render("dry run")
}

func (m *Model) flowTitle() string {
	if m.flowView == viewPath && m.run != nil && len(m.run.path) > 0 {
		return "[1] Flow · path  (v graph)"
	}
	return "[1] Flow"
}

func (m *Model) inputsTitle() string {
	if m.active < 0 {
		return "Inputs · unsaved"
	}
	t := "Inputs · test " + m.cases[m.active].Name
	if m.dirty {
		t += "*"
	}
	return t
}

// activeResults are the expectation checks for the input on screen: only
// when it's an unedited test case that has been run.
func (m *Model) activeResults() []cases.Result {
	if m.active < 0 || m.dirty {
		return nil
	}
	return m.results[m.cases[m.active].Name]
}

func (m *Model) caseLines(w, h int) []string {
	if len(m.cases) == 0 {
		return []string{sDim.Render("none yet · type input, then n to save it as a test")}
	}
	var lines []string
	for i, c := range m.cases {
		icon := sDim.Render("○")
		if rs, ok := m.results[c.Name]; ok {
			if cases.Passed(rs) {
				icon = sGreen.Render("✓")
			} else {
				icon = sRed.Render("✗")
			}
		} else if len(c.Expect) == 0 {
			icon = sDim.Render("·")
		}
		marker := " "
		if i == m.caseCursor && m.focus == focusTests {
			marker = sTitle.Render("▸")
		}
		name := c.Name
		if i == m.active {
			name = sBold.Render(name)
		}
		line := fmt.Sprintf("%s%s %s", marker, icon, name)
		if c.Description != "" {
			line += "  " + sDim.Render(c.Description)
		}
		if i == m.caseCursor && m.focus == focusTests {
			line = sSelected.Width(w).Render(line)
		}
		lines = append(lines, line)
	}
	if top := m.caseCursor - h + 1; top > 0 {
		lines = lines[top:]
	}
	return lines
}

func (m *Model) statusBar() string {
	var left string
	switch {
	case m.run != nil && !m.run.done && m.run.paused == "" && m.editing == editNone:
		left = " x stop · 1-4 panes"
	case m.pausedRun():
		left = " s step · r run to end · x stop · 1-4 panes"
	case m.editing == editKey || m.editing == editCaseName:
		left = " enter ok · esc cancel"
	case m.editing == editPick:
		left = " j/k choose · enter select · esc cancel"
	case m.editing == editValue:
		left = " enter save · alt+enter newline · esc cancel"
	case m.focus == focusInputs:
		left = " j/k move · enter edit · a add · d delete · w save test · n save as new test · r run · tab next"
	case m.focus == focusTests:
		left = " j/k move · enter load · r run · s step · 1-4 panes · q quit"
	case m.focus == focusNode:
		left = " j/k scroll · r run · s step · 1-4 panes · q quit"
	default:
		left = " j/k select · r run · s step · v graph/path · b backend · L live/dry · 1-4 panes · q quit"
	}

	right := "backend " + m.backend + " · " + m.modeLabel()
	if r := m.run; r != nil {
		switch {
		case r.paused != "":
			right = fmt.Sprintf("%s ⏸ next: %s · %d steps · $%.6f · %s", sYellow.Render("paused"), r.paused, len(r.path), r.cost, right)
		case !r.done:
			running := fmt.Sprintf("running %s… %.1fs", r.current, time.Since(r.nodeAt).Seconds())
			if r.status != "" {
				running += " · " + sYellow.Render(r.status)
			}
			right = running + " · " + right
		case r.err != nil:
			right = sRed.Render("failed") + " · " + right
		default:
			right = fmt.Sprintf("%d steps · $%.6f · %s · %s", len(r.path), r.cost, r.elapsed.Round(1e6), right)
		}
	}
	if m.flash != "" {
		right = m.flash + " · " + right
	}
	right += " "

	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return sStatus.Width(m.width).Render(ansi.Truncate(left+strings.Repeat(" ", gap)+right, m.width, "…"))
}

func wrap(s string, w int) []string {
	if w <= 0 {
		return []string{s}
	}
	return strings.Split(ansi.Wordwrap(s, w, ""), "\n")
}
