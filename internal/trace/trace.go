// Package trace records runs as JSON lines and reads them back, so a run
// that happened elsewhere (e.g. under hunch serve) can be replayed in the TUI.
//
// Each run is a "start" line (id, time, flow, input), one "step" line per
// node, and an "end" line (path, outputs, cost, error).
package trace

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Andree37/hunch/internal/runner"
)

type start struct {
	Type    string         `json:"type"`
	Run     string         `json:"run"`
	Time    time.Time      `json:"time"`
	Flow    string         `json:"flow"`
	Backend string         `json:"backend,omitempty"`
	Input   map[string]any `json:"input"`
}

type step struct {
	Type string `json:"type"`
	Run  string `json:"run"`
	runner.Event
}

type end struct {
	Type    string         `json:"type"`
	Run     string         `json:"run"`
	Path    []string       `json:"path"`
	Outputs map[string]any `json:"outputs,omitempty"`
	CostUSD float64        `json:"cost_usd"`
	Error   string         `json:"error,omitempty"`
}

// Writer appends runs to w; safe for concurrent runs.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (t *Writer) write(v any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	json.NewEncoder(t.w).Encode(v)
}

func (t *Writer) Start(id, flow, backend string, input map[string]any) {
	t.write(start{Type: "start", Run: id, Time: time.Now().UTC(), Flow: flow, Backend: backend, Input: input})
}

func (t *Writer) Step(id string, ev runner.Event) {
	t.write(step{Type: "step", Run: id, Event: ev})
}

func (t *Writer) End(id string, res *runner.Result, err error) {
	e := end{Type: "end", Run: id}
	if res != nil {
		e.Path, e.Outputs, e.CostUSD = res.Path, res.Outputs, res.CostUSD
	}
	if err != nil {
		e.Error = err.Error()
	}
	t.write(e)
}

// NewID returns a short random run id.
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Run is one recorded run, as read back.
type Run struct {
	ID      string
	Time    time.Time
	Flow    string
	Backend string
	Input   map[string]any
	Steps   []runner.Event
	Path    []string
	Outputs map[string]any
	CostUSD float64
	Error   string
	Done    bool // an end line was seen
}

// Read parses a trace, oldest run first. Runs still in progress (no end
// line yet) are included with Done false.
func Read(r io.Reader) ([]*Run, error) {
	var runs []*Run
	byID := map[string]*Run{}
	get := func(id string) *Run {
		if run, ok := byID[id]; ok {
			return run
		}
		run := &Run{ID: id}
		byID[id] = run
		runs = append(runs, run)
		return run
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
			Run  string `json:"run"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return runs, fmt.Errorf("line %d: %v", n, err)
		}
		switch head.Type {
		case "start":
			var s start
			if err := json.Unmarshal(line, &s); err != nil {
				return runs, fmt.Errorf("line %d: %v", n, err)
			}
			run := get(s.Run)
			run.Time, run.Flow, run.Backend, run.Input = s.Time, s.Flow, s.Backend, s.Input
		case "step":
			var s step
			if err := json.Unmarshal(line, &s); err != nil {
				return runs, fmt.Errorf("line %d: %v", n, err)
			}
			run := get(s.Run)
			run.Steps = append(run.Steps, s.Event)
		case "end":
			var e end
			if err := json.Unmarshal(line, &e); err != nil {
				return runs, fmt.Errorf("line %d: %v", n, err)
			}
			run := get(e.Run)
			run.Path, run.Outputs, run.CostUSD, run.Error, run.Done = e.Path, e.Outputs, e.CostUSD, e.Error, true
		default:
			return runs, fmt.Errorf("line %d: not a hunch trace line", n)
		}
	}
	return runs, sc.Err()
}

// RotatingFile is an append-only file that rolls over at a size: path is
// renamed to path.1 (older ones shift to .2 ... .keep) and a new file starts.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func OpenRotating(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{path: path, maxBytes: maxBytes, keep: max(keep, 1)}
	return r, r.open()
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxBytes > 0 && r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotate() error {
	r.f.Close()
	os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return err
	}
	return r.open()
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
