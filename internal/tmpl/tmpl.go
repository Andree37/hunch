// Package tmpl resolves {{path.to.value}} references against flow state.
// A trailing ? ({{path?}}) marks a ref optional: it renders empty when the
// value is missing instead of failing.
package tmpl

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var refRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][\w-]*(?:\.[\w-]+)*)(\?)?\s*\}\}`)

type Ref struct {
	Path     string // e.g. "intent.answer"
	Optional bool
}

// Root returns the first segment of the path.
func (r Ref) Root() string {
	root, _, _ := strings.Cut(r.Path, ".")
	return root
}

func Refs(s string) []Ref {
	var out []Ref
	for _, m := range refRE.FindAllStringSubmatch(s, -1) {
		out = append(out, Ref{Path: m[1], Optional: m[2] != ""})
	}
	return out
}

func Render(s string, state map[string]any) (string, error) {
	return RenderFunc(s, state, nil)
}

// RenderFunc renders s, passing every substituted value through escape along
// with the byte offset in s where its ref starts.
func RenderFunc(s string, state map[string]any, escape func(val string, at int) string) (string, error) {
	var b strings.Builder
	var missing []string
	last := 0
	for _, m := range refRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:m[0]])
		last = m[1]
		path, optional := s[m[2]:m[3]], m[4] >= 0

		v, ok := Lookup(state, path)
		if !ok && !optional {
			missing = append(missing, path)
			continue
		}
		str := ""
		if ok {
			str = Format(v)
		}
		if escape != nil {
			str = escape(str, m[0])
		}
		b.WriteString(str)
	}
	b.WriteString(s[last:])
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown ref {{%s}}", strings.Join(missing, "}}, {{"))
	}
	return b.String(), nil
}

// Lookup resolves a dotted path in state. Keys may themselves contain dots
// ("v2.5"): at each level the longest key that exists wins.
func Lookup(state map[string]any, path string) (any, bool) {
	return lookup(state, strings.Split(path, "."))
}

func lookup(cur any, parts []string) (any, bool) {
	if len(parts) == 0 {
		return cur, true
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	for i := len(parts); i > 0; i-- {
		if next, ok := m[strings.Join(parts[:i], ".")]; ok {
			if v, ok := lookup(next, parts[i:]); ok {
				return v, true
			}
		}
	}
	return nil, false
}

func Format(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', 2, 64)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
