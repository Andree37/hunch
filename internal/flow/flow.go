// Package flow defines hunch's flow file: a graph of decision nodes (answered
// by a decision backend such as Jev) and action nodes (plain code), parsed
// from YAML.
package flow

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Andree37/hunch/internal/tmpl"
)

type Kind string

const (
	Bool      Kind = "bool"
	Choice    Kind = "choice"
	Score     Kind = "score"
	Questions Kind = "questions"
	Action    Kind = "action"
)

// IsDecision reports whether nodes of this kind are answered by a backend.
func (k Kind) IsDecision() bool {
	return k == Bool || k == Choice || k == Score || k == Questions
}

// Reserved branch keys.
const (
	Unsure  = "unsure" // taken when confidence is below the node's threshold
	Default = "_"      // taken when nothing else matches
)

// DefaultThreshold is the confidence below which a decision takes its
// "unsure" route, when the node has one.
const DefaultThreshold = 0.6

type Scale struct{ Min, Max int }

func (s Scale) String() string { return fmt.Sprintf("%d-%d", s.Min, s.Max) }

type Question struct {
	ID      string // unique within the flow: "node" or "node.name"
	Name    string // key under the node's state entry
	Kind    Kind   // Bool, Choice or Score
	Text    string
	Options []string // Choice only
	Scale   Scale    // Score only

	// Optional descriptions that help the backend judge: per option for
	// Choice, "yes"/"no" for Bool, and one per level (Min first) for Score.
	Criteria map[string]string
	Levels   []string
}

// Action types.
const (
	ActLog    = "log"    // print a message
	ActShell  = "shell"  // run a command
	ActLLM    = "llm"    // generate text with a writer backend
	ActHTTP   = "http"   // call an API
	ActOutput = "output" // record part of the flow's result
)

type ActionSpec struct {
	Type    string
	Message string // log
	Run     string // shell

	Prompt    string // llm
	System    string // llm
	MaxTokens int    // llm, 0 = backend default

	Method  string // http, default POST
	URL     string // http
	Headers []KV   // http
	Body    any    // http: a template string, or a map/list whose strings are templates

	Set []KV // output

	Timeout float64 // seconds, shell and http
}

// KV is one entry of an ordered map from the flow file. Value is a
// template string unless the YAML gave a number or bool (then Literal).
type KV struct {
	Key     string
	Value   string
	Literal any
}

// actionFields lists the fields each action type accepts.
var actionFields = map[string]map[string]bool{
	ActLog:    set("message"),
	ActShell:  set("run", "timeout"),
	ActLLM:    set("prompt", "system", "max_tokens", "backend"),
	ActHTTP:   set("method", "url", "headers", "body", "timeout"),
	ActOutput: set("set"),
}

var actionOnly = []string{"message", "run", "prompt", "system", "max_tokens", "method", "url", "headers", "body", "set", "timeout"}

type Branch struct{ When, To string }

// Routes is a node's `then`: either a single unconditional target or an
// ordered list of branches keyed by answer.
type Routes struct {
	Next     string
	Branches []Branch
}

func (r Routes) Get(when string) (string, bool) {
	for _, b := range r.Branches {
		if b.When == when {
			return b.To, true
		}
	}
	return "", false
}

func (r Routes) Targets() []string {
	if len(r.Branches) == 0 {
		if r.Next == "" {
			return nil
		}
		return []string{r.Next}
	}
	out := make([]string, 0, len(r.Branches))
	for _, b := range r.Branches {
		out = append(out, b.To)
	}
	return out
}

type Node struct {
	ID        string
	Kind      Kind
	Questions []Question // one for bool/choice/score, several for questions
	Action    *ActionSpec
	Backend   string
	Threshold float64 // 0 means use the flow's threshold
	Then      Routes

	Src *yaml.Node // source mapping, kept for round-trip editing
}

// Texts returns every templated string on the node: question texts, and an
// action's message or command.
func (n *Node) Texts() []string {
	var out []string
	for _, q := range n.Questions {
		out = append(out, q.Text)
	}
	if a := n.Action; a != nil {
		out = append(out, a.Message, a.Run, a.Prompt, a.System, a.URL)
		for _, h := range a.Headers {
			out = append(out, h.Value)
		}
		for _, kv := range a.Set {
			out = append(out, kv.Value)
		}
		out = append(out, bodyStrings(a.Body)...)
	}
	return out
}

func bodyStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, bodyStrings(e)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, bodyStrings(e)...)
		}
		return out
	}
	return nil
}

// Inputs returns the values a run takes: declared inputs first, then any
// other ref root that no node produces, in first-use order.
func (f *Flow) Inputs() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range f.InputSpecs {
		seen[s.Name] = true
		out = append(out, s.Name)
	}
	for _, n := range f.Nodes {
		for _, t := range n.Texts() {
			for _, ref := range tmpl.Refs(t) {
				if r := ref.Root(); f.Node(r) == nil && !seen[r] {
					seen[r] = true
					out = append(out, r)
				}
			}
		}
	}
	return out
}

type BackendConfig struct {
	Name    string
	Kind    string
	Options map[string]any
}

type Flow struct {
	Path           string
	Name           string
	Start          string
	Threshold      float64
	DefaultBackend string
	Backends       map[string]BackendConfig
	State          map[string]any
	WriterBackend  string      // default backend for llm nodes
	InputSpecs     []InputSpec // declared inputs, in file order
	Nodes          []*Node     // in file order

	Root  *yaml.Node // document root, kept for round-trip editing
	index map[string]*Node
}

func (f *Flow) Node(id string) *Node { return f.index[id] }

// ThresholdFor returns the confidence threshold that applies to n.
func (f *Flow) ThresholdFor(n *Node) float64 {
	if n.Threshold > 0 {
		return n.Threshold
	}
	return f.Threshold
}

// BackendFor returns the name of the backend that answers n, or for an
// llm node, the one that writes its text.
func (f *Flow) BackendFor(n *Node) string {
	if n.Backend != "" {
		return n.Backend
	}
	if n.Kind == Action && n.Action.Type == ActLLM && f.WriterBackend != "" {
		return f.WriterBackend
	}
	return f.DefaultBackend
}

func Load(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.Path = path
	return f, nil
}

func Parse(data []byte) (*Flow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty flow")
	}
	root := doc.Content[0]
	fields, keys, err := mapping(root)
	if err != nil {
		return nil, err
	}

	f := &Flow{
		Root:     &doc,
		Backends: map[string]BackendConfig{},
		State:    map[string]any{},
		index:    map[string]*Node{},
	}
	for _, k := range keys {
		v := fields[k]
		switch k {
		case "name":
			f.Name = v.Value
		case "start":
			f.Start = v.Value
		case "threshold":
			if err := v.Decode(&f.Threshold); err != nil {
				return nil, errAt(v, "threshold: %v", err)
			}
		case "state":
			if err := v.Decode(&f.State); err != nil {
				return nil, errAt(v, "state: %v", err)
			}
		case "inputs":
			if err := parseInputs(f, v); err != nil {
				return nil, err
			}
		case "backends":
			if err := parseBackends(f, v); err != nil {
				return nil, err
			}
		case "nodes":
			if err := parseNodes(f, v); err != nil {
				return nil, err
			}
		default:
			return nil, errAt(v, "unknown field %q", k)
		}
	}

	if f.Threshold == 0 {
		f.Threshold = DefaultThreshold
	}
	if f.Start == "" && len(f.Nodes) > 0 {
		f.Start = f.Nodes[0].ID
	}
	if len(f.Backends) == 0 {
		f.Backends["mock"] = BackendConfig{Name: "mock", Kind: "mock"}
	}
	if f.DefaultBackend == "" && len(f.Backends) == 1 {
		for name := range f.Backends {
			f.DefaultBackend = name
		}
	}
	return f, nil
}

func parseBackends(f *Flow, n *yaml.Node) error {
	fields, keys, err := mapping(n)
	if err != nil {
		return err
	}
	for _, name := range keys {
		v := fields[name]
		switch name {
		case "default":
			f.DefaultBackend = v.Value
			continue
		case "writer":
			f.WriterBackend = v.Value
			continue
		}
		var opts map[string]any
		if err := v.Decode(&opts); err != nil {
			return errAt(v, "backend %q: %v", name, err)
		}
		kind, _ := opts["kind"].(string)
		if kind == "" {
			return errAt(v, "backend %q: missing kind", name)
		}
		delete(opts, "kind")
		f.Backends[name] = BackendConfig{Name: name, Kind: kind, Options: opts}
	}
	return nil
}

