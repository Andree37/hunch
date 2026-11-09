package flow

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SpliceSection rewrites one top-level section of a YAML file as a block map
// of values, in keys order. Only that section's lines change; the rest of the
// file, comments and blank lines included, stays byte for byte. Comments
// inside the old section are not kept. A missing section is inserted above
// the `before` key (and its comments), or at the end.
func SpliceSection(src []byte, section string, keys []string, values map[string]any, before string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("file is not a map")
	}
	root := doc.Content[0]
	block, err := encodeSection(section, keys, values)
	if err != nil {
		return nil, err
	}

	lines := strings.SplitAfter(string(src), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += "\n"
	}

	start, end := -1, -1
	if i := keyIndex(root, section); i >= 0 {
		start = root.Content[i].Line - 1
		end = len(lines)
		if i+2 < len(root.Content) {
			end = root.Content[i+2].Line - 1
		}
		// Blank lines and the next key's comments stay put.
		for end > start+1 && isBlankOrComment(lines[end-1]) {
			end--
		}
	} else {
		at := len(lines)
		if i := keyIndex(root, before); before != "" && i >= 0 {
			at = root.Content[i].Line - 1
			for at > 0 && strings.HasPrefix(strings.TrimSpace(lines[at-1]), "#") {
				at--
			}
		}
		start, end = at, at
		if at < len(lines) {
			block += "\n"
		}
	}

	var out strings.Builder
	for _, l := range lines[:start] {
		out.WriteString(l)
	}
	out.WriteString(block)
	for _, l := range lines[end:] {
		out.WriteString(l)
	}
	return []byte(out.String()), nil
}

// encodeSection renders `key:` followed by a block map of values.
func encodeSection(key string, keys []string, values map[string]any) (string, error) {
	if len(keys) == 0 {
		return key + ": {}\n", nil
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range keys {
		var v yaml.Node
		if err := v.Encode(values[k]); err != nil {
			return "", fmt.Errorf("%s %q: %w", key, k, err)
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &v)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return "", err
	}
	enc.Close()

	var b strings.Builder
	b.WriteString(key + ":\n")
	for _, l := range strings.SplitAfter(buf.String(), "\n") {
		if l != "" {
			b.WriteString("  " + l)
		}
	}
	return b.String(), nil
}

func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// SetThreshold sets one node's `threshold:` in a flow file, changing only
// that value (or adding it as the node's first field) so the rest of the
// file, comments included, stays byte for byte.
func SetThreshold(src []byte, node string, value float64) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("file is not a map")
	}
	root := doc.Content[0]
	i := keyIndex(root, "nodes")
	if i < 0 {
		return nil, fmt.Errorf("no nodes")
	}
	nodes := root.Content[i+1]
	j := keyIndex(nodes, node)
	if j < 0 {
		return nil, fmt.Errorf("no node %q", node)
	}
	n := nodes.Content[j+1]
	if n.Kind != yaml.MappingNode || len(n.Content) == 0 {
		return nil, fmt.Errorf("node %q is not a map", node)
	}
	text := strconv.FormatFloat(value, 'f', -1, 64)
	lines := strings.SplitAfter(string(src), "\n")
	edit := func(line, col int, cut int, insert string) {
		r := []rune(lines[line-1])
		at := min(col-1, len(r))
		lines[line-1] = string(r[:at]) + insert + string(r[min(at+cut, len(r)):])
	}
	switch k := keyIndex(n, "threshold"); {
	case k >= 0:
		v := n.Content[k+1]
		r := []rune(lines[v.Line-1])
		end := v.Column - 1
		for end < len(r) && strings.ContainsRune("0123456789.+-eE", r[end]) {
			end++
		}
		edit(v.Line, v.Column, end-(v.Column-1), text)
	case n.Style&yaml.FlowStyle != 0:
		edit(n.Line, n.Column+1, 0, "threshold: "+text+", ")
	default:
		first := n.Content[0]
		indent := strings.Repeat(" ", first.Column-1)
		lines[first.Line-1] = indent + "threshold: " + text + "\n" + lines[first.Line-1]
	}
	out := []byte(strings.Join(lines, ""))
	f, err := Parse(out)
	if err != nil {
		return nil, fmt.Errorf("setting %s's threshold broke the file: %w", node, err)
	}
	if got := f.Node(node); got == nil || got.Threshold != value {
		return nil, fmt.Errorf("couldn't set %s's threshold", node)
	}
	return out, nil
}
