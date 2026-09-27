// Package trace records runs as JSON lines and reads them back, so a run
// that happened elsewhere (e.g. under hunch serve) can be replayed in the TUI.
//
// Each run is a "start" line (id, time, flow, input), one "step" line per
// node, and an "end" line (path, outputs, cost, error).
package trace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/store"
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

// Writer records runs; safe for concurrent runs. It either streams lines
// to one file, or (on a store) keeps each run's lines until it ends and
// writes them as one object, since S3 objects can't be appended to.
type Writer struct {
	mu      sync.Mutex
	w       io.Writer
	st      store.Store
	pending map[string]*runBuf
	OnError func(error) // store writes that fail; default logs nothing
}

type runBuf struct {
	started time.Time
	buf     bytes.Buffer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// NewStoreWriter writes one object per run into st, keyed by start time.
func NewStoreWriter(st store.Store) *Writer {
	return &Writer{st: st, pending: map[string]*runBuf{}}
}

// Open records to loc: a path ending in .jsonl is one file, appended to and
// rolled over at maxBytes (keeping 3); a directory or s3://bucket/prefix
// gets one object per run.
func Open(ctx context.Context, loc string, maxBytes int64) (*Writer, io.Closer, error) {
	if strings.HasSuffix(loc, ".jsonl") && !store.IsRemote(loc) {
		f, err := OpenRotating(loc, maxBytes, 3)
		if err != nil {
			return nil, nil, err
		}
		return NewWriter(f), f, nil
	}
	st, err := store.Open(ctx, loc)
	if err != nil {
		return nil, nil, err
	}
	return NewStoreWriter(st), io.NopCloser(nil), nil
}

func (t *Writer) write(id string, v any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.st == nil {
		json.NewEncoder(t.w).Encode(v)
		return
	}
	rb, ok := t.pending[id]
	if !ok {
		rb = &runBuf{started: time.Now().UTC()}
		t.pending[id] = rb
	}
	json.NewEncoder(&rb.buf).Encode(v)
}

func (t *Writer) Start(id, flow, backend string, input map[string]any) {
	t.write(id, start{Type: "start", Run: id, Time: time.Now().UTC(), Flow: flow, Backend: backend, Input: input})
}

func (t *Writer) Step(id string, ev runner.Event) {
	t.write(id, step{Type: "step", Run: id, Event: ev})
}

func (t *Writer) End(id string, res *runner.Result, err error) {
	e := end{Type: "end", Run: id}
	if res != nil {
		e.Path, e.Outputs, e.CostUSD = res.Path, res.Outputs, res.CostUSD
	}
	if err != nil {
		e.Error = err.Error()
	}
	t.write(id, e)
	if t == nil || t.st == nil {
		return
	}
	t.mu.Lock()
	rb := t.pending[id]
	delete(t.pending, id)
	t.mu.Unlock()
	if rb == nil {
		return
	}
	// Keys sort by start time: 2026-09-27/18-17-19.123-<id>.jsonl
	key := rb.started.Format("2006-01-02/15-04-05.000") + "-" + id + ".jsonl"
	if err := t.st.Put(context.Background(), key, rb.buf.Bytes()); err != nil && t.OnError != nil {
		t.OnError(fmt.Errorf("recording run %s to %s: %w", id, t.st, err))
	}
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

// ReadAll reads recorded runs from loc, oldest first: a .jsonl file (with
// its rolled-over files), a directory or s3://bucket/prefix of per-run
// objects.
func ReadAll(ctx context.Context, loc string) ([]*Run, error) {
	if !store.IsRemote(loc) {
		st, err := os.Stat(loc)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing recorded yet
		}
		if err == nil && !st.IsDir() {
			return ReadFiles(loc)
		}
	}
	st, err := store.Open(ctx, loc)
	if err != nil {
		return nil, err
	}
	keys, err := st.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", st, err)
	}
	var all []byte
	for _, k := range keys {
		if !strings.HasSuffix(k, ".jsonl") {
			continue
		}
		data, err := st.Get(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		all = append(all, data...)
	}
	return Read(bytes.NewReader(all))
}

// ReadFiles reads a trace file together with the older files it rolled over
// into (path.3, path.2, path.1), oldest first, as one stream.
func ReadFiles(path string) ([]*Run, error) {
	var readers []io.Reader
	for i := 3; i >= 1; i-- {
		if f, err := os.Open(fmt.Sprintf("%s.%d", path, i)); err == nil {
			defer f.Close()
			readers = append(readers, f)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	runs, err := Read(io.MultiReader(append(readers, f)...))
	if err != nil {
		return runs, fmt.Errorf("%s: %w", path, err)
	}
	return runs, nil
}

// FromFlow reports whether the run was recorded with the flow file at path.
// Runs that don't name their flow are assumed to match.
func (r *Run) FromFlow(path string) bool {
	if r.Flow == "" {
		return true
	}
	a, err1 := filepath.Abs(r.Flow)
	b, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return filepath.Clean(r.Flow) == filepath.Clean(path)
	}
	return a == b
}

// OfFlow splits runs into those recorded with the flow at path and a count
// of the others by the flow they came from.
func OfFlow(runs []*Run, path string) (mine []*Run, others map[string]int) {
	others = map[string]int{}
	for _, r := range runs {
		if r.FromFlow(path) {
			mine = append(mine, r)
		} else {
			others[r.Flow]++
		}
	}
	return mine, others
}
