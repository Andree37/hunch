package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/trace"
)

// cmdServe runs a flow for every POST it receives: the JSON body is the
// input, the response is the path taken and the outputs. It is meant to sit
// behind a webhook from another system, e.g. "record created".
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	backendName := fs.String("backend", "", "use this backend instead of the flow's default")
	dryRun := fs.Bool("dry-run", false, "don't send http requests that write; record them instead")
	tokenEnv := fs.String("token-env", "", "require `Authorization: Bearer <token>` with the token read from this env var")
	traceFile := fs.String("trace", "", "append every run's events to this JSONL file")
	timeout := fs.Duration("timeout", 5*time.Minute, "longest a single run may take")
	dedupeKey := fs.String("dedupe-key", "", "template naming a run from its input, e.g. '{{record.id}}'; repeats of a finished run get its reply without running again")
	dedupeTTL := fs.Duration("dedupe-ttl", 24*time.Hour, "how long a finished run's reply is remembered for duplicates")
	maxConcurrent := fs.Int("max-concurrent", 4, "runs at once; more wait up to 30s, then get 503")
	dedupeFile := fs.String("dedupe-file", "", "keep finished runs' replies in this file so duplicates are still caught after a restart")
	traceMaxMB := fs.Int("trace-max-mb", 100, "roll the trace file over at this size, keeping 3 old ones (0 = never)")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	h := &server{
		path:      path,
		backend:   *backendName,
		dryRun:    *dryRun,
		timeout:   *timeout,
		dedupeKey: *dedupeKey,
		seen:      newDedupe(*dedupeTTL),
		slots:     make(chan struct{}, max(*maxConcurrent, 1)),
	}
	if err := h.load(); err != nil {
		return err
	}
	if *dedupeFile != "" {
		n, err := h.seen.persist(*dedupeFile)
		if err != nil {
			return err
		}
		log.Printf("dedupe: %d finished runs remembered from %s", n, *dedupeFile)
	}
	var token string
	if *tokenEnv != "" {
		if token = os.Getenv(*tokenEnv); token == "" {
			return fmt.Errorf("--token-env: $%s is not set", *tokenEnv)
		}
	}
	var rec *trace.Writer
	if *traceFile != "" {
		tf, err := trace.OpenRotating(*traceFile, int64(*traceMaxMB)<<20, 3)
		if err != nil {
			return err
		}
		defer tf.Close()
		rec = trace.NewWriter(tf)
	}

	h.token, h.trace = token, rec
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	mode := "live"
	if *dryRun {
		mode = "dry run"
	}
	log.Printf("serving %s on http://%s (backend %s, %s, up to %d at once)", path, *addr, h.flow.DefaultBackend, mode, cap(h.slots))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type server struct {
	path      string
	backend   string // overrides the flow's default, if set
	dryRun    bool
	token     string
	timeout   time.Duration
	dedupeKey string
	seen      *dedupe
	slots     chan struct{} // one per run allowed at once
	busyWait  time.Duration // how long a request waits for a slot; 0 means 30s

	mu       sync.RWMutex // guards the fields below, swapped on reload
	flow     *flow.Flow
	backends map[string]backend.Backend
	modTime  time.Time
	checked  time.Time

	trace *trace.Writer
}

type runReply struct {
	Path      []string       `json:"path"`
	Outputs   map[string]any `json:"outputs"`
	CostUSD   float64        `json:"cost_usd"`
	Error     string         `json:"error,omitempty"`
	Node      string         `json:"failed_at,omitempty"`
	Duplicate bool           `json:"duplicate,omitempty"`
}

// load (re)reads the flow and builds its backends.
func (s *server) load() error {
	st, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	f, err := loadChecked(s.path, os.Stderr)
	if err != nil {
		return err
	}
	if s.backend != "" {
		if _, ok := f.Backends[s.backend]; !ok {
			return fmt.Errorf("backend %q is not defined in %s", s.backend, s.path)
		}
		f.DefaultBackend = s.backend
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			return err
		}
		backends[name] = b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flow, s.backends, s.modTime = f, backends, st.ModTime()
	return nil
}

