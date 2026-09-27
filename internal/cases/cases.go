// Package cases handles test cases: saved inputs for a flow, with optional
// expected answers. They live next to the flow, in <flow>.tests/<name>.yaml:
//
//	description: Boss wants to meet
//	input:
//	  sender: boss
//	  message: Can we sync Thursday?
//	expect:
//	  worth_it: yes          # bool: yes / no
//	  intent: meeting        # choice: an option
//	  effort: "<=3"          # score: a condition, as in branches
//	  tone.urgency: ">=4"    # a question inside a questions node
package cases

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/tmpl"
)

type Case struct {
	Name        string
	Path        string
	Description string
	Keys        []string // input keys, in file order
	Input       map[string]any
	Expect      []Expectation
}

type Expectation struct {
	Target string // "node" or "node.question"
	Want   string
}

type Result struct {
	Expectation
	Got string
	OK  bool
}

// Dir is where a flow's cases live: examples/inbox.yaml → examples/inbox.tests.
func Dir(flowPath string) string {
	return strings.TrimSuffix(flowPath, filepath.Ext(flowPath)) + ".tests"
}

// Load reads every case in dir, sorted by name. A missing dir means no cases.
func Load(dir string) ([]*Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	var out []*Case
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		c, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		c.Path = p
		c.Name = strings.TrimSuffix(filepath.Base(p), ".yaml")
		out = append(out, c)
	}
	return out, nil
}

func Parse(data []byte) (*Case, error) {
	c := &Case{Input: map[string]any{}}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return c, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: expected a map", root.Line)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "description":
			c.Description = v.Value
		case "input":
			if v.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: input must be a map", v.Line)
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				var val any
				if err := v.Content[j+1].Decode(&val); err != nil {
					return nil, fmt.Errorf("line %d: %v", v.Content[j+1].Line, err)
				}
				key := v.Content[j].Value
				c.Keys = append(c.Keys, key)
				c.Input[key] = val
			}
		case "expect":
			if v.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: expect must be a map", v.Line)
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				c.Expect = append(c.Expect, Expectation{Target: v.Content[j].Value, Want: v.Content[j+1].Value})
			}
		default:
			return nil, fmt.Errorf("line %d: unknown field %q (want description, input, expect)", k.Line, k.Value)
		}
	}
	return c, nil
}

// Save writes the case's input. An existing file only has its `input:`
// section replaced, so hand-written expectations and comments survive.
func (c *Case) Save() error {
	src, err := os.ReadFile(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		src = nil
		if c.Description != "" {
			d, _ := yaml.Marshal(map[string]string{"description": c.Description})
			src = d
		}
		if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(src) == 0 {
		src = []byte("{}")
	}
	out, err := flow.SpliceSection(src, "input", c.Keys, c.Input, "expect")
	if err != nil {
		return err
	}
	if string(src) == "{}" {
		out = []byte(strings.TrimPrefix(string(out), "{}\n"))
	}
	return os.WriteFile(c.Path, out, 0o644)
}

// Check compares a finished run's state against the case's expectations.
func (c *Case) Check(f *flow.Flow, state map[string]any) []Result {
	out := make([]Result, len(c.Expect))
	for i, e := range c.Expect {
		out[i] = check(f, state, e)
	}
	return out
}

func Passed(rs []Result) bool {
	return !slices.ContainsFunc(rs, func(r Result) bool { return !r.OK })
}

func check(f *flow.Flow, state map[string]any, e Expectation) Result {
	r := Result{Expectation: e}
	nodeID, qname, multi := strings.Cut(e.Target, ".")
	n := f.Node(nodeID)
	if n == nil || !n.Kind.IsDecision() {
		r.Got = "not a decision node"
		return r
	}
	q := n.Questions[0]
	if multi {
		i := slices.IndexFunc(n.Questions, func(q flow.Question) bool { return q.Name == qname })
		if n.Kind != flow.Questions || i < 0 {
			r.Got = "no such question"
			return r
		}
		q = n.Questions[i]
	}
	v, ok := tmpl.Lookup(state, e.Target+".answer")
	if !ok {
		r.Got = "didn't run"
		return r
	}

	switch q.Kind {
	case flow.Bool:
		yes, _ := v.(bool)
		r.Got = "no"
		if yes {
			r.Got = "yes"
		}
		r.OK = r.Got == normBool(e.Want)
	case flow.Choice:
		r.Got = tmpl.Format(v)
		r.OK = r.Got == e.Want
	case flow.Score:
		x, _ := v.(float64)
		r.Got = fmt.Sprintf("%.2f", x)
		ok, err := flow.MatchScore(e.Want, x)
		if err != nil {
			r.Got = err.Error()
		}
		r.OK = ok
	}
	return r
}

func normBool(s string) string {
	switch strings.ToLower(s) {
	case "yes", "true", "y":
		return "yes"
	case "no", "false", "n":
		return "no"
	}
	return s
}

// ValidName reports whether name works as a case file name.
func ValidName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\:*?"<>|`) && !strings.HasPrefix(name, ".")
}
