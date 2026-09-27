// Package tui is hunch's terminal UI: pick a test case or type input, watch
// the flow run node by node, inspect each decision's probabilities, poke the
// input and re-run.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/trace"
	"github.com/Andree37/hunch/internal/validate"
)

type focus int

// Panes, numbered as on screen: 1-4 jump to them.
const (
	focusFlow focus = iota
	focusTests
	focusNode
	focusInputs
	numPanes
)

type input struct{ key, value string }

type editMode int

const (
	editNone editMode = iota
	editKey           // naming a new input
	editValue
	editCaseName // naming a new test case
	editPick     // choosing from a choice or bool input's options
)

// run is one execution of the flow, as seen so far.
type run struct {
	events  map[string]runner.Event // latest event per node
	path    []string
	current string // node running now
	paused  string // node the run is paused before, when stepping
	status  string // backend note while the current node is slow, e.g. retries
	nodeAt  time.Time
	inputs  map[string]any // what the run was started with
	err     error
	errNode string
	cost    float64
	done    bool
	elapsed time.Duration
	started time.Time
	state   map[string]any // final state, once done
	caseIdx int            // test case the input came from unchanged, or -1
}

type Model struct {
	path    string
	flow    *flow.Flow
	loadErr error
	issues  []validate.Issue
	modTime time.Time

	backend string
	inputs  []input

	cases      []*cases.Case
	casesStamp string // detects edits to the cases dir
	active     int    // case the inputs came from, or -1
	dirty      bool   // inputs edited since loading/saving the active case
	caseCursor int
	results    map[string][]cases.Result // last check per case name

	focus       focus
	cursor      int // selected node
	inputCursor int
	editing     editMode
	keyEditor   textinput.Model
	editor      textarea.Model
	pickOptions []string
	pickCursor  int

	run, prev  *run
	runID      int
	runCh      chan tea.Msg
	cancel     context.CancelFunc
	stepping   *atomic.Bool  // pause before each node
	stepCh     chan struct{} // one token lets one node run
	nodeScroll int

	flowView flowView

	runsPath  string // recorded runs file, if any
	runsMod   time.Time
	runs      []*trace.Run // newest first
	runCursor int
	showRuns  bool       // pane 2 lists runs instead of test cases
	replay    *trace.Run // recorded run shown in the views
	replayK   int        // how many of its steps are shown
	live      bool       // http nodes really send; off by default

	flash         string
	width, height int
}

// New builds the model. state is the initial input (flow defaults plus any
// --set / --state values); with fromCase set and test cases present, the
// first case's input is loaded instead. backendName overrides the flow's
// default backend.
func New(path string, state map[string]any, backendName string, fromCase bool) (*Model, error) {
	m := &Model{path: path, backend: backendName, active: -1, results: map[string][]cases.Result{}}
	for _, k := range slices.Sorted(maps.Keys(state)) {
		m.inputs = append(m.inputs, input{k, tmpl.Format(state[k])})
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := m.loadCases(); err != nil {
		return nil, err
	}
	if fromCase && len(m.cases) > 0 {
		m.useCase(0)
	}
	if m.backend == "" {
		m.backend = m.flow.DefaultBackend
	}
	if _, ok := m.flow.Backends[m.backend]; !ok {
		return nil, fmt.Errorf("backend %q is not defined in %s", m.backend, path)
	}

	m.keyEditor = textinput.New()
	m.keyEditor.Prompt = "name: "
	m.editor = textarea.New()
	m.editor.ShowLineNumbers = false
	m.editor.Prompt = ""
	m.editor.CharLimit = 0
	m.editor.SetHeight(6)
	// Enter saves; newlines take alt+enter or ctrl+j so pasted emails fit.
	m.editor.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))

	// Start where the work is: inputs that still need a value.
	if m.active < 0 && slices.ContainsFunc(m.inputs, func(in input) bool { return in.value == "" }) {
		m.focus = focusInputs
		m.inputCursor = slices.IndexFunc(m.inputs, func(in input) bool { return in.value == "" })
	}
	return m, nil
}