// current returns the flow to run, reloading it (at most once a second) if
// the file changed. A broken edit keeps the last good flow running.
func (s *server) current() (*flow.Flow, map[string]backend.Backend) {
	s.mu.RLock()
	f, b, mod, checked := s.flow, s.backends, s.modTime, s.checked
	s.mu.RUnlock()
	if s.path == "" || time.Since(checked) < time.Second {
		return f, b
	}
	s.mu.Lock()
	s.checked = time.Now()
	s.mu.Unlock()
	if st, err := os.Stat(s.path); err == nil && !st.ModTime().Equal(mod) {
		if err := s.load(); err != nil {
			log.Printf("reload failed, still serving the previous flow: %v", err)
			s.mu.Lock()
			s.modTime = st.ModTime() // don't retry until it changes again
			s.mu.Unlock()
		} else {
			log.Printf("reloaded %s", s.path)
		}
		s.mu.RLock()
		f, b = s.flow, s.backends
		s.mu.RUnlock()
	}
	return f, b
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		io.WriteString(w, "ok\n")
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST a JSON object with the flow's inputs", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var input map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, runReply{Error: "body must be a JSON object: " + err.Error()})
		return
	}
	f, backends := s.current()
	state := maps.Clone(f.State)
	maps.Copy(state, input)
	if err := checkInputs(f, state); err != nil {
		writeJSON(w, http.StatusBadRequest, runReply{Error: err.Error()})
		return
	}

	// Duplicates: the sender's Idempotency-Key, else --dedupe-key.
	key := r.Header.Get("Idempotency-Key")
	if key == "" && s.dedupeKey != "" {
		k, err := tmpl.Render(s.dedupeKey, state)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, runReply{Error: "--dedupe-key: " + err.Error()})
			return
		}
		key = k
	}
	if key != "" {
		prev, running, fresh := s.seen.begin(key)
		switch {
		case running:
			writeJSON(w, http.StatusConflict, runReply{Error: "a run for " + key + " is already in progress", Duplicate: true})
			return
		case !fresh:
			log.Printf("duplicate %s: returning the earlier reply", key)
			prev.Duplicate = true
			writeJSON(w, http.StatusOK, prev)
			return
		}
	}

	// Wait for a free slot; after a while ask the sender to come back later.
	wait := s.busyWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-time.After(wait):
		s.seen.forget(key)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, runReply{Error: "busy, try again later"})
		return
	case <-r.Context().Done():
		s.seen.forget(key)
		return
	}

	// The run must finish even if the sender hangs up: stopping halfway
	// could leave one write done and the next not.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.timeout)
	defer cancel()
	start := time.Now()
	runID := trace.NewID()
	s.trace.Start(runID, s.path, f.DefaultBackend, inputsOnly(f, state))
	res, err := runner.Run(ctx, f, state, runner.Options{
		Backends: backends,
		DryRun:   s.dryRun,
		OnEvent:  func(ev runner.Event) { s.trace.Step(runID, ev) },
	})
	s.trace.End(runID, res, err)
	reply := runReply{Path: res.Path, Outputs: res.Outputs, CostUSD: res.CostUSD}
	status := http.StatusOK
	if err != nil {
		status = http.StatusInternalServerError
		reply.Error = err.Error()
		if len(res.Path) > 0 {
			reply.Node = res.Path[len(res.Path)-1]
		}
	}
	if key != "" {
		// Only finished runs are remembered; a failed one may be retried.
		if err == nil {
			s.seen.finish(key, reply)
		} else {
			s.seen.forget(key)
		}
	}
	log.Printf("%d %s · %s · $%.6f · %s", status, strings.Join(res.Path, " → "),
		formatOutputs(res.Outputs), res.CostUSD, time.Since(start).Round(time.Millisecond))
	writeJSON(w, status, reply)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func formatOutputs(outputs map[string]any) string {
	if len(outputs) == 0 {
		return "no outputs"
	}
	data, _ := json.Marshal(outputs)
	return string(data)
}

// dedupe remembers runs by key: in progress, or finished with their reply.
type dedupe struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]*dedupeEntry
	file    *os.File // finished runs are appended here when persisting
}

// savedReply is one line of the dedupe file.
type savedReply struct {
	Key   string    `json:"key"`
	At    time.Time `json:"at"`
	Reply runReply  `json:"reply"`
}

// persist loads finished runs from path (dropping expired ones, which also
// compacts the file) and appends every run that finishes from now on.
func (d *dedupe) persist(path string) (int, error) {
	var kept []savedReply
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			var r savedReply
			if line == "" || json.Unmarshal([]byte(line), &r) != nil || time.Since(r.At) > d.ttl {
				continue
			}
			kept = append(kept, r)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(f)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range kept {
		d.entries[r.Key] = &dedupeEntry{reply: r.Reply, at: r.At}
		enc.Encode(r)
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	if d.file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		return 0, err
	}
	return len(kept), nil
}

type dedupeEntry struct {
	running bool
	reply   runReply
	at      time.Time
}

func newDedupe(ttl time.Duration) *dedupe {
	return &dedupe{ttl: ttl, entries: map[string]*dedupeEntry{}}
}

// begin claims key. fresh means the caller should run; otherwise the key is
// either running or finished with prev as its reply.
func (d *dedupe) begin(key string) (prev runReply, running, fresh bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, e := range d.entries {
		if !e.running && time.Since(e.at) > d.ttl {
			delete(d.entries, k)
		}
	}
	if e, ok := d.entries[key]; ok {
		return e.reply, e.running, false
	}
	d.entries[key] = &dedupeEntry{running: true, at: time.Now()}
	return runReply{}, false, true
}

func (d *dedupe) finish(key string, reply runReply) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	d.entries[key] = &dedupeEntry{reply: reply, at: now}
	if d.file != nil {
		if err := json.NewEncoder(d.file).Encode(savedReply{Key: key, At: now, Reply: reply}); err != nil {
			log.Printf("dedupe file: %v", err)
		}
	}
}

func (d *dedupe) forget(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
}
