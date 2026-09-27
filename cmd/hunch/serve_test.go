package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
)

func testServer(t *testing.T, token string) *server {
	t.Helper()
	f, err := flow.Parse([]byte(`
inputs:
  severity: [sev1, sev2, sev3]
nodes:
  scope: {switch: "{{severity}}", then: {sev2: check, _: skip}}
  check: {bool: "Is {{ticket.title}} really {{severity}}?", then: {yes: keep, no: lower}}
  keep: {action: output, set: {action: keep}}
  lower: {action: output, set: {action: lower, id: "{{ticket.id}}"}}
  skip: {action: output, set: {action: skip}}
backends: {m: {kind: mock, answers: {check: no}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := backend.New(f.Backends["m"])
	return &server{flow: f, backends: map[string]backend.Backend{"m": b}, token: token, timeout: 5 * time.Second}
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
	code, reply := post(t, s, `{"severity": "sev2", "ticket": {"id": 42, "title": "Login broken"}}`, "")
	if code != 200 || reply.Outputs["action"] != "lower" || reply.Outputs["id"] != 42.0 {
		t.Fatalf("code=%d reply=%+v", code, reply)
	}
	if strings.Join(reply.Path, ",") != "scope,check,lower" {
		t.Errorf("path = %v", reply.Path)
	}
	code, reply = post(t, s, `{"severity": "sev1", "ticket": {"id": 1, "title": "x"}}`, "")
	if code != 200 || reply.Outputs["action"] != "skip" {
		t.Errorf("sev1: code=%d reply=%+v", code, reply)
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
		"bad choice":    {`{"severity": "sev9", "ticket": {}}`, 400, "sev9 is not one of"},
		"missing input": {`{"severity": "sev2"}`, 400, "missing input: ticket"},
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
	body := `{"severity": "sev1", "ticket": {}}`
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
	s.trace = &buf
	// check's question needs ticket.title; an empty ticket makes it fail there.
	code, reply := post(t, s, `{"severity": "sev2", "ticket": {}}`, "")
	if code != 500 || reply.Node != "check" || !strings.Contains(reply.Error, "ticket.title") {
		t.Errorf("code=%d reply=%+v", code, reply)
	}
	if !strings.Contains(buf.String(), `"node":"scope"`) {
		t.Errorf("trace = %q", buf.String())
	}
}