func (m *Model) load() error {
	st, err := os.Stat(m.path)
	if err != nil {
		return err
	}
	m.modTime = st.ModTime()
	f, err := flow.Load(m.path)
	if err != nil {
		m.loadErr = err
		return err
	}
	m.flow, m.loadErr = f, nil
	m.cursor = min(m.cursor, max(len(m.rows())-1, 0))
	// Offer every value the flow reads but nothing produces.
	for _, k := range f.Inputs() {
		if !slices.ContainsFunc(m.inputs, func(in input) bool { return in.key == k }) {
			m.inputs = append(m.inputs, input{k, ""})
		}
	}
	m.revalidate()
	return nil
}

// revalidate checks the flow with the inputs counted as state, so refs the
// inputs satisfy don't warn.
func (m *Model) revalidate() {
	g := *m.flow
	g.State = maps.Clone(m.flow.State)
	for _, in := range m.inputs {
		if _, ok := g.State[in.key]; !ok {
			g.State[in.key] = ""
		}
	}
	m.issues = validate.Flow(&g)
}

type (
	startMsg struct {
		id   int
		node string
	}
	pausedMsg struct {
		id   int
		node string
	}
	statusMsg struct {
		id   int
		note string
	}
	eventMsg struct {
		id int
		ev runner.Event
	}
	doneMsg struct {
		id    int
		state map[string]any
		err   error
	}
	tickMsg time.Time
)

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Init() tea.Cmd { return tick() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		// Poll rather than watch: survives editors that save by rename.
		if st, err := os.Stat(m.path); err == nil && !st.ModTime().Equal(m.modTime) {
			if err := m.load(); err != nil {
				m.flash = "reload failed"
			} else {
				m.flash = "reloaded " + filepath.Base(m.path)
			}
		}
		m.refreshRuns()
		if casesStamp(m.path) != m.casesStamp {
			if err := m.loadCases(); err != nil {
				m.flash = "tests: " + err.Error()
			}
		}
		return m, tick()

	case pausedMsg:
		if msg.id != m.runID {
			return m, nil
		}
		m.run.paused = msg.node
		return m, m.next()

	case statusMsg:
		if msg.id != m.runID {
			return m, nil
		}
		m.run.status = msg.note
		return m, m.next()

	case startMsg:
		if msg.id != m.runID {
			return m, nil
		}
		m.run.paused, m.run.status = "", ""
		m.run.nodeAt = time.Now()
		m.run.current = msg.node
		m.run.path = append(m.run.path, msg.node)
		return m, m.next()

	case eventMsg:
		if msg.id != m.runID {
			return m, nil
		}
		m.run.events[msg.ev.Node] = msg.ev
		m.run.cost += msg.ev.CostUSD
		if m.run.current == msg.ev.Node {
			m.run.current = ""
		}
		// When stepping, show each node's result as it lands.
		if m.stepping.Load() {
			m.selectNode(msg.ev.Node)
		}
		return m, m.next()

	case doneMsg:
		if msg.id != m.runID {
			return m, nil
		}
		m.run.done = true
		m.run.elapsed = time.Since(m.run.started)
		m.run.state = msg.state
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.run.err = msg.err
			m.run.errNode = m.run.current
		}
		m.run.current, m.run.paused = "", ""
		m.checkRun()
		return m, nil

	case tea.KeyMsg:
		switch m.editing {
		case editKey:
			return m.updateKeyEditor(msg)
		case editValue:
			return m.updateEditor(msg)
		case editCaseName:
			return m.updateCaseNameEditor(msg)
		case editPick:
			return m.updatePicker(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

func (m *Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.flash = ""
	switch msg.String() {
	case "q", "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "tab":
		m.focus = (m.focus + 1) % numPanes
	case "shift+tab":
		m.focus = (m.focus + numPanes - 1) % numPanes
	case "1", "2", "3", "4":
		m.focus = focus(msg.String()[0] - '1')
	case "j", "down":
		switch m.focus {
		case focusFlow:
			m.cursor = max(min(m.cursor+1, len(m.rows())-1), 0)
			m.nodeScroll = 0
		case focusInputs:
			m.inputCursor = min(m.inputCursor+1, len(m.inputs)-1)
		case focusTests:
			if m.showRuns {
				m.runCursor = max(min(m.runCursor+1, len(m.runs)-1), 0)
			} else {
				m.caseCursor = min(m.caseCursor+1, len(m.cases)-1)
			}
		case focusNode:
			m.nodeScroll++
		}
	case "k", "up":
		switch m.focus {
		case focusFlow:
			m.cursor = max(m.cursor-1, 0)
			m.nodeScroll = 0
		case focusInputs:
			m.inputCursor = max(m.inputCursor-1, 0)
		case focusTests:
			if m.showRuns {
				m.runCursor = max(m.runCursor-1, 0)
			} else {
				m.caseCursor = max(m.caseCursor-1, 0)
			}
		case focusNode:
			m.nodeScroll = max(m.nodeScroll-1, 0)
		}
	case "r":
		if m.replaying() {
			m.replay = nil // run the loaded input again, live
			return m, m.startRun(false)
		}
		// A paused run carries on to the end, unless the inputs changed
		// since it started; then start fresh.
		if m.resumable() {
			m.stepping.Store(false)
			m.step()
			return m, nil
		}
		return m, m.startRun(false)
	case "s":
		if m.replaying() {
			m.stepReplay()
			return m, nil
		}
		if m.resumable() {
			m.step()
			return m, nil
		}
		return m, m.startRun(true)
	case "x":
		if m.run != nil && !m.run.done && m.cancel != nil {
			m.cancel()
			m.run.done, m.run.current, m.run.paused = true, "", ""
			m.run.elapsed = time.Since(m.run.started)
			m.flash = "stopped"
		}
	case "v":
		// Keep the same node selected across views.
		var id string
		if n := m.selected(); n != nil {
			id = n.ID
		}
		if m.flowView == viewGraph {
			if m.run == nil || len(m.run.path) == 0 {
				m.flash = "run first to see its path"
				return m, nil
			}
			m.flowView = viewPath
		} else {
			m.flowView = viewGraph
		}
		m.cursor = 0
		m.selectNode(id)
	case "L":
		m.live = !m.live
		if m.live {
			m.flash = "LIVE: http nodes will really send"
		} else {
			m.flash = "dry run: http nodes only record their request"
		}
	case "b":
		names := slices.Sorted(maps.Keys(m.flow.Backends))
		i := slices.Index(names, m.backend)
		m.backend = names[(i+1)%len(names)]
		m.flash = "backend: " + m.backend
	case "enter", "e":
		switch {
		case m.focus == focusInputs && len(m.inputs) > 0:
			return m, m.editInput()
		case m.focus == focusTests && m.showRuns && len(m.runs) > 0:
			m.openRun(m.runCursor)
		case m.focus == focusTests && !m.showRuns && len(m.cases) > 0:
			m.replay = nil
			m.useCase(m.caseCursor)
		}
	case "t":
		if m.runsPath != "" {
			m.showRuns = !m.showRuns
		}
	case "a":
		m.focus = focusInputs
		m.editing = editKey
		m.keyEditor.Prompt = "input name: "
		m.keyEditor.SetValue("")
		return m, m.keyEditor.Focus()
	case "d":
		if m.focus == focusInputs && len(m.inputs) > 0 {
			m.flash = "removed " + m.inputs[m.inputCursor].key
			m.inputs = slices.Delete(m.inputs, m.inputCursor, m.inputCursor+1)
			m.inputCursor = max(min(m.inputCursor, len(m.inputs)-1), 0)
			if len(m.inputs) == 0 {
				m.focus = focusFlow
			}
			m.dirty = true
			m.revalidate()
		}
	case "w":
		if m.active >= 0 {
			m.saveCase(m.cases[m.active])
			return m, nil
		}
		return m, m.nameCase()
	case "n":
		return m, m.nameCase()
	}
	return m, nil
}

// spec returns the declared spec for an input, or a free-form one.
func (m *Model) spec(key string) flow.InputSpec {
	if m.flow == nil {
		return flow.InputSpec{Name: key}
	}
	s, _ := m.flow.InputSpec(key)
	return s
}

func (m *Model) editInput() tea.Cmd {
	in := m.inputs[m.inputCursor]
	switch s := m.spec(in.key); s.Type {
	case flow.InputChoice, flow.InputBool:
		m.pickOptions = s.Options
		if s.Type == flow.InputBool {
			m.pickOptions = []string{"yes", "no"}
		}
		m.pickCursor = max(slices.Index(m.pickOptions, displayValue(s, in.value)), 0)
		m.editing = editPick
		return nil
	}
	m.editing = editValue
	m.editor.SetValue(m.inputs[m.inputCursor].value)
	m.editor.SetWidth(max(m.width*55/100-8, 10))
	return m.editor.Focus()
}

func (m *Model) updateKeyEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = editNone
		m.keyEditor.Blur()
		return m, nil
	case "enter":
		k := strings.TrimSpace(m.keyEditor.Value())
		switch {
		case !validKey(k):
			m.flash = "input names are letters, digits, _ and -"
			return m, nil
		case slices.ContainsFunc(m.inputs, func(in input) bool { return in.key == k }):
			m.flash = k + " already exists"
			return m, nil
		}
		m.keyEditor.Blur()
		m.inputs = append(m.inputs, input{k, ""})
		m.inputCursor = len(m.inputs) - 1
		m.dirty = true
		m.revalidate()
		return m, m.editInput()
	}
	var cmd tea.Cmd
	m.keyEditor, cmd = m.keyEditor.Update(msg)
	return m, cmd
}

