package tui

import (
	"slices"

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
		return "→"
	case flow.Default:
		return "else →"
	}
	return when + " →"
}

// reached reports whether the run got to this row's node by this row's
// way in: the start node, or the branch drawn into it.
func (m *Model) reached(r row) bool {
	if m.run == nil {
		return false
	}
	if r.parent == "" {
		return !r.orphan && slices.Contains(m.run.path, r.node)
	}
	return m.taken(r)
}

// rowIcon marks a map row: reached this way, failed, running, paused,
// skipped or not run yet.
func (m *Model) rowIcon(row row) string {
	r := m.run
	reached := m.reached(row)
	switch {
	case r == nil:
		return sDim.Render("○")
	case reached && r.errNode == row.node:
		return sRed.Render("✗")
	case r.paused == row.node && !row.ref:
		return sYellow.Render("⏸")
	case reached && r.current == row.node:
		return sYellow.Render("◐")
	case reached:
		return sGreen.Render("●")
	case r.done:
		return sDim.Render("·")
	}
	return sDim.Render("○")
}

// rowLine renders one map row: the tree drawing and branch, an icon, the
// node's name and its answer (or a pointer to where it's drawn in full).
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
			lead = sDim.Render(r.prefix+" ") + sGreen.Render(edgeLabel(r.edge)+" ")
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
	switch {
	case selected:
		name = sBold.Render(name)
	case m.reached(r):
		name = sGreen.Render(name)
	}
	head := marker + lead + m.rowIcon(r) + " " + name
	if r.ref {
		return head + sDim.Render("  ↑ drawn above")
	}
	ev, ran := m.eventFor(r.node)
	if !ran && n.Kind.IsDecision() {
		return head + "  " + nodeDetail(n, ev, false)
	}
	return head + "  " + nodeDetail(n, ev, ran)
}

type flowView int

const (
	viewGraph flowView = iota
	viewPath           // only what the last run did, in order
	viewGrid           // every test at once
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
