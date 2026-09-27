// Command ticketdesk is a tiny ticket system for trying hunch locally. It
// keeps tickets in memory, sends a webhook to hunch when one is created, and
// accepts the severity changes and comments hunch makes, keeping a history
// so you can see what happened.
//
//	go run ./examples/ticketdesk --webhook http://127.0.0.1:8080/
//	curl -d '{"severity":"sev2","title":"...","description":"...","request":"..."}' http://127.0.0.1:8090/tickets
//	curl http://127.0.0.1:8090/tickets/1
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Ticket struct {
	ID          int       `json:"id"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Request     string    `json:"request"`
	Comments    []Comment `json:"comments"`
	History     []string  `json:"history"`
}

type Comment struct {
	At   time.Time `json:"at"`
	Body string    `json:"body"`
}

type desk struct {
	mu      sync.Mutex
	tickets map[int]*Ticket
	next    int
	webhook string
	token   string // bearer token for the webhook, and required from callers if set
	seen    map[string]bool
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "address to listen on")
	webhook := flag.String("webhook", "", "URL to POST {\"ticket_id\": n} to when a ticket is created")
	tokenEnv := flag.String("token-env", "", "env var holding a bearer token: sent with webhooks and required on the API")
	flag.Parse()
	d := &desk{tickets: map[int]*Ticket{}, next: 1, webhook: *webhook, seen: map[string]bool{}}
	if *tokenEnv != "" {
		d.token = os.Getenv(*tokenEnv)
	}
	log.Printf("ticketdesk on http://%s (webhook: %s)", *addr, orNone(*webhook))
	log.Fatal(http.ListenAndServe(*addr, d))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (d *desk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.token != "" && r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "tickets" && r.Method == http.MethodGet:
		d.list(w)
	case len(parts) == 1 && parts[0] == "tickets" && r.Method == http.MethodPost:
		d.create(w, r)
	case len(parts) >= 2 && parts[0] == "tickets":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case len(parts) == 2 && r.Method == http.MethodGet:
			d.get(w, id)
		case len(parts) == 2 && r.Method == http.MethodPatch:
			d.update(w, r, id)
		case len(parts) == 3 && parts[2] == "comments" && r.Method == http.MethodPost:
			d.comment(w, r, id)
		default:
			http.Error(w, "not supported", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func (d *desk) list(w http.ResponseWriter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*Ticket
	for _, t := range d.tickets {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *Ticket) int { return a.ID - b.ID })
	writeJSON(w, http.StatusOK, out)
}

func (d *desk) create(w http.ResponseWriter, r *http.Request) {
	var t Ticket
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.Title == "" || t.Severity == "" {
		http.Error(w, "want JSON with at least severity and title", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	t.ID, d.next = d.next, d.next+1
	t.Comments = []Comment{}
	t.History = []string{fmt.Sprintf("created as %s", t.Severity)}
	d.tickets[t.ID] = &t
	d.mu.Unlock()
	log.Printf("#%d created: %s [%s]", t.ID, t.Title, t.Severity)
	writeJSON(w, http.StatusCreated, t)
	if d.webhook != "" {
		go d.notify(t.ID)
	}
}

// notify tells hunch about a new ticket, retrying like a real webhook sender.
func (d *desk) notify(id int) {
	body, _ := json.Marshal(map[string]any{"ticket_id": id})
	for attempt := 1; attempt <= 3; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, d.webhook, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("ticket-created-%d", id))
		if d.token != "" {
			req.Header.Set("Authorization", "Bearer "+d.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			reply, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Printf("#%d webhook → %d %s", id, resp.StatusCode, strings.TrimSpace(string(reply)))
			if resp.StatusCode < 500 {
				return
			}
		} else {
			log.Printf("#%d webhook failed: %v", id, err)
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
}

func (d *desk) get(w http.ResponseWriter, id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tickets[id]
	if !ok {
		http.Error(w, "no such ticket", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// seenKey reports whether an Idempotency-Key was used before, so a retried
// write isn't applied twice.
func (d *desk) seenKey(r *http.Request) bool {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return false
	}
	if d.seen[key] {
		return true
	}
	d.seen[key] = true
	return false
}

func (d *desk) update(w http.ResponseWriter, r *http.Request, id int) {
	var change struct {
		Severity string `json:"severity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&change); err != nil || change.Severity == "" {
		http.Error(w, "want {\"severity\": \"...\"}", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tickets[id]
	if !ok {
		http.Error(w, "no such ticket", http.StatusNotFound)
		return
	}
	if !d.seenKey(r) && change.Severity != t.Severity {
		t.History = append(t.History, fmt.Sprintf("severity %s → %s", t.Severity, change.Severity))
		log.Printf("#%d severity %s → %s", id, t.Severity, change.Severity)
		t.Severity = change.Severity
	}
	writeJSON(w, http.StatusOK, t)
}

func (d *desk) comment(w http.ResponseWriter, r *http.Request, id int) {
	var c struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || c.Body == "" {
		http.Error(w, "want {\"body\": \"...\"}", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tickets[id]
	if !ok {
		http.Error(w, "no such ticket", http.StatusNotFound)
		return
	}
	if !d.seenKey(r) {
		t.Comments = append(t.Comments, Comment{At: time.Now(), Body: c.Body})
		t.History = append(t.History, "comment added")
		log.Printf("#%d comment: %s", id, c.Body)
	}
	writeJSON(w, http.StatusCreated, t)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