func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		ok := r == '_' || unicode.IsLetter(r) || (i > 0 && (r == '-' || unicode.IsDigit(r)))
		if !ok {
			return false
		}
	}
	return true
}

func (m *Model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = editNone
	case "j", "down":
		m.pickCursor = min(m.pickCursor+1, len(m.pickOptions)-1)
	case "k", "up":
		m.pickCursor = max(m.pickCursor-1, 0)
	case "enter", " ":
		m.editing = editNone
		if v := m.pickOptions[m.pickCursor]; v != m.inputs[m.inputCursor].value {
			m.inputs[m.inputCursor].value = v
			m.dirty = true
		}
	}
	return m, nil
}

// displayValue maps a stored value to how the picker names it.
func displayValue(s flow.InputSpec, v string) string {
	if s.Type == flow.InputBool {
		if b, err := s.Parse(v); err == nil {
			if b.(bool) {
				return "yes"
			}
			return "no"
		}
	}
	return v
}

// inputError reports why an input's value doesn't fit its spec. Empty
// values are left to the run, which knows whether they're needed.
func (m *Model) inputError(in input) error {
	if in.value == "" {
		return nil
	}
	_, err := m.spec(in.key).Parse(in.value)
	return err
}

func (m *Model) updateEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = editNone
		m.editor.Blur()
		return m, nil
	case "enter":
		if err := m.inputError(input{m.inputs[m.inputCursor].key, m.editor.Value()}); err != nil {
			m.flash = err.Error()
			return m, nil
		}
		m.editing = editNone
		m.editor.Blur()
		if v := m.editor.Value(); v != m.inputs[m.inputCursor].value {
			m.inputs[m.inputCursor].value = v
			m.dirty = true
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	return m, cmd
}

