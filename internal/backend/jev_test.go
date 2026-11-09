package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Andree37/hunch/internal/flow"
)

// Response body from OpenRouter's Jev tutorial.
const tutorialResponse = `{
  "id": "gen-dec-1790015143-AIaTutprXsJ5EwohRSjb",
  "model": "typesafe/jev-1.13-20260917",
  "provider": "TypeSafe",
  "answers": {
    "is_bug": { "type": "noul", "noul": 0.96 },
    "team": {
      "type": "choice",
      "choice": "payments",
      "confidence": 0.67,
      "probabilities": { "payments": 0.78, "frontend": 0.22, "account": 0 }
    },
    "urgency": {
      "type": "score",
      "score": 1.99,
      "confidence": 0.99,
      "probabilities": { "0": 0, "1": 0, "2": 1 },
      "legend": { "0": "Can wait", "1": "This week", "2": "Now" }
    }
  },
  "usage": { "input_tokens": 476, "output_tokens": 70, "cost": 0.000019992 }
}`

var tutorialQuestions = []flow.Question{
	{ID: "t.is_bug", Name: "is_bug", Kind: flow.Bool, Text: "Is this a defect?",
		Criteria: map[string]string{"yes": "broken behavior", "no": "a question"}},
	{ID: "t.team", Name: "team", Kind: flow.Choice, Text: "Which team?",
		Options: []string{"payments", "frontend", "account"}, Criteria: map[string]string{"payments": "billing"}},
	{ID: "t.urgency", Name: "urgency", Kind: flow.Score, Text: "How urgent?", Scale: flow.Scale{Min: 1, Max: 3}},
}

func newTestJev(t *testing.T, url string, opts map[string]any) *Jev {
	t.Helper()
	if opts == nil {
		opts = map[string]any{}
	}
	opts["url"] = url
	opts["api_key"] = "sk-test"
	j, err := NewJev(flow.BackendConfig{Name: "jev", Kind: "jev", Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	j.backoff = time.Millisecond
	return j
}

func TestJevRequestAndResponse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("auth header = %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, tutorialResponse)
	}))
	defer srv.Close()

	j := newTestJev(t, srv.URL, map[string]any{"model": "typesafe/jev-1.13"})
	resp, err := j.Decide(context.Background(), map[string]any{"ticket": "blank checkout"}, tutorialQuestions)
	if err != nil {
		t.Fatal(err)
	}

	// Request shape.
	if got["model"] != "typesafe/jev-1.13" {
		t.Errorf("model = %v", got["model"])
	}
	if got["state"].(map[string]any)["ticket"] != "blank checkout" {
		t.Errorf("state = %v", got["state"])
	}
	qs := got["questions"].(map[string]any)
	bug := qs["is_bug"].(map[string]any)
	if bug["type"] != "noul" || bug["criteria"].(map[string]any)["true"] != "broken behavior" {
		t.Errorf("is_bug = %v", bug)
	}
	team := qs["team"].(map[string]any)["criteria"].(map[string]any)
	if team["payments"] != "billing" || team["account"] != "account" {
		t.Errorf("team criteria = %v", team)
	}
	levels := qs["urgency"].(map[string]any)["criteria"].([]any)
	if len(levels) != 3 || levels[0] != "1 (lowest)" || levels[2] != "3 (highest)" {
		t.Errorf("urgency levels = %v", levels)
	}

	// Response mapping.
	if resp.CostUSD != 0.000019992 {
		t.Errorf("cost = %v", resp.CostUSD)
	}
	bugD, teamD, urgD := resp.Decisions[0], resp.Decisions[1], resp.Decisions[2]
	if bugD.Answer != true || bugD.Confidence != 0.96 || bugD.QuestionID != "t.is_bug" {
		t.Errorf("is_bug = %+v", bugD)
	}
	if teamD.Answer != "payments" || teamD.Confidence != 0.67 {
		t.Errorf("team = %+v", teamD)
	}
	// Jev's 0-based 1.99 on a 1-3 scale is 2.99, and its level "2" is our 3.
	if v := urgD.Answer.(float64); v < 2.989 || v > 2.991 || urgD.Probs["3"] != 1 {
		t.Errorf("urgency = %+v", urgD)
	}
}

func TestJevRetriesOverload(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(529)
			return
		}
		io.WriteString(w, `{"answers": {"is_bug": {"type": "noul", "noul": 0.2}}, "usage": {"input_tokens": 1000}}`)
	}))
	defer srv.Close()

	j := newTestJev(t, srv.URL, map[string]any{"price_per_mtok": 0.042})
	var notes []string
	ctx := WithStatus(context.Background(), func(s string) { notes = append(notes, s) })
	resp, err := j.Decide(ctx, nil, tutorialQuestions[:1])
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "Jev overloaded (529), retrying in 1ms (attempt 2/4)") {
		t.Errorf("status notes = %q", notes)
	}
	if resp.Decisions[0].Answer != false {
		t.Errorf("answer = %v", resp.Decisions[0].Answer)
	}
	// No reported cost, so it's estimated from input tokens.
	if resp.CostUSD < 0.0000419 || resp.CostUSD > 0.0000421 {
		t.Errorf("estimated cost = %v", resp.CostUSD)
	}
}

func TestJevDoesNotRetryValidationErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(422)
		io.WriteString(w, `{"error": "criteria required"}`)
	}))
	defer srv.Close()

	_, err := newTestJev(t, srv.URL, nil).Decide(context.Background(), nil, tutorialQuestions[:1])
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "criteria required") {
		t.Errorf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestJevBadAnswers(t *testing.T) {
	cases := map[string]string{
		"missing answer": `{"answers": {}}`,
		"unknown option": `{"answers": {"team": {"type": "choice", "choice": "legal", "probabilities": {}}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer srv.Close()
			if _, err := newTestJev(t, srv.URL, nil).Decide(context.Background(), nil, tutorialQuestions[1:2]); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// Laya served locally by laya-serve: same API, no key, extra fields ignored.
func TestLayaLocal(t *testing.T) {
	var auth string
	var asked map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&asked)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model": "laya-rl-agent", "answers": {
			"queue": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.9519, "tech": 0.0327, "other": 0.0154},
				"confidence": 0.797, "answer_confidence": 0.9519, "action": {"act_probability": 1.0}, "x_jev_confidence": 0.9278},
			"urgency": {"type": "score", "score": 1.6994, "legend": {"0": "calm", "1": "firm", "2": "angry", "3": "furious"},
				"probabilities": {"0": 0.1, "1": 0.2, "2": 0.4136, "3": 0.2864}, "confidence": 0.1925},
			"refund": {"type": "noul", "noul": 0.91}},
			"usage": {"input_tokens": 83, "output_tokens": 0}, "routing": {"model": "english"}}`)
	}))
	defer srv.Close()
	b, err := New(flow.BackendConfig{Name: "laya", Kind: "jev", Options: map[string]any{"url": srv.URL + "/v1/systemone", "model": "convaiinnovations/laya"}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := flow.Parse([]byte(`nodes:
  q:
    questions:
      queue: {choice: "Which team?", options: [billing, tech, other]}
      urgency: {score: "How urgent?", levels: [calm, firm, angry, furious]}
      refund: {bool: "Wants a refund?"}
    then: done
  done: {action: log, message: ok}
`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.Decide(context.Background(), map[string]any{"message": "charged twice"}, f.Node("q").Questions)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" || asked["model"] != "convaiinnovations/laya" {
		t.Errorf("auth=%q model=%v", auth, asked["model"])
	}
	d := resp.Decisions
	if d[0].Answer != "billing" || d[0].Confidence != 0.9278 { // x_jev_confidence, on Jev's scale
		t.Errorf("choice = %+v", d[0])
	}
	if d[1].Answer.(float64) < 2.69 || d[1].Answer.(float64) > 2.71 || d[1].Confidence != 0.1925 {
		t.Errorf("score = %+v", d[1])
	}
	if d[2].Answer != true || d[2].Confidence < 0.9 {
		t.Errorf("bool = %+v", d[2])
	}
}

func TestJevDefaultsByHost(t *testing.T) {
	for url, want := range map[string]string{
		"https://api.typesafe.ai/v1/systemone":      "jev-latest",
		"https://openrouter.ai/api/alpha/decisions": "typesafe/jev-1.13",
		"http://localhost:8000/v1/systemone":        "",
	} {
		j, err := NewJev(flow.BackendConfig{Name: "j", Options: map[string]any{"url": url}})
		if err != nil || j.model != want {
			t.Errorf("%s: model %q, %v", url, j.model, err)
		}
		if (j.pricePerTok > 0) != (want != "") {
			t.Errorf("%s: price %v", url, j.pricePerTok)
		}
	}
}

func TestKeyFromAVariable(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"answers": {"is_bug": {"noul": 0.9}}}`)
	}))
	defer srv.Close()
	cfg := flow.BackendConfig{Name: "j", Kind: "jev", Options: map[string]any{"url": srv.URL, "api_key_env": "HUNCH_TEST_LATE_KEY"}}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "$HUNCH_TEST_LATE_KEY is not set") {
		t.Errorf("unset: err = %v", err)
	}
	t.Setenv("HUNCH_TEST_LATE_KEY", "sk-late")
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decide(context.Background(), nil, tutorialQuestions[:1]); err != nil || auth != "Bearer sk-late" {
		t.Errorf("set: err=%v auth=%q", err, auth)
	}
}

func TestForFlowBuildsOnlyTheModelsUsed(t *testing.T) {
	f, err := flow.ParseWithProject([]byte("nodes: {a: {bool: q, then: {_: b}}, b: {action: log, message: m}}"), []byte(`
default: mock
models:
  jev:  {url: https://api.typesafe.ai/v1/systemone, key: $HUNCH_TEST_NEVER_SET}
  nova: {url: https://bedrock-runtime.us-east-1.amazonaws.com, model: x, profile: hunch-test-no-such-profile}
`))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := ForFlow(f)
	if err != nil || len(bs) != 1 || bs["mock"] == nil {
		t.Fatalf("a flow on mock shouldn't need jev's key or nova's profile: %v %v", bs, err)
	}
	f.DefaultBackend = "jev"
	if _, err := ForFlow(f); err == nil || !strings.Contains(err.Error(), "$HUNCH_TEST_NEVER_SET is not set") {
		t.Errorf("a flow on jev needs its key, before it runs: %v", err)
	}
}
