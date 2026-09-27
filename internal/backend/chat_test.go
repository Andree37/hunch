package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Andree37/hunch/internal/flow"
)

var chatQuestions = []flow.Question{
	{ID: "n.bug", Name: "bug", Kind: flow.Bool, Text: "Is it a bug?", Criteria: map[string]string{"yes": "broken"}},
	{ID: "n.team", Name: "team", Kind: flow.Choice, Text: "Which team?", Options: []string{"web", "api"}},
	{ID: "n.urgency", Name: "urgency", Kind: flow.Score, Text: "How urgent?", Scale: flow.Scale{Min: 1, Max: 3}, Levels: []string{"later", "soon", "now"}},
}

// fakeOpenAI replies to every chat completion with reply, recording requests.
func fakeOpenAI(t *testing.T, reply string, got *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		*got = append(*got, body)
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 100},
		})
		w.Write(out)
	}))
}

func TestChatDecide(t *testing.T) {
	reply := "Sure!\n```json\n" + `{"answers": {
		"bug": {"probs": {"yes": 0.8, "no": 0.2}},
		"team": {"probs": {"web": 1, "api": 3}},
		"urgency": {"probs": {"1": 0, "2": 0.5, "3": 0.5}}
	}}` + "\n```"
	var got []map[string]any
	srv := fakeOpenAI(t, reply, &got)
	defer srv.Close()

	b, err := New(flow.BackendConfig{Name: "gpt", Kind: "openai", Options: map[string]any{
		"model": "some-model", "base_url": srv.URL + "/v1", "price_in_per_mtok": 1.0, "price_out_per_mtok": 10.0,
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.Decide(context.Background(), map[string]any{"report": "page is blank"}, chatQuestions)
	if err != nil {
		t.Fatal(err)
	}

	prompt := got[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	for _, want := range []string{`"report": "page is blank"`, "id: bug", "- yes: broken", "- 3: now"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	bug, team, urg := resp.Decisions[0], resp.Decisions[1], resp.Decisions[2]
	if bug.Answer != true || bug.Confidence != 0.8 || bug.QuestionID != "n.bug" {
		t.Errorf("bug = %+v", bug)
	}
	if team.Answer != "api" || team.Probs["api"] != 0.75 {
		t.Errorf("team = %+v (probs should be normalised)", team)
	}
	if v := urg.Answer.(float64); v != 2.5 {
		t.Errorf("urgency = %v, want expected value 2.5", v)
	}
	if resp.CostUSD != 0.002 {
		t.Errorf("cost = %v, want 0.002", resp.CostUSD)
	}
	if b.Caps().Calibrated {
		t.Error("chat backends self-report confidence; they must not claim calibration")
	}
}

func TestChatDecideBadReplies(t *testing.T) {
	cases := map[string]string{
		"no json":        "I think yes.",
		"missing answer": `{"answers": {}}`,
		"no probs":       `{"answers": {"bug": {"probs": {"maybe": 1}}}}`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			var got []map[string]any
			srv := fakeOpenAI(t, reply, &got)
			defer srv.Close()
			b, _ := New(flow.BackendConfig{Name: "gpt", Kind: "openai", Options: map[string]any{"model": "m", "base_url": srv.URL + "/v1"}})
			if _, err := b.Decide(context.Background(), nil, chatQuestions[:1]); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestChatWrite(t *testing.T) {
	var got []map[string]any
	srv := fakeOpenAI(t, "Hello there", &got)
	defer srv.Close()
	b, _ := New(flow.BackendConfig{Name: "gpt", Kind: "openai", Options: map[string]any{"model": "m", "base_url": srv.URL + "/v1"}})
	resp, err := b.(Writer).Write(context.Background(), WriteRequest{System: "be kind", Prompt: "greet", MaxTokens: 50})
	if err != nil || resp.Text != "Hello there" {
		t.Fatalf("write = %+v, %v", resp, err)
	}
	msgs := got[0]["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "be kind" || got[0]["max_tokens"] != 50.0 {
		t.Errorf("request = %v", got[0])
	}
}

func TestAnthropicWrite(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "sk-test" {
			t.Errorf("request %s key=%q", r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
			"content":[{"type":"text","text":"Hi from Claude"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()
	t.Setenv("HUNCH_TEST_ANTHROPIC", "sk-test")

	b, err := New(flow.BackendConfig{Name: "claude", Kind: "anthropic", Options: map[string]any{
		"model": "claude-opus-5", "base_url": srv.URL, "api_key_env": "HUNCH_TEST_ANTHROPIC",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.(Writer).Write(context.Background(), WriteRequest{System: "sys", Prompt: "hi"})
	if err != nil || resp.Text != "Hi from Claude" {
		t.Fatalf("write = %+v, %v", resp, err)
	}
	if body["model"] != "claude-opus-5" || body["system"] == nil {
		t.Errorf("body = %v", body)
	}
}

func TestAnthropicRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],
			"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},"usage":{"input_tokens":1,"output_tokens":0}}`)
	}))
	defer srv.Close()
	t.Setenv("HUNCH_TEST_ANTHROPIC", "sk-test")
	b, _ := New(flow.BackendConfig{Name: "claude", Kind: "anthropic", Options: map[string]any{
		"model": "claude-opus-5", "base_url": srv.URL, "api_key_env": "HUNCH_TEST_ANTHROPIC",
	}})
	_, err := b.(Writer).Write(context.Background(), WriteRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("err = %v", err)
	}
}

func TestNewRequiresModel(t *testing.T) {
	for _, kind := range []string{"openai", "anthropic", "bedrock"} {
		if _, err := New(flow.BackendConfig{Name: "x", Kind: kind}); err == nil || !strings.Contains(err.Error(), "model is required") {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
	if !CanWrite("bedrock") || CanWrite("jev") {
		t.Error("CanWrite disagrees")
	}
}

func TestMockWrites(t *testing.T) {
	m, _ := NewMock(flow.BackendConfig{Name: "mock"})
	a, _ := m.Write(context.Background(), WriteRequest{Prompt: "Draft a reply\nmore"})
	b, _ := m.Write(context.Background(), WriteRequest{Prompt: "Draft a reply\nmore"})
	if a.Text != b.Text || !strings.HasSuffix(a.Text, "Draft a reply") {
		t.Errorf("mock write = %q / %q", a.Text, b.Text)
	}
}

func TestChatDecideSendsSchema(t *testing.T) {
	var got []map[string]any
	srv := fakeOpenAI(t, `{"answers": {"bug": {"probs": {"yes": 1, "no": 0}}}}`, &got)
	defer srv.Close()
	b, _ := New(flow.BackendConfig{Name: "gpt", Kind: "openai", Options: map[string]any{"model": "m", "base_url": srv.URL + "/v1"}})
	if _, err := b.Decide(context.Background(), nil, chatQuestions[:1]); err != nil {
		t.Fatal(err)
	}
	rf, ok := got[0]["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v", got[0]["response_format"])
	}
	schema, _ := json.Marshal(rf["json_schema"])
	if !strings.Contains(string(schema), `"required":["no","yes"]`) || !strings.Contains(string(schema), `"strict":true`) {
		t.Errorf("schema = %s", schema)
	}

	// Writing never sends a schema, and structured: false turns it off.
	b.(Writer).Write(context.Background(), WriteRequest{Prompt: "hi"})
	if _, ok := got[1]["response_format"]; ok {
		t.Error("writes must not send a schema")
	}
	off, _ := New(flow.BackendConfig{Name: "gpt", Kind: "openai", Options: map[string]any{"model": "m", "base_url": srv.URL + "/v1", "structured": false}})
	off.Decide(context.Background(), nil, chatQuestions[:1])
	if _, ok := got[2]["response_format"]; ok {
		t.Error("structured: false should not send a schema")
	}
}

func TestParseDecisionsIsLenient(t *testing.T) {
	cases := map[string]string{
		"strings for numbers":  `{"answers": {"bug": {"probs": {"yes": "0.9", "no": "0.1"}}}}`,
		"answer + confidence":  `{"answers": {"bug": {"answer": "yes", "confidence": 0.9}}}`,
		"text around the JSON": "Here you go:\n{\"answers\": {\"bug\": {\"probs\": {\"yes\": 0.9, \"no\": 0.1}}}} Hope that helps!",
		"a second object":      `{"answers": {"bug": {"probs": {"yes": 0.9, "no": 0.1}}}}, {"extra": true}`,
	}
	for name, reply := range cases {
		decs, err := parseDecisions(reply, chatQuestions[:1])
		if err != nil || decs[0].Answer != true || decs[0].Probs["yes"] < 0.89 {
			t.Errorf("%s: %+v, %v", name, decs, err)
		}
	}
}

func TestChatDecideRetriesOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		reply := `not json`
		if calls == 2 {
			reply = `{"answers": {"bug": {"probs": {"yes": 1, "no": 0}}}}`
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply}}}})
		w.Write(out)
	}))
	defer srv.Close()
	b, _ := New(flow.BackendConfig{Name: "gpt", Kind: "openai", Options: map[string]any{"model": "m", "base_url": srv.URL + "/v1"}})
	var notes []string
	ctx := WithStatus(context.Background(), func(s string) { notes = append(notes, s) })
	if _, err := b.Decide(ctx, nil, chatQuestions[:1]); err != nil || calls != 2 || len(notes) != 1 {
		t.Errorf("err=%v calls=%d notes=%q", err, calls, notes)
	}
}
