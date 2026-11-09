// Command hunch runs and checks decision flows.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Andree37/hunch/internal/trace"
	"github.com/Andree37/hunch/internal/tui"
	"github.com/Andree37/hunch/internal/validate"
)

const usage = `hunch: decision flows with typed, calibrated answers

usage:
  hunch tui [FLOW] [--model name] [--writer name] [--set key=value]... [--state file.json] [--runs file.jsonl]
  hunch test FLOW [--model name] [--writer name] [--live] [name...]
  hunch run FLOW [--model name] [--writer name] [--case name] [--set key=value]... [--state file.json] [--trace file.jsonl|dir] [--json] [--dry-run]

Models come from hunch.yaml, in the flow's folder or one above it:
  default: jev
  models:
    jev: {url: https://api.typesafe.ai/v1/systemone, key: tsk-...}

Every command checks the flow first and stops on errors.

shell nodes and http nodes that write run for real in run (unless --dry-run)
and are only recorded in test and the TUI (unless --live / L).

Test cases live in FLOW's sibling directory, e.g. inbox.yaml → inbox.tests/*.yaml.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
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

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "set a state field, key=value (repeatable; values parse as JSON when they can)")
	stateFile := fs.String("state", "", "JSON file with initial state")
	traceFile := fs.String("trace", "", "where to record the run: a .jsonl file, a directory, or off (default: the flow's runs location)")
	asJSON := fs.Bool("json", false, "print final state as JSON on stdout; progress goes to stderr")
	maxVisits := fs.Int("max-visits", 5, "max times a single node may run")
	backendName := fs.String("model", "", "decide with this model from hunch.yaml instead of its default")
	writerName := fs.String("writer", "", "write text for llm nodes with this model from hunch.yaml")
	caseName := fs.String("case", "", "take input from this test case (--set still overrides)")
	dryRun := fs.Bool("dry-run", false, "don't run shell commands or send http writes; record them instead")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	progress := io.Writer(os.Stdout)
	if *asJSON {
		progress = os.Stderr
	}
	f, err := loadChecked(path, *backendName, *writerName, os.Stderr)
	if err != nil {
		return err
	}

	var c *cases.Case
	if *caseName != "" {
		if c, err = findCase(path, *caseName); err != nil {
			return err
		}
		if err := c.CheckFakes(f); err != nil {
			return err
		}
	}
	state, err := initialState(f, caseInput(c), *stateFile, sets)
	if err != nil {
		return err
	}
	if err := checkInputs(f, state); err != nil {
		return err
	}

	backends, err := backend.ForFlow(f)
	if err != nil {
		return err
	}

	var rec *trace.Writer
	if loc := recordTo(*traceFile, f); loc != "" {
		w, closer, err := trace.Open(ctx, loc, 100<<20)
		if err != nil {
			return err
		}
		defer closer.Close()
		w.OnError = func(err error) { fmt.Fprintln(os.Stderr, "hunch:", err) }
		rec = w
	}
	runID := trace.NewID()
	rec.Start(runID, path, f.DefaultBackend, inputsOnly(f, state))

	ctx = backend.WithStatus(ctx, func(note string) { fmt.Fprintln(os.Stderr, "    …", note) })
	res, err := runner.Run(ctx, f, state, runner.Options{
		Backends:  backends,
		MaxVisits: *maxVisits,
		DryRun:    *dryRun,
		Fake:      caseFakes(c),
		Stdout:    progress,
		OnEvent: func(ev runner.Event) {
			printEvent(progress, ev)
			rec.Step(runID, ev)
		},
	})
	rec.End(runID, res, err)
	if err != nil {
		return err
	}

	fmt.Fprintf(progress, "\n✓ done · %d steps · $%.6f · %s\n", len(res.Path), res.CostUSD, strings.Join(res.Path, " → "))
	printOutputs(progress, res.Outputs)
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
	backendName := fs.String("model", "", "decide with this model from hunch.yaml instead of its default")
	writerName := fs.String("writer", "", "write text for llm nodes with this model from hunch.yaml")
	runsFile := fs.String("runs", "", "read recorded runs from here instead of the flow's runs location: a .jsonl file or a directory")
	paths, err := parseOptionalArg(fs, args)
	if err != nil {
		return err
	}
	opts := tui.OpenOptions{Backend: *backendName, Writer: *writerName, Runs: *runsFile}

	// No flow given: start on the picker, listing the flows in this folder.
	if len(paths) == 0 {
		if len(sets) > 0 || *stateFile != "" {
			return fmt.Errorf("--set and --state need a flow: hunch tui FLOW --set ...")
		}
		opts.FromCase = true
		return tui.RunApp(tui.NewApp(".", opts))
	}

	path := paths[0]
	f, err := flow.Load(path)
	if err != nil {
		return err
	}
	// Only the caller's input: the flow's own state is constants, added by
	// the runner, not something to edit in the TUI.
	inputs := *f
	inputs.State = nil
	if opts.State, err = initialState(&inputs, nil, *stateFile, sets); err != nil {
		return err
	}
	// Explicit input wins; otherwise start from the first test case.
	opts.FromCase = len(sets) == 0 && *stateFile == "" && *runsFile == ""
	app, err := tui.OpenFlow(".", path, opts)
	if err != nil {
		return err
	}
	return tui.RunApp(app)
}

// parseOptionalArg parses flags around at most one positional argument.
func parseOptionalArg(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("want at most one flow file\n\n%s", usage)
	}
	return positional, nil
}

func cmdTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	backendName := fs.String("model", "", "decide with this model from hunch.yaml instead of its default")
	writerName := fs.String("writer", "", "write text for llm nodes with this model from hunch.yaml")
	live := fs.Bool("live", false, "run shell commands and send http writes for real (default: record them only)")
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

	f, err := loadChecked(path, *backendName, *writerName, os.Stderr)
	if err != nil {
		return err
	}
	all, err := cases.Load(cases.Dir(path))
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no test cases in %s", cases.Dir(path))
	}
	// A name that matches no test is a mistake, not "nothing to run".
	for _, name := range only {
		if !slices.ContainsFunc(all, func(c *cases.Case) bool { return c.Name == name }) {
			names := make([]string, len(all))
			for i, c := range all {
				names[i] = c.Name
			}
			return fmt.Errorf("no test named %q in %s (there's %s)", name, cases.Dir(path), strings.Join(names, ", "))
		}
	}
	backends, err := backend.ForFlow(f)
	if err != nil {
		return err
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
		if err := c.CheckFakes(f); err != nil {
			failed++
			fmt.Printf("✗ %s: %v\n", c.Name, err)
			continue
		}
		res, err := runner.Run(ctx, f, state, runner.Options{Backends: backends, DryRun: !*live, Fake: c.HTTP})
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
	fmt.Printf("\n%d/%d passed · model %s · $%.6f\n", ran-failed, ran, f.DefaultBackend, cost)
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

// pickBackends applies --model (who decides) and --writer (who writes for
// llm nodes) to the flow.
func pickBackends(f *flow.Flow, path, decider, writer string) error {
	if f.Project == nil && decider != "mock" {
		return errors.New(flow.ProjectHelp(path))
	}
	for _, name := range []string{decider, writer} {
		if _, ok := f.Backends[name]; name != "" && !ok {
			if f.Project == nil {
				return errors.New(flow.ProjectHelp(path))
			}
			return fmt.Errorf("model %q is not in %s (it has %s)", name, f.Project.Path, strings.Join(f.Project.Order, ", "))
		}
	}
	if decider != "" {
		f.DefaultBackend = decider
	}
	if writer != "" {
		f.WriterBackend = writer
	}
	return nil
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

// recordTo picks where runs are recorded: --trace if given ("off" for
// nowhere), else the flow's runs location.
func recordTo(flag string, f *flow.Flow) string {
	switch flag {
	case "off":
		return ""
	case "":
		return f.RunsLocation()
	}
	return flag
}

func caseInput(c *cases.Case) map[string]any {
	if c == nil {
		return nil
	}
	return c.Input
}

// inputsOnly drops the flow's own constants from a run's state, leaving what
// the run was given.
func inputsOnly(f *flow.Flow, state map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range state {
		if _, constant := f.State[k]; !constant {
			out[k] = v
		}
	}
	return out
}

func caseFakes(c *cases.Case) map[string]runner.FakeResponse {
	if c == nil {
		return nil
	}
	return c.HTTP
}

func printOutputs(w io.Writer, outputs map[string]any) {
	if len(outputs) == 0 {
		return
	}
	fmt.Fprintln(w, "outputs:")
	for _, k := range slices.Sorted(maps.Keys(outputs)) {
		fmt.Fprintf(w, "    %s: %s\n", k, tmpl.Format(outputs[k]))
	}
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

// loadChecked loads a flow, applies --model and --writer, and checks it
// with those, so the flags can fix what the defaults would get wrong.
func loadChecked(path, decider, writer string, w io.Writer) (*flow.Flow, error) {
	f, err := flow.Load(path)
	if err != nil {
		return nil, err
	}
	if err := pickBackends(f, path, decider, writer); err != nil {
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

// initialState layers a run's state: the flow's constants, then a test
// case's input, then --state, then --set.
func initialState(f *flow.Flow, caseIn map[string]any, stateFile string, sets []string) (map[string]any, error) {
	state := map[string]any{}
	maps.Copy(state, f.State)
	maps.Copy(state, caseIn)
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
		out, _ := ev.Output.(map[string]any)
		switch {
		case out["message"] != nil:
			detail = fmt.Sprintf("%q", out["message"])
		case out["text"] != nil:
			first, _, _ := strings.Cut(tmpl.Format(out["text"]), "\n")
			detail = fmt.Sprintf("wrote %q", first)
		case out["dry_run"] == true && out["command"] != nil:
			detail = fmt.Sprintf("dry run: $ %v", out["command"])
		case out["dry_run"] == true:
			detail = fmt.Sprintf("dry run: %v %v", out["method"], out["url"])
		case out["faked"] == true:
			detail = fmt.Sprintf("faked HTTP %v (from the test case)", out["status"])
		case out["status"] != nil:
			detail = fmt.Sprintf("HTTP %v", out["status"])
		case out["exit_code"] != nil:
			if code := out["exit_code"]; code != 0 {
				detail = fmt.Sprintf("exit %v", code)
			}
		default:
			parts := make([]string, 0, len(out))
			for _, k := range slices.Sorted(maps.Keys(out)) {
				parts = append(parts, k+"="+tmpl.Format(out[k]))
			}
			detail = strings.Join(parts, " ")
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
