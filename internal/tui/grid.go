package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/validate"
)

// The grid runs every test case at once and shows them side by side: one
// row per test, one column per question. While it's up, saving the flow or
// a test runs them all again and marks what changed, so the flow can be
// edited in any editor and judged here.

type rowStatus int

const (
	rowPending rowStatus = iota
	rowRunning
	rowDone
)

type gridRow struct {
	name    string
	status  rowStatus
	inputs  map[string]any
	path    []string
	events  map[string]runner.Event
	final   map[string]any
	results []cases.Result
	err     error
	cost    float64
}

// passed is "pass", "fail" or "" (ran with nothing to check).
func (r *gridRow) passed() string {
	switch {
	case r.status != rowDone:
		return ""
	case r.err != nil:
		return "fail"
	case len(r.results) == 0:
		return ""
	case cases.Passed(r.results):
		return "pass"
	}
	return "fail"
}

type gridRun struct {
	id      int
	rows    []*gridRow
	prev    map[string]*gridRow // the previous grid's rows, to show what changed
	started time.Time
	elapsed time.Duration
	done    bool
	cancel  context.CancelFunc
	ch      chan tea.Msg
}

type (
	gridStartMsg struct{ id, i int }
	gridRowMsg   struct {
		id, i int
		row   *gridRow
	}
	gridDoneMsg struct{ id int }
)

// gridParallel is how many tests run at once.
const gridParallel = 4

// startGrid runs every test case in the background and shows the grid.
func (m *Model) startGrid() tea.Cmd {
	if m.flow == nil || validate.HasErrors(m.issues) {
		m.flash = "fix the flow's errors first"
		return nil
	}
	if len(m.cases) == 0 {
		m.flash = "no tests yet · n saves the input as one"
		return nil
	}
	var prev map[string]*gridRow
	if g := m.grid; g != nil {
		if g.cancel != nil {
			g.cancel()
		}
		prev = g.prev
		if g.done {
			prev = map[string]*gridRow{}
			for _, r := range g.rows {
				prev[r.name] = r
			}
		}
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range m.flow.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			m.flash = "backend " + name + ": " + err.Error()
			return nil
		}
		backends[name] = b
	}
	f := *m.flow
	f.DefaultBackend = m.backend
	if m.writer != "" {
		f.WriterBackend = m.writer
	}

	m.gridID++
	ctx, cancel := context.WithCancel(context.Background())
	g := &gridRun{id: m.gridID, prev: prev, started: time.Now(), cancel: cancel, ch: make(chan tea.Msg, 2*len(m.cases)+1)}
	all := slices.Clone(m.cases)
	for _, c := range all {
		state := maps.Clone(f.State)
		maps.Copy(state, c.Input)
		g.rows = append(g.rows, &gridRow{name: c.Name, inputs: state})
	}
	m.grid = g
	m.gridCursor = min(m.gridCursor, len(g.rows)-1)
	m.flowView = viewGrid

	go func() {
		defer close(g.ch)
		send := func(msg tea.Msg) {
			select {
			case g.ch <- msg:
			case <-ctx.Done():
			}
		}
		sem := make(chan struct{}, gridParallel)
		var wg sync.WaitGroup
		for i, c := range all {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				send(gridStartMsg{g.id, i})
				row := &gridRow{name: c.Name, status: rowDone, inputs: g.rows[i].inputs, events: map[string]runner.Event{}}
				var mu sync.Mutex
				if err := c.CheckFakes(&f); err != nil {
					row.err = err
				} else {
					res, err := runner.Run(ctx, &f, maps.Clone(row.inputs), runner.Options{
						Backends: backends, DryRun: true, Fake: c.HTTP,
						OnStart: func(node string) {
							mu.Lock()
							row.path = append(row.path, node)
							mu.Unlock()
						},
						OnEvent: func(ev runner.Event) {
							mu.Lock()
							row.events[ev.Node] = ev
							mu.Unlock()
						},
					})
					row.err, row.cost = err, res.CostUSD
					row.final = res.State
					if err == nil && len(c.Expect) > 0 {
						row.results = c.Check(&f, res.State)
					}
				}
				if ctx.Err() == nil {
					send(gridRowMsg{g.id, i, row})
				}
			}()
		}
		wg.Wait()
		send(gridDoneMsg{g.id})
	}()
	return tea.Batch(m.gridNext(), m.kick())
}

// gridNext waits for the grid's next message.
func (m *Model) gridNext() tea.Cmd {
	ch := m.grid.ch
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *Model) gridRunning() bool { return m.grid != nil && !m.grid.done }

