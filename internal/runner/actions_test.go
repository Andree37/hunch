package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
)

func runFlow(t *testing.T, src string, state map[string]any, dry bool) (*Result, error) {
	t.Helper()
	f, err := flow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		backends[name] = b
	}
	return Run(context.Background(), f, state, Options{Backends: backends, DryRun: dry})
}

func TestLLMAndOutput(t *testing.T) {
	res, err := runFlow(t, `
backends: {m: {kind: mock, answers: {ok: yes}}}
nodes:
  draft: {action: llm, prompt: "Reply to {{who}}", then: ok}
  ok: {bool: "Good enough? {{draft.text}}", then: {yes: send, no: hold}}
  send: {action: output, set: {action: send, text: "{{draft.text}}", n: "{{count}}", fixed: 2}}
  hold: {action: output, set: {action: hold}}
`, map[string]any{"who": "ann", "count": 3.0}, false)
	if err != nil {
		t.Fatal(err)
	}
	text := res.State["draft"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Reply to ann") {
		t.Errorf("draft text = %q", text)
	}
	want := map[string]any{"action": "send", "text": text, "n": 3.0, "fixed": 2}
	for k, v := range want {
		if res.Outputs[k] != v {
			t.Errorf("output %s = %#v, want %#v", k, res.Outputs[k], v)
		}
	}
}

func TestLLMNeedsAWriter(t *testing.T) {
	_, err := runFlow(t, `
backends: {j: {kind: jev}}
nodes: {draft: {action: llm, prompt: hi}}
`, nil, false)
	if err == nil || !strings.Contains(err.Error(), "can't write text") {
		t.Errorf("err = %v", err)
	}
}

func TestHTTPLive(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id": 7}`)
	}))
	defer srv.Close()
	t.Setenv("HUNCH_TEST_TOKEN", "secret")

	res, err := runFlow(t, `
nodes:
  post:
    action: http
    url: "{{base}}/items/{{name}}?q={{q}}"
    headers: {Authorization: "Bearer $HUNCH_TEST_TOKEN"}
    body: {name: "{{name}}", count: "{{n}}", tags: [a, "{{q}}"]}
`, map[string]any{"base": srv.URL, "name": "a b/c", "q": "x&y", "n": 2.0}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/items/a%20b%2Fc" || gotQuery != "q=x%26y" || gotAuth != "Bearer secret" {
		t.Errorf("request path=%q query=%q auth=%q", gotPath, gotQuery, gotAuth)
	}
	if gotBody["name"] != "a b/c" || gotBody["count"] != 2.0 || gotBody["tags"].([]any)[1] != "x&y" {
		t.Errorf("body = %v", gotBody)
	}
	out := res.State["post"].(map[string]any)
	if out["status"] != 200 || out["body"].(map[string]any)["id"] != 7.0 {
		t.Errorf("output = %v", out)
	}
}

func TestHTTPDryRunSendsNothing(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	res, err := runFlow(t, `nodes: {post: {action: http, url: "{{base}}/x", body: "hi {{who}}"}}`,
		map[string]any{"base": srv.URL, "who": "ann"}, true)
	if err != nil {
		t.Fatal(err)
	}
	out := res.State["post"].(map[string]any)
	if called || out["dry_run"] != true || out["body"] != "hi ann" || out["method"] != "POST" {
		t.Errorf("dry run: called=%v output=%v", called, out)
	}
}

func TestHTTPErrorStopsRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "boom")
	}))
	defer srv.Close()
	res, err := runFlow(t, `nodes: {post: {action: http, url: "{{base}}", then: after}, after: {action: log, message: no}}`,
		map[string]any{"base": srv.URL}, false)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500: boom") || len(res.Path) != 1 {
		t.Errorf("err = %v, path = %v", err, res.Path)
	}
}

func TestShellTimeout(t *testing.T) {
	_, err := runFlow(t, `nodes: {slow: {action: shell, run: "sleep 5", timeout: 0.2}}`, nil, false)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
}

