// Package runner walks a flow: it asks backends at decision nodes, runs
// action nodes, merges every result into state, and follows the routes.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/tmpl"
)

type Options struct {
	Backends  map[string]backend.Backend
	MaxSteps  int       // default 100
	MaxVisits int       // per node, default 5
	Stdout    io.Writer // shell action output
	DryRun    bool      // http actions record their request instead of sending it
	// Before is called before each node runs and may block, e.g. to step
	// through a flow one node at a time. An error stops the run.
	Before  func(ctx context.Context, node string) error
	OnStart func(node string)
	OnEvent func(Event)
}

// Event records one step of a run. Events are what traces are made of.
type Event struct {
	Step      int                `json:"step"`
	Node      string             `json:"node"`
	Kind      flow.Kind          `json:"kind"`
	Backend   string             `json:"backend,omitempty"`
	Asked     []string           `json:"asked,omitempty"` // question texts as sent
	Saw       map[string]any     `json:"saw,omitempty"`   // state sent, when the node limits it with sees
	Decisions []backend.Decision `json:"decisions,omitempty"`
	Output    any                `json:"output"`
	Branch    string             `json:"branch,omitempty"`
	Next      string             `json:"next,omitempty"`
	CostUSD   float64            `json:"cost_usd"`
	Latency   time.Duration      `json:"latency_ns"`
}

type Result struct {
	State   map[string]any
	Path    []string
	CostUSD float64
	Outputs map[string]any // everything output nodes set, later ones winning
}