func (m *Model) pausedRun() bool {
	return m.run != nil && !m.run.done && m.run.paused != ""
}

func (m *Model) resumable() bool {
	return m.pausedRun() && reflect.DeepEqual(m.run.inputs, m.state())
}

// step lets the paused run execute one node.
func (m *Model) step() {
	select {
	case m.stepCh <- struct{}{}:
	default: // a token is already waiting
	}
}

// selectNode moves the cursor to a node, e.g. to follow a stepped run.
func (m *Model) selectNode(id string) {
	if m.flow == nil {
		return
	}
	rows := m.rows()
	if i := slices.IndexFunc(rows, func(r row) bool { return r.node == id && !r.ref }); i >= 0 && i != m.cursor {
		m.cursor, m.nodeScroll = i, 0
	}
}

// startRun cancels any run in flight and starts a new one in the background.
// Its progress comes back as messages through runCh. With stepping, the
// first node runs and the run pauses before each one after it.
func (m *Model) startRun(stepping bool) tea.Cmd {
	if m.flow == nil || validate.HasErrors(m.issues) {
		m.flash = "fix the flow's errors first"
		return nil
	}
	for _, in := range m.inputs {
		if in.value == "" {
			m.flash = "fill in " + in.key + " first"
			m.focus, m.inputCursor = focusInputs, slices.Index(m.inputs, in)
			return nil
		}
		if err := m.inputError(in); err != nil {
			m.flash = "fix input " + err.Error()
			return nil
		}
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.run != nil && m.run.done && m.run.err == nil {
		m.prev = m.run
	}
	m.replay = nil
	m.runID++
	id := m.runID
	m.run = &run{events: map[string]runner.Event{}, started: time.Now(), caseIdx: -1}
	if m.active >= 0 && !m.dirty {
		m.run.caseIdx = m.active
	}

	backends := map[string]backend.Backend{}
	for name, cfg := range m.flow.Backends {
		b, err := backend.New(cfg)
		if err != nil {
			m.run.err, m.run.done = err, true
			return nil
		}
		backends[name] = b
	}

	f := *m.flow
	f.DefaultBackend = m.backend
	// A loaded test case's canned http responses apply to its runs.
	var fakes map[string]runner.FakeResponse
	if m.active >= 0 {
		fakes = m.cases[m.active].HTTP
	}
	state := m.state()
	m.run.inputs = m.state()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	ch := make(chan tea.Msg, 16)
	m.runCh = ch
	stepMode, stepCh := &atomic.Bool{}, make(chan struct{}, 1)
	stepMode.Store(stepping)
	stepCh <- struct{}{}
	m.stepping, m.stepCh = stepMode, stepCh
	send := func(msg tea.Msg) {
		select {
		case ch <- msg:
		case <-ctx.Done():
		}
	}
	ctx = backend.WithStatus(ctx, func(note string) { send(statusMsg{id, note}) })
	go func() {
		defer close(ch)
		res, err := runner.Run(ctx, &f, state, runner.Options{
			Backends: backends,
			Stdout:   io.Discard,
			DryRun:   !m.live,
			Fake:     fakes,
			Before: func(ctx context.Context, node string) error {
				if !stepMode.Load() {
					return nil
				}
				select {
				case <-stepCh: // a step was already requested
					return nil
				default:
				}
				send(pausedMsg{id, node})
				select {
				case <-stepCh:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			OnStart: func(node string) { send(startMsg{id, node}) },
			OnEvent: func(ev runner.Event) { send(eventMsg{id, ev}) },
		})
		// A cancelled run has been replaced; nobody is waiting for its end.
		if ctx.Err() == nil {
			ch <- doneMsg{id, res.State, err}
		}
	}()
	return m.next()
}

// next waits for the current run's next message.
func (m *Model) next() tea.Cmd {
	ch := m.runCh
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// state turns the inputs back into run state, typed by their specs.
// Undeclared inputs read as JSON when they can, like --set.
func (m *Model) state() map[string]any {
	s := map[string]any{}
	for _, in := range m.inputs {
		v, err := m.spec(in.key).Parse(in.value)
		if err != nil {
			v = in.value
		}
		s[in.key] = v
	}
	return s
}

func Run(m *Model) error {
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
