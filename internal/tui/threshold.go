package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
)

// In the grid, ←/→ pick a question and [ and ] try another threshold for
// it without editing the flow. The grid routes each test again on the
// answers the model already gave, so trying a threshold costs nothing; a
// test that now reaches a question the model was never asked shows ? until
// A asks it. w writes the tried thresholds into the flow.

const thresholdStep = 0.05

// shiftsOf turns steps by node into threshold shifts.
func shiftsOf(steps map[string]int) map[string]float64 {
	out := make(map[string]float64, len(steps))
	for id, n := range steps {
		out[id] = float64(n) * thresholdStep
	}
	return out
}

// threshold is the threshold n routes on in this TUI, with any tried shift.
func (m *Model) threshold(n *flow.Node) float64 {
	return runner.Threshold(m.flow, n, float64(m.shifts[n.ID])*thresholdStep)
}

// tunable reports whether n routes on its confidence, so its threshold
// matters. Several questions at once route on their `then` alone.
func tunable(n *flow.Node) bool {
	return n != nil && n.Kind.IsDecision() && n.Kind != flow.Questions
}

// triedText is "intent 0.70, owner 0.55" for the thresholds tried, in flow
// order, or "the flow's thresholds".
func (m *Model) triedText(steps map[string]int) string {
	var parts []string
	for _, n := range m.flow.Nodes {
		if s, ok := steps[n.ID]; ok && s != 0 {
			parts = append(parts, fmt.Sprintf("%s %.2f", n.ID, runner.Threshold(m.flow, n, float64(s)*thresholdStep)))
		}
	}
	if len(parts) == 0 {
		return "the flow's thresholds"
	}
	return strings.Join(parts, ", ")
}

// gridNode is the question selected in the grid, if any.
func (m *Model) gridNode() *flow.Node {
	if m.grid == nil {
		return nil
	}
	cols := m.gridColumns()
	if len(cols) == 0 {
		return nil
	}
	return cols[min(max(m.gridCol, 0), len(cols)-1)]
}

// moveThreshold moves the selected question's threshold by steps and
// routes the grid again.
func (m *Model) moveThreshold(steps int) {
	n := m.gridNode()
	if m.flow == nil || m.flowView != viewGrid || n == nil {
		m.flash = "R runs every test; then ←/→ pick a question and [ ] try its threshold"
		return
	}
	if m.focus != focusFlow { // w, which keeps them, means "save test" elsewhere
		m.flash = "1 selects the grid; then [ ] try thresholds and w keeps them"
		return
	}
	if !tunable(n) {
		m.flash = n.ID + " doesn't route on confidence · ←/→ pick another question"
		return
	}
	if m.grid.asking {
		m.flash = "wait for the model to answer"
		return
	}
	next := m.shifts[n.ID] + steps
	cur := m.threshold(n)
	if runner.Threshold(m.flow, n, float64(next)*thresholdStep) == cur {
		return // pinned at 0.01 or 1
	}
	if m.shifts == nil {
		m.shifts = map[string]int{}
	}
	m.shifts[n.ID] = next
	if next == 0 {
		delete(m.shifts, n.ID)
	}
	if next == 0 {
		m.flash = fmt.Sprintf("%s back to %.2f, as in the flow", n.ID, m.threshold(n))
	} else {
		m.flash = fmt.Sprintf("%s: trying %.2f (%.2f in the flow) · w keeps it", n.ID, m.threshold(n), m.flow.ThresholdFor(n))
	}
	m.retune()
}

// keepThresholds writes the tried thresholds into the flow file. The file
// watcher then reloads it and runs the tests again.
func (m *Model) keepThresholds() {
	if len(m.shifts) == 0 {
		return
	}
	src, err := os.ReadFile(m.path)
	if err != nil {
		m.flash = err.Error()
		return
	}
	var kept []string
	for _, n := range m.flow.Nodes {
		if _, ok := m.shifts[n.ID]; !ok {
			continue
		}
		v := m.threshold(n)
		if src, err = flow.SetThreshold(src, n.ID, v); err != nil {
			m.flash = "couldn't keep it: " + err.Error()
			return
		}
		kept = append(kept, fmt.Sprintf("%s %.2f", n.ID, v))
	}
	if err := os.WriteFile(m.path, src, 0o644); err != nil {
		m.flash = err.Error()
		return
	}
	m.shifts = nil
	m.flash = "kept " + strings.Join(kept, ", ") + " in " + filepath.Base(m.path)
}