func parseNodes(f *Flow, n *yaml.Node) error {
	fields, keys, err := mapping(n)
	if err != nil {
		return err
	}
	for _, id := range keys {
		node, err := parseNode(id, fields[id])
		if err != nil {
			return err
		}
		f.Nodes = append(f.Nodes, node)
		f.index[id] = node
	}
	return nil
}

var (
	questionKeys = set("bool", "choice", "score", "options", "scale", "levels", "criteria")
	nodeKeys     = set(append([]string{"bool", "choice", "score", "questions", "action",
		"options", "scale", "levels", "criteria", "backend", "threshold", "then"}, actionOnly...)...)
)

func parseNode(id string, n *yaml.Node) (*Node, error) {
	fields, keys, err := mapping(n)
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", id, err)
	}
	for _, k := range keys {
		if !nodeKeys[k] {
			return nil, errAt(fields[k], "node %q: unknown field %q", id, k)
		}
	}

	kind, err := oneKind(fields, Bool, Choice, Score, Questions, Action)
	if err != nil {
		return nil, errAt(n, "node %q: %v", id, err)
	}
	node := &Node{ID: id, Kind: kind, Src: n}
	for _, k := range []string{"options", "scale", "levels", "criteria"} {
		if fields[k] != nil && (kind == Questions || kind == Action) {
			return nil, errAt(fields[k], "node %q: %s nodes don't take %s", id, kind, k)
		}
	}
	if kind != Action {
		for _, k := range actionOnly {
			if fields[k] != nil {
				return nil, errAt(fields[k], "node %q: %s nodes don't take %s", id, kind, k)
			}
		}
	}

	switch kind {
	case Bool, Choice, Score:
		q, err := parseQuestion(id, id, kind, fields)
		if err != nil {
			return nil, errAt(n, "node %q: %v", id, err)
		}
		node.Questions = []Question{q}
	case Questions:
		qfields, qkeys, err := mapping(fields["questions"])
		if err != nil {
			return nil, fmt.Errorf("node %q: questions: %w", id, err)
		}
		if len(qkeys) == 0 {
			return nil, errAt(n, "node %q: questions is empty", id)
		}
		for _, name := range qkeys {
			qf, qk, err := mapping(qfields[name])
			if err != nil {
				return nil, fmt.Errorf("node %q: question %q: %w", id, name, err)
			}
			for _, k := range qk {
				if !questionKeys[k] {
					return nil, errAt(qf[k], "node %q: question %q: unknown field %q", id, name, k)
				}
			}
			qkind, err := oneKind(qf, Bool, Choice, Score)
			if err != nil {
				return nil, errAt(qfields[name], "node %q: question %q: %v", id, name, err)
			}
			q, err := parseQuestion(id+"."+name, name, qkind, qf)
			if err != nil {
				return nil, errAt(qfields[name], "node %q: question %q: %v", id, name, err)
			}
			node.Questions = append(node.Questions, q)
		}
	case Action:
		a, err := parseAction(id, n, fields)
		if err != nil {
			return nil, err
		}
		node.Action = a
	}

	if b := fields["backend"]; b != nil {
		node.Backend = b.Value
	}
	if t := fields["threshold"]; t != nil {
		if err := t.Decode(&node.Threshold); err != nil {
			return nil, errAt(t, "node %q: threshold: %v", id, err)
		}
	}
	if t := fields["then"]; t != nil {
		node.Then, err = parseRoutes(t)
		if err != nil {
			return nil, fmt.Errorf("node %q: then: %w", id, err)
		}
	}
	return node, nil
}

