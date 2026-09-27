package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/trace"
)

// sample is one answer a node gave: its confidence, and whether it was right
// when a test case says what right is.
type sample struct {
	confidence float64
	labelled   bool
	right      bool
}

// cmdTune shows, for each node that routes on confidence, what every
// threshold would do to the answers already collected: from recorded runs
// (real traffic, no right answer) and from running the test cases once
// (right answer known). No threshold needs a model call of its own.
func cmdTune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tune", flag.ExitOnError)
	runsFile := fs.String("runs", "", "recorded runs to learn from: a .jsonl file, a directory or s3://bucket/prefix")
	backendName := fs.String("backend", "", "backend for running the test cases")
	writerName := fs.String("writer", "", "backend for llm nodes when running the test cases")
	noTests := fs.Bool("no-tests", false, "don't run the test cases; use recorded runs only")
	all := fs.Bool("all", false, "show every decision node, not only those that route on confidence")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	f, err := loadChecked(path, os.Stderr)
	if err != nil {
		return err
	}
	if err := pickBackends(f, path, *backendName, *writerName); err != nil {
		return err
	}

	samples := map[string][]sample{}
	var sources []string
	if *runsFile != "" {
		runs, err := trace.ReadAll(ctx, *runsFile)
		if err != nil {
			return err
		}
		for _, r := range runs {
			for _, ev := range r.Steps {
				addSamples(samples, f, ev, nil)
			}
		}
		sources = append(sources, fmt.Sprintf("%d recorded runs", len(runs)))
	}
	if !*noTests {
		n, cost, err := sampleTests(ctx, f, path, samples)
		if err != nil {
			return err
		}
		if n > 0 {
			sources = append(sources, fmt.Sprintf("%d test cases ($%.6f, backend %s)", n, cost, f.DefaultBackend))
		}
	}
	if len(sources) == 0 {
		return fmt.Errorf("nothing to learn from: give --runs, or add test cases in %s", cases.Dir(path))
	}
	fmt.Printf("learning from %s\n", strings.Join(sources, " and "))
	printTuning(os.Stdout, f, samples, *all)
	return nil
}

// sampleTests runs each test case once (dry run, with its fakes) and keeps
// every decision, marked right or wrong where the case expects it.
func sampleTests(ctx context.Context, f *flow.Flow, path string, samples map[string][]sample) (int, float64, error) {
	all, err := cases.Load(cases.Dir(path))
	if err != nil || len(all) == 0 {
		return 0, 0, err
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			return 0, 0, err
		}
		backends[name] = b
	}
	var cost float64
	for _, c := range all {
		want := map[string]string{}
		for _, e := range c.Expect {
			want[e.Target] = e.Want
		}
		state := maps.Clone(f.State)
		maps.Copy(state, c.Input)
		res, err := runner.Run(ctx, f, state, runner.Options{
			Backends: backends, DryRun: true, Fake: c.HTTP,
			OnEvent: func(ev runner.Event) { addSamples(samples, f, ev, want) },
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "test %s: %v\n", c.Name, err)
		}
		if res != nil {
			cost += res.CostUSD
		}
	}
	return len(all), cost, nil
}

func addSamples(samples map[string][]sample, f *flow.Flow, ev runner.Event, want map[string]string) {
	n := f.Node(ev.Node)
	if n == nil || !n.Kind.IsDecision() || n.Kind == flow.Questions || len(ev.Decisions) == 0 {
		return // multi-question nodes route on their `then`, not on confidence
	}
	d := ev.Decisions[0]
	s := sample{confidence: d.Confidence}
	if w, ok := want[ev.Node]; ok {
		s.labelled, s.right = true, cases.Matches(d, w)
	}
	samples[ev.Node] = append(samples[ev.Node], s)
}

// minKnown is how many answers with a known right answer it takes before a
// suggested threshold is worth more than a hint.
const minKnown = 10

func printTuning(w io.Writer, f *flow.Flow, samples map[string][]sample, all bool) {
	shown := 0
	for _, n := range f.Nodes {
		ss := samples[n.ID]
		_, unsure := n.Then.Get(flow.Unsure)
		if len(ss) == 0 || (!all && !unsure && n.Threshold == 0) {
			continue
		}
		shown++
		current := f.ThresholdFor(n)
		labelled := 0
		for _, s := range ss {
			if s.labelled {
				labelled++
			}
		}
		fmt.Fprintf(w, "\n%s · %s · threshold now %.2f · %d answers, %d with a known right answer\n", n.ID, n.Kind, current, len(ss), labelled)
		if !unsure {
			fmt.Fprintln(w, "  (no unsure route: below the threshold it still follows its answer)")
		}
		fmt.Fprintf(w, "  %-9s  %-11s  %-10s  %-14s  %s\n", "threshold", "automatic", "to unsure", "right (known)", "confident & wrong")

		thresholds := []float64{0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95}
		if !slices.Contains(thresholds, current) {
			thresholds = append(thresholds, current)
			slices.Sort(thresholds)
		}
		suggest := -1.0
		for _, t := range thresholds {
			var auto, right, known, wrong int
			for _, s := range ss {
				if s.confidence < t {
					continue
				}
				auto++
				if s.labelled {
					known++
					if s.right {
						right++
					} else {
						wrong++
					}
				}
			}
			mark := " "
			if t == current {
				mark = "◂ now"
			}
			rightCol := "-"
			if known > 0 {
				rightCol = fmt.Sprintf("%d/%d", right, known)
			}
			fmt.Fprintf(w, "  %-9.2f  %-11s  %-10s  %-14s  %-17d %s\n", t,
				fmt.Sprintf("%d/%d", auto, len(ss)), fmt.Sprintf("%d/%d", len(ss)-auto, len(ss)), rightCol, wrong, mark)
			if suggest < 0 && labelled > 0 && wrong == 0 {
				suggest = t
			}
		}
		switch {
		case labelled == 0:
			fmt.Fprintln(w, "  no test case says what's right here yet; add expectations to judge a threshold")
		case suggest < 0:
			fmt.Fprintln(w, "  → even at 0.95 some confident answers are wrong: improve the question or descriptions")
		case suggest != current:
			fmt.Fprintf(w, "  → %.2f is the lowest threshold with no confident wrong answers among the known ones (now %.2f)\n", suggest, current)
		default:
			fmt.Fprintln(w, "  → the current threshold is already the lowest with no confident wrong answers")
		}
		if labelled > 0 && labelled < minKnown {
			fmt.Fprintf(w, "  ⚠ only %d known answers: too few to trust; save more runs as test cases (n in the TUI)\n", labelled)
		}
	}
	if shown == 0 {
		fmt.Fprintln(w, "\nno node routes on confidence (add an unsure route or threshold), or none of them ran; --all shows every decision node")
	}
}
