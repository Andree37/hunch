package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
)

func TestTuning(t *testing.T) {
	f, err := flow.Parse([]byte(`
nodes:
  gate: {bool: q, threshold: 0.7, then: {yes: done, no: done, unsure: done}}
  plain: {bool: q, then: done}
  done: {action: log, message: m}
`))
	if err != nil {
		t.Fatal(err)
	}
	samples := map[string][]sample{}
	// 12 labelled answers: right when confident, but two wrong at 0.62/0.72.
	for _, c := range []float64{0.99, 0.97, 0.95, 0.93, 0.9, 0.88, 0.86, 0.84, 0.82, 0.8} {
		samples["gate"] = append(samples["gate"], sample{confidence: c, labelled: true, right: true})
	}
	samples["gate"] = append(samples["gate"],
		sample{confidence: 0.62, labelled: true, right: false},
		sample{confidence: 0.72, labelled: true, right: false},
		sample{confidence: 0.55}) // unlabelled, from a recorded run
	samples["plain"] = []sample{{confidence: 0.9}}

	var buf bytes.Buffer
	printTuning(&buf, f, samples, false)
	out := buf.String()
	for _, want := range []string{
		"gate · bool · threshold now 0.70 · 13 answers, 12 with a known right answer",
		"0.70       11/13        2/13        10/11           1                 ◂ now",
		"0.75       10/13        3/13        10/10           0",
		"→ 0.75 is the lowest threshold with no confident wrong answers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "plain ·") {
		t.Error("nodes that don't route on confidence are hidden unless --all")
	}
	if strings.Contains(out, "too few to trust") {
		t.Error("12 known answers is enough")
	}
}

func TestAddSamplesLabelsFromExpectations(t *testing.T) {
	f, _ := flow.Parse([]byte(`nodes: {k: {choice: q, options: [a, b]}, s: {score: q}}`))
	samples := map[string][]sample{}
	ev := func(node string, kind flow.Kind, answer any, conf float64) runner.Event {
		return runner.Event{Node: node, Decisions: []backend.Decision{{Kind: kind, Answer: answer, Confidence: conf}}}
	}
	want := map[string]string{"k": "a", "s": ">=4"}
	addSamples(samples, f, ev("k", flow.Choice, "b", 0.8), want)
	addSamples(samples, f, ev("s", flow.Score, 4.2, 0.6), want)
	addSamples(samples, f, ev("k", flow.Choice, "a", 0.9), nil)
	if k := samples["k"]; len(k) != 2 || !k[0].labelled || k[0].right || k[1].labelled {
		t.Errorf("k = %+v", k)
	}
	if s := samples["s"][0]; !s.labelled || !s.right {
		t.Errorf("s = %+v", s)
	}
}
