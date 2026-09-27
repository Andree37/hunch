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
)

// cmdServe runs a flow for every POST it receives: the JSON body is the
// input, the response is the path taken and the outputs. It is meant to sit
// behind a webhook, e.g. "ticket created".
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	backendName := fs.String("backend", "", "use this backend instead of the flow's default")
	dryRun := fs.Bool("dry-run", false, "don't send http requests that write; record them instead")
	tokenEnv := fs.String("token-env", "", "require `Authorization: Bearer <token>` with the token read from this env var")
	traceFile := fs.String("trace", "", "append every run's events to this JSONL file")
	timeout := fs.Duration("timeout", 5*time.Minute, "longest a single run may take")
	path, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	f, err := loadChecked(path, os.Stderr)
	if err != nil {
		return err
	}
	if *backendName != "" {
		if _, ok := f.Backends[*backendName]; !ok {
			return fmt.Errorf("backend %q is not defined in %s", *backendName, path)
		}
		f.DefaultBackend = *backendName
	}
	backends := map[string]backend.Backend{}
	for name, cfg := range f.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			return err
		}
		backends[name] = b
	}
	var token string
	if *tokenEnv != "" {
		if token = os.Getenv(*tokenEnv); token == "" {
			return fmt.Errorf("--token-env: $%s is not set", *tokenEnv)
		}
	}
	var trace io.Writer
	if *traceFile != "" {
		tf, err := os.OpenFile(*traceFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer tf.Close()
		trace = tf
	}

	h := &server{flow: f, backends: backends, dryRun: *dryRun, token: token, timeout: *timeout, trace: trace}
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
	log.Printf("serving %s on http://%s (backend %s, %s)", path, *addr, f.DefaultBackend, mode)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type server struct {
	flow     *flow.Flow
	backends map[string]backend.Backend
	dryRun   bool
	token    string
	timeout  time.Duration

	traceMu sync.Mutex
	trace   io.Writer
}

type runReply struct {
	Path    []string       `json:"path"`
	Outputs map[string]any `json:"outputs"`
	CostUSD float64        `json:"cost_usd"`
	Error   string         `json:"error,omitempty"`
	Node    string         `json:"failed_at,omitempty"`
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
	state := maps.Clone(s.flow.State)
	maps.Copy(state, input)
	if err := checkInputs(s.flow, state); err != nil {
		writeJSON(w, http.StatusBadRequest, runReply{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	start := time.Now()
	res, err := runner.Run(ctx, s.flow, state, runner.Options{
		Backends: s.backends,
		DryRun:   s.dryRun,
		OnEvent:  s.writeTrace,
	})
	reply := runReply{Path: res.Path, Outputs: res.Outputs, CostUSD: res.CostUSD}
	status := http.StatusOK
	if err != nil {
		status = http.StatusInternalServerError
		reply.Error = err.Error()
		if len(res.Path) > 0 {
			reply.Node = res.Path[len(res.Path)-1]
		}
	}
	log.Printf("%d %s · %s · $%.6f · %s", status, strings.Join(res.Path, " → "),
		formatOutputs(res.Outputs), res.CostUSD, time.Since(start).Round(time.Millisecond))
	writeJSON(w, status, reply)
}

func (s *server) writeTrace(ev runner.Event) {
	if s.trace == nil {
		return
	}
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	json.NewEncoder(s.trace).Encode(ev)
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
