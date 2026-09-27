package flow

import "testing"

func TestParseActions(t *testing.T) {
	f, err := Parse([]byte(`
backends:
  default: jev
  writer: claude
  jev: {kind: jev}
  claude: {kind: anthropic, model: claude-opus-5}
  local: {kind: openai, model: llama3}
nodes:
  draft:
    action: llm
    system: You write short replies.
    prompt: "Reply to {{message}}"
    max_tokens: 300
    then: other
  other:
    action: llm
    prompt: again
    backend: local
    then: post
  post:
    action: http
    method: put
    url: "https://example.com/items/{{id}}"
    headers: {Authorization: "Bearer $TOKEN"}
    body: {text: "{{draft.text}}", tags: [auto]}
    timeout: 5
    then: result
  result:
    action: output
    set: {action: posted, severity: "{{sev}}", score: 3, ok: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	draft := f.Node("draft").Action
	if draft.Prompt != "Reply to {{message}}" || draft.System == "" || draft.MaxTokens != 300 {
		t.Errorf("draft = %+v", draft)
	}
	if f.BackendFor(f.Node("draft")) != "claude" || f.BackendFor(f.Node("other")) != "local" {
		t.Errorf("writer backends: draft=%q other=%q", f.BackendFor(f.Node("draft")), f.BackendFor(f.Node("other")))
	}
	post := f.Node("post").Action
	if post.Method != "PUT" || post.Headers[0] != (KV{Key: "Authorization", Value: "Bearer $TOKEN"}) || post.Timeout != 5 {
		t.Errorf("post = %+v", post)
	}
	if body, ok := post.Body.(map[string]any); !ok || body["text"] != "{{draft.text}}" {
		t.Errorf("body = %#v", post.Body)
	}
	set := f.Node("result").Action.Set
	if len(set) != 4 || set[1].Value != "{{sev}}" || set[2].Literal != 3 || set[3].Literal != true || set[0].Literal != nil {
		t.Errorf("set = %+v", set)
	}
	if got := f.Inputs(); len(got) != 3 { // message, id, sev
		t.Errorf("inputs = %v", got)
	}
}

func TestParseActionErrors(t *testing.T) {
	cases := map[string]string{
		"llm without prompt":  "nodes: {a: {action: llm}}",
		"http without url":    "nodes: {a: {action: http}}",
		"bad method":          "nodes: {a: {action: http, url: x, method: FETCH}}",
		"empty output":        "nodes: {a: {action: output}}",
		"prompt on log":       "nodes: {a: {action: log, message: m, prompt: p}}",
		"url on bool":         "nodes: {a: {bool: q, url: x}}",
		"nested set value":    "nodes: {a: {action: output, set: {x: [1, 2]}}}",
		"unknown action type": "nodes: {a: {action: email}}",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
