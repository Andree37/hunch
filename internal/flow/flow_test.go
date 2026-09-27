package flow

import (
	"strings"
	"testing"
)

const sample = `
name: t
state: {x: 1}
nodes:
  gate:
    bool: "ok?"
    threshold: 0.8
    then: {yes: pick, no: done, unsure: done}
  pick:
    choice: "which?"
    options: [a, b]
    then: {a: multi, _: done}
  multi:
    questions:
      u: {score: "how much?", scale: 0-10}
      f: {bool: "formal?"}
    then: done
  done:
    action: log
    message: bye
`

func TestParse(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if f.Start != "gate" || f.DefaultBackend != "mock" || f.Threshold != DefaultThreshold {
		t.Errorf("defaults: start=%q backend=%q threshold=%v", f.Start, f.DefaultBackend, f.Threshold)
	}
	if got := len(f.Nodes); got != 4 {
		t.Fatalf("nodes = %d, want 4", got)
	}
	gate := f.Node("gate")
	if f.ThresholdFor(gate) != 0.8 {
		t.Errorf("gate threshold = %v", f.ThresholdFor(gate))
	}
	if to, _ := gate.Then.Get("unsure"); to != "done" {
		t.Errorf("gate unsure route = %q", to)
	}
	multi := f.Node("multi")
	if multi.Kind != Questions || len(multi.Questions) != 2 {
		t.Fatalf("multi = %+v", multi)
	}
	if q := multi.Questions[0]; q.ID != "multi.u" || q.Name != "u" || q.Scale != (Scale{0, 10}) {
		t.Errorf("multi.u = %+v", q)
	}
	if f.Node("done").Action.Message != "bye" {
		t.Errorf("done action = %+v", f.Node("done").Action)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"two kinds":         "nodes: {n: {bool: x, choice: y, options: [a, b]}}",
		"no kind":           "nodes: {n: {then: m}}",
		"unknown field":     "nodes: {n: {bool: x, colour: red}}",
		"one option":        "nodes: {n: {choice: x, options: [a]}}",
		"bad scale":         "nodes: {n: {score: x, scale: five}}",
		"unknown action":    "nodes: {n: {action: email}}",
		"unknown top":       "nodez: {}",
		"scale on bool":     "nodes: {n: {bool: x, scale: 1-5}}",
		"run on bool":       "nodes: {n: {bool: x, run: ls}}",
		"options on log":    "nodes: {n: {action: log, message: m, options: [a, b]}}",
		"levels vs scale":   "nodes: {n: {score: x, scale: 1-5, levels: [a, b]}}",
		"bad criteria":      "nodes: {n: {bool: x, criteria: {maybe: m}}}",
		"empty switch":      "nodes: {n: {switch: \"\"}}",
		"options on switch": "nodes: {n: {switch: x, options: [a, b]}}",
		"sees on action":    "nodes: {n: {action: log, message: m, sees: [x]}}",
		"sees not a list":   "nodes: {n: {bool: q, sees: {a: b}}}",
		"typo in multi":     "nodes: {n: {questions: {a: {bool: x, optoins: [a, b]}}}}",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseDescriptions(t *testing.T) {
	f, err := Parse([]byte(`
nodes:
  b:
    bool: bug?
    criteria: {yes: "broken behavior", no: "a question"}
    then: c
  c:
    choice: team?
    options: {payments: "billing issues", frontend: "rendering issues"}
    then: s
  s:
    score: urgent?
    scale: 0-2
    levels: [can wait, this week, now]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Node("b").Questions[0].Criteria["yes"]; got != "broken behavior" {
		t.Errorf("bool criteria = %q", got)
	}
	c := f.Node("c").Questions[0]
	if len(c.Options) != 2 || c.Options[0] != "payments" || c.Criteria["frontend"] != "rendering issues" {
		t.Errorf("choice = %+v", c)
	}
	s := f.Node("s").Questions[0]
	if s.Scale != (Scale{0, 2}) || s.Levels[2] != "now" {
		t.Errorf("score = %+v", s)
	}
}

func TestParseErrorHasLine(t *testing.T) {
	_, err := Parse([]byte("nodes:\n  n:\n    bool: x\n    colour: red\n"))
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Errorf("err = %v, want line 4", err)
	}
}

func TestMatchScore(t *testing.T) {
	cases := []struct {
		when string
		v    float64
		want bool
	}{
		{">=4", 4, true}, {">=4", 3.9, false},
		{">3", 3, false}, {"<=2", 2, true}, {"<3", 2.99, true},
		{"2-4", 4, true}, {"2-4", 4.01, false},
		{"3", 3.4, true}, {"3", 3.6, false},
	}
	for _, c := range cases {
		got, err := MatchScore(c.when, c.v)
		if err != nil || got != c.want {
			t.Errorf("MatchScore(%q, %v) = %v, %v; want %v", c.when, c.v, got, err, c.want)
		}
	}
	if _, err := MatchScore("lots", 1); err == nil {
		t.Error("expected error for bad condition")
	}
}

func TestRunsLocation(t *testing.T) {
	cases := []struct{ runs, want string }{
		{"", "examples/triage.runs"},
		{"history", "examples/history"},
		{"s3://bucket/triage/", "s3://bucket/triage/"},
		{"/var/hunch/runs", "/var/hunch/runs"},
		{"off", ""},
	}
	for _, c := range cases {
		f := &Flow{Path: "examples/triage.yaml", Runs: c.runs}
		if got := f.RunsLocation(); got != c.want {
			t.Errorf("runs %q: got %q, want %q", c.runs, got, c.want)
		}
	}
	f, err := Parse([]byte("runs: history\nnodes: {a: {action: log, message: m}}"))
	if err != nil || f.Runs != "history" {
		t.Errorf("parse runs: %v %q", err, f.Runs)
	}
}
