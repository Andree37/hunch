package tui

import (
	"fmt"

	"github.com/Andree37/hunch/internal/flow"
)

// row is one line of the flow drawn as a tree from the start node. A node
// reachable from several places (or through a loop) is drawn in full once;
// later occurrences are references to it.
type row struct {
	node   string
	parent string // node the edge comes from, "" for the start
	edge   string // branch label; "" for an unconditional edge
	prefix string // tree drawing to the left of the edge
	ref    bool   // already drawn above
	orphan bool   // not reachable from start
}

func (m *Model) layout() []row {
	if m.flow == nil {
		return nil
	}
	f := m.flow
	var rows []row
	seen := map[string]bool{}

	var walk func(id, parent, edge, prefix, childPrefix string)
	walk = func(id, parent, edge, prefix, childPrefix string) {
		r := row{node: id, parent: parent, edge: edge, prefix: prefix}
		if seen[id] {
			r.ref = true
			rows = append(rows, r)
			return
		}
		seen[id] = true
		rows = append(rows, r)
		n := f.Node(id)
		if n == nil {
			return
		}
		edges := outEdges(n)
		for i, e := range edges {
			conn, next := "├─", "│  "
			if i == len(edges)-1 {
				conn, next = "└─", "   "
			}
			walk(e.To, id, e.When, childPrefix+conn, childPrefix+next)
		}
	}
	walk(f.Start, "", "", "", "")

	for _, n := range f.Nodes {
		if !seen[n.ID] {
			seen[n.ID] = true
			rows = append(rows, row{node: n.ID, orphan: true})
		}
	}
	return rows
}

func outEdges(n *flow.Node) []flow.Branch {
	if len(n.Then.Branches) > 0 {
		return n.Then.Branches
	}
	if n.Then.Next != "" {
		return []flow.Branch{{To: n.Then.Next}}
	}
	return nil
}

// taken reports whether the current run went along the edge into r.
func (m *Model) taken(r row) bool {
	if r.parent == "" {
		return false
	}
	ev, ran := m.eventFor(r.parent)
	if !ran || ev.Next != r.node {
		return false
	}
	return r.edge == "" || ev.Branch == r.edge
}

func edgeLabel(when string) string {
	switch when {
	case "":
		return "▶"
	case flow.Default:
		return "else ▶"
	}
	return when + " ▶"
}

// rowLine renders one tree row: drawing, edge, status, kind badge, name and
// the node's answer (or a reference back to where it's drawn).
func (m *Model) rowLine(r row, selected bool) string {
	marker := " "
	if selected {
		marker = sTitle.Render("▸")
	}
	n := m.flow.Node(r.node)

	lead := ""
	if r.parent != "" {
		edge := r.prefix + " " + edgeLabel(r.edge) + " "
		if m.taken(r) {
			lead = sGreen.Render(edge)
		} else {
			lead = sDim.Render(edge)
		}
	}
	if r.orphan {
		lead = sYellow.Render("unreachable ")
	}
	if n == nil {
		return marker + lead + sRed.Render(r.node+" (missing)")
	}

	name := r.node
	if selected {
		name = sBold.Render(name)
	}
	head := fmt.Sprintf("%s%s%s %s %s", marker, lead, m.statusIcon(r.node), badge(n), name)
	if r.ref {
		return head + sDim.Render(" ↑")
	}
	ev, ran := m.eventFor(r.node)
	return head + "  " + nodeDetail(n, ev, ran)
}

type flowView int

const (
	viewGraph flowView = iota
	viewPath           // only what the last run did, in order
)

// rows are the Flow pane's selectable rows in the current view.
func (m *Model) rows() []row {
	if m.flowView == viewPath && m.run != nil && len(m.run.path) > 0 {
		rows := make([]row, len(m.run.path))
		for i, id := range m.run.path {
			rows[i] = row{node: id}
		}
		return rows
	}
	return m.layout()
}

// closeCall reports whether a decision only just cleared its threshold.
func (m *Model) closeCall(n *flow.Node, conf float64) bool {
	return conf < m.flow.ThresholdFor(n)+0.1
}

// pathLines tells the last run as a numbered story: the result first, then
// each step with its answer and, beneath, the question as it was asked.
func (m *Model) pathLines() (lines []string, rowLine []int) {
	r := m.run
	last := r.path[len(r.path)-1]
	switch {
	case !r.done:
		lines = append(lines, sYellow.Render("Running…"), "")
	case r.err != nil:
		lines = append(lines, sRed.Render("Failed at "+r.errNode), sRed.Render(r.err.Error()), "")
	default:
		lines = append(lines, sBold.Render("Result  ")+last)
		if n := m.flow.Node(last); n != nil && n.Kind == flow.Action {
			ev, _ := m.eventFor(last)
			lines = append(lines, nodeDetail(n, ev, true))
		}
		lines = append(lines, "")
	}

	for i, id := range r.path {
		rowLine = append(rowLine, len(lines))
		n := m.flow.Node(id)
		if n == nil {
			continue
		}
		marker := " "
		name := fmt.Sprintf("%-12s", id)
		if i == m.cursor {
			marker, name = sTitle.Render("▸"), sBold.Render(name)
		}
		ev, ran := m.eventFor(id)
		head := fmt.Sprintf("%s%2d %s %s %s", marker, i+1, m.statusIcon(id), badge(n), name)
		if ran && n.Kind != flow.Action {
			detail := nodeDetail(n, ev, true)
			// The story needs the answer, not every option it beat.
			if n.Kind == flow.Choice && len(ev.Decisions) > 0 {
				d := ev.Decisions[0]
				detail = compactAnswer(n.Questions[0], d) + sDim.Render(fmt.Sprintf(" %.0f%%", d.Confidence*100))
			}
			head += " " + detail
			if n.Kind != flow.Questions && len(ev.Decisions) > 0 && m.closeCall(n, ev.Decisions[0].Confidence) {
				head += sYellow.Render("  ⚠ close call")
			}
		}
		lines = append(lines, head)
		if ran && n.Kind != flow.Questions && len(ev.Asked) > 0 {
			lines = append(lines, "      "+sDim.Render(ev.Asked[0]))
		}
	}
	return lines, rowLine
}