// retune shows the grid at the current thresholds: the model's own rows
// when they match, otherwise those rows routed again on the same answers.
// What changed is marked against the rows the model gave.
func (m *Model) retune() {
	g := m.grid
	if g == nil || !g.done || g.model == nil {
		return
	}
	if maps.Equal(m.shifts, g.ranShifts) {
		g.rows, g.prev = g.model, g.savedPrev
		return
	}
	g.prev = map[string]*gridRow{}
	g.rows = make([]*gridRow, len(g.model))
	for i, r := range g.model {
		g.prev[r.name] = r
		g.rows[i] = rerouted(context.Background(), m.flow, r, shiftsOf(m.shifts), nil)
	}
}

// rerouted runs a test again with every model call answered from what the
// model said when the grid ran. With real backends, what it was never asked
// is asked now, and kept with the rest.
func rerouted(ctx context.Context, f *flow.Flow, base *gridRow, shifts map[string]float64, real map[string]backend.Backend) *gridRow {
	if base.kase == nil || (base.err != nil && len(base.path) == 0) {
		return base
	}
	row := &gridRow{
		name: base.name, status: rowDone, inputs: base.inputs, events: map[string]runner.Event{},
		kase: base.kase, asked: base.asked, wrote: base.wrote, fetched: base.fetched,
	}
	rb := &replayBackend{asked: base.asked, wrote: base.wrote}
	backends := map[string]backend.Backend{}
	for name := range f.Backends {
		backends[name] = rb
		if b, ok := real[name]; ok {
			backends[name] = &askingBackend{rb, b}
		}
	}
	fakes := maps.Clone(base.fetched)
	if fakes == nil {
		fakes = map[string]runner.FakeResponse{}
	}
	maps.Copy(fakes, base.kase.HTTP)
	res, err := runner.Run(ctx, f, maps.Clone(base.inputs), runner.Options{
		Backends: backends, DryRun: true, Fake: fakes, ThresholdShifts: shifts,
		// Trying a threshold never touches the network: a GET the grid
		// didn't make waits for A, like a question.
		Before: func(_ context.Context, id string) error {
			n := f.Node(id)
			_, faked := fakes[id]
			if real == nil && !faked && n != nil && n.Kind == flow.Action && n.Action.Type == flow.ActHTTP && n.Action.Method == http.MethodGet {
				return &notAskedError{id, id + " was never fetched"}
			}
			return nil
		},
		OnStart: func(node string) { row.path = append(row.path, node) },
		OnEvent: func(ev runner.Event) {
			row.events[ev.Node] = ev
			row.remember(f.Node(ev.Node), ev)
		},
	})
	row.final, row.cost = res.State, res.CostUSD
	var na *notAskedError
	switch {
	case errors.As(err, &na):
		row.needs = na.node
		if row.needs == "" && len(row.path) > 0 {
			row.needs = row.path[len(row.path)-1]
		}
	case err != nil:
		row.err = err
	case len(base.kase.Expect) > 0:
		row.results = base.kase.Check(f, res.State)
	}
	return row
}

// remember keeps what a model call said during a grid run, so the run can
// be routed again at another threshold without calling it.
func (r *gridRow) remember(n *flow.Node, ev runner.Event) {
	if n == nil {
		return
	}
	out, _ := ev.Output.(map[string]any)
	switch {
	case n.Kind.IsDecision():
		for i, d := range ev.Decisions {
			if i < len(n.Questions) {
				r.asked[n.Questions[i].ID] = d
			}
		}
	case n.Kind == flow.Action && n.Action.Type == flow.ActLLM && len(ev.Asked) > 0:
		r.wrote[ev.Asked[0]] = fmt.Sprint(out["text"])
	case n.Kind == flow.Action && n.Action.Type == flow.ActHTTP && out["status"] != nil && out["dry_run"] != true:
		r.fetched[ev.Node] = runner.FakeResponse{Status: asInt(out["status"]), Body: out["body"]}
	}
}

// replayBackend answers with what the model said before, and fails for
// anything it was never asked.
type replayBackend struct {
	asked map[string]backend.Decision
	wrote map[string]string
}

// notAskedError is a model call, or an http GET, a rerouted run would have
// to make. node is where, when known.
type notAskedError struct{ node, what string }

