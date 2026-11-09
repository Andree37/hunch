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
	editRunsPath // typing where to read runs from
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
	path      string
	flow      *flow.Flow
	loadErr   error
	issues    []validate.Issue
	modTime   time.Time
	projStamp string // the hunch.yaml in use and when it changed, to reload on save

	backend string
	writer  string // overrides the flow's writer for llm nodes, if set
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

	runsPath    string // recorded runs: a .jsonl file or a directory
	runsMod     time.Time
	runsLoading bool
	runs        []*trace.Run   // of this flow, newest first
	otherRuns   map[string]int // runs of other flows in the same place, by flow
	runCursor   int
	showRuns    bool       // pane 2 lists runs instead of test cases
	replay      *trace.Run // recorded run shown in the views
	beforeRun   *snapshot  // what the views showed before a run was opened
	replayK     int        // how many of its steps are shown
	live        bool       // shell and http nodes really run; off by default

	grid       *gridRun       // every test run at once, shown side by side
	gridCol    int            // the question selected in the grid
	shifts     map[string]int // thresholds tried with [ and ], in steps, by node
	gridID     int
	gridCursor int

	picking     bool   // the model list (b) covers the screen
	pickedModel bool   // chosen with --model or b, so a new default in hunch.yaml doesn't replace it
	lastDefault string // hunch.yaml's default when last read, to follow when it changes
	modelCursor int
	checks      map[string]*modelCheck // whether each model answered, by name
	checksFor   string                 // the hunch.yaml they were checked against
	compare     *comparison            // two recorded runs side by side
	compareMark *trace.Run             // the first run picked with c

	help   bool // the key list covers the screen
	detail bool // the selected step covers the screen

	// Steps of a run appear one at a time, each answer's bar filling in.
	record        bool      // keep runs next to the flow; off in tests
	animate       bool      // off in tests, so a finished run shows whole
	reveal        int       // how many steps of the run are on screen
	revealAt      time.Time // when the last of them appeared
	frame         int       // animation frame, for spinners
	ticking       bool
	flash         string
	width, height int
}

