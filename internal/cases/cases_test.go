package cases

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
)

func TestDir(t *testing.T) {
	if got := Dir("examples/inbox.yaml"); got != "examples/inbox.tests" {
		t.Errorf("Dir = %q", got)
	}
}

func TestLoadAndSave(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(`# keep me
description: second
input:
  who: bob
  n: 3

# expectations
expect:
  gate: yes
`), 0o644)
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("input: {who: ann}\n"), 0o644)

	cs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Name != "a" || cs[1].Name != "b" {
		t.Fatalf("cases = %+v", cs)
	}
	b := cs[1]
	if b.Description != "second" || b.Input["n"] != 3 || b.Keys[0] != "who" || b.Expect[0] != (Expectation{"gate", "yes"}) {
		t.Errorf("b = %+v", b)
	}

	b.Keys = append(b.Keys, "note")
	b.Input["who"], b.Input["note"] = "carol", "line 1\nline 2"
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(b.Path)
	for _, keep := range []string{"# keep me", "# expectations", "gate: yes", "who: carol", "note: |-"} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("saved file missing %q:\n%s", keep, out)
		}
	}
}

func TestSaveNew(t *testing.T) {
	c := &Case{
		Path:        filepath.Join(t.TempDir(), "inbox.tests", "new.yaml"),
		Description: "fresh",
		Keys:        []string{"who"},
		Input:       map[string]any{"who": "ann"},
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(c.Path)
	if want := "description: fresh\ninput:\n  who: ann\n"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}

	c.Path = filepath.Join(t.TempDir(), "bare.yaml")
	c.Description = ""
	c.Save()
	out, _ = os.ReadFile(c.Path)
	if want := "input:\n  who: ann\n"; string(out) != want {
		t.Errorf("bare: got %q", out)
	}
}

func TestCheck(t *testing.T) {
	f, err := flow.Parse([]byte(`
nodes:
  gate: {bool: q, then: kind}
  kind: {choice: q, options: [a, b], then: multi}
  multi:
    questions:
      u: {score: q}
    then: size
  size: {score: q}
  never: {bool: q}
  result: {action: output, set: {action: x}}
`))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"gate":   map[string]any{"answer": true},
		"kind":   map[string]any{"answer": "b"},
		"multi":  map[string]any{"u": map[string]any{"answer": 4.4}},
		"size":   map[string]any{"answer": 2.0},
		"result": map[string]any{"action": "send", "n": 2},
	}
	c := &Case{Expect: []Expectation{
		{"gate", "yes"}, {"kind", "a"}, {"multi.u", ">=4"}, {"size", "2"},
		{"never", "no"}, {"nope", "x"}, {"multi.zz", "1"},
		{"result.action", "send"}, {"result.n", "3"}, {"result", "x"},
	}}
	rs := c.Check(f, state)
	wantOK := []bool{true, false, true, true, false, false, false, true, false, false}
	wantGot := []string{"yes", "b", "4.40", "2.00", "didn't run", "no such node", "no such question", "send", "2", "name a field, e.g. result.text"}
	for i, r := range rs {
		if r.OK != wantOK[i] || r.Got != wantGot[i] {
			t.Errorf("%s: ok=%v got=%q; want ok=%v got=%q", r.Target, r.OK, r.Got, wantOK[i], wantGot[i])
		}
	}
	if Passed(rs) {
		t.Error("Passed should be false")
	}
}

func TestFakeHTTP(t *testing.T) {
	c, err := Parse([]byte(`
input: {id: 7}
http:
  fetch:
    body: {title: "Logo blurry", tags: [ui], meta: {open: true}}
  update: {status: 500}
`))
	if err != nil {
		t.Fatal(err)
	}
	fetch := c.HTTP["fetch"]
	if fetch.Status != 200 {
		t.Errorf("default status = %d", fetch.Status)
	}
	body := fetch.Body.(map[string]any)
	if body["title"] != "Logo blurry" || body["meta"].(map[string]any)["open"] != true {
		t.Errorf("body = %#v", body)
	}
	if c.HTTP["update"].Status != 500 {
		t.Errorf("update = %+v", c.HTTP["update"])
	}

	f, _ := flow.Parse([]byte(`nodes:
  fetch: {action: http, method: GET, url: "x", then: update}
  update: {action: http, url: "x"}
  note: {action: log, message: m}
`))
	c.Name = "t"
	if err := c.CheckFakes(f); err != nil {
		t.Errorf("valid fakes: %v", err)
	}
	c.HTTP["note"] = c.HTTP["fetch"]
	if err := c.CheckFakes(f); err == nil || !strings.Contains(err.Error(), `"note", which isn't an http node`) {
		t.Errorf("err = %v", err)
	}
}

func TestFromRunRoundTrip(t *testing.T) {
	f, err := flow.Parse([]byte(`
backends: {m: {kind: mock, answers: {gate: yes, kind: b, multi.n: 4, multi.ok: no}}}
nodes:
  fetch: {action: http, method: GET, url: "{{base}}", then: gate}
  gate: {bool: "q?", then: {yes: kind, no: done}}
  kind: {choice: "which?", options: [a, b], then: multi}
  multi:
    questions:
      n: {score: "how many?"}
      ok: {bool: "ok?"}
    then: done
  done: {action: output, set: {action: filed, kind: "{{kind.answer}}"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := backend.New(f.Backends["m"])
	input := map[string]any{"base": "https://example.invalid", "who": "ann"}
	res, err := runner.Run(context.Background(), f, input, runner.Options{
		Backends: map[string]backend.Backend{"m": b},
		Fake:     map[string]runner.FakeResponse{"fetch": {Status: 200, Body: map[string]any{"title": "T"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := recordEvents(t, f, input, b)

	c := FromRun(f, input, res.Path, events)
	c.Name, c.Path = "saved", filepath.Join(t.TempDir(), "f.tests", "saved.yaml")
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(); err == nil {
		t.Error("Create must not overwrite")
	}
	back, err := Load(filepath.Dir(c.Path))
	if err != nil {
		t.Fatal(err)
	}
	got := back[0]
	want := map[string]string{"gate": "yes", "kind": "b", "multi.n": "4", "multi.ok": "no", "done.action": "filed", "done.kind": "b"}
	if len(got.Expect) != len(want) {
		t.Errorf("expect = %+v", got.Expect)
	}
	for _, e := range got.Expect {
		if want[e.Target] != e.Want {
			t.Errorf("expect %s = %q, want %q", e.Target, e.Want, want[e.Target])
		}
	}
	if got.HTTP["fetch"].Body.(map[string]any)["title"] != "T" || got.Input["who"] != "ann" {
		t.Errorf("case = %+v", got)
	}
	// The saved case passes against the same flow, offline.
	res2, err := runner.Run(context.Background(), f, got.Input, runner.Options{Backends: map[string]backend.Backend{"m": b}, Fake: got.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	if rs := got.Check(f, res2.State); !Passed(rs) {
		t.Errorf("saved case fails: %+v", rs)
	}
}

func recordEvents(t *testing.T, f *flow.Flow, input map[string]any, b backend.Backend) map[string]runner.Event {
	t.Helper()
	events := map[string]runner.Event{}
	_, err := runner.Run(context.Background(), f, input, runner.Options{
		Backends: map[string]backend.Backend{"m": b},
		Fake:     map[string]runner.FakeResponse{"fetch": {Status: 200, Body: map[string]any{"title": "T"}}},
		OnEvent:  func(ev runner.Event) { events[ev.Node] = ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}
