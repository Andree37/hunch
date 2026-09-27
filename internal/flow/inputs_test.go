package flow

import "testing"

func TestParseInputs(t *testing.T) {
	f, err := Parse([]byte(`
inputs:
  channel: [email, slack]
  vip: {type: bool}
  count: {type: number, min: 1, max: 10, description: How many}
  body: text
  mood: {options: [calm, angry]}
nodes:
  a: {bool: "{{extra}} {{body}}?", then: {_: a}}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"channel": InputChoice, "vip": InputBool, "count": InputNumber, "body": InputText, "mood": InputChoice}
	for name, typ := range want {
		s, ok := f.InputSpec(name)
		if !ok || s.Type != typ {
			t.Errorf("%s = %+v, want type %s", name, s, typ)
		}
	}
	if s, _ := f.InputSpec("count"); *s.Min != 1 || *s.Max != 10 || s.Description != "How many" {
		t.Errorf("count = %+v", s)
	}
	got := f.Inputs()
	if len(got) != 6 || got[0] != "channel" || got[5] != "extra" {
		t.Errorf("Inputs() = %v, want declared first then discovered", got)
	}
}

func TestParseInputErrors(t *testing.T) {
	cases := map[string]string{
		"unknown type":    "inputs: {a: {type: date}}",
		"one option":      "inputs: {a: [x]}",
		"options on bool": "inputs: {a: {type: bool, options: [x, y]}}",
		"min on text":     "inputs: {a: {type: text, min: 1}}",
		"unknown field":   "inputs: {a: {type: text, colour: red}}",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInputParseAndCheck(t *testing.T) {
	one, ten := 1.0, 10.0
	choice := InputSpec{Name: "c", Type: InputChoice, Options: []string{"a", "b"}}
	boolean := InputSpec{Name: "v", Type: InputBool}
	num := InputSpec{Name: "n", Type: InputNumber, Min: &one, Max: &ten}
	text := InputSpec{Name: "t", Type: InputText}
	loose := InputSpec{Name: "x"}

	ok := []struct {
		s    InputSpec
		in   string
		want any
	}{
		{choice, "a", "a"}, {boolean, "yes", true}, {boolean, "False", false},
		{num, "3.5", 3.5}, {text, "42", "42"}, {text, "true", "true"},
		{loose, "42", 42}, {loose, "hello", "hello"},
	}
	for _, c := range ok {
		got, err := c.s.Parse(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s.Parse(%q) = %v, %v; want %v", c.s.Name, c.in, got, err, c.want)
		}
	}
	bad := []struct {
		s  InputSpec
		in string
	}{{choice, "z"}, {boolean, "maybe"}, {num, "x"}, {num, "11"}, {num, "0"}}
	for _, c := range bad {
		if _, err := c.s.Parse(c.in); err == nil {
			t.Errorf("%s.Parse(%q): expected error", c.s.Name, c.in)
		}
	}
	if choice.Check("z") == nil || boolean.Check("yes") == nil || num.Check(5) != nil || text.Check(3) == nil {
		t.Error("Check disagrees")
	}
}
