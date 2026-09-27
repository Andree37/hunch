package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/trace"
)

func testServer(t *testing.T, token string) *server {
	t.Helper()
	f, err := flow.Parse([]byte(`
inputs:
  plan: [free, pro, team]
nodes:
  scope: {switch: "{{plan}}", then: {pro: check, _: skip}}
  check: {bool: "Is {{order.note}} asking for a refund?", then: {yes: refund, no: keep}}
  keep: {action: output, set: {action: keep}}
  refund: {action: output, set: {action: refund, id: "{{order.id}}"}}
  skip: {action: output, set: {action: skip}}
backends: {m: {kind: mock, answers: {check: yes}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := backend.New(f.Backends["m"])
	return &server{flow: f, backends: map[string]backend.Backend{"m": b}, token: token, timeout: 5 * time.Second,
		seen: newDedupe(time.Hour), slots: make(chan struct{}, 4)}
}

func post(t *testing.T, s *server, body, auth string) (int, runReply) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var reply runReply
	json.Unmarshal(rec.Body.Bytes(), &reply)
	return rec.Code, reply
}

func TestServeRunsFlowFromWebhook(t *testing.T) {
	s := testServer(t, "")
	code, reply := post(t, s, `{"plan": "pro", "order": {"id": 42, "note": "please refund me"}}`, "")
	if code != 200 || reply.Outputs["action"] != "refund" || reply.Outputs["id"] != 42.0 {
		t.Fatalf("code=%d reply=%+v", code, reply)
	}
	if strings.Join(reply.Path, ",") != "scope,check,refund" {
		t.Errorf("path = %v", reply.Path)
	}
	code, reply = post(t, s, `{"plan": "free", "order": {"id": 1, "note": "x"}}`, "")
	if code != 200 || reply.Outputs["action"] != "skip" {
		t.Errorf("free plan: code=%d reply=%+v", code, reply)
	}
}

func TestServeRejectsBadRequests(t *testing.T) {
	s := testServer(t, "")
	cases := map[string]struct {
		body string
		code int
		msg  string
	}{
		"not json":      {`nope`, 400, "JSON object"},
		"bad choice":    {`{"plan": "gold", "order": {}}`, 400, "gold is not one of"},
		"missing input": {`{"plan": "pro"}`, 400, "missing input: order"},
	}
	for name, c := range cases {
		code, reply := post(t, s, c.body, "")
		if code != c.code || !strings.Contains(reply.Error, c.msg) {
			t.Errorf("%s: code=%d error=%q", name, code, reply.Error)
		}
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET / = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz = %d", rec.Code)
	}
}

func TestServeToken(t *testing.T) {
	s := testServer(t, "s3cret")
	body := `{"plan": "free", "order": {}}`
	if code, _ := post(t, s, body, ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := post(t, s, body, "Bearer wrong"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, _ := post(t, s, body, "Bearer s3cret"); code != 200 {
		t.Errorf("right token: %d", code)
	}
}

func TestServeReportsFailedNode(t *testing.T) {
	s := testServer(t, "")
	var buf bytes.Buffer
	s.trace = trace.NewWriter(&buf)
	// check's question needs order.note; an empty order makes it fail there.
	code, reply := post(t, s, `{"plan": "pro", "order": {}}`, "")
	if code != 500 || reply.Node != "check" || !strings.Contains(reply.Error, "order.note") {
		t.Errorf("code=%d reply=%+v", code, reply)
	}
	if !strings.Contains(buf.String(), `"node":"scope"`) {
		t.Errorf("trace = %q", buf.String())
	}
}

// runs counts how many times the flow started, from the trace.
func runs(buf *bytes.Buffer) int { return strings.Count(buf.String(), `"node":"scope"`) }

func TestServeDedupe(t *testing.T) {
	s := testServer(t, "")
	s.dedupeKey = "{{order.id}}"
	var buf bytes.Buffer
	s.trace = trace.NewWriter(&buf)

	body := `{"plan": "pro", "order": {"id": 42, "note": "refund please"}}`
	code1, first := post(t, s, body, "")
	code2, second := post(t, s, body, "")
	if code1 != 200 || code2 != 200 || runs(&buf) != 1 {
		t.Fatalf("codes %d/%d, runs %d; want one run", code1, code2, runs(&buf))
	}
	if !second.Duplicate || second.Outputs["action"] != first.Outputs["action"] {
		t.Errorf("duplicate reply = %+v", second)
	}
	// A different record runs.
	post(t, s, `{"plan": "pro", "order": {"id": 43, "note": "x"}}`, "")
	if runs(&buf) != 2 {
		t.Errorf("runs = %d, want 2", runs(&buf))
	}
}

func TestServeIdempotencyKeyHeader(t *testing.T) {
	s := testServer(t, "")
	var buf bytes.Buffer
	s.trace = trace.NewWriter(&buf)
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"plan": "free", "order": {}}`))
		req.Header.Set("Idempotency-Key", "evt-1")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}
	if send() != 200 || send() != 200 || runs(&buf) != 1 {
		t.Errorf("runs = %d, want 1", runs(&buf))
	}
}

func TestServeFailedRunCanBeRetried(t *testing.T) {
	s := testServer(t, "")
	s.dedupeKey = "{{order.id}}"
	var buf bytes.Buffer
	s.trace = trace.NewWriter(&buf)
	body := `{"plan": "pro", "order": {"id": 9}}` // no note: fails at check
	c1, _ := post(t, s, body, "")
	c2, _ := post(t, s, body, "")
	if c1 != 500 || c2 != 500 || runs(&buf) != 2 {
		t.Errorf("codes %d/%d runs %d; a failed run must run again", c1, c2, runs(&buf))
	}
}

func TestServeInProgressAndBusy(t *testing.T) {
	s := testServer(t, "")
	s.dedupeKey = "{{order.id}}"
	s.seen.begin("5") // as if a run for order 5 were in flight
	if code, reply := post(t, s, `{"plan": "free", "order": {"id": 5}}`, ""); code != 409 || !reply.Duplicate {
		t.Errorf("in progress: code=%d reply=%+v", code, reply)
	}

	s.slots = make(chan struct{}, 1)
	s.slots <- struct{}{} // every slot taken
	s.busyWait = 10 * time.Millisecond
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"plan": "free", "order": {"id": 6}}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy: code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if _, running, _ := s.seen.begin("6"); running {
		t.Error("a request turned away as busy must not stay claimed")
	}
}

func TestServeReloadsFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.yaml")
	write := func(action string) {
		os.WriteFile(path, []byte(`nodes: {done: {action: output, set: {action: `+action+`}}}`), 0o644)
	}
	write("first")
	s := &server{path: path, seen: newDedupe(time.Hour), slots: make(chan struct{}, 1), timeout: time.Second}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if _, r := post(t, s, `{}`, ""); r.Outputs["action"] != "first" {
		t.Fatalf("first = %+v", r)
	}

	time.Sleep(10 * time.Millisecond)
	write("second")
	s.checked = time.Time{} // skip the once-a-second throttle
	if _, r := post(t, s, `{}`, ""); r.Outputs["action"] != "second" {
		t.Errorf("after edit = %+v", r)
	}

	time.Sleep(10 * time.Millisecond)
	os.WriteFile(path, []byte(`nodes: {broken`), 0o644)
	s.checked = time.Time{}
	if code, r := post(t, s, `{}`, ""); code != 200 || r.Outputs["action"] != "second" {
		t.Errorf("broken edit should keep the last good flow: %d %+v", code, r)
	}
}
