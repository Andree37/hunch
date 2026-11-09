package tui

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/trace"
)

// Two recorded runs side by side: c on one run in the runs list marks it, c
// on another compares them. Each question's answer and confidence in both,
// where they differ, and where their paths split.

type comparison struct {
	a, b   *trace.Run
	scroll int
}

// markForCompare is c on the highlighted recorded run.
func (m *Model) markForCompare() {
	if !m.showRuns || len(m.runs) == 0 {
		m.flash = "t shows the recorded runs; c on two of them compares them"
		return
	}
	tr := m.runs[min(m.runCursor, len(m.runs)-1)]
	switch {
	case m.compareMark == tr:
		m.compareMark = nil
		m.flash = "unmarked"
	case m.compareMark == nil:
		m.compareMark = tr
		m.flash = "marked · c on another run compares the two"
	default:
		a, b := m.compareMark, tr
		if b.Time.Before(a.Time) {
			a, b = b, a // older on the left
		}
		m.compare, m.compareMark = &comparison{a: a, b: b}, nil
	}
}

func (m *Model) updateCompare(key string) {
	switch key {
	case "esc", "q", "c", "enter", "backspace":
		m.compare = nil
	case "j", "down":
		m.compare.scroll++
	case "k", "up":
		m.compare.scroll = max(m.compare.scroll-1, 0)
	}
}

func (m *Model) compareView(h int) string {
	lines := m.compareLines(m.width - 4)
	c := m.compare
	c.scroll = min(c.scroll, max(len(lines)-(h-3), 0))
	return pane(paneTitle(1, "Compare runs", "esc back"), lines[c.scroll:], m.width, h, true)
}

func runLabel(tr *trace.Run) string {
	s := tr.Time.Local().Format("Jan 2 15:04:05")
	if tr.Backend != "" {
		s += " · " + tr.Backend
	}
	return s
}

