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
	"github.com/charmbracelet/x/ansi"

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

	// What the model said, so the run can be routed again at another
	// threshold without asking it (see threshold.go).
	kase    *cases.Case
	asked   map[string]backend.Decision // by question ID
	wrote   map[string]string           // llm text by prompt
	fetched map[string]runner.FakeResponse
	needs   string // rerouted to a node the model was never asked
}

// passed is "pass", "fail", "ask" (needs a model call to know) or ""
// (ran with nothing to check).
func (r *gridRow) passed() string {
	switch {
	case r.status != rowDone:
		return ""
	case r.needs != "":
		return "ask"
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
	id   int
	rows []*gridRow
	prev map[string]*gridRow // the previous grid's rows, to show what changed
	// model is the rows as the model ran them, at ranSteps; rows differ
	// from them while another threshold is tried. savedPrev is prev for
	// those rows.
	model     []*gridRow
	ranShifts map[string]int
	savedPrev map[string]*gridRow
	asking    bool    // A is asking the model what ? tests need
	askCost   float64 // what those questions cost
	askNext   tea.Cmd // waits for the next answer
	askCancel context.CancelFunc
	started   time.Time
	elapsed   time.Duration
	done      bool
	cancel    context.CancelFunc
	ch        chan tea.Msg
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
		if g.askCancel != nil {
			g.askCancel()
		}
		prev = g.savedPrev
		if g.done {
			prev = map[string]*gridRow{}
			for _, r := range g.model {
				prev[r.name] = r
			}
		}
	}
	pf, backends, err := m.gridFlow()
	if err != nil {
		m.flash = err.Error()
		return nil
	}
	f := *pf

	m.gridID++
	ctx, cancel := context.WithCancel(context.Background())
	g := &gridRun{
		id: m.gridID, prev: prev, savedPrev: prev, ranShifts: maps.Clone(m.shifts),
		started: time.Now(), cancel: cancel, ch: make(chan tea.Msg, 2*len(m.cases)+1),
	}
	shifts := shiftsOf(m.shifts)
	all := slices.Clone(m.cases)
	for _, c := range all {
		state := maps.Clone(f.State)
		maps.Copy(state, c.Input)
		g.rows = append(g.rows, &gridRow{name: c.Name, inputs: state})
	}
	g.model = slices.Clone(g.rows)
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
				row := &gridRow{
					name: c.Name, status: rowDone, inputs: g.rows[i].inputs, events: map[string]runner.Event{}, kase: c,
					asked: map[string]backend.Decision{}, wrote: map[string]string{}, fetched: map[string]runner.FakeResponse{},
				}
				var mu sync.Mutex
				if err := c.CheckFakes(&f); err != nil {
					row.err = err
				} else {
					res, err := runner.Run(ctx, &f, maps.Clone(row.inputs), runner.Options{
						Backends: backends, DryRun: true, Fake: c.HTTP, ThresholdShifts: shifts,
						OnStart: func(node string) {
							mu.Lock()
							row.path = append(row.path, node)
							mu.Unlock()
						},
						OnEvent: func(ev runner.Event) {
							mu.Lock()
							row.events[ev.Node] = ev
							row.remember(f.Node(ev.Node), ev)
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

// gridFlow is a copy of the flow set to the backends picked in the TUI,
// and those backends.
func (m *Model) gridFlow() (*flow.Flow, map[string]backend.Backend, error) {
	backends := map[string]backend.Backend{}
	for name, cfg := range m.flow.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("backend %s: %w", name, err)
		}
		backends[name] = b
	}
	f := *m.flow
	f.DefaultBackend = m.backend
	if m.writer != "" {
		f.WriterBackend = m.writer
	}
	return &f, backends, nil
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
		g.rows[msg.i], g.model[msg.i] = msg.row, msg.row
		if msg.row.results != nil {
			m.results[msg.row.name] = msg.row.results
		}
	case gridDoneMsg:
		if g == nil || msg.id != g.id {
			return nil
		}
		g.done, g.elapsed = true, time.Since(g.started)
		m.retune() // the thresholds may have moved while it ran
		return nil
	case askRowMsg:
		if g == nil || msg.id != g.id {
			return nil
		}
		g.rows[msg.i] = msg.row
		g.askCost += msg.row.cost
		return g.askNext
	case askDoneMsg:
		if g == nil || msg.id != g.id {
			return nil
		}
		g.asking = false
		m.retune() // with the new answers, at whatever threshold is up now
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

// gridLines draws the grid in w columns and h lines; when the tests don't
// fit, the rows scroll to keep the selected one in view.
func (m *Model) gridLines(w, h int) []string {
	g := m.grid
	lines := []string{m.gridSummary(), ""}

	cols := m.gridColumns()
	nameW := 4
	for _, r := range g.rows {
		nameW = max(nameW, lipgloss.Width(r.name))
	}
	nameW = min(nameW, 30)
	widths := make([]int, len(cols))
	for i, n := range cols {
		widths[i] = lipgloss.Width(n.ID)
		for _, r := range g.rows {
			widths[i] = max(widths[i], lipgloss.Width(m.gridCell(n, r)))
		}
		widths[i] = min(max(widths[i], 5), 16)
	}
	// Columns that don't fit are left out, and counted; they scroll so the
	// selected question stays in view.
	sel := min(max(m.gridCol, 0), max(len(cols)-1, 0))
	fit := func(from int) int {
		room, n := w-4-nameW-2-14, 0
		for _, cw := range widths[from:] {
			if room < cw+2 {
				break
			}
			room -= cw + 2
			n++
		}
		return n
	}
	firstCol := 0
	for firstCol < sel && sel >= firstCol+fit(firstCol) {
		firstCol++
	}
	shown := make([]int, 0, len(cols))
	for i := range fit(firstCol) {
		shown = append(shown, firstCol+i)
	}

	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0)) }
	head := sDim.Render("      " + pad("test", nameW))
	ths := "      " + pad("threshold", nameW)
	for _, i := range shown {
		id := pad(ansi.Truncate(cols[i].ID, widths[i], "…"), widths[i])
		if i == sel {
			id = sTitle.Underline(true).Render(id)
		} else {
			id = sDim.Render(id)
		}
		head += "  " + id
		th := ""
		if n := cols[i]; tunable(n) {
			th = fmt.Sprintf("%.2f", m.threshold(n))
			if _, tried := m.shifts[n.ID]; tried {
				th = sYellow.Render(pad(th+"*", widths[i]))
			} else {
				th = sDim.Render(pad(th, widths[i]))
			}
		} else {
			th = pad("", widths[i])
		}
		ths += "  " + th
	}
	head += sDim.Render("  ends at")
	lines = append(lines, head, sDim.Render(ths[:6+nameW])+ths[6+nameW:])

	// Rows that don't fit scroll, keeping room for a line on each side
	// saying how many are hidden.
	first, last := 0, len(g.rows)
	if room := h - len(lines) - 2; room > 0 && len(g.rows) > room+2 {
		first = min(max(m.gridCursor-room/2, 0), len(g.rows)-room)
		last = first + room
	}
	if first > 0 {
		lines = append(lines, sDim.Render(fmt.Sprintf("      ↑ %d more", first)))
	}
	for ri, r := range g.rows {
		if ri < first || ri >= last {
			continue
		}
		marker := " "
		name := ansi.Truncate(r.name, nameW, "…")
		name += strings.Repeat(" ", max(nameW-lipgloss.Width(name), 0))
		if ri == m.gridCursor {
			marker, name = sTitle.Render("▸"), sBold.Render(name)
		}
		line := marker + m.rowMark(r) + " " + name
		for _, i := range shown {
			cell := m.gridCell(cols[i], r)
			line += "  " + cell + strings.Repeat(" ", max(widths[i]-lipgloss.Width(cell), 0))
		}
		end := ""
		switch {
		case r.status == rowPending:
			end = sDim.Render("waiting")
		case r.status == rowRunning:
			end = sYellow.Render(m.spinner() + " running")
		case r.needs != "":
			end = sYellow.Render("asks " + r.needs + " · A")
		case r.err != nil:
			end = sRed.Render("error")
		case len(r.path) > 0:
			end = r.path[len(r.path)-1]
		}
		lines = append(lines, line+"  "+end)
	}
	if last < len(g.rows) {
		lines = append(lines, sDim.Render(fmt.Sprintf("      ↓ %d more", len(g.rows)-last)))
	}
	if len(shown) < len(cols) {
		lines = append(lines, sDim.Render(fmt.Sprintf("%d of %d questions shown · ←/→ scroll", len(shown), len(cols))))
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
	pass, total, done, close, ask := 0, 0, 0, 0, 0
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
		case "ask":
			ask++
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
	if ask > 0 {
		need := " need"
		if ask == 1 {
			need = " needs"
		}
		s += sYellow.Render(fmt.Sprintf(" · %d%s the model (A asks it)", ask, need))
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
		if !maps.Equal(m.shifts, g.ranShifts) {
			s += sDim.Render(" at " + m.triedText(g.ranShifts))
		}
	}
	if close > 0 {
		s += sYellow.Render(" · " + plural(close, "close call"))
	}
	if g.done && !maps.Equal(m.shifts, g.ranShifts) {
		s += sYellow.Render(" · trying " + m.triedText(m.shifts))
		if g.askCost > 0 {
			s += sDim.Render(fmt.Sprintf(" · asked $%.6f more", g.askCost))
		}
	} else if g.done {
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
	case now == "ask":
		return sYellow.Render("   ?")
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
		th := m.threshold(n)
		switch {
		case ev.Branch == flow.Unsure:
			style = sOrange.Bold(true)
		case d.Confidence < th || m.closeCall(n, d.Confidence):
			style = sYellow.Bold(true)
		}
	}
	text = ansi.Truncate(text, 12, "…")
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

// plural is "1 close call" or "2 close calls".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
