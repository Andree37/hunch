package flow

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

func parseAction(id string, n *yaml.Node, fields map[string]*yaml.Node) (*ActionSpec, error) {
	a := &ActionSpec{Type: fields["action"].Value}
	allowed, ok := actionFields[a.Type]
	if !ok {
		return nil, errAt(n, "node %q: unknown action %q (want log, shell, llm, http, output)", id, a.Type)
	}
	for _, k := range actionOnly {
		if fields[k] != nil && !allowed[k] {
			return nil, errAt(fields[k], "node %q: %s actions don't take %s", id, a.Type, k)
		}
	}
	str := func(k string) string {
		if v := fields[k]; v != nil {
			return v.Value
		}
		return ""
	}
	a.Message, a.Run, a.Prompt, a.System = str("message"), str("run"), str("prompt"), str("system")
	a.Method, a.URL = strings.ToUpper(str("method")), str("url")
	for _, k := range []struct {
		key string
		dst any
	}{{"max_tokens", &a.MaxTokens}, {"timeout", &a.Timeout}} {
		if v := fields[k.key]; v != nil {
			if err := v.Decode(k.dst); err != nil {
				return nil, errAt(v, "node %q: %s: %v", id, k.key, err)
			}
		}
	}
	if v := fields["body"]; v != nil {
		if err := v.Decode(&a.Body); err != nil {
			return nil, errAt(v, "node %q: body: %v", id, err)
		}
	}
	var err error
	if a.Headers, err = kvs(fields["headers"]); err != nil {
		return nil, fmt.Errorf("node %q: headers: %w", id, err)
	}
	if a.Set, err = kvs(fields["set"]); err != nil {
		return nil, fmt.Errorf("node %q: set: %w", id, err)
	}

	need := map[string]string{ActLog: a.Message, ActShell: a.Run, ActLLM: a.Prompt, ActHTTP: a.URL}
	if v, ok := need[a.Type]; ok && v == "" {
		field := map[string]string{ActLog: "message", ActShell: "run", ActLLM: "prompt", ActHTTP: "url"}[a.Type]
		return nil, errAt(n, "node %q: %s action needs %s", id, a.Type, field)
	}
	if a.Type == ActOutput && len(a.Set) == 0 {
		return nil, errAt(n, "node %q: output action needs set: {name: value}", id)
	}
	if a.Type == ActHTTP {
		if a.Method == "" {
			a.Method = "POST"
		}
		switch a.Method {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			return nil, errAt(fields["method"], "node %q: unknown method %q", id, a.Method)
		}
	}
	return a, nil
}

// kvs reads an ordered map of scalars. Strings are templates; numbers and
// bools are kept as literals.
func kvs(n *yaml.Node) ([]KV, error) {
	if n == nil {
		return nil, nil
	}
	fields, keys, err := mapping(n)
	if err != nil {
		return nil, err
	}
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		v := fields[k]
		if v.Kind != yaml.ScalarNode {
			return nil, errAt(v, "%q: want a single value", k)
		}
		kv := KV{Key: k, Value: v.Value}
		if v.Tag != "!!str" {
			if err := v.Decode(&kv.Literal); err != nil {
				return nil, errAt(v, "%q: %v", k, err)
			}
		}
		out = append(out, kv)
	}
	return out, nil
}