func TestDryRunStillReads(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status": "open", "name": "Widget"}`)
	}))
	defer srv.Close()
	res, err := runFlow(t, `
nodes:
  fetch: {action: http, method: GET, url: "{{base}}/items/{{id}}", then: update}
  update: {action: http, method: PATCH, url: "{{base}}/items/{{id}}", body: {status: closed, was: "{{fetch.body.status}}"}}
`, map[string]any{"base": srv.URL, "id": 42.0}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || methods[0] != "GET" {
		t.Errorf("server saw %v, want only the GET", methods)
	}
	upd := res.State["update"].(map[string]any)
	if upd["dry_run"] != true || !strings.Contains(upd["body"].(string), `"was":"open"`) {
		t.Errorf("update = %v", upd)
	}
}

func TestOutputsAndTemplatedCriteria(t *testing.T) {
	res, err := runFlow(t, `
state:
  sizes: {big: "Over a kilogram", small: "Fits in an envelope"}
backends: {m: {kind: mock, answers: {a: small, b: small}}}
nodes:
  scope: {switch: "{{sev}}", then: {x: a, _: b}}
  a:
    choice: "Which severity?"
    options: {big: "{{sizes.big}}", small: "{{sizes.small}}"}
    then: {_: set_a}
  b: {choice: "Which?", options: [big, small], then: {_: set_b}}
  set_a: {action: output, set: {new: "{{a.answer}}"}, then: explain}
  set_b: {action: output, set: {new: "{{b.answer}}"}, then: explain}
  explain: {action: log, message: "packing as {{outputs.new}}"}
`, map[string]any{"sev": "x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if msg := res.State["explain"].(map[string]any)["message"]; msg != "packing as small" {
		t.Errorf("explain = %q", msg)
	}
}

func TestHTTPRetries(t *testing.T) {
	retryWait = time.Millisecond
	cases := []struct {
		name     string
		method   string
		extra    string
		statuses []int // server replies, in order; then 200
		wantHits int
		wantErr  bool
	}{
		{"GET retries a 500", "GET", "", []int{500, 502}, 3, false},
		{"POST retries a 503", "POST", "", []int{503}, 2, false},
		{"POST retries a 429", "POST", "", []int{429, 429}, 3, false},
		{"POST won't repeat a 500", "POST", "", []int{500}, 1, true},
		{"POST with an idempotency key retries a 500", "POST", `, idempotency_key: "order-{{id}}"`, []int{500}, 2, false},
		{"retries: 0 turns it off", "GET", ", retries: 0", []int{500}, 1, true},
		{"gives up after the retries", "GET", ", retries: 1", []int{500, 500, 500}, 2, true},
		{"4xx is not retried", "GET", "", []int{404}, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits := 0
			var keys []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				if hits < len(c.statuses) {
					w.WriteHeader(c.statuses[hits])
				}
				hits++
			}))
			defer srv.Close()
			src := `nodes: {call: {action: http, method: ` + c.method + `, url: "{{base}}"` + c.extra + `}}`
			_, err := runFlow(t, src, map[string]any{"base": srv.URL, "id": 7.0}, false)
			if hits != c.wantHits || (err != nil) != c.wantErr {
				t.Errorf("hits=%d err=%v; want hits=%d err=%v", hits, err, c.wantHits, c.wantErr)
			}
			if strings.Contains(c.extra, "idempotency") && keys[0] != "order-7" {
				t.Errorf("Idempotency-Key = %q", keys[0])
			}
		})
	}
}

func TestHTTPRetryReportsStatus(t *testing.T) {
	retryWait = time.Millisecond
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits++; hits == 1 {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	f, _ := flow.Parse([]byte(`nodes: {call: {action: http, url: "{{base}}"}}`))
	var notes []string
	ctx := backend.WithStatus(context.Background(), func(s string) { notes = append(notes, s) })
	if _, err := Run(ctx, f, map[string]any{"base": srv.URL}, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "POST call: HTTP 503, retrying") {
		t.Errorf("notes = %q", notes)
	}
}
