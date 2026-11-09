package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/validate"
)

//go:embed new_prompt.md
var newPrompt string

// newAttempts is how many drafts a model gets to produce a flow that
// validates, each one shown what was wrong with the last.
const newAttempts = 3

// cmdNew drafts a flow and its test cases from a sentence, checks them the
// way hunch would, and writes them once they pass.
func cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, newHelp()) }
	out := fs.String("o", "", "where to write the flow (default: NAME.yaml from the flow's name)")
	writer := fs.String("with", "", "model that drafts it: anthropic[:MODEL], openai:MODEL, openrouter:MODEL or ollama:MODEL (default: anthropic if ANTHROPIC_API_KEY is set)")
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
	want := strings.TrimSpace(strings.Join(words, " "))
	if want == "" {
		fmt.Print(newHelp())
		return nil
	}
	w, label, err := draftWriter(*writer)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "drafting with %s…\n", label)
	d, err := draftFlow(ctx, w, want, os.Stderr)
	if err != nil {
		return err
	}

	path := *out
	if path == "" {
		path = d.flow.Name + ".yaml"
	}
	if err := d.write(path); err != nil {
		return err
	}
	fmt.Printf("✓ wrote %s and %d tests in %s\n", path, len(d.tests), cases.Dir(path))
	if d.cost > 0 {
		fmt.Printf("  drafting cost $%.4f\n", d.cost)
	}
	fmt.Printf("\nnext:\n  hunch tui %s     # press R to run every test\n", path)
	return nil
}

// newHelp explains hunch new, and says which model it would use here.
func newHelp() string {
	model := "  none yet. Jev can only answer questions, not write text.\n" +
		"  Add ANTHROPIC_API_KEY=... to .env, or pick another model with --with.\n"
	if _, label, err := draftWriter(""); err == nil {
		model = "  " + label + "\n"
	}
	return `hunch new: write a flow and its test cases from one sentence

usage:
  hunch new "what the flow should decide" [-o FILE.yaml] [--with MODEL]

example:
  hunch new "approve refund requests"

  A model writes refunds.yaml (the questions and where each answer leads)
  and refunds.tests/ (4-6 example inputs with the answers you'd expect).
  hunch checks them and sends any problems back to the model, up to 3
  times, then writes the files. Nothing existing is overwritten.

  Then: hunch tui refunds.yaml, and press R to run every test.

model that would write it:
` + model + `
flags:
  -o FILE.yaml   where to write the flow (default: named after the flow)
  --with MODEL   anthropic[:MODEL] (default, claude-opus-5-5), openai:MODEL,
                 openrouter:MODEL or ollama:MODEL
`
}

// draftWriter picks the model that drafts flows.
func draftWriter(spec string) (backend.Writer, string, error) {
	kind, model, _ := strings.Cut(spec, ":")
	if kind == "" {
		switch {
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			kind = "anthropic"
		case os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("OPENROUTER_API_KEY") != "":
			return nil, "", errors.New("say which model drafts the flow: --with openai:MODEL or --with openrouter:MODEL")
		default:
			return nil, "", errors.New("hunch new needs a model that writes: set ANTHROPIC_API_KEY (in .env), or pass --with openai:MODEL, openrouter:MODEL or ollama:MODEL")
		}
	}
	opts := map[string]any{"max_tokens": 16000, "timeout": 300}
	cfg := flow.BackendConfig{Name: "drafter", Options: opts}
	switch kind {
	case "anthropic":
		cfg.Kind = "anthropic"
		if model == "" {
			model = "claude-opus-5-5"
		}
	case "openai":
		cfg.Kind = "openai"
	case "openrouter":
		cfg.Kind = "openai"
		opts["base_url"] = "https://openrouter.ai/api/v1"
		opts["api_key_env"] = "OPENROUTER_API_KEY"
	case "ollama":
		cfg.Kind = "openai"
		opts["base_url"] = "http://localhost:11434/v1"
	default:
		return nil, "", fmt.Errorf("--with %s: want anthropic, openai, openrouter or ollama", kind)
	}
	if model == "" {
		return nil, "", fmt.Errorf("--with %s:MODEL: say which model", kind)
	}
	opts["model"] = model
	b, err := backend.New(cfg)
	if err != nil {
		return nil, "", err
	}
	return b.(backend.Writer), kind + " " + model, nil
}

type draft struct {
	flowSrc string
	flow    *flow.Flow
	tests   []draftTest
	cost    float64
}

type draftTest struct {
	name string
	src  string
}

// draftFlow asks the model for a flow and tests, and asks again with the
// problems found until they check out.
func draftFlow(ctx context.Context, w backend.Writer, want string, log io.Writer) (*draft, error) {
	prompt := "Write a hunch flow, with test cases, for this:\n\n" + want
	var cost float64
	for attempt := 1; ; attempt++ {
		resp, err := w.Write(ctx, backend.WriteRequest{System: newPrompt, Prompt: prompt})
		cost += resp.CostUSD
		if err != nil {
			return nil, err
		}
		d, problems := checkDraft(resp.Text)
		if len(problems) == 0 {
			d.cost = cost
			return d, nil
		}
		fmt.Fprintf(log, "draft %d has %d problems:\n", attempt, len(problems))
		for _, p := range problems {
			fmt.Fprintln(log, "  "+p)
		}
		if attempt == newAttempts {
			return nil, fmt.Errorf("no draft passed the checks after %d tries", newAttempts)
		}
		fmt.Fprintln(log, "asking for a fix…")
		prompt = "Write a hunch flow, with test cases, for this:\n\n" + want +
			"\n\nYour last draft is below. It has these problems:\n- " + strings.Join(problems, "\n- ") +
			"\n\nReply with the whole corrected flow and test cases, in the same format.\n\n" + resp.Text
	}
}

