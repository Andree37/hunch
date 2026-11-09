package tui

import (
	"context"
	"errors"
	"fmt"
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
		g.rows[i] = m.rerouted(r)
	}
}

// rerouted runs a test again with every model call answered from what the
// model said when the grid ran.
func (m *Model) rerouted(base *gridRow) *gridRow {
	if base.kase == nil || (base.err != nil && len(base.path) == 0) {
		return base
	}
	row := &gridRow{
		name: base.name, status: rowDone, inputs: base.inputs, events: map[string]runner.Event{},
		kase: base.kase, asked: base.asked, wrote: base.wrote, fetched: base.fetched,
	}
	rb := &replayBackend{asked: base.asked, wrote: base.wrote}
	backends := map[string]backend.Backend{}
	for name := range m.flow.Backends {
		backends[name] = rb
	}
	fakes := maps.Clone(base.fetched)
	if fakes == nil {
		fakes = map[string]runner.FakeResponse{}
	}
	maps.Copy(fakes, base.kase.HTTP)
	res, err := runner.Run(context.Background(), m.flow, maps.Clone(base.inputs), runner.Options{
		Backends: backends, DryRun: true, Fake: fakes, ThresholdShift: m.shift(),
		OnStart: func(node string) { row.path = append(row.path, node) },
		OnEvent: func(ev runner.Event) { row.events[ev.Node] = ev },
	})
	row.final = res.State
	var na *notAskedError
	switch {
	case errors.As(err, &na):
		if len(row.path) > 0 {
			row.needs = row.path[len(row.path)-1]
		}
	case err != nil:
		row.err = err
	case len(base.kase.Expect) > 0:
		row.results = base.kase.Check(m.flow, res.State)
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

type notAskedError struct{ what string }

func (e *notAskedError) Error() string { return "the model was never asked " + e.what }

func (b *replayBackend) Name() string { return "replay" }

func (b *replayBackend) Caps() backend.Caps { return backend.Caps{MultiQuestion: true} }

func (b *replayBackend) Decide(_ context.Context, _ map[string]any, qs []flow.Question) (backend.Response, error) {
	var resp backend.Response
	for _, q := range qs {
		d, ok := b.asked[q.ID]
		if !ok {
			node, _, _ := strings.Cut(q.ID, ".")
			return resp, &notAskedError{node}
		}
		resp.Decisions = append(resp.Decisions, d)
	}
	return resp, nil
}

func (b *replayBackend) Write(_ context.Context, req backend.WriteRequest) (backend.WriteResponse, error) {
	text, ok := b.wrote[req.Prompt]
	if !ok {
		return backend.WriteResponse{}, &notAskedError{"to write this"}
	}
	return backend.WriteResponse{Text: text}, nil
}
