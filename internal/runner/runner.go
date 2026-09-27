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

	res := &Result{State: maps.Clone(initial)}
	if res.State == nil {
		res.State = map[string]any{}
	}
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
		if n.Kind.IsDecision() {
			err = decideNode(ctx, f, n, res.State, opts, &ev)
		} else {
			err = actionNode(ctx, n, res.State, opts, &ev)
		}
		ev.Latency = time.Since(start)
		if err != nil {
			return res, fmt.Errorf("%s: %w", n.ID, err)
		}

		res.State[n.ID] = ev.Output
		res.CostUSD += ev.CostUSD
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
		text, err := tmpl.Render(q.Text, state)
		if err != nil {
			return err
		}
		q.Text = text
		qs[i] = q
		ev.Asked = append(ev.Asked, text)
	}

	resp, err := decide(ctx, b, state, qs)
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

func actionNode(ctx context.Context, n *flow.Node, state map[string]any, opts Options, ev *Event) error {
	ev.Next = n.Then.Next
	a := n.Action
	switch a.Type {
	case "log":
		msg, err := tmpl.Render(a.Message, state)
		if err != nil {
			return err
		}
		ev.Output = map[string]any{"message": msg}
	case "shell":
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