// updateGrid takes the grid's progress messages.
func (m *Model) updateGrid(msg tea.Msg) tea.Cmd {
	g := m.grid
	switch msg := msg.(type) {
	case gridStartMsg:
		if g == nil || msg.id != g.id {
			return nil
		}
		g.rows[msg.i].status = rowRunning
	case gridRowMsg:
		if g == nil || msg.id != g.id {
			return nil
		}
		g.rows[msg.i] = msg.row
		if msg.row.results != nil {
			m.results[msg.row.name] = msg.row.results
		}
	case gridDoneMsg:
		if g == nil || msg.id != g.id {
			return nil
		}
		g.done, g.elapsed = true, time.Since(g.started)
		return nil
	}
	return m.gridNext()
}

// openGridRow shows one test's run from the grid in the run view, without
// running it again.
func (m *Model) openGridRow(i int) {
	row := m.grid.rows[i]
	if row.status != rowDone {
		return
	}
	ci := slices.IndexFunc(m.cases, func(c *cases.Case) bool { return c.Name == row.name })
	if ci < 0 {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.replay = nil
	m.useCase(ci)
	if m.run != nil && m.run.done && m.run.err == nil {
		m.prev = m.run
	}
	m.runID++
	r := &run{events: row.events, path: row.path, done: true, state: row.final, inputs: row.inputs, caseIdx: ci, err: row.err, cost: row.cost}
	if row.err != nil && len(row.path) > 0 {
		r.errNode = row.path[len(row.path)-1]
	}
	m.run = r
	if row.results != nil {
		m.results[row.name] = row.results
	}
	m.flowView, m.cursor, m.nodeScroll = viewPath, 0, 0
	m.restartReveal()
}

// gridColumns are the flow's questions and rules, in flow order, that any
// test reached.
func (m *Model) gridColumns() []*flow.Node {
	reached := map[string]bool{}
	for _, r := range m.grid.rows {
		for _, id := range r.path {
			reached[id] = true
		}
	}
	var cols []*flow.Node
	for _, n := range m.flow.Nodes {
		if n.Kind != flow.Action && (reached[n.ID] || !m.grid.done) {
			cols = append(cols, n)
		}
	}
	return cols
}

func (m *Model) gridLines(w int) []string {
	g := m.grid
	lines := []string{m.gridSummary(), ""}

	cols := m.gridColumns()
	nameW := 4
	for _, r := range g.rows {
		nameW = max(nameW, len(r.name))
	}
	nameW = min(nameW, 30)
	widths := make([]int, len(cols))
	for i, n := range cols {
		widths[i] = len(n.ID)
		for _, r := range g.rows {
			widths[i] = max(widths[i], lipgloss.Width(m.gridCell(n, r)))
		}
		widths[i] = min(max(widths[i], 5), 16)
	}
	// Columns that don't fit are left out, and counted.
	room := w - 4 - nameW - 2 - 14
	shown := 0
	for _, cw := range widths {
		if room < cw+2 {
			break
		}
		room -= cw + 2
		shown++
	}

	head := "      " + fmt.Sprintf("%-*s", nameW, "test")
	for i := range shown {
		head += "  " + fmt.Sprintf("%-*s", widths[i], cols[i].ID)
	}
	head += "  ends at"
	lines = append(lines, sDim.Render(head))

	for ri, r := range g.rows {
		marker := " "
		name := r.name
		if len(name) > nameW {
			name = name[:nameW-1] + "…"
		}
		name = fmt.Sprintf("%-*s", nameW, name)
		if ri == m.gridCursor {
			marker, name = sTitle.Render("▸"), sBold.Render(name)
		}
		line := marker + m.rowMark(r) + " " + name
		for i := range shown {
			cell := m.gridCell(cols[i], r)
			line += "  " + cell + strings.Repeat(" ", max(widths[i]-lipgloss.Width(cell), 0))
		}
		end := ""
		switch {
		case r.status == rowPending:
			end = sDim.Render("waiting")
		case r.status == rowRunning:
			end = sYellow.Render(m.spinner() + " running")
		case r.err != nil:
			end = sRed.Render("error")
		case len(r.path) > 0:
			end = r.path[len(r.path)-1]
		}
		lines = append(lines, line+"  "+end)
	}
	if shown < len(cols) {
		lines = append(lines, sDim.Render(fmt.Sprintf("+%d more questions than fit · a wider window shows them", len(cols)-shown)))
	}

	// What went wrong, for every test that failed.
	var why []string
	for _, r := range g.rows {
		if r.status != rowDone {
			continue
		}
		if r.err != nil {
			why = append(why, wrap(sBold.Render(r.name)+"  "+sRed.Render(r.err.Error()), w)...)
		}
		for _, res := range r.results {
			if !res.OK {
				why = append(why, wrap(sBold.Render(r.name)+"  "+fmt.Sprintf("the test expects %s = %s, but got %s", res.Target, res.Want, res.Got), w)...)
			}
		}
	}
	if len(why) > 0 {
		lines = append(lines, "", sRed.Render("Failed"))
		lines = append(lines, why...)
	}
	if m.mock() && slices.ContainsFunc(g.rows, func(r *gridRow) bool { return r.passed() == "fail" }) {
		lines = append(lines, "", sDim.Render("mock makes its answers up, so tests fail on it. b switches backend."))
	}
	return lines
}

// gridSummary is "✓ 11/12 passed · was 9/12 · 2 close calls · $… · 3.1s".
func (m *Model) gridSummary() string {
	g := m.grid
	pass, total, done, close := 0, 0, 0, 0
	for _, r := range g.rows {
		if r.status == rowDone {
			done++
		}
		switch r.passed() {
		case "pass":
			pass++
			total++
		case "fail":
			total++
		}
		for _, id := range r.path {
			if n := m.flow.Node(id); n != nil && n.Kind.IsDecision() && len(n.Questions) == 1 {
				if ev, ok := r.events[id]; ok && len(ev.Decisions) > 0 && m.closeCall(n, ev.Decisions[0].Confidence) {
					close++
				}
			}
		}
	}
	var s string
	switch {
	case !g.done:
		s = sYellow.Render(fmt.Sprintf("%s running %d of %d tests", m.spinner(), len(g.rows)-done, len(g.rows)))
	case pass == total:
		s = sGreen.Render(fmt.Sprintf("✓ %d/%d passed", pass, total))
	default:
		s = sRed.Render(fmt.Sprintf("✗ %d/%d passed", pass, total))
	}
	if g.done && g.prev != nil {
		was, wasTotal := 0, 0
		for _, r := range g.prev {
			switch r.passed() {
			case "pass":
				was++
				wasTotal++
			case "fail":
				wasTotal++
			}
		}
		s += sDim.Render(fmt.Sprintf(" · was %d/%d", was, wasTotal))
	}
	if close > 0 {
		s += sYellow.Render(fmt.Sprintf(" · %d close calls", close))
	}
	if g.done {
		cost := 0.0
		for _, r := range g.rows {
			cost += r.cost
		}
		s += sDim.Render(fmt.Sprintf(" · $%.6f · %s", cost, g.elapsed.Round(100*time.Millisecond)))
	}
	return s
}

// rowMark is ✓ or ✗ for a test, or what changed since the last grid.
func (m *Model) rowMark(r *gridRow) string {
	now := r.passed()
	var was string
	if p, ok := m.grid.prev[r.name]; ok && r.status == rowDone {
		was = p.passed()
	}
	switch {
	case r.status != rowDone:
		return sDim.Render("   ·")
	case was == "fail" && now == "pass":
		return sGreen.Render("✗→✓")
	case was == "pass" && now == "fail":
		return sRed.Render("✓→✗")
	case now == "pass":
		return sGreen.Render("   ✓")
	case now == "fail":
		return sRed.Render("   ✗")
	}
	return sDim.Render("   ·")
}

// gridCell is one answer in the grid: coloured by how sure it was, with ✓
// or ✗ when the test expects something of it, and * when it changed since
// the last grid.
func (m *Model) gridCell(n *flow.Node, r *gridRow) string {
	ev, ok := r.events[n.ID]
	if !ok {
		if r.status == rowDone {
			return sDim.Render("·")
		}
		return ""
	}
	var text string
	style := sAnswer
	switch {
	case n.Kind == flow.Switch:
		out, _ := ev.Output.(map[string]any)
		text = fmt.Sprint(out["value"])
	case len(ev.Decisions) == 0:
		return sDim.Render("·")
	case len(n.Questions) > 1:
		parts := make([]string, len(ev.Decisions))
		for i, d := range ev.Decisions {
			if i < len(n.Questions) {
				parts[i] = answerLabel(n.Questions[i], d)
			}
		}
		text = strings.Join(parts, "/")
	default:
		d := ev.Decisions[0]
		text = answerLabel(n.Questions[0], d)
		th := m.flow.ThresholdFor(n)
		switch {
		case ev.Branch == flow.Unsure:
			style = sOrange.Bold(true)
		case d.Confidence < th || m.closeCall(n, d.Confidence):
			style = sYellow.Bold(true)
		}
	}
	if len(text) > 12 {
		text = text[:11] + "…"
	}
	cell := style.Render(text)
	if p, ok := m.grid.prev[r.name]; ok {
		if pe, ok := p.events[n.ID]; ok && len(pe.Decisions) > 0 && len(ev.Decisions) > 0 && changed(pe.Decisions[0], ev.Decisions[0]) {
			cell += sYellow.Render("*")
		}
	}
	checked, ok := false, true
	for _, res := range r.results {
		if resultNode(res) == n.ID {
			checked, ok = true, ok && res.OK
		}
	}
	switch {
	case checked && ok:
		cell += sGreen.Render(" ✓")
	case checked:
		cell += sRed.Render(" ✗")
	}
	return cell
}
