package tmpl

import "testing"

func TestRender(t *testing.T) {
	state := map[string]any{
		"who":    "ann",
		"intent": map[string]any{"answer": "meeting", "confidence": 0.8123},
		"n":      3.0,
	}
	got, err := Render("{{who}} wants {{ intent.answer }} ({{intent.confidence}}, {{n}})", state)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ann wants meeting (0.81, 3)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := Render("{{intent.nope}}", state); err == nil {
		t.Error("expected error for missing ref")
	}
	if got, err := Render("[{{intent.nope?}}]", state); err != nil || got != "[]" {
		t.Errorf("optional missing ref = %q, %v", got, err)
	}
}

func TestRefs(t *testing.T) {
	refs := Refs("{{a.b}} and {{ c? }} but not {x}")
	want := []Ref{{"a.b", false}, {"c", true}}
	if len(refs) != 2 || refs[0] != want[0] || refs[1] != want[1] || refs[0].Root() != "a" {
		t.Errorf("refs = %v", refs)
	}
}

func TestRenderFuncOffsets(t *testing.T) {
	s := "x {{a}} y {{b}}"
	var offsets []int
	RenderFunc(s, map[string]any{"a": 1.0, "b": 2.0}, func(v string, at int) string {
		offsets = append(offsets, at)
		return v
	})
	if len(offsets) != 2 || s[offsets[0]:offsets[0]+5] != "{{a}}" || s[offsets[1]:offsets[1]+5] != "{{b}}" {
		t.Errorf("offsets = %v", offsets)
	}
}

func TestLookupDottedKeys(t *testing.T) {
	state := map[string]any{"versions": map[string]any{
		"v2": "two", "v2.5": "two and a half", "v3.1": map[string]any{"x": "deep"},
	}}
	for path, want := range map[string]any{"versions.v2": "two", "versions.v2.5": "two and a half", "versions.v3.1.x": "deep"} {
		if got, ok := Lookup(state, path); !ok || got != want {
			t.Errorf("Lookup(%q) = %v, %v; want %v", path, got, ok, want)
		}
	}
	if _, ok := Lookup(state, "versions.v2.6"); ok {
		t.Error("versions.v2.6 should not resolve")
	}
}

func TestBooleansReadAsYesNo(t *testing.T) {
	got, _ := Render("down: {{a}}, workaround: {{b}}", map[string]any{"a": true, "b": false})
	if got != "down: yes, workaround: no" {
		t.Errorf("got %q", got)
	}
	if Format(true) != "true" {
		t.Error("Format itself stays true/false")
	}
}