func (e *notAskedError) Error() string { return e.what }

func (b *replayBackend) Name() string { return "replay" }

func (b *replayBackend) Caps() backend.Caps { return backend.Caps{MultiQuestion: true} }

func (b *replayBackend) Decide(_ context.Context, _ map[string]any, qs []flow.Question) (backend.Response, error) {
	var resp backend.Response
	for _, q := range qs {
		d, ok := b.asked[q.ID]
		if !ok {
			node, _, _ := strings.Cut(q.ID, ".")
			return resp, &notAskedError{node, "the model was never asked " + node}
		}
		resp.Decisions = append(resp.Decisions, d)
	}
	return resp, nil
}

func (b *replayBackend) Write(_ context.Context, req backend.WriteRequest) (backend.WriteResponse, error) {
	text, ok := b.wrote[req.Prompt]
	if !ok {
		return backend.WriteResponse{}, &notAskedError{"", "the model was never asked to write this"}
	}
	return backend.WriteResponse{Text: text}, nil
}

// askingBackend answers from what the model said before and asks it the
// rest, keeping the new answers for the next threshold tried.
type askingBackend struct {
	*replayBackend
	real backend.Backend
}

func (b *askingBackend) Caps() backend.Caps { return b.real.Caps() }

func (b *askingBackend) Decide(ctx context.Context, state map[string]any, qs []flow.Question) (backend.Response, error) {
	resp, err := b.replayBackend.Decide(ctx, state, qs)
	var na *notAskedError
	if !errors.As(err, &na) {
		return resp, err
	}
	resp, err = b.real.Decide(ctx, state, qs)
	if err == nil {
		for i, d := range resp.Decisions {
			if i < len(qs) {
				b.asked[qs[i].ID] = d
			}
		}
	}
	return resp, err
}

func (b *askingBackend) Write(ctx context.Context, req backend.WriteRequest) (backend.WriteResponse, error) {
	if resp, err := b.replayBackend.Write(ctx, req); err == nil {
		return resp, nil
	}
	w, ok := b.real.(backend.Writer)
	if !ok {
		return backend.WriteResponse{}, fmt.Errorf("backend %s can't write text", b.real.Name())
	}
	resp, err := w.Write(ctx, req)
	if err == nil {
		b.wrote[req.Prompt] = resp.Text
	}
	return resp, err
}

type (
	askRowMsg struct {
		id, i int
		row   *gridRow
	}
	askDoneMsg struct{ id int }
)

// askMissing asks the model only what the ? tests need at the threshold
// being tried, instead of running every test again.
func (m *Model) askMissing() tea.Cmd {
	g := m.grid
	if g == nil || !g.done || g.asking {
		return nil
	}
	var need []int
	for i, r := range g.rows {
		if r.needs != "" {
			need = append(need, i)
		}
	}
	if len(need) == 0 {
		m.flash = "no test needs the model · ? marks those that do"
		return nil
	}
	if g.ranWith != m.modelsInUse() {
		// Asking a different model now would mix two models' answers in one
		// row.
		ran, _, _ := strings.Cut(g.ranWith, "/")
		m.flash = "the grid's answers came from " + ran + "; R runs it again with " + m.backend
		return nil
	}
	f, backends, err := m.gridFlow()
	if err != nil {
		m.flash = err.Error()
		return nil
	}
	shifts := shiftsOf(m.shifts)
	g.asking = true
	ctx, cancel := context.WithCancel(context.Background())
	g.askCancel = cancel
	ch := make(chan tea.Msg, len(need)+1)
	bases := make([]*gridRow, len(need))
	for k, i := range need {
		bases[k] = g.model[i]
		g.rows[i] = &gridRow{name: g.rows[i].name, status: rowRunning, inputs: g.rows[i].inputs}
	}
	id := g.id
	go func() {
		defer close(ch)
		sem := make(chan struct{}, gridParallel)
		var wg sync.WaitGroup
		for k, i := range need {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				row := rerouted(ctx, f, bases[k], shifts, backends)
				if ctx.Err() == nil {
					ch <- askRowMsg{id, i, row}
				}
			}()
		}
		wg.Wait()
		cancel()
		ch <- askDoneMsg{id}
	}()
	next := func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
	g.askNext = next
	return tea.Batch(next, m.kick())
}
