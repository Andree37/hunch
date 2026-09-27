// Package validate checks a flow for broken wiring before it runs.
package validate

import (
	"fmt"
	"maps"
	"slices"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/tmpl"
)

type Severity int

const (
	Error Severity = iota
	Warning
)

func (s Severity) String() string {
	if s == Error {
		return "error"
	}
	return "warning"
}

type Issue struct {
	Severity Severity
	Node     string
	Msg      string
}

func (i Issue) String() string {
	if i.Node == "" {
		return fmt.Sprintf("%s: %s", i.Severity, i.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", i.Severity, i.Node, i.Msg)
}

func HasErrors(issues []Issue) bool {
	return slices.ContainsFunc(issues, func(i Issue) bool { return i.Severity == Error })
}

func Flow(f *flow.Flow) []Issue {
	v := &validator{f: f}
	if len(f.Nodes) == 0 {
		v.errorf("", "flow has no nodes")
		return v.issues
	}
	if f.Node(f.Start) == nil {
		v.errorf("", "start node %q does not exist", f.Start)
	}
	if _, ok := f.Backends[f.DefaultBackend]; f.DefaultBackend != "" && !ok {
		v.errorf("", "default backend %q is not defined", f.DefaultBackend)
	}
	for _, n := range f.Nodes {
		v.node(n)
	}
	v.reachability()
	v.cycles()
	v.refs()
	v.unusedInputs()
	return v.issues
}

type validator struct {
	f      *flow.Flow
	issues []Issue
}

func (v *validator) errorf(node, format string, args ...any) {
	v.issues = append(v.issues, Issue{Error, node, fmt.Sprintf(format, args...)})
}

func (v *validator) warnf(node, format string, args ...any) {
	v.issues = append(v.issues, Issue{Warning, node, fmt.Sprintf(format, args...)})
}

func (v *validator) node(n *flow.Node) {
	for _, t := range n.Then.Targets() {
		if v.f.Node(t) == nil {
			v.errorf(n.ID, "routes to %q, which does not exist", t)
		}
	}

	if n.Kind.IsDecision() {
		name := v.f.BackendFor(n)
		if name == "" {
			v.errorf(n.ID, "no backend (set backends.default or backend on the node)")
		} else if _, ok := v.f.Backends[name]; !ok {
			v.errorf(n.ID, "backend %q is not defined", name)
		}
		if len(n.Then.Targets()) == 0 {
			v.warnf(n.ID, "has no `then`, so its answer is never used")
		}
	} else if n.Action.Type == flow.ActLLM {
		name := v.f.BackendFor(n)
		cfg, ok := v.f.Backends[name]
		switch {
		case name == "":
			v.errorf(n.ID, "no backend to write with (set backends.writer or backend on the node)")
		case !ok:
			v.errorf(n.ID, "backend %q is not defined", name)
		case !backend.CanWrite(cfg.Kind):
			v.errorf(n.ID, "backend %q (%s) can't write text; use openai, anthropic, bedrock or mock", name, cfg.Kind)
		}
	}

	r := n.Then
	if len(r.Branches) == 0 {
		return
	}
	switch n.Kind {
	case flow.Questions, flow.Action:
		v.errorf(n.ID, "%s nodes take a single target (then: node), not branches", n.Kind)
	case flow.Bool:
		v.branchKeys(n, func(k string) bool { return k == "yes" || k == "no" })
		v.covered(n, "yes", "no")
	case flow.Choice:
		opts := n.Questions[0].Options
		v.branchKeys(n, func(k string) bool { return slices.Contains(opts, k) })
		v.covered(n, opts...)
	case flow.Score:
		v.scoreBranches(n)
	}
}

func (v *validator) branchKeys(n *flow.Node, valid func(string) bool) {
	for _, b := range n.Then.Branches {
		if b.When != flow.Unsure && b.When != flow.Default && !valid(b.When) {
			v.errorf(n.ID, "branch %q can never match a %s answer", b.When, n.Kind)
		}
	}
}

func (v *validator) covered(n *flow.Node, answers ...string) {
	if _, ok := n.Then.Get(flow.Default); ok {
		return
	}
	for _, a := range answers {
		if _, ok := n.Then.Get(a); !ok {
			v.warnf(n.ID, "no route for answer %q (add it or a `_` fallback)", a)
		}
	}
}

func (v *validator) scoreBranches(n *flow.Node) {
	var conds []string
	for _, b := range n.Then.Branches {
		if b.When == flow.Unsure || b.When == flow.Default {
			continue
		}
		if _, err := flow.MatchScore(b.When, 0); err != nil {
			v.errorf(n.ID, "%v", err)
			continue
		}
		conds = append(conds, b.When)
	}
	if _, ok := n.Then.Get(flow.Default); ok {
		return
	}
	// Answers are expected values, so any point on the scale can come back.
	sc := n.Questions[0].Scale
	for x := float64(sc.Min); x <= float64(sc.Max); x += 0.05 {
		hit := slices.ContainsFunc(conds, func(c string) bool {
			ok, _ := flow.MatchScore(c, x)
			return ok
		})
		if !hit {
			v.warnf(n.ID, "no route for scores around %.2f (add a range or a `_` fallback)", x)
			return
		}
	}
}

func (v *validator) reachability() {
	seen := v.reach(v.f.Start, func(n *flow.Node) []string { return n.Then.Targets() })
	for _, n := range v.f.Nodes {
		if !seen[n.ID] {
			v.warnf(n.ID, "unreachable from start node %q", v.f.Start)
		}
	}
}

func (v *validator) cycles() {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(id string, path []string)
	visit = func(id string, path []string) {
		n := v.f.Node(id)
		if n == nil || state[id] == done {
			return
		}
		if state[id] == visiting {
			i := slices.Index(path, id)
			v.warnf(id, "loops back via %v; runs are capped by --max-visits", append(path[i:], id))
			return
		}
		state[id] = visiting
		for _, t := range n.Then.Targets() {
			visit(t, append(path, id))
		}
		state[id] = done
	}
	for _, n := range v.f.Nodes {
		visit(n.ID, nil)
	}
}

// refs checks that every {{ref}} points at initial state or at a node that
// has always run by the time the node using it runs.
func (v *validator) refs() {
	parents := map[string][]string{}
	for _, n := range v.f.Nodes {
		for _, t := range n.Then.Targets() {
			parents[t] = append(parents[t], n.ID)
		}
	}
	always := v.alwaysBefore(parents)
	for _, n := range v.f.Nodes {
		var ancestors map[string]bool
		for _, text := range n.Texts() {
			for _, ref := range tmpl.Refs(text) {
				root := ref.Root()
				if v.f.Node(root) == nil {
					continue // an input; runs check they're given
				}
				if ancestors == nil {
					ancestors = v.ancestors(n.ID, parents)
				}
				switch {
				case !ancestors[root]:
					v.errorf(n.ID, "{{%s}} refers to node %q, which never runs before %q", ref.Path, root, n.ID)
				case always[n.ID] != nil && !always[n.ID][root] && !ref.Optional:
					v.warnf(n.ID, "{{%s}}: some paths reach %q without running %q, and the run fails there (use {{%s?}} if that's expected)",
						ref.Path, n.ID, root, ref.Path)
				}
			}
		}
	}
}

// alwaysBefore returns, for each reachable node, the nodes that run before it
// on every path from start. It iterates must[n] = ∩ over parents p of
// (must[p] ∪ {p}) to a fixpoint, starting from "everything".
func (v *validator) alwaysBefore(parents map[string][]string) map[string]map[string]bool {
	reachable := v.reach(v.f.Start, func(n *flow.Node) []string { return n.Then.Targets() })
	must := map[string]map[string]bool{}
	for id := range reachable {
		must[id] = maps.Clone(reachable)
	}
	must[v.f.Start] = map[string]bool{}

	for changed := true; changed; {
		changed = false
		for _, n := range v.f.Nodes {
			if !reachable[n.ID] || n.ID == v.f.Start {
				continue
			}
			var acc map[string]bool
			for _, p := range parents[n.ID] {
				if !reachable[p] {
					continue
				}
				via := maps.Clone(must[p])
				via[p] = true
				if acc == nil {
					acc = via
				} else {
					maps.DeleteFunc(acc, func(k string, _ bool) bool { return !via[k] })
				}
			}
			if acc == nil {
				acc = map[string]bool{}
			}
			if !maps.Equal(acc, must[n.ID]) {
				must[n.ID] = acc
				changed = true
			}
		}
	}
	return must
}

func (v *validator) ancestors(id string, parents map[string][]string) map[string]bool {
	up := func(n *flow.Node) []string { return parents[n.ID] }
	seen := map[string]bool{}
	for _, p := range parents[id] {
		maps.Copy(seen, v.reach(p, up))
	}
	return seen
}

func (v *validator) reach(from string, next func(*flow.Node) []string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := v.f.Node(id)
		if n == nil || seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, next(n)...)
	}
	return seen
}

func (v *validator) unusedInputs() {
	used := map[string]bool{}
	for _, n := range v.f.Nodes {
		for _, t := range n.Texts() {
			for _, r := range tmpl.Refs(t) {
				used[r.Root()] = true
			}
		}
	}
	for _, s := range v.f.InputSpecs {
		if !used[s.Name] {
			v.warnf("", "input %q is declared but no node uses it", s.Name)
		}
	}
}
