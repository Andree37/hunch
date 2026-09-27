package cases

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
)

// FromRun turns a finished run into a test case: its input, the http
// replies it got (as fakes, so it re-runs offline) and every answer it gave
// (as expectations). Correct the expectations the run got wrong.
func FromRun(f *flow.Flow, input map[string]any, path []string, events map[string]runner.Event) *Case {
	c := &Case{Input: input, HTTP: map[string]runner.FakeResponse{}}
	c.Keys = slices.Sorted(func(yield func(string) bool) {
		for k := range input {
			if !yield(k) {
				return
			}
		}
	})
	seen := map[string]bool{}
	for _, id := range path {
		ev, ok := events[id]
		n := f.Node(id)
		if !ok || n == nil || seen[id] {
			continue
		}
		seen[id] = true
		switch {
		case n.Kind.IsDecision():
			for i, d := range ev.Decisions {
				target := id
				if n.Kind == flow.Questions && i < len(n.Questions) {
					target = id + "." + n.Questions[i].Name
				}
				c.Expect = append(c.Expect, Expectation{Target: target, Want: expectedAnswer(d)})
			}
		case n.Kind == flow.Action && n.Action.Type == flow.ActHTTP:
			out, _ := ev.Output.(map[string]any)
			if status, ok := asInt(out["status"]); ok && out["dry_run"] != true {
				c.HTTP[id] = runner.FakeResponse{Status: status, Body: out["body"]}
			}
		case n.Kind == flow.Action && n.Action.Type == flow.ActOutput:
			out, _ := ev.Output.(map[string]any)
			for _, kv := range n.Action.Set {
				if v, ok := out[kv.Key]; ok {
					c.Expect = append(c.Expect, Expectation{Target: id + "." + kv.Key, Want: tmpl.Format(v)})
				}
			}
		}
	}
	return c
}

// expectedAnswer writes an answer the way an expectation checks it; scores
// expect the level they rounded to.
func expectedAnswer(d backend.Decision) string {
	switch d.Kind {
	case flow.Bool:
		if yes, _ := d.Answer.(bool); yes {
			return "yes"
		}
		return "no"
	case flow.Score:
		v, _ := d.Answer.(float64)
		return fmt.Sprint(math.Round(v))
	}
	return tmpl.Format(d.Answer)
}

func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	}
	return 0, false
}

// Create writes the case as a new file, refusing to overwrite one.
func (c *Case) Create() error {
	if _, err := os.Stat(c.Path); err == nil {
		return fmt.Errorf("%s already exists", c.Path)
	}
	doc := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v *yaml.Node) {
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	}
	if c.Description != "" {
		add("description", &yaml.Node{Kind: yaml.ScalarNode, Value: c.Description})
	}
	input := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range c.Keys {
		var v yaml.Node
		if err := v.Encode(c.Input[k]); err != nil {
			return err
		}
		input.Content = append(input.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &v)
	}
	add("input", input)
	if len(c.HTTP) > 0 {
		fakes := &yaml.Node{Kind: yaml.MappingNode}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for k := range c.HTTP {
				if !yield(k) {
					return
				}
			}
		}) {
			var v yaml.Node
			if err := v.Encode(map[string]any{"status": c.HTTP[name].Status, "body": c.HTTP[name].Body}); err != nil {
				return err
			}
			fakes.Content = append(fakes.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, &v)
		}
		add("http", fakes)
	}
	if len(c.Expect) > 0 {
		expect := &yaml.Node{Kind: yaml.MappingNode}
		for _, e := range c.Expect {
			expect.Content = append(expect.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: e.Target},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: e.Want})
		}
		add("expect", expect)
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()
	return os.WriteFile(c.Path, buf.Bytes(), 0o644)
}
