package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/validate"
)

type scriptedWriter struct {
	replies []string
	prompts []string
}

func (w *scriptedWriter) Write(_ context.Context, req backend.WriteRequest) (backend.WriteResponse, error) {
	w.prompts = append(w.prompts, req.Prompt)
	r := w.replies[0]
	w.replies = w.replies[1:]
	return backend.WriteResponse{Text: r, CostUSD: 0.01}, nil
}

const goodDraft = "```\n=== flow\n" + `name: refunds
inputs:
  request: {type: text}
nodes:
  valid:
    bool: "Is this a refund request for a real order? {{request}}"
    then: {yes: size, no: reject, unsure: person}
  size:
    choice: "How big is the refund?"
    options: [small, large]
    then: {small: approve, large: person}
  approve: {action: output, set: {action: approve}}
  reject:  {action: output, set: {action: reject}}
  person:  {action: output, set: {action: needs_human}}
=== test small-refund
input: {request: "Order 1234 arrived broken, please refund the $8"}
expect: {valid: yes, size: small, approve.action: approve}
=== test spam
input: {request: "Buy cheap watches"}
expect: {valid: no}
=== test large-refund
input: {request: "Refund my $2,400 laptop order 99"}
expect: {valid: yes, size: large}
` + "```\n"

func TestNewFixesItsDraftThenWrites(t *testing.T) {
	bad := strings.Replace(goodDraft, "large: person", "large: nobody", 1)
	bad = strings.Replace(bad, "size: small,", "size: tiny,", 1)
	w := &scriptedWriter{replies: []string{bad, goodDraft}}

	d, err := draftFlow(context.Background(), w, "approve refunds", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.prompts) != 2 || !strings.Contains(w.prompts[1], "nobody") || !strings.Contains(w.prompts[1], `size is one of small, large, not "tiny"`) {
		t.Fatalf("the second prompt should name the problems:\n%s", w.prompts[len(w.prompts)-1])
	}
	if d.flow.Name != "refunds" || len(d.tests) != 3 || d.cost != 0.02 {
		t.Fatalf("draft = %s, %d tests, $%v", d.flow.Name, len(d.tests), d.cost)
	}

	t.Setenv("TYPESAFE_API_KEY", "x")
	path := filepath.Join(t.TempDir(), "refunds.yaml")
	if err := d.write(path); err != nil {
		t.Fatal(err)
	}
	f, err := flow.Load(path)
	if err != nil || validate.HasErrors(validate.Flow(f)) || f.DefaultBackend != "jev" {
		t.Fatalf("written flow: %v, backend %q", err, f.DefaultBackend)
	}
	all, err := cases.Load(cases.Dir(path))
	if err != nil || len(all) != 3 {
		t.Fatalf("tests: %d, %v", len(all), err)
	}
	if err := d.write(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("writing over a flow: %v", err)
	}
	src, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(src), "name: refunds\n\nbackends:") {
		t.Errorf("backends go after the name:\n%s", src)
	}
}

func TestNewGivesUpAfterThreeBadDrafts(t *testing.T) {
	w := &scriptedWriter{replies: []string{"no", "still no", "nope"}}
	if _, err := draftFlow(context.Background(), w, "x", io.Discard); err == nil || len(w.prompts) != 3 {
		t.Errorf("err = %v after %d tries", err, len(w.prompts))
	}
}

func TestNewNeedsAWriter(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, _, err := draftWriter(""); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("err = %v", err)
	}
	if _, label, err := draftWriter("ollama:qwen2.5:1.5b"); err != nil || label != "ollama qwen2.5:1.5b" {
		t.Errorf("ollama: %q, %v", label, err)
	}
}