func Run(ctx context.Context, f *flow.Flow, initial map[string]any, opts Options) (*Result, error) {
	if opts.MaxSteps == 0 {
		opts.MaxSteps = 100
	}
	if opts.MaxVisits == 0 {
		opts.MaxVisits = 5
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	// The flow's own state holds constants (e.g. shared definitions); the
	// caller's input goes on top.
	state := maps.Clone(f.State)
	if state == nil {
		state = map[string]any{}
	}
	maps.Copy(state, initial)
	res := &Result{State: state, Outputs: map[string]any{}}
	visits := map[string]int{}

	for step, cur := 1, f.Start; cur != ""; step++ {
		n := f.Node(cur)
		if n == nil {
			return res, fmt.Errorf("node %q does not exist", cur)
		}
		if step > opts.MaxSteps {
			return res, fmt.Errorf("stopped after %d steps", opts.MaxSteps)
		}
		if visits[cur]++; visits[cur] > opts.MaxVisits {
			return res, fmt.Errorf("node %q ran more than %d times", cur, opts.MaxVisits)
		}
		if opts.Before != nil {
			if err := opts.Before(ctx, cur); err != nil {
				return res, err
			}
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Path = append(res.Path, cur)
		if opts.OnStart != nil {
			opts.OnStart(cur)
		}

		ev := Event{Step: step, Node: n.ID, Kind: n.Kind}
		start := time.Now()
		var err error
		switch {
		case n.Kind.IsDecision():
			err = decideNode(ctx, f, n, res.State, opts, &ev)
		case n.Kind == flow.Switch:
			err = switchNode(n, res.State, &ev)
		default:
			err = actionNode(ctx, f, n, res.State, opts, &ev)
		}
		ev.Latency = time.Since(start)
		if err != nil {
			return res, fmt.Errorf("%s: %w", n.ID, err)
		}

		res.State[n.ID] = ev.Output
		res.CostUSD += ev.CostUSD
		if n.Kind == flow.Action && n.Action.Type == flow.ActOutput {
			maps.Copy(res.Outputs, ev.Output.(map[string]any))
			// Later nodes read them as {{outputs.name}}; kept out of state
			// until something is set, so deciders aren't shown an empty map.
			res.State[flow.OutputsRef] = res.Outputs
		}
		if opts.OnEvent != nil {
			opts.OnEvent(ev)
		}
		cur = ev.Next
	}
	return res, nil
}

func decideNode(ctx context.Context, f *flow.Flow, n *flow.Node, state map[string]any, opts Options, ev *Event) error {
	name := f.BackendFor(n)
	b, ok := opts.Backends[name]
	if !ok {
		return fmt.Errorf("backend %q is not configured", name)
	}
	ev.Backend = name

	qs := make([]flow.Question, len(n.Questions))
	for i, q := range n.Questions {
		q, err := renderQuestion(q, state)
		if err != nil {
			return err
		}
		qs[i] = q
		ev.Asked = append(ev.Asked, q.Text)
	}

	shown := state
	if n.Sees != nil {
		var err error
		if shown, err = seen(n.Sees, state); err != nil {
			return err
		}
		ev.Saw = shown
	}
	resp, err := decide(ctx, b, shown, qs)
	if err != nil {
		return err
	}
	ev.Decisions = resp.Decisions
	ev.CostUSD = resp.CostUSD

	if n.Kind == flow.Questions {
		out := map[string]any{}
		for i, d := range resp.Decisions {
			out[qs[i].Name] = decisionState(d)
		}
		ev.Output = out
		ev.Next = n.Then.Next
		return nil
	}

	d := resp.Decisions[0]
	ev.Output = decisionState(d)
	ev.Branch, ev.Next, err = route(n, d, f.ThresholdFor(n))
	return err
}

// seen picks the paths a node may see out of state, keyed by path. A path
// ending in ? is optional.
func seen(paths []string, state map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(paths))
	for _, p := range paths {
		path, optional := strings.CutSuffix(p, "?")
		v, ok := tmpl.Lookup(state, path)
		if !ok {
			if optional {
				continue
			}
			return nil, fmt.Errorf("sees %s, which isn't in state", path)
		}
		out[path] = v
	}
	return out, nil
}

// renderQuestion fills refs in a question's text, answer descriptions and
// score levels, so shared definitions can live once in state.
func renderQuestion(q flow.Question, state map[string]any) (flow.Question, error) {
	var err error
	if q.Text, err = tmpl.Render(q.Text, state); err != nil {
		return q, err
	}
	if len(q.Criteria) > 0 {
		c := make(map[string]string, len(q.Criteria))
		for k, v := range q.Criteria {
			if c[k], err = tmpl.Render(v, state); err != nil {
				return q, err
			}
		}
		q.Criteria = c
	}
	if len(q.Levels) > 0 {
		l := make([]string, len(q.Levels))
		for i, v := range q.Levels {
			if l[i], err = tmpl.Render(v, state); err != nil {
				return q, err
			}
		}
		q.Levels = l
	}
	return q, nil
}

// decide asks the backend, splitting multi-question requests for backends
// that can only take one question at a time.
func decide(ctx context.Context, b backend.Backend, state map[string]any, qs []flow.Question) (backend.Response, error) {
	var resp backend.Response
	if len(qs) == 1 || b.Caps().MultiQuestion {
		r, err := b.Decide(ctx, state, qs)
		if err != nil {
			return resp, err
		}
		resp = r
	} else {
		for _, q := range qs {
			r, err := b.Decide(ctx, state, []flow.Question{q})
			if err != nil {
				return resp, err
			}
			resp.Decisions = append(resp.Decisions, r.Decisions...)
			resp.CostUSD += r.CostUSD
		}
	}
	if len(resp.Decisions) != len(qs) {
		return resp, fmt.Errorf("backend %s returned %d answers for %d questions", b.Name(), len(resp.Decisions), len(qs))
	}
	return resp, nil
}

// decisionState is how a decision appears in state, e.g. {{intent.answer}}.
func decisionState(d backend.Decision) map[string]any {
	m := map[string]any{"answer": d.Answer, "confidence": d.Confidence}
	probs := map[string]any{}
	for k, p := range d.Probs {
		probs[k] = p
	}
	switch d.Kind {
	case flow.Bool:
		m["p"] = d.Probs["yes"]
	case flow.Choice:
		m["probs"] = probs
	case flow.Score:
		m["probs"] = probs
		if v, ok := d.Answer.(float64); ok {
			m["level"] = int(math.Round(v))
		}
	}
	return m
}

func route(n *flow.Node, d backend.Decision, threshold float64) (branch, next string, err error) {
	r := n.Then
	if len(r.Branches) == 0 {
		return "", r.Next, nil
	}
	if to, ok := r.Get(flow.Unsure); ok && d.Confidence < threshold {
		return flow.Unsure, to, nil
	}
	switch d.Kind {
	case flow.Bool:
		key := "no"
		if yes, _ := d.Answer.(bool); yes {
			key = "yes"
		}
		if to, ok := r.Get(key); ok {
			return key, to, nil
		}
	case flow.Choice:
		key, _ := d.Answer.(string)
		if to, ok := r.Get(key); ok {
			return key, to, nil
		}
	case flow.Score:
		v, _ := d.Answer.(float64)
		for _, b := range r.Branches {
			if b.When == flow.Unsure || b.When == flow.Default {
				continue
			}
			ok, err := flow.MatchScore(b.When, v)
			if err != nil {
				return "", "", err
			}
			if ok {
				return b.When, b.To, nil
			}
		}
	}
	if to, ok := r.Get(flow.Default); ok {
		return flow.Default, to, nil
	}
	return "", "", fmt.Errorf("no route for answer %s", tmpl.Format(d.Answer))
}

// switchNode routes on a rendered value: the branch named after it, else _.
func switchNode(n *flow.Node, state map[string]any, ev *Event) error {
	v, err := tmpl.Render(n.Switch, state)
	if err != nil {
		return err
	}
	ev.Output = map[string]any{"value": v}
	r := n.Then
	if len(r.Branches) == 0 {
		ev.Next = r.Next
		return nil
	}
	for _, key := range []string{v, flow.Default} {
		if to, ok := r.Get(key); ok {
			ev.Branch, ev.Next = key, to
			return nil
		}
	}
	return fmt.Errorf("no route for value %q", v)
}

func actionNode(ctx context.Context, f *flow.Flow, n *flow.Node, state map[string]any, opts Options, ev *Event) error {
	ev.Next = n.Then.Next
	a := n.Action
	switch a.Type {
	case flow.ActLLM:
		return llmNode(ctx, f, n, state, opts, ev)
	case flow.ActHTTP:
		return httpNode(ctx, n, state, opts, ev)
	case flow.ActOutput:
		return outputNode(n, state, ev)
	case flow.ActLog:
		msg, err := tmpl.Render(a.Message, state)
		if err != nil {
			return err
		}
		ev.Output = map[string]any{"message": msg}
	case flow.ActShell:
		timeout := 60 * time.Second
		if a.Timeout > 0 {
			timeout = time.Duration(a.Timeout * float64(time.Second))
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// Every substituted value is escaped for the quoting it sits in, so
		// state can't inject commands.
		script, err := tmpl.RenderFunc(a.Run, state, func(v string, at int) string {
			return shellEscape(v, quoteContext(a.Run[:at]))
		})
		if err != nil {
			return err
		}
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		cmd.Stdout = io.MultiWriter(&out, opts.Stdout)
		cmd.Stderr = opts.Stdout
		code := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("shell command timed out after %s", timeout)
			}
			if !errors.As(err, &exitErr) {
				return err
			}
			code = exitErr.ExitCode()
		}
		ev.Output = map[string]any{"stdout": strings.TrimSpace(out.String()), "exit_code": code}
	}
	return nil
}

// quoteContext reports the quote a POSIX shell would be inside at the end of
// s: 0 when unquoted, otherwise the single or double quote character.
func quoteContext(s string) byte {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q == '\'':
			if c == '\'' {
				q = 0
			}
		case c == '\\':
			i++ // escaped character, outside single quotes
		case q == '"':
			if c == '"' {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		}
	}
	return q
}

func shellEscape(v string, quote byte) string {
	switch quote {
	case '\'':
		return strings.ReplaceAll(v, "'", `'\''`)
	case '"':
		var b strings.Builder
		for _, r := range v {
			if strings.ContainsRune("\\\"$`", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