var marker = regexp.MustCompile(`(?m)^=== (flow|test [a-z0-9][a-z0-9_-]*)\s*$`)

// checkDraft splits a reply into its files and checks them the way hunch
// would: the flow validates, and each test parses, gives every input, and
// expects only answers the flow can give.
func checkDraft(reply string) (*draft, []string) {
	reply = regexp.MustCompile("(?m)^```.*$").ReplaceAllString(reply, "")
	d := &draft{}
	var problems []string
	locs := marker.FindAllStringSubmatchIndex(reply, -1)
	if len(locs) == 0 {
		return d, []string{"the reply has no `=== flow` section"}
	}
	for i, loc := range locs {
		end := len(reply)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		head, body := reply[loc[2]:loc[3]], strings.TrimSpace(reply[loc[1]:end])+"\n"
		if head == "flow" {
			d.flowSrc = body
		} else {
			d.tests = append(d.tests, draftTest{name: strings.TrimPrefix(head, "test "), src: body})
		}
	}
	if d.flowSrc == "" {
		return d, []string{"the reply has no `=== flow` section"}
	}
	f, err := flow.Parse([]byte(d.flowSrc))
	if err != nil {
		return d, []string{"flow: " + err.Error()}
	}
	d.flow = f
	if f.Name == "" || !cases.ValidName(f.Name) {
		problems = append(problems, "flow: give it a `name:` in kebab-case")
	}
	if regexp.MustCompile(`(?m)^backends:`).MatchString(d.flowSrc) {
		problems = append(problems, "flow: remove `backends:`; hunch adds them")
	}
	for _, is := range validate.Flow(f) {
		if is.Severity == validate.Error {
			problems = append(problems, "flow: "+is.String())
		}
	}
	if len(d.tests) < 3 {
		problems = append(problems, fmt.Sprintf("only %d test cases; write 4-6", len(d.tests)))
	}
	for _, t := range d.tests {
		c, err := cases.Parse([]byte(t.src))
		if err != nil {
			problems = append(problems, "test "+t.name+": "+err.Error())
			continue
		}
		for _, in := range f.Inputs() {
			if _, ok := c.Input[in]; !ok {
				problems = append(problems, fmt.Sprintf("test %s: input %s is missing", t.name, in))
			}
		}
		if len(c.Expect) == 0 {
			problems = append(problems, "test "+t.name+": expect nothing; say which answers are right")
		}
		for _, e := range c.Expect {
			if p := checkExpect(f, e); p != "" {
				problems = append(problems, "test "+t.name+": "+p)
			}
		}
	}
	return d, problems
}

// checkExpect reports an expectation the flow can never meet.
func checkExpect(f *flow.Flow, e cases.Expectation) string {
	id, sub, _ := strings.Cut(e.Target, ".")
	n := f.Node(id)
	if n == nil {
		return fmt.Sprintf("expects %s, but there's no node %s", e.Target, id)
	}
	if !n.Kind.IsDecision() {
		return ""
	}
	var q *flow.Question
	for i := range n.Questions {
		if (len(n.Questions) == 1 && sub == "") || n.Questions[i].Name == sub {
			q = &n.Questions[i]
		}
	}
	if q == nil {
		return fmt.Sprintf("expects %s, but %s has no such question", e.Target, id)
	}
	switch q.Kind {
	case flow.Bool:
		if !slices.Contains([]string{"yes", "no", "true", "false"}, strings.ToLower(e.Want)) {
			return fmt.Sprintf("%s is yes or no, not %q", e.Target, e.Want)
		}
	case flow.Choice:
		if !slices.Contains(q.Options, e.Want) {
			return fmt.Sprintf("%s is one of %s, not %q", e.Target, strings.Join(q.Options, ", "), e.Want)
		}
	}
	return ""
}

// write saves the flow, with backends for the keys at hand, and its tests.
// Nothing is overwritten.
func (d *draft) write(path string) error {
	dir := cases.Dir(path)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; pass -o to write somewhere else", path)
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists; pass -o to write somewhere else", dir)
	}
	src := withBackends(d.flowSrc)
	if _, err := flow.Parse([]byte(src)); err != nil {
		return fmt.Errorf("adding backends: %w", err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, t := range d.tests {
		if err := os.WriteFile(filepath.Join(dir, t.name+".yaml"), []byte(t.src), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// withBackends puts a backends section after the flow's name: Jev when a
// key for it is set, and mock to run offline.
func withBackends(src string) string {
	var block string
	switch {
	case os.Getenv("TYPESAFE_API_KEY") != "":
		block = "backends:\n  default: jev\n  jev: {kind: jev, provider: typesafe}  # needs TYPESAFE_API_KEY\n  mock: {kind: mock}                    # --backend mock: offline, made-up answers\n"
	case os.Getenv("OPENROUTER_API_KEY") != "":
		block = "backends:\n  default: jev\n  jev: {kind: jev}      # through OpenRouter, needs OPENROUTER_API_KEY\n  mock: {kind: mock}    # --backend mock: offline, made-up answers\n"
	default:
		block = "backends:\n  default: mock\n  mock: {kind: mock}    # made-up answers; add jev or a chat model to decide for real\n"
	}
	lines := strings.SplitAfter(src, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "name:") {
			return strings.Join(lines[:i+1], "") + "\n" + block + strings.Join(lines[i+1:], "")
		}
	}
	return block + "\n" + src
}
