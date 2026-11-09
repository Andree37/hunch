package tui

import (
	"context"
	"net/http"
	"sync"

	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"maps"
	"strings"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
)

// [ and ] try other thresholds without editing the flow: every decision's
// threshold moves by thresholdStep. The grid routes each test again on the
// answers the model already gave, so trying a threshold costs nothing; a
// test that now reaches a question the model was never asked shows ? until
// R asks it.

const thresholdStep = 0.05

// shift is how far the thresholds are moved from the flow's.
func (m *Model) shift() float64 { return float64(m.shiftSteps) * thresholdStep }

// threshold is the threshold n routes on in this TUI, shifted.
func (m *Model) threshold(n *flow.Node) float64 {
	return runner.Threshold(m.flow, n, m.shift())
}

// ownThresholds reports whether any decision sets its own threshold, so a
// shift can't be shown as one number.
func (m *Model) ownThresholds() bool {
	for _, n := range m.flow.Nodes {
		if n.Kind.IsDecision() && n.Threshold > 0 {
			return true
		}
	}
	return false
}

// thresholdText is "threshold 0.70", or "thresholds +0.10" when decisions
// have their own.
func (m *Model) thresholdText(steps int) string {
	shift := float64(steps) * thresholdStep
	if m.ownThresholds() {
		if steps == 0 {
			return "the flow's thresholds"
		}
		return fmt.Sprintf("thresholds %+.2f", shift)
	}
	return fmt.Sprintf("threshold %.2f", runner.Threshold(m.flow, &flow.Node{}, shift))
}

// moveThreshold shifts every threshold by steps and routes the grid again.
func (m *Model) moveThreshold(steps int) {
	if m.flow == nil {
		return
	}
	next := m.shiftSteps + steps
	// Stop once every threshold is pinned at 0 or 1.
	moves := false
	for _, n := range m.flow.Nodes {
		if !n.Kind.IsDecision() {
			continue
		}
		cur := runner.Threshold(m.flow, n, m.shift())
		if runner.Threshold(m.flow, n, float64(next)*thresholdStep) != cur {
			moves = true
		}
	}
	if !moves {
		return
	}
	if m.grid != nil && m.grid.asking {
		m.flash = "wait for the model to answer"
		return
	}
	m.shiftSteps = next
	switch {
	case m.shiftSteps == 0:
		m.flash = "back to " + m.thresholdText(0)
	case m.ownThresholds():
		m.flash = fmt.Sprintf("trying %s · to keep it, change them in the flow", m.thresholdText(m.shiftSteps))
	default:
		m.flash = fmt.Sprintf("trying %s · to keep it, set threshold: %.2f in the flow", m.thresholdText(m.shiftSteps), m.threshold(&flow.Node{}))
	}
	m.retune()
}

// retune shows the grid at the current thresholds: the model's own rows
// when they match, otherwise those rows routed again on the same answers.
// What changed is marked against the rows the model gave.
func (m *Model) retune() {
	g := m.grid
	if g == nil || !g.done || g.model == nil {
		return
	}
	if m.shiftSteps == g.ranSteps {
		g.rows, g.prev = g.model, g.savedPrev
		return
	}
	g.prev = map[string]*gridRow{}
	g.rows = make([]*gridRow, len(g.model))
	for i, r := range g.model {
		g.prev[r.name] = r
		g.rows[i] = rerouted(context.Background(), m.flow, r, m.shift(), nil)
	}
}

// rerouted runs a test again with every model call answered from what the
// model said when the grid ran. With real backends, what it was never asked
// is asked now, and kept with the rest.
func rerouted(ctx context.Context, f *flow.Flow, base *gridRow, shift float64, real map[string]backend.Backend) *gridRow {
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
		Backends: backends, DryRun: true, Fake: fakes, ThresholdShift: shift,
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
	f, backends, err := m.gridFlow()
	if err != nil {
		m.flash = err.Error()
		return nil
	}
	shift := m.shift()
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
				row := rerouted(ctx, f, bases[k], shift, backends)
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
