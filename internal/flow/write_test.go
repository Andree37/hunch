package flow

import (
	"os"
	"strings"
	"testing"
)

func TestSpliceSectionOnlyTouchesSection(t *testing.T) {
	src, err := os.ReadFile("../../examples/inbox.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := SpliceSection(src, "state", []string{"message", "sender", "extra"}, map[string]any{
		"message": "Line one\nline two", "sender": "boss", "extra": 3.0,
	}, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(out)
	if err != nil {
		t.Fatalf("result doesn't parse: %v\n%s", err, out)
	}
	if g.State["sender"] != "boss" || g.State["message"] != "Line one\nline two" || g.State["extra"] != 3 {
		t.Errorf("state = %v", g.State)
	}
	if before, after := outside(string(src), "state:"), outside(string(out), "state:"); before != after {
		t.Errorf("text outside the section changed:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

// outside drops the lines from the section header up to the next top-level key.
func outside(s, header string) string {
	var keep []string
	in := false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, header) {
			in = true
			continue
		}
		if in && (strings.HasPrefix(l, "  ") || l == "") {
			continue
		}
		in = false
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

func TestSpliceSectionInserts(t *testing.T) {
	src := "name: x\n\n# the graph\nnodes:\n  a: {action: log, message: hi}"
	out, err := SpliceSection([]byte(src), "state", []string{"who"}, map[string]any{"who": "ann"}, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	want := "name: x\n\nstate:\n  who: ann\n\n# the graph\nnodes:\n  a: {action: log, message: hi}\n"
	if string(out) != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}

	out, _ = SpliceSection([]byte("a: 1\n"), "input", []string{"k"}, map[string]any{"k": "v"}, "")
	if want := "a: 1\ninput:\n  k: v\n"; string(out) != want {
		t.Errorf("append: got %q", out)
	}
}

func TestSpliceSectionLast(t *testing.T) {
	src := "nodes:\n  a: {action: log, message: hi}\nstate: {who: bob}\n\n# end\n"
	out, err := SpliceSection([]byte(src), "state", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "nodes:\n  a: {action: log, message: hi}\nstate: {}\n\n# end\n"; string(out) != want {
		t.Errorf("got %q", out)
	}
}