func (m *Model) compareLines(w int) []string {
	a, b := m.compare.a, m.compare.b
	lines := []string{
		sBold.Render("A") + "  " + runLabel(a),
		sBold.Render("B") + "  " + runLabel(b),
		"",
	}

	// Input: the same, or what differs.
	keys := slices.Sorted(maps.Keys(a.Input))
	for k := range b.Input {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var same []string
	var diff []string
	for _, k := range keys {
		va, oka := a.Input[k]
		vb, okb := b.Input[k]
		if oka && okb && tmpl.Format(va) == tmpl.Format(vb) {
			same = append(same, k)
			continue
		}
		diff = append(diff, k)
	}
	if len(diff) == 0 {
		lines = append(lines, sTitle.Render("Same input")+"  "+sDim.Render(inputSummary(a.Input)), "")
	} else {
		lines = append(lines, sTitle.Render("Input"))
		for _, k := range diff {
			for _, side := range []struct {
				name string
				in   map[string]any
			}{{"A", a.Input}, {"B", b.Input}} {
				v, ok := side.in[k]
				text := sDim.Render("(not given)")
				if ok {
					text = strings.ReplaceAll(tmpl.Format(v), "\n", " ")
				}
				lines = append(lines, ansi.Truncate(fmt.Sprintf("  %s %s  %s", sYellow.Render(k), sDim.Render(side.name), text), w, "…"))
			}
		}
		if len(same) > 0 {
			lines = append(lines, sDim.Render("  same: "+strings.Join(same, ", ")))
		}
		lines = append(lines, "")
	}

	// Where the paths part.
	split := -1
	for i := range min(len(a.Path), len(b.Path)) {
		if a.Path[i] != b.Path[i] {
			split = i
			break
		}
	}
	if split < 0 && len(a.Path) != len(b.Path) {
		split = min(len(a.Path), len(b.Path))
	}
	switch {
	case split < 0:
		lines = append(lines, sGreen.Render("Same path")+"  "+sDim.Render(strings.Join(a.Path, " → ")), "")
	case split == 0:
		lines = append(lines, sYellow.Render("Different from the start"), "")
	default:
		lines = append(lines, sYellow.Render("Paths split after "+a.Path[split-1])+"  "+
			sDim.Render(fmt.Sprintf("A → %s · B → %s", at(a.Path, split), at(b.Path, split))), "")
	}

	// Each step, in flow order, as it went in each run.
	evA, evB := stepsByNode(a), stepsByNode(b)
	var order []string
	for _, n := range m.flow.Nodes {
		if _, ok := evA[n.ID]; ok {
			order = append(order, n.ID)
		} else if _, ok := evB[n.ID]; ok {
			order = append(order, n.ID)
		}
	}
	for _, p := range [][]string{a.Path, b.Path} { // nodes the flow no longer has
		for _, id := range p {
			if !slices.Contains(order, id) {
				order = append(order, id)
			}
		}
	}
	nameW := 6
	for _, id := range order {
		nameW = max(nameW, lipgloss.Width(id))
	}
	nameW = min(nameW, 24)
	colW := min(max((w-nameW-4-12)/2, 10), 36)
	pad := func(s string, n int) string {
		s = ansi.Truncate(s, n, "…")
		return s + strings.Repeat(" ", max(n-lipgloss.Width(s), 0))
	}
	lines = append(lines, sDim.Render(pad("", nameW)+"  "+pad("A", colW)+"  "+pad("B", colW)))
	differs := 0
	for _, id := range order {
		ea, oka := evA[id]
		eb, okb := evB[id]
		ta, tb := m.compareCell(id, ea, oka), m.compareCell(id, eb, okb)
		note := ""
		switch {
		case !oka || !okb:
			note = sYellow.Render("only " + map[bool]string{true: "A", false: "B"}[oka])
			differs++
		case decides(ea) && answerOf(ea) != answerOf(eb):
			note = sYellow.Render("← differs")
			differs++
		case len(ea.Decisions) > 0 && len(eb.Decisions) > 0 &&
			math.Abs(ea.Decisions[0].Confidence-eb.Decisions[0].Confidence) >= 0.1:
			note = sDim.Render(fmt.Sprintf("sure %+.2f", eb.Decisions[0].Confidence-ea.Decisions[0].Confidence))
		}
		lines = append(lines, pad(id, nameW)+"  "+pad(ta, colW)+"  "+pad(tb, colW)+"  "+note)
	}
	lines = append(lines, "",
		pad("ends at", nameW)+"  "+pad(endOf(a), colW)+"  "+pad(endOf(b), colW),
		sDim.Render(pad("cost", nameW)+"  "+pad(fmt.Sprintf("$%.6f", a.CostUSD), colW)+"  "+pad(fmt.Sprintf("$%.6f", b.CostUSD), colW)))
	if differs == 0 && split < 0 {
		lines = append(lines, "", sGreen.Render("Every answer is the same."))
	}
	return lines
}

func at(path []string, i int) string {
	if i < len(path) {
		return path[i]
	}
	return "(ended)"
}

func stepsByNode(tr *trace.Run) map[string]runner.Event {
	out := map[string]runner.Event{}
	for _, ev := range tr.Steps {
		out[ev.Node] = ev // a node visited twice shows its last visit
	}
	return out
}

func endOf(tr *trace.Run) string {
	switch {
	case tr.Error != "":
		return sRed.Render("✗ " + tr.Error)
	case len(tr.Path) > 0:
		return tr.Path[len(tr.Path)-1]
	}
	return "·"
}

// decides reports whether a step chose something (an answer or a route),
// as opposed to an action, whose output naturally follows its input.
func decides(ev runner.Event) bool { return len(ev.Decisions) > 0 || ev.Branch != "" }

// answerOf is what a step decided or produced, to compare two runs by.
func answerOf(ev runner.Event) string {
	if len(ev.Decisions) > 0 {
		parts := make([]string, len(ev.Decisions))
		for i, d := range ev.Decisions {
			parts[i] = tmpl.Format(d.Answer)
		}
		return strings.Join(parts, "/")
	}
	if ev.Branch != "" {
		return ev.Branch
	}
	return tmpl.Format(ev.Output)
}

// compareCell is one step in one run: the answer and how sure, or what an
// action did.
func (m *Model) compareCell(id string, ev runner.Event, ok bool) string {
	if !ok {
		return sDim.Render("·")
	}
	n := m.flow.Node(id)
	switch {
	case len(ev.Decisions) == 1 && n != nil && len(n.Questions) == 1:
		d := ev.Decisions[0]
		style := sAnswer
		if ev.Branch == flow.Unsure {
			style = sOrange
		}
		return style.Render(answerLabel(n.Questions[0], d)) + sDim.Render(fmt.Sprintf(" %.2f", d.Confidence))
	case len(ev.Decisions) > 0:
		return sAnswer.Render(answerOf(ev))
	case ev.Branch != "":
		return sAnswer.Render(ev.Branch)
	}
	out, _ := ev.Output.(map[string]any)
	if n != nil && n.Kind == flow.Action && n.Action.Type == flow.ActOutput {
		parts := make([]string, 0, len(out))
		for _, k := range slices.Sorted(maps.Keys(out)) {
			parts = append(parts, k+"="+tmpl.Format(out[k]))
		}
		return strings.Join(parts, " ")
	}
	return sDim.Render("✓")
}
