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
	"github.com/Andree37/hunch/internal/store"
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
	writerName := fs.String("writer", "", "use this backend for llm nodes instead of the flow's writer")
	dryRun := fs.Bool("dry-run", false, "don't send http requests that write; record them instead")
	tokenEnv := fs.String("token-env", "", "require `Authorization: Bearer <token>` with the token read from this env var")
	traceFile := fs.String("trace", "", "record every run: a .jsonl file, a directory or s3://bucket/prefix")
	timeout := fs.Duration("timeout", 5*time.Minute, "longest a single run may take")
	dedupeKey := fs.String("dedupe-key", "", "template naming a run from its input, e.g. '{{record.id}}'; repeats of a finished run get its reply without running again")
	dedupeTTL := fs.Duration("dedupe-ttl", 24*time.Hour, "how long a finished run's reply is remembered for duplicates")
	maxConcurrent := fs.Int("max-concurrent", 4, "runs at once; more wait up to 30s, then get 503")
	dedupeStore := fs.String("dedupe-store", "", "remember runs in a directory or s3://bucket/prefix, shared by every serve using it and kept across restarts (default: in memory)")
	traceMaxMB := fs.Int("trace-max-mb", 100, "roll a .jsonl trace file over at this size, keeping 3 old ones (0 = never)")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	h := &server{
		path:      path,
		backend:   *backendName,
		writer:    *writerName,
		dryRun:    *dryRun,
		timeout:   *timeout,
		dedupeKey: *dedupeKey,
		seen:      newMemDedupe(*dedupeTTL),
		slots:     make(chan struct{}, max(*maxConcurrent, 1)),
	}
	if err := h.load(); err != nil {
		return err
	}
	if *dedupeStore != "" {
		st, err := store.Open(ctx, *dedupeStore)
		if err != nil {
			return err
		}
		// A run still "running" after the longest a run can take belonged
		// to a serve that died; it may be taken over.
		h.seen = &storeDedupe{st: st, ttl: *dedupeTTL, stale: *timeout + time.Minute}
		log.Printf("dedupe: remembering runs in %s", st)
	}
	var token string
	if *tokenEnv != "" {
		if token = os.Getenv(*tokenEnv); token == "" {
			return fmt.Errorf("--token-env: $%s is not set", *tokenEnv)
		}
	}
	var rec *trace.Writer
	if *traceFile != "" {
		w, closer, err := trace.Open(ctx, *traceFile, int64(*traceMaxMB)<<20)
		if err != nil {
			return err
		}
		defer closer.Close()
		w.OnError = func(err error) { log.Print(err) }
		rec = w
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
	writer    string // overrides the flow's writer, if set
	dryRun    bool
	token     string
	timeout   time.Duration
	dedupeKey string
	seen      dedupe
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
	if err := pickBackends(f, s.path, s.backend, s.writer); err != nil {
		return err
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
		prev, running, fresh, err := s.seen.begin(r.Context(), key)
		switch {
		case err != nil:
			// Without knowing whether it already ran, running it could do
			// it twice; the sender will retry.
			log.Printf("dedupe: %v", err)
			w.Header().Set("Retry-After", "10")
			writeJSON(w, http.StatusServiceUnavailable, runReply{Error: "duplicate check unavailable, try again later"})
			return
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
		s.seen.forget(r.Context(), key)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, runReply{Error: "busy, try again later"})
		return
	case <-r.Context().Done():
		s.seen.forget(r.Context(), key)
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
			s.seen.finish(ctx, key, reply)
		} else {
			s.seen.forget(ctx, key)
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

func logf(format string, args ...any) { log.Printf(format, args...) }
