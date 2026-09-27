package validate

import (
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/flow"
)

func check(t *testing.T, src string) []Issue {
	t.Helper()
	f, err := flow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return Flow(f)
}

func has(issues []Issue, sev Severity, substr string) bool {
	for _, i := range issues {
		if i.Severity == sev && strings.Contains(i.Msg, substr) {
			return true
		}
	}
	return false
}

func TestValidFlow(t *testing.T) {
	issues := check(t, `
nodes:
  a: {bool: "q?", then: {yes: b, no: c}}
  b: {action: log, message: "{{a.answer}}"}
  c: {action: log, message: bye}
`)
	if len(issues) != 0 {
		t.Errorf("unexpected issues: %v", issues)
	}
}

func TestIssues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		sev  Severity
		msg  string
	}{
		{"dangling", `nodes: {a: {bool: q, then: {yes: nope, no: nope}}}`, Error, "does not exist"},
		{"bad bool branch", `nodes: {a: {bool: q, then: {maybe: a, _: a}}}`, Error, "can never match"},
		{"bad option", `nodes: {a: {choice: q, options: [x, y], then: {z: a, _: a}}}`, Error, "can never match"},
		{"missing option", `nodes: {a: {choice: q, options: [x, y], then: {x: a}}}`, Warning, `no route for answer "y"`},
		{"score gap", `nodes: {a: {score: q, then: {"<2": a, ">=4": a}}}`, Warning, "no route for scores around 2.00"},
		{"bad score cond", `nodes: {a: {score: q, then: {lots: a, _: a}}}`, Error, "bad score condition"},
		{"branches on action", `nodes: {a: {action: log, message: m, then: {x: a}}}`, Error, "single target"},
		{"unreachable", `nodes: {a: {action: log, message: m}, b: {action: log, message: m}}`, Warning, "unreachable"},
		{"loop", `nodes: {a: {bool: q, then: {yes: a, no: b}}, b: {action: log, message: m}}`, Warning, "loops back"},
		{"unknown backend", `nodes: {a: {bool: q, backend: jev, then: a}}`, Error, `backend "jev" is not defined`},
		{"unused answer", `nodes: {a: {bool: q}}`, Warning, "never used"},
		{"unused input", `{inputs: {x: [a, b]}, nodes: {a: {action: log, message: m}}}`, Warning, `input "x" is declared but no node uses it`},
		{"ref to later node", `nodes: {a: {bool: "{{b.answer}}?", then: b}, b: {action: log, message: m}}`, Error, "never runs before"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := check(t, c.src)
			if !has(issues, c.sev, c.msg) {
				t.Errorf("want %s containing %q, got %v", c.sev, c.msg, issues)
			}
		})
	}
}

func TestRefOnSkippablePath(t *testing.T) {
	src := `
nodes:
  gate: {bool: q, then: {yes: kind, no: done}}
  kind: {choice: q, options: [x, y], then: done}
  done: {action: log, message: "{{kind.answer}}"}
`
	issues := check(t, src)
	if !has(issues, Warning, `reach "done" without running "kind"`) {
		t.Errorf("want skipped-path warning, got %v", issues)
	}

	issues = check(t, strings.Replace(src, "{{kind.answer}}", "{{kind.answer?}}", 1))
	if len(issues) != 0 {
		t.Errorf("optional ref should silence it, got %v", issues)
	}
}

func TestRefOnEveryPathIsFine(t *testing.T) {
	issues := check(t, `
nodes:
  gate: {bool: q, then: {yes: a, no: b}}
  a: {action: log, message: a, then: done}
  b: {action: log, message: b, then: done}
  done: {action: log, message: "{{gate.answer}}"}
`)
	if len(issues) != 0 {
		t.Errorf("unexpected issues: %v", issues)
	}
}

func TestSelfRefInLoop(t *testing.T) {
	src := `
nodes:
  a: {score: "again, last was {{a.answer}}", then: {">=3": b, _: a}}
  b: {action: log, message: done}
`
	// The first visit has no previous answer.
	if issues := check(t, src); !has(issues, Warning, "without running") {
		t.Errorf("want warning, got %v", issues)
	}
	if issues := check(t, strings.Replace(src, "a.answer", "a.answer?", 1)); has(issues, Warning, "without running") {
		t.Errorf("optional ref should silence it, got %v", issues)
	}
}
