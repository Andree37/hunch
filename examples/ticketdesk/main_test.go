package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func do(t *testing.T, h http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDesk(t *testing.T) {
	hooks := make(chan string, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hooks <- r.Header.Get("Idempotency-Key") + " " + string(body)
	}))
	defer hook.Close()
	d := &desk{tickets: map[int]*Ticket{}, next: 1, webhook: hook.URL, seen: map[string]bool{}}

	if rec := do(t, d, "POST", "/tickets", `{"severity":"sev2","title":"Blurry logo"}`); rec.Code != 201 {
		t.Fatalf("create: %d", rec.Code)
	}
	select {
	case got := <-hooks:
		if !strings.Contains(got, "ticket-created-1") || !strings.Contains(got, `"ticket_id":1`) {
			t.Errorf("webhook = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no webhook sent")
	}

	// A repeated write with the same Idempotency-Key applies once.
	for i := 0; i < 2; i++ {
		do(t, d, "PATCH", "/tickets/1", `{"severity":"sev3"}`, "Idempotency-Key", "k1")
		do(t, d, "POST", "/tickets/1/comments", `{"body":"why"}`, "Idempotency-Key", "k2")
	}
	var tk Ticket
	json.Unmarshal(do(t, d, "GET", "/tickets/1", "").Body.Bytes(), &tk)
	if tk.Severity != "sev3" || len(tk.Comments) != 1 || len(tk.History) != 3 {
		t.Errorf("ticket = %+v", tk)
	}
}

func TestDeskToken(t *testing.T) {
	d := &desk{tickets: map[int]*Ticket{}, next: 1, token: "s", seen: map[string]bool{}}
	if rec := do(t, d, "GET", "/tickets", ""); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := do(t, d, "GET", "/tickets", "", "Authorization", "Bearer s"); rec.Code != 200 {
		t.Errorf("with token: %d", rec.Code)
	}
}