// New builds the model. state is the initial input (flow defaults plus any
// --set / --state values); with fromCase set and test cases present, the
// first case's input is loaded instead. backendName overrides the flow's
// default backend.
func New(path string, state map[string]any, backendName string, fromCase bool) (*Model, error) {
	m := &Model{path: path, active: -1, results: map[string][]cases.Result{}, animate: animations, record: animations}
	for _, k := range slices.Sorted(maps.Keys(state)) {
		m.inputs = append(m.inputs, input{k, tmpl.Format(state[k])})
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := m.loadCases(); err != nil {
		return nil, err
	}
	m.useFlowRuns()
	if fromCase && len(m.cases) > 0 {
		m.useCase(0)
	}
	if m.flow.Project == nil && backendName != "mock" {
		return nil, errors.New(flow.ProjectHelp(path))
	}
	m.backend = m.flow.DefaultBackend
	if backendName != "" {
		if _, ok := m.flow.Backends[backendName]; !ok {
			return nil, fmt.Errorf("model %q is not in %s (it has %s)", backendName, m.flow.Project.Path, strings.Join(m.flow.Project.Order, ", "))
		}
		m.backend, m.pickedModel = backendName, true
	}
	m.revalidate()

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
	m.projStamp = projectStamp(m.path)
	f, err := flow.Load(m.path)
	if err != nil {
		m.loadErr = err
		return err
	}
	m.flow, m.loadErr = f, nil
	// A changed hunch.yaml: follow a default: that changed unless a model
	// was picked (not the first model standing in for a missing default:,
	// which adding a model would change), and notice a model that's gone.
	prevDefault := m.lastDefault
	m.lastDefault = f.DefaultBackend
	if m.backend != "" && !m.pickedModel && f.Project != nil && f.Project.DefaultGiven &&
		f.DefaultBackend != prevDefault && f.DefaultBackend != m.backend {
		m.flash = "deciding with " + f.DefaultBackend + ", the default in hunch.yaml"
		m.backend = f.DefaultBackend
	}
	if _, ok := f.Backends[m.backend]; m.backend != "" && !ok {
		gone := " is no longer in hunch.yaml"
		if f.Project == nil {
			gone = ": hunch.yaml is gone"
		}
		m.flash = m.backend + gone + " · deciding with " + f.DefaultBackend
		m.backend, m.pickedModel = f.DefaultBackend, false
	}
	m.modelCursor = max(min(m.modelCursor, len(m.modelNames())-1), 0)
	if _, ok := f.Backends[m.writer]; m.writer != "" && !ok {
		m.writer = ""
	}
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

// projectStamp names the hunch.yaml a flow would use and when it last
// changed, so a save, a new file or a removed one shows up.
func projectStamp(flowPath string) string {
	path, err := flow.ProjectPath(filepath.Dir(flowPath))
	if err != nil || path == "" {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return path + "@" + st.ModTime().String()
}

// pickedFlow is a copy of the flow deciding and writing with the models
// picked here (--model, --writer, b), which may not be hunch.yaml's.
func (m *Model) pickedFlow() *flow.Flow {
	f := *m.flow
	if m.backend != "" {
		f.DefaultBackend = m.backend
	}
	if m.writer != "" {
		f.WriterBackend = m.writer
	}
	return &f
}

// revalidate checks the flow, with the models picked here and the inputs
// counted as state, so refs the inputs satisfy don't warn.
func (m *Model) revalidate() {
	g := *m.pickedFlow()
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
	animMsg struct{}
)

// animations is turned on by Run and RunApp; models built in tests show
// runs whole.
var animations = false

const (
	stepDelay = 300 * time.Millisecond // between one step appearing and the next
	barFill   = 350 * time.Millisecond // how long an answer's bar takes to fill
)

func animTick() tea.Cmd {
	return tea.Tick(40*time.Millisecond, func(time.Time) tea.Msg { return animMsg{} })
}

// kick starts the animation ticking, unless it already is.
func (m *Model) kick() tea.Cmd {
	if !m.animate || m.ticking {
		return nil
	}
	m.ticking = true
	return animTick()
}

// restartReveal makes the run on screen appear again from its first step.
func (m *Model) restartReveal() {
	m.reveal, m.revealAt = 0, time.Now().Add(-stepDelay)
}

// shown is how many steps of the run are on screen.
func (m *Model) shown() int {
	if m.run == nil {
		return 0
	}
	if !m.animate {
		return len(m.run.path)
	}
	return min(m.reveal, len(m.run.path))
}

// fill is how far the newest step's bar has filled, 0 to 1.
func (m *Model) fill(i int) float64 {
	if !m.animate || i != m.shown()-1 {
		return 1
	}
	p := min(float64(time.Since(m.revealAt))/float64(barFill), 1)
	return 1 - (1-p)*(1-p) // ease out
}

// spinner turns while a step waits on the model.
func (m *Model) spinner() string {
	return string([]rune("◐◓◑◒")[m.frame%4])
}

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
		changedFiles := false
		st, err := os.Stat(m.path)
		flowChanged := err == nil && !st.ModTime().Equal(m.modTime)
		projChanged := projectStamp(m.path) != m.projStamp
		if flowChanged || projChanged {
			name := filepath.Base(m.path)
			if !flowChanged {
				name = flow.ProjectFile
			}
			before := m.flash
			if err := m.load(); err != nil {
				m.flash = "reload failed: " + err.Error()
			} else {
				changedFiles = true
				if m.flash == before { // load didn't have more to say
					m.flash = "reloaded " + name
				}
			}
		}
		refresh := m.refreshRuns()
		if casesStamp(m.path) != m.casesStamp {
			if err := m.loadCases(); err != nil {
				m.flash = "tests: " + err.Error()
			} else {
				changedFiles = true
			}
		}
		// With the grid up, every save runs the tests again.
		var rerun tea.Cmd
		if changedFiles && m.grid != nil && m.flowView == viewGrid {
			if rerun = m.startGrid(); rerun != nil {
				// Keep what the reload had to say, e.g. that the model changed.
				if m.flash == "" || strings.HasPrefix(m.flash, "reloaded ") {
					m.flash = "changed · running every test again"
				} else {
					m.flash += " · running every test again"
				}
			}
		}
		return m, tea.Batch(tick(), refresh, rerun)

	case checkMsg:
		m.gotCheck(msg)
		return m, nil

	case gridStartMsg, gridRowMsg, gridDoneMsg, askRowMsg, askDoneMsg:
		return m, m.updateGrid(msg)

	case animMsg:
		m.frame++
		r := m.run
		if r != nil && m.reveal < len(r.path) && time.Since(m.revealAt) >= stepDelay {
			m.reveal++
			m.revealAt = time.Now()
		}
		busy := m.checking() || m.gridRunning() || m.grid != nil && m.grid.asking || r != nil && (m.reveal < len(r.path) || time.Since(m.revealAt) < barFill || (!r.done && r.paused == ""))
		if !busy {
			m.ticking = false
			return m, nil
		}
		return m, animTick()

	case runsLoadedMsg:
		m.runsLoading, m.runsMod = false, time.Now()
		if msg.err != nil {
			m.flash = "runs: " + msg.err.Error()
		} else {
			m.setRuns(msg.runs)
		}
		return m, nil

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
		if m.record && m.runsPath == m.flow.RunsLocation() {
			m.runsMod = time.Time{} // list them again, with this run
			return m, m.refreshRuns()
		}
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
		case editRunsPath:
			return m.updateRunsPathEditor(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

func (m *Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.flash = ""
	if m.help {
		m.help = false
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.compare != nil {
		m.updateCompare(msg.String())
		return m, nil
	}
	if m.picking {
		return m, m.updateModels(msg.String())
	}
	if m.detail {
		switch msg.String() {
		case "esc", "enter", "q", "backspace":
			m.detail = false
		case "j", "down":
			m.nodeScroll++
		case "k", "up":
			m.nodeScroll = max(m.nodeScroll-1, 0)
		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}
	switch msg.String() {
	case "?":
		m.help = true
	case "R":
		m.focus = focusFlow
		return m, m.startGrid()
	case "A":
		return m, m.askMissing()
	case "left", "h":
		if m.flowView == viewGrid && m.grid != nil {
			m.gridCol = max(min(m.gridCol, len(m.gridColumns())-1)-1, 0)
		}
	case "right", "l":
		if m.flowView == viewGrid && m.grid != nil {
			m.gridCol = min(m.gridCol+1, max(len(m.gridColumns())-1, 0))
		}
	case "[":
		m.moveThreshold(-1)
	case "]":
		m.moveThreshold(1)
	case "g":
		if m.grid == nil {
			m.flash = "R runs every test"
			return m, nil
		}
		m.flowView, m.focus = viewGrid, focusFlow
	case "esc":
		if m.replaying() {
			m.closeRun()
		}
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
		switch {
		case m.focus == focusFlow && m.flowView == viewGrid:
			m.gridCursor = min(m.gridCursor+1, len(m.grid.rows)-1)
			return m, nil
		}
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
		switch {
		case m.focus == focusFlow && m.flowView == viewGrid:
			m.gridCursor = max(m.gridCursor-1, 0)
			return m, nil
		}
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
		m.loadHighlighted()
		if m.replaying() {
			m.replay = nil // run the loaded input again, live
			return m, m.startRun(false)
		}
		// A paused run carries on to the end, unless the inputs changed
		// since it started; then start fresh.
		if m.resumable() {
			m.stepping.Store(false)
			m.step()
			return m, m.kick()
		}
		return m, m.startRun(false)
	case "s":
		if m.focus == focusTests && !m.showRuns {
			m.loadHighlighted()
		}
		if m.replaying() {
			m.stepReplay()
			return m, m.kick()
		}
		if m.resumable() {
			m.step()
			return m, m.kick()
		}
		return m, m.startRun(true)
	case "x":
		if m.run != nil && !m.run.done && m.cancel != nil {
			m.cancel()
			m.run.done, m.run.current, m.run.paused = true, "", ""
			m.run.elapsed = time.Since(m.run.started)
			m.flash = "stopped"
		}
	case "m", "v":
		// Keep the same node selected across views.
		var id string
		if n := m.selected(); n != nil {
			id = n.ID
		}
		if m.flowView == viewGraph {
			if m.run == nil || len(m.run.path) == 0 {
				m.flash = "run first: r"
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
			m.flash = "LIVE: shell nodes run and http nodes send"
		} else {
			m.flash = "dry run: shell and http writes are only recorded"
		}
	case "b":
		return m, m.openModels()
	case "enter", "e":
		switch {
		case m.focus == focusFlow && m.flowView == viewGrid:
			m.openGridRow(m.gridCursor)
			return m, m.kick()
		case (m.focus == focusFlow || m.focus == focusNode) && m.selected() != nil:
			m.detail, m.nodeScroll = true, 0
		case m.focus == focusInputs && len(m.inputs) > 0:
			return m, m.editInput()
		case m.focus == focusTests && m.showRuns && len(m.runs) > 0:
			if m.replay == m.runs[m.runCursor] {
				m.closeRun() // Enter on the open run closes it
			} else {
				m.openRun(m.runCursor)
				return m, m.kick()
			}
		case m.focus == focusTests && !m.showRuns && len(m.cases) > 0:
			m.replay = nil
			m.useCase(m.caseCursor)
		}
	case "c":
		if m.focus == focusTests {
			m.markForCompare()
		}
	case "F":
		return m, func() tea.Msg { return backToPickerMsg{} }
	case "t":
		if m.runsPath != "" {
			m.showRuns = !m.showRuns
		}
	case "o":
		if m.focus == focusTests {
			m.showRuns = true
			return m, m.openRunsPrompt()
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
		if m.flowView == viewGrid && m.focus == focusFlow && len(m.shifts) > 0 {
			m.keepThresholds()
			return m, nil
		}
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

// loadHighlighted makes r (and s) in the tests pane act on the highlighted
// test or recorded run, so there's no need to load it with enter first. The
// test already loaded keeps any edits made to its input.
func (m *Model) loadHighlighted() {
	if m.focus != focusTests || m.editing != editNone {
		return
	}
	switch {
	case m.showRuns && len(m.runs) > 0:
		if m.replay != m.runs[m.runCursor] {
			m.openRun(m.runCursor)
		}
	case !m.showRuns && len(m.cases) > 0:
		if m.caseCursor != m.active || m.replaying() {
			m.replay = nil
			m.useCase(m.caseCursor)
		}
	}
}

// SetWriter makes llm nodes write with the named model instead of
// hunch.yaml's writer.
func (m *Model) SetWriter(name string) error {
	if name == "" {
		return nil
	}
	if _, ok := m.flow.Backends[name]; !ok {
		return fmt.Errorf("model %q is not in hunch.yaml (it has %s)", name, strings.Join(m.modelNames(), ", "))
	}
	m.writer = name
	m.revalidate()
	return nil
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
	m.editor.SetWidth(max(m.width-6, 10))
	m.editor.SetHeight(max(m.height-9, 3))
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
	return m.pausedRun() && sameInputs(m.run.inputs, m.state())
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
	m.replay, m.beforeRun = nil, nil
	m.runID++
	id := m.runID
	m.restartReveal()
	// The middle pane tells the run as it happens.
	if m.flowView != viewPath {
		m.flowView, m.cursor, m.nodeScroll = viewPath, 0, 0
	}
	m.run = &run{events: map[string]runner.Event{}, started: time.Now(), caseIdx: -1}
	if m.active >= 0 && !m.dirty {
		m.run.caseIdx = m.active
	}

	pf := m.pickedFlow()
	backends, err := backend.ForFlow(pf)
	if err != nil {
		m.run.err, m.run.done = err, true
		return nil
	}
	f := *pf
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
	// Record the run next to the flow, like hunch run does, so it shows up
	// with the flow's recorded runs.
	rec, closeRec := m.recorder(ctx)
	recID := trace.NewID()
	recBackend, recState := m.backend, maps.Clone(state)
	var recSteps []runner.Event // written once the run ends, so a stopped run leaves nothing half-written
	go func() {
		defer close(ch)
		defer closeRec()
		res, err := runner.Run(ctx, &f, state, runner.Options{
			Backends:        backends,
			Stdout:          io.Discard,
			DryRun:          !m.live,
			Fake:            fakes,
			ThresholdShifts: shiftsOf(m.shifts),
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
			OnEvent: func(ev runner.Event) {
				recSteps = append(recSteps, ev)
				send(eventMsg{id, ev})
			},
		})
		if ctx.Err() == nil { // a stopped or replaced run isn't kept
			rec.Start(recID, m.path, recBackend, recState)
			for _, ev := range recSteps {
				rec.Step(recID, ev)
			}
			rec.End(recID, res, err)
		}
		// A cancelled run has been replaced; nobody is waiting for its end.
		if ctx.Err() == nil {
			ch <- doneMsg{id, res.State, err}
		}
	}()
	return tea.Batch(m.next(), m.kick())
}

// recorder opens where this flow's runs are kept. With nowhere to record
// (runs: off) it returns a nil writer, which records nothing.
func (m *Model) recorder(ctx context.Context) (*trace.Writer, func()) {
	// Mock answers are made up; they'd only mislead anyone reading the runs.
	loc := m.flow.RunsLocation()
	if loc == "" || !m.record || m.mock() {
		return nil, func() {}
	}
	w, c, err := trace.Open(ctx, loc, 0)
	if err != nil {
		m.flash = "can't record runs: " + err.Error()
		return nil, func() {}
	}
	return w, func() { c.Close() }
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
	animations, m.animate, m.record = true, true, true
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
