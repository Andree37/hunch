package runner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
)

const inbox = `
backends:
  mock:
    kind: mock
    answers:
      gate: yes
      intent: meeting
      tone.urgency: 5
      tone.formal: no
nodes:
  gate: {bool: "reply?", then: {yes: intent, no: archive}}
  intent:
    choice: "want?"
    options: [meeting, sales]
    then: {sales: archive, _: tone}
  tone:
    questions:
      urgency: {score: "urgent?"}
      formal: {bool: "formal?"}
    then: effort
  effort:
    score: "effort? urgency {{tone.urgency.level}}"
    then: {"<3": reply, _: reply}
  reply:
    action: shell
    run: printf %s {{intent.answer}}
  archive: {action: log, message: archived}
`

func run(t *testing.T, src string, state map[string]any) *Result {
	t.Helper()
	f, err := flow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		backends[name] = b
	}
	res, err := Run(context.Background(), f, state, Options{Backends: backends})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRunForcedPath(t *testing.T) {
	res := run(t, inbox, nil)
	want := []string{"gate", "intent", "tone", "effort", "reply"}
	if !slices.Equal(res.Path, want) {
		t.Fatalf("path = %v, want %v", res.Path, want)
	}
	tone := res.State["tone"].(map[string]any)
	if lvl := tone["urgency"].(map[string]any)["level"]; lvl != 5 {
		t.Errorf("urgency level = %v, want 5", lvl)
	}
	if out := res.State["reply"].(map[string]any)["stdout"]; out != "meeting" {
		t.Errorf("reply stdout = %q", out)
	}
}

func TestShellValuesAreEscaped(t *testing.T) {
	evil := `hi'; echo "pwned $HOME" $(id) ` + "`id`" + ` \ end`
	runs := map[string]string{
		"unquoted":      `printf %s {{x}}`,
		"single quotes": `printf %s 'got: {{x}}'`,
		"double quotes": `printf %s "got: {{x}}"`,
		"after escapes": `printf %s "a \" b" 'c' got:\ {{x}}`,
	}
	for name, cmd := range runs {
		t.Run(name, func(t *testing.T) {
			src := "nodes: {a: {action: shell, run: '" + strings.ReplaceAll(cmd, "'", "''") + "'}}"
			res := run(t, src, map[string]any{"x": evil})
			out := res.State["a"].(map[string]any)["stdout"].(string)
			if !strings.HasSuffix(out, evil) {
				t.Errorf("stdout = %q, want suffix %q", out, evil)
			}
		})
	}
}

func TestUnsureRoute(t *testing.T) {
	// A choice forced at 0.9 confidence is unsure under a 0.95 threshold.
	src := `
backends: {m: {kind: mock, answers: {c: b}}}
nodes:
  c: {choice: q, options: [a, b], threshold: 0.95, then: {b: done, unsure: ask}}
  done: {action: log, message: done}
  ask: {action: log, message: ask}
`
	res := run(t, src, nil)
	if !slices.Equal(res.Path, []string{"c", "ask"}) {
		t.Errorf("path = %v", res.Path)
	}
}

func TestMaxVisits(t *testing.T) {
	f, err := flow.Parse([]byte(`nodes: {a: {bool: q, then: {_: a}}}`))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := backend.New(f.Backends["mock"])
	_, err = Run(context.Background(), f, nil, Options{Backends: map[string]backend.Backend{"mock": m}, MaxVisits: 3})
	if err == nil {
		t.Error("expected max visits error")
	}
}

// oneAtATime hides the mock's multi-question support to exercise fan-out.
type oneAtATime struct{ backend.Backend }

func (oneAtATime) Caps() backend.Caps { return backend.Caps{} }

func TestFanOutForSingleQuestionBackends(t *testing.T) {
	f, _ := flow.Parse([]byte(inbox))
	m, _ := backend.New(f.Backends["mock"])
	res, err := Run(context.Background(), f, nil, Options{Backends: map[string]backend.Backend{"mock": oneAtATime{m}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Path) != 5 {
		t.Errorf("path = %v", res.Path)
	}
}

func TestDeterministic(t *testing.T) {
	src := `
nodes:
  a: {choice: "pick for {{who}}", options: [x, y, z], then: b}
  b: {action: log, message: "{{a.answer}}"}
`
	a := run(t, src, map[string]any{"who": "ann"}).State["a"]
	b := run(t, src, map[string]any{"who": "ann"}).State["a"]
	if a.(map[string]any)["answer"] != b.(map[string]any)["answer"] {
		t.Error("same input gave different answers")
	}
}

func TestBeforeCanPauseAndStop(t *testing.T) {
	f, err := flow.Parse([]byte(inbox))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := backend.New(f.Backends["mock"])
	var seen []string
	res, err := Run(context.Background(), f, nil, Options{
		Backends: map[string]backend.Backend{"mock": m},
		Before: func(_ context.Context, node string) error {
			seen = append(seen, node)
			if node == "tone" {
				return context.Canceled
			}
			return nil
		},
	})
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !slices.Equal(seen, []string{"gate", "intent", "tone"}) || !slices.Equal(res.Path, []string{"gate", "intent"}) {
		t.Errorf("seen = %v, path = %v", seen, res.Path)
	}
}

func TestSwitch(t *testing.T) {
	src := `
nodes:
  scope: {switch: "{{order.status}}", then: {paid: in, part.paid: in, _: out}}
  in: {action: output, set: {scope: in}}
  out: {action: output, set: {scope: out}}
`
	for status, want := range map[string]string{"paid": "in", "part.paid": "in", "unpaid": "out"} {
		res, err := runFlow(t, src, map[string]any{"order": map[string]any{"status": status}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outputs["scope"] != want {
			t.Errorf("%s: scope = %v, want %s", status, res.Outputs["scope"], want)
		}
	}
	_, err := runFlow(t, `nodes: {s: {switch: "{{x}}", then: {a: s}}}`, map[string]any{"x": "b"}, false)
	if err == nil || !strings.Contains(err.Error(), `no route for value "b"`) {
		t.Errorf("err = %v", err)
	}
}
