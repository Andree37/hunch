package trace

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
)

func TestWriteAndReadInterleavedRuns(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Start("a", "f.yaml", "jev", map[string]any{"who": "ann"})
	w.Start("b", "f.yaml", "jev", map[string]any{"who": "bob"})
	w.Step("a", runner.Event{Step: 1, Node: "gate", Kind: flow.Bool, Branch: "yes", Next: "done",
		Decisions: []backend.Decision{{QuestionID: "gate", Kind: flow.Bool, Answer: true, Probs: map[string]float64{"yes": 0.9, "no": 0.1}, Confidence: 0.9}}})
	w.Step("b", runner.Event{Step: 1, Node: "gate", Kind: flow.Bool})
	w.Step("a", runner.Event{Step: 2, Node: "done", Kind: flow.Action, Output: map[string]any{"message": "hi"}})
	w.End("a", &runner.Result{Path: []string{"gate", "done"}, Outputs: map[string]any{"x": 1.0}, CostUSD: 0.01}, nil)

	runs, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	a, b := runs[0], runs[1]
	if a.ID != "a" || a.Input["who"] != "ann" || a.Backend != "jev" || len(a.Steps) != 2 || !a.Done || a.CostUSD != 0.01 {
		t.Errorf("a = %+v", a)
	}
	if d := a.Steps[0].Decisions[0]; d.Answer != true || d.Probs["yes"] != 0.9 {
		t.Errorf("decision = %+v", d)
	}
	if out := a.Steps[1].Output.(map[string]any); out["message"] != "hi" {
		t.Errorf("output = %v", a.Steps[1].Output)
	}
	if b.Done || len(b.Steps) != 1 {
		t.Errorf("b should be in progress: %+v", b)
	}
}

func TestEndRecordsError(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Start("r", "f.yaml", "", nil)
	w.End("r", &runner.Result{Path: []string{"a"}}, errors.New("a: boom"))
	runs, _ := Read(&buf)
	if runs[0].Error != "a: boom" || !runs[0].Done {
		t.Errorf("run = %+v", runs[0])
	}
}

func TestNilWriterIsANoOp(t *testing.T) {
	var w *Writer
	w.Start("x", "", "", nil) // must not panic
	w.Step("x", runner.Event{})
	w.End("x", nil, nil)
}

func TestReadRejectsOtherFiles(t *testing.T) {
	if _, err := Read(strings.NewReader(`{"step": 1, "node": "a"}`)); err == nil {
		t.Error("expected an error for a non-trace line")
	}
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	r, err := OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 39) + "\n") // 40 bytes
	for i := 0; i < 7; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	size := func(p string) int64 {
		st, err := os.Stat(p)
		if err != nil {
			return -1
		}
		return st.Size()
	}
	// 7 lines of 40 bytes, 2 per file: current 40, .1 80, .2 80, .3 dropped.
	if size(path) != 40 || size(path+".1") != 80 || size(path+".2") != 80 || size(path+".3") != -1 {
		t.Errorf("sizes: %d %d %d %d", size(path), size(path+".1"), size(path+".2"), size(path+".3"))
	}
}
