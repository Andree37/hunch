package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseActions(t *testing.T) {
	f, err := ParseWithProject([]byte(`
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
    model: local
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
`), []byte(`
default: jev
writer: words
models:
  jev:   https://api.typesafe.ai/v1/systemone
  words: {url: https://api.anthropic.com/v1/messages, model: test-model}
  local: {url: http://localhost:11434/v1/chat/completions, model: llama3}
`))
	if err != nil {
		t.Fatal(err)
	}
	draft := f.Node("draft").Action
	if draft.Prompt != "Reply to {{message}}" || draft.System == "" || draft.MaxTokens != 300 {
		t.Errorf("draft = %+v", draft)
	}
	if f.BackendFor(f.Node("draft")) != "words" || f.BackendFor(f.Node("other")) != "local" {
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

func TestProjectModelsFromURLs(t *testing.T) {
	p, err := ParseProject([]byte(`
default: jev
models:
  jev:     {url: https://api.typesafe.ai/v1/systemone, key: tsk-1}
  laya:    http://localhost:8000/v1/systemone
  runware: {url: https://api.runware.ai/v1/decisions, key: rw, model: "runware:laya@1"}
  claude:  {url: https://api.anthropic.com/v1/messages, key: sk, model: claude-opus-5-5}
  llama:   {url: http://localhost:11434/v1/chat/completions, model: llama3.2}
  azure:   {url: https://team.openai.azure.com/openai/v1/chat/completions, key: az, model: gpt-5}
  nova:    {url: https://bedrock-runtime.us-east-1.amazonaws.com, model: amazon.nova-pro-v1:0}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"jev": "jev", "laya": "jev", "runware": "jev", "claude": "anthropic",
		"llama": "openai", "azure": "openai", "nova": "bedrock", "mock": "mock"}
	for name, kind := range want {
		if got := p.Models[name].Kind; got != kind {
			t.Errorf("%s: kind %q, want %q", name, got, kind)
		}
	}
	if o := p.Models["jev"].Options; o["api_key"] != "tsk-1" || o["url"] != "https://api.typesafe.ai/v1/systemone" {
		t.Errorf("jev options = %v", o)
	}
	if o := p.Models["claude"].Options; o["base_url"] != "https://api.anthropic.com/" {
		t.Errorf("claude base_url = %v", o["base_url"])
	}
	if o := p.Models["azure"].Options; o["key_header"] != "api-key" || o["endpoint"] == nil {
		t.Errorf("azure options = %v", o)
	}
	if o := p.Models["nova"].Options; o["region"] != "us-east-1" {
		t.Errorf("nova region = %v", o["region"])
	}
	if p.Default != "jev" || p.Order[len(p.Order)-1] != "mock" {
		t.Errorf("default %q, order %v", p.Default, p.Order)
	}

	t.Setenv("HUNCH_TEST_KEY", "from-env")
	p, err = ParseProject([]byte("models: {j: {url: http://x/v1/systemone, key: $HUNCH_TEST_KEY}}"))
	if err != nil || p.Models["j"].Options["api_key"] != "from-env" {
		t.Errorf("$ key: %v %v", err, p.Models["j"].Options)
	}
	for src, want := range map[string]string{
		"models: {x: http://localhost:11434}":                         "can't tell what http://localhost:11434 speaks",
		"models: {x: {key: k}}":                                       "needs a url",
		"models: {j: {url: http://x/v1/systemone, key: $NOPE_UNSET}}": "$NOPE_UNSET is not set",
		"default: nope\nmodels: {}":                                   "not one of the models",
	} {
		if _, err := ParseProject([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	if _, err := Parse([]byte("backends: {m: {kind: mock}}\nnodes: {a: {action: log, message: m}}")); err == nil || !strings.Contains(err.Error(), "hunch.yaml") {
		t.Errorf("backends in a flow: %v", err)
	}
}

func TestFindProjectWalksUp(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ProjectFile), []byte("models: {laya: http://localhost:8000/v1/systemone}"), 0o644)
	sub := filepath.Join(root, "flows", "deep")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "f.yaml"), []byte("nodes: {a: {bool: q, then: {_: b}}, b: {action: log, message: m}}"), 0o644)
	f, err := Load(filepath.Join(sub, "f.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Project == nil || f.DefaultBackend != "laya" || f.Backends["laya"].Kind != "jev" {
		t.Errorf("project = %+v", f.Project)
	}
}