func parseQuestion(id, name string, kind Kind, fields map[string]*yaml.Node) (Question, error) {
	q := Question{ID: id, Name: name, Kind: kind, Text: fields[string(kind)].Value}
	if q.Text == "" {
		return q, fmt.Errorf("%s question text is empty", kind)
	}
	for _, k := range []string{"options", "scale", "levels", "criteria"} {
		if fields[k] != nil && !fieldApplies(kind, k) {
			return q, fmt.Errorf("%s questions don't take %s", kind, k)
		}
	}

	switch kind {
	case Bool:
		if c := fields["criteria"]; c != nil {
			if err := c.Decode(&q.Criteria); err != nil {
				return q, fmt.Errorf("criteria: %v", err)
			}
			for k := range q.Criteria {
				if k != "yes" && k != "no" {
					return q, fmt.Errorf("criteria: want yes and/or no, got %q", k)
				}
			}
		}
	case Choice:
		o := fields["options"]
		if o == nil {
			return q, fmt.Errorf("choice needs options")
		}
		// A list of names, or a map of name to description.
		if o.Kind == yaml.MappingNode {
			q.Criteria = map[string]string{}
			for i := 0; i+1 < len(o.Content); i += 2 {
				q.Options = append(q.Options, o.Content[i].Value)
				q.Criteria[o.Content[i].Value] = o.Content[i+1].Value
			}
		} else if err := o.Decode(&q.Options); err != nil {
			return q, fmt.Errorf("options: want a list or a map of descriptions: %v", err)
		}
		if len(q.Options) < 2 {
			return q, fmt.Errorf("choice needs at least 2 options")
		}
	case Score:
		q.Scale = Scale{1, 5}
		if s := fields["scale"]; s != nil {
			sc, err := ParseScale(s.Value)
			if err != nil {
				return q, err
			}
			q.Scale = sc
		}
		if l := fields["levels"]; l != nil {
			if err := l.Decode(&q.Levels); err != nil {
				return q, fmt.Errorf("levels: want a list of descriptions: %v", err)
			}
			if len(q.Levels) < 2 {
				return q, fmt.Errorf("score needs at least 2 levels")
			}
			// Levels define the scale: they number up from scale's min (1 by default).
			if fields["scale"] != nil && len(q.Levels) != q.Scale.Max-q.Scale.Min+1 {
				return q, fmt.Errorf("%d levels don't fit scale %s", len(q.Levels), q.Scale)
			}
			q.Scale.Max = q.Scale.Min + len(q.Levels) - 1
		}
	}
	return q, nil
}

func fieldApplies(kind Kind, field string) bool {
	switch field {
	case "options":
		return kind == Choice
	case "scale", "levels":
		return kind == Score
	case "criteria":
		return kind == Bool
	}
	return true
}

// ParseScale parses a scale such as "1-5".
func ParseScale(s string) (Scale, error) {
	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Scale{}, fmt.Errorf("bad scale %q (want e.g. 1-5)", s)
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(lo))
	b, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || a >= b {
		return Scale{}, fmt.Errorf("bad scale %q (want e.g. 1-5)", s)
	}
	return Scale{a, b}, nil
}

func parseRoutes(n *yaml.Node) (Routes, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return Routes{}, nil
		}
		return Routes{Next: n.Value}, nil
	case yaml.MappingNode:
		var r Routes
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Kind != yaml.ScalarNode {
				return r, errAt(v, "branch %q: target must be a node name", k.Value)
			}
			r.Branches = append(r.Branches, Branch{When: k.Value, To: v.Value})
		}
		return r, nil
	}
	return Routes{}, errAt(n, "want a node name or a map of branches")
}

// oneKind finds which of the given kinds is set in fields; exactly one must be.
func oneKind(fields map[string]*yaml.Node, kinds ...Kind) (Kind, error) {
	var found []Kind
	for _, k := range kinds {
		if fields[string(k)] != nil {
			found = append(found, k)
		}
	}
	if len(found) != 1 {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = string(k)
		}
		return "", fmt.Errorf("needs exactly one of %s", strings.Join(names, ", "))
	}
	return found[0], nil
}

func mapping(n *yaml.Node) (map[string]*yaml.Node, []string, error) {
	if n.Kind != yaml.MappingNode {
		return nil, nil, errAt(n, "expected a map")
	}
	fields := map[string]*yaml.Node{}
	var keys []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if _, dup := fields[k]; dup {
			return nil, nil, errAt(n.Content[i], "duplicate key %q", k)
		}
		fields[k] = n.Content[i+1]
		keys = append(keys, k)
	}
	return fields, keys, nil
}

func errAt(n *yaml.Node, format string, args ...any) error {
	return fmt.Errorf("line %d: %s", n.Line, fmt.Sprintf(format, args...))
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}
