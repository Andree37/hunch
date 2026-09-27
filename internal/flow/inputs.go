package flow

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Input types. Undeclared inputs are free-form: typed text is kept as is
// unless it reads as JSON (a number, true/false, a list...).
const (
	InputText   = "text"
	InputChoice = "choice"
	InputBool   = "bool"
	InputNumber = "number"
)

// InputSpec declares what an input accepts. It never holds a value; values
// come from test cases, --set, or typing in the TUI.
type InputSpec struct {
	Name        string
	Type        string
	Options     []string // choice
	Min, Max    *float64 // number, optional bounds
	Description string
}

func (f *Flow) InputSpec(name string) (InputSpec, bool) {
	i := slices.IndexFunc(f.InputSpecs, func(s InputSpec) bool { return s.Name == name })
	if i < 0 {
		return InputSpec{Name: name}, false
	}
	return f.InputSpecs[i], true
}

// Parse turns text (as typed or given with --set) into a value of the
// input's type.
func (s InputSpec) Parse(text string) (any, error) {
	switch s.Type {
	case InputText:
		return text, nil
	case InputChoice:
		if !slices.Contains(s.Options, text) {
			return nil, fmt.Errorf("%s: %q is not one of %s", s.Name, text, strings.Join(s.Options, ", "))
		}
		return text, nil
	case InputBool:
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "yes", "true", "y":
			return true, nil
		case "no", "false", "n":
			return false, nil
		}
		return nil, fmt.Errorf("%s: want yes or no, got %q", s.Name, text)
	case InputNumber:
		x, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", s.Name, text)
		}
		return x, s.Check(x)
	}
	return guess(text), nil
}

// Check validates a value that is already typed, e.g. from a test case file.
func (s InputSpec) Check(v any) error {
	switch s.Type {
	case InputChoice:
		if str, ok := v.(string); !ok || !slices.Contains(s.Options, str) {
			return fmt.Errorf("%s: %v is not one of %s", s.Name, v, strings.Join(s.Options, ", "))
		}
	case InputBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: want yes or no, got %v", s.Name, v)
		}
	case InputNumber:
		var x float64
		switch n := v.(type) {
		case int:
			x = float64(n)
		case float64:
			x = n
		default:
			return fmt.Errorf("%s: %v is not a number", s.Name, v)
		}
		if s.Min != nil && x < *s.Min {
			return fmt.Errorf("%s: %v is below the minimum %v", s.Name, x, *s.Min)
		}
		if s.Max != nil && x > *s.Max {
			return fmt.Errorf("%s: %v is above the maximum %v", s.Name, x, *s.Max)
		}
	case InputText:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: want text, got %v", s.Name, v)
		}
	}
	return nil
}

func guess(text string) any {
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err != nil || v == nil {
		return text
	}
	switch v.(type) {
	case bool, int, float64, []any, map[string]any:
		return v
	}
	return text
}

var inputKeys = set("type", "options", "min", "max", "description")

// parseInputs reads the `inputs:` section. Short forms: `name: [a, b]` is a
// choice, `name: text` a type.
func parseInputs(f *Flow, n *yaml.Node) error {
	fields, keys, err := mapping(n)
	if err != nil {
		return fmt.Errorf("inputs: %w", err)
	}
	for _, name := range keys {
		v := fields[name]
		s := InputSpec{Name: name, Type: InputText}
		switch v.Kind {
		case yaml.SequenceNode:
			s.Type = InputChoice
			if err := v.Decode(&s.Options); err != nil {
				return errAt(v, "input %q: %v", name, err)
			}
		case yaml.ScalarNode:
			s.Type = v.Value
		case yaml.MappingNode:
			spec, skeys, _ := mapping(v)
			for _, k := range skeys {
				if !inputKeys[k] {
					return errAt(spec[k], "input %q: unknown field %q", name, k)
				}
			}
			if o := spec["options"]; o != nil {
				s.Type = InputChoice
				if err := o.Decode(&s.Options); err != nil {
					return errAt(o, "input %q: options: %v", name, err)
				}
			}
			if t := spec["type"]; t != nil {
				s.Type = t.Value
			}
			if d := spec["description"]; d != nil {
				s.Description = d.Value
			}
			for _, b := range []struct {
				key string
				dst **float64
			}{{"min", &s.Min}, {"max", &s.Max}} {
				if bn := spec[b.key]; bn != nil {
					var x float64
					if err := bn.Decode(&x); err != nil {
						return errAt(bn, "input %q: %s: %v", name, b.key, err)
					}
					*b.dst = &x
				}
			}
		}
		switch {
		case s.Type != InputText && s.Type != InputChoice && s.Type != InputBool && s.Type != InputNumber:
			return errAt(v, "input %q: unknown type %q (want text, choice, bool, number)", name, s.Type)
		case s.Type == InputChoice && len(s.Options) < 2:
			return errAt(v, "input %q: choice needs at least 2 options", name)
		case s.Type != InputChoice && len(s.Options) > 0:
			return errAt(v, "input %q: options only apply to choice inputs", name)
		case s.Type != InputNumber && (s.Min != nil || s.Max != nil):
			return errAt(v, "input %q: min/max only apply to number inputs", name)
		}
		f.InputSpecs = append(f.InputSpecs, s)
	}
	return nil
}
