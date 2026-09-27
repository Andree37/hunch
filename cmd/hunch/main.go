// Command hunch runs and checks decision flows.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/tui"
	"github.com/Andree37/hunch/internal/validate"
)

const usage = `hunch: decision flows with typed, calibrated answers

usage:
  hunch validate FLOW
  hunch tui FLOW [--backend name] [--set key=value]... [--state file.json]
  hunch run FLOW [--backend name] [--case name] [--set key=value]... [--state file.json] [--trace file.jsonl] [--json]
  hunch test FLOW [--backend name] [name...]

Test cases live in FLOW's sibling directory, e.g. inbox.yaml → inbox.tests/*.yaml.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := loadDotenv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "hunch:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "tui":
		err = cmdTUI(os.Args[2:])
	case "test":
		err = cmdTest(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hunch:", err)
		os.Exit(1)
	}
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	f, err := loadChecked(path, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("✓ %s: %d nodes, starts at %q\n", path, len(f.Nodes), f.Start)
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "set a state field, key=value (repeatable; values parse as JSON when they can)")
	stateFile := fs.String("state", "", "JSON file with initial state")
	traceFile := fs.String("trace", "", "append run events to this JSONL file")
	asJSON := fs.Bool("json", false, "print final state as JSON on stdout; progress goes to stderr")
	maxVisits := fs.Int("max-visits", 5, "max times a single node may run")
	backendName := fs.String("backend", "", "use this backend instead of the flow's default")
	caseName := fs.String("case", "", "take input from this test case (--set still overrides)")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	progress := io.Writer(os.Stdout)
	if *asJSON {
		progress = os.Stderr
	}
	f, err := loadChecked(path, os.Stderr)
	if err != nil {
		return err
	}

	if *backendName != "" {
		if _, ok := f.Backends[*backendName]; !ok {
			return fmt.Errorf("backend %q is not defined in %s", *backendName, path)
		}
		f.DefaultBackend = *backendName
	}

	var c *cases.Case
	if *caseName != "" {
		if c, err = findCase(path, *caseName); err != nil {
			return err
		}
		for k, v := range c.Input {
			f.State[k] = v
		}
	}
	state, err := initialState(f, *stateFile, sets)
	if err != nil {
		return err
	}
	if err := checkInputs(f, state); err != nil {
		return err
	}

	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			return err
		}
		backends[name] = b
	}

	var trace *json.Encoder
	if *traceFile != "" {
		tf, err := os.OpenFile(*traceFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer tf.Close()
		trace = json.NewEncoder(tf)
	}

	ctx = backend.WithStatus(ctx, func(note string) { fmt.Fprintln(os.Stderr, "    …", note) })
	res, err := runner.Run(ctx, f, state, runner.Options{
		Backends:  backends,
		MaxVisits: *maxVisits,
		Stdout:    progress,
		OnEvent: func(ev runner.Event) {
			printEvent(progress, ev)
			if trace != nil {
				trace.Encode(ev)
			}
		},
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(progress, "\n✓ done · %d steps · $%.6f · %s\n", len(res.Path), res.CostUSD, strings.Join(res.Path, " → "))
	if c != nil && len(c.Expect) > 0 {
		printResults(progress, c.Check(f, res.State))
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res.State)
	}
	return nil
}

func cmdTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "set a state field, key=value (repeatable)")
	stateFile := fs.String("state", "", "JSON file with initial state")
	backendName := fs.String("backend", "", "use this backend instead of the flow's default")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	f, err := flow.Load(path)
	if err != nil {
		return err
	}
	state, err := initialState(f, *stateFile, sets)
	if err != nil {
		return err
	}
	// Explicit input wins; otherwise start from the first test case.
	fromCase := len(sets) == 0 && *stateFile == ""
	m, err := tui.New(path, state, *backendName, fromCase)
	if err != nil {
		return err
	}
	return tui.Run(m)
}

func cmdTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	backendName := fs.String("backend", "", "use this backend instead of the flow's default")
	var paths []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		paths = append(paths, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(paths) == 0 {
		return fmt.Errorf("want a flow file\n\n%s", usage)
	}
	path, only := paths[0], paths[1:]

	f, err := loadChecked(path, os.Stderr)
	if err != nil {
		return err
	}
	if *backendName != "" {
		if _, ok := f.Backends[*backendName]; !ok {
			return fmt.Errorf("backend %q is not defined in %s", *backendName, path)
		}
		f.DefaultBackend = *backendName
	}
	all, err := cases.Load(cases.Dir(path))
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no test cases in %s", cases.Dir(path))
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			return err
		}
		backends[name] = b
	}

	ctx = backend.WithStatus(ctx, func(note string) { fmt.Fprintln(os.Stderr, "    …", note) })
	var ran, failed int
	var cost float64
	for _, c := range all {
		if len(only) > 0 && !slices.Contains(only, c.Name) {
			continue
		}
		ran++
		state := maps.Clone(f.State)
		maps.Copy(state, c.Input)
		if err := checkInputs(f, state); err != nil {
			failed++
			fmt.Printf("✗ %s: %v\n", c.Name, err)
			continue
		}
		res, err := runner.Run(ctx, f, state, runner.Options{Backends: backends})
		cost += res.CostUSD
		switch {
		case err != nil:
			failed++
			fmt.Printf("✗ %s: %v\n", c.Name, err)
		case len(c.Expect) == 0:
			fmt.Printf("· %s: ran (%s), no expectations\n", c.Name, strings.Join(res.Path, " → "))
		default:
			rs := c.Check(f, res.State)
			if cases.Passed(rs) {
				fmt.Printf("✓ %s\n", c.Name)
			} else {
				failed++
				fmt.Printf("✗ %s\n", c.Name)
				printResults(os.Stdout, rs)
			}
		}
	}
	fmt.Printf("\n%d/%d passed · backend %s · $%.6f\n", ran-failed, ran, f.DefaultBackend, cost)
	if failed > 0 {
		return fmt.Errorf("%d failed", failed)
	}
	return nil
}

// missingInputs lists the inputs the flow reads that state doesn't have.
// Optional refs ({{x?}}) don't count.
func missingInputs(f *flow.Flow, state map[string]any) []string {
	required := map[string]bool{}
	for _, n := range f.Nodes {
		for _, t := range n.Texts() {
			for _, r := range tmpl.Refs(t) {
				if !r.Optional && f.Node(r.Root()) == nil {
					required[r.Root()] = true
				}
			}
		}
	}
	var out []string
	for _, k := range f.Inputs() {
		if _, ok := state[k]; !ok && required[k] {
			out = append(out, k)
		}
	}
	return out
}

// checkInputs fails on required inputs that are missing and on values that
// don't fit their declared type.
func checkInputs(f *flow.Flow, state map[string]any) error {
	if missing := missingInputs(f, state); len(missing) > 0 {
		return fmt.Errorf("missing input: %s (give with --set %s=..., or --case NAME)", strings.Join(missing, ", "), missing[0])
	}
	for _, s := range f.InputSpecs {
		if v, ok := state[s.Name]; ok {
			if err := s.Check(v); err != nil {
				return fmt.Errorf("input %w", err)
			}
		}
	}
	return nil
}

func findCase(flowPath, name string) (*cases.Case, error) {
	all, err := cases.Load(cases.Dir(flowPath))
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no test case %q in %s", name, cases.Dir(flowPath))
}

func printResults(w io.Writer, rs []cases.Result) {
	for _, r := range rs {
		mark := "✓"
		if !r.OK {
			mark = "✗"
		}
		fmt.Fprintf(w, "    %s %s: want %s, got %s\n", mark, r.Target, r.Want, r.Got)
	}
}

// loadChecked loads a flow and prints validation issues, failing on errors.
func loadChecked(path string, w io.Writer) (*flow.Flow, error) {
	f, err := flow.Load(path)
	if err != nil {
		return nil, err
	}
	issues := validate.Flow(f)
	for _, is := range issues {
		fmt.Fprintln(w, is)
	}
	if validate.HasErrors(issues) {
		return nil, fmt.Errorf("%s is invalid", path)
	}
	return f, nil
}

func initialState(f *flow.Flow, stateFile string, sets []string) (map[string]any, error) {
	state := map[string]any{}
	for k, v := range f.State {
		state[k] = v
	}
	if stateFile != "" {
		data, err := os.ReadFile(stateFile)
		if err != nil {
			return nil, err
		}
		var fromFile map[string]any
		if err := json.Unmarshal(data, &fromFile); err != nil {
			return nil, fmt.Errorf("%s: %w", stateFile, err)
		}
		for k, v := range fromFile {
			state[k] = v
		}
	}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set %q: want key=value", s)
		}
		spec, _ := f.InputSpec(k)
		parsed, err := spec.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("--set %s", err)
		}
		state[k] = parsed
	}
	return state, nil
}

func printEvent(w io.Writer, ev runner.Event) {
	var detail string
	switch {
	case ev.Kind == flow.Questions:
		parts := make([]string, len(ev.Decisions))
		for i, d := range ev.Decisions {
			_, name, _ := strings.Cut(d.QuestionID, ".")
			parts[i] = name + "=" + answer(d)
		}
		detail = strings.Join(parts, " ")
	case ev.Kind.IsDecision():
		d := ev.Decisions[0]
		detail = fmt.Sprintf("%s  conf %.2f", answer(d), d.Confidence)
	default:
		if out, ok := ev.Output.(map[string]any); ok {
			if msg, ok := out["message"]; ok {
				detail = fmt.Sprintf("%q", msg)
			} else if code := out["exit_code"]; code != 0 {
				detail = fmt.Sprintf("exit %v", code)
			}
		}
	}
	if ev.Branch == flow.Unsure {
		detail += "  (unsure)"
	}
	next := ""
	if ev.Next != "" {
		next = "→ " + ev.Next
	}
	fmt.Fprintf(w, "%2d  %-14s %-9s %-44s %s\n", ev.Step, ev.Node, ev.Kind, detail, next)
}

func answer(d backend.Decision) string {
	switch d.Kind {
	case flow.Bool:
		if yes, _ := d.Answer.(bool); yes {
			return fmt.Sprintf("yes %.2f", d.Probs["yes"])
		}
		return fmt.Sprintf("no %.2f", d.Probs["no"])
	case flow.Score:
		v, _ := d.Answer.(float64)
		return fmt.Sprintf("%.2f", v)
	}
	return tmpl.Format(d.Answer)
}

// parseArgs parses flags that may appear before or after the single
// positional FLOW argument.
func parseArgs(fs *flag.FlagSet, args []string) (string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return "", fmt.Errorf("want exactly one flow file\n\n%s", usage)
	}
	return positional[0], nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
