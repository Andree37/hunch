package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/trace"
)

// Recorded runs (from `hunch serve --trace` or `hunch run --trace`) can be
// opened with --runs, listed in pane 2, loaded into the views and replayed
// one step at a time.

// SetRuns makes the TUI read recorded runs from path and show them in pane 2.
func (m *Model) SetRuns(path string) error {
	m.runsPath = path
	if err := m.loadRuns(); err != nil {
		return err
	}
	m.showRuns = true
	return nil
}

// useFlowRuns reads the runs from the flow's own runs location, if it has
// one, and shows them when there are any.
func (m *Model) useFlowRuns() {
	loc := m.flow.RunsLocation()
	if loc == "" {
		return
	}
	m.runsPath = loc
	if err := m.loadRuns(); err != nil {
		m.flash = "runs: " + err.Error()
		return
	}
	m.showRuns = len(m.runs) > 0
}

// openRunsPrompt asks for another place to read runs from.
func (m *Model) openRunsPrompt() tea.Cmd {
	m.editing = editRunsPath
	m.keyEditor.Prompt = "runs from (folder, .jsonl or s3://…): "
	m.keyEditor.SetValue(m.runsPath)
	m.keyEditor.CursorEnd()
	return m.keyEditor.Focus()
}

func (m *Model) updateRunsPathEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = editNone
		m.keyEditor.Blur()
		return m, nil
	case "enter":
		loc := strings.TrimSpace(m.keyEditor.Value())
		if loc == "" {
			return m, nil
		}
		if err := m.SetRuns(loc); err != nil {
			m.flash = "can't read runs: " + err.Error()
			return m, nil
		}
		m.editing = editNone
		m.keyEditor.Blur()
		m.runCursor = 0
		m.flash = fmt.Sprintf("%d run(s) of this flow in %s", len(m.runs), loc)
		return m, nil
	}
	var cmd tea.Cmd
	m.keyEditor, cmd = m.keyEditor.Update(msg)
	return m, cmd
}

func (m *Model) loadRuns() error {
	runs, err := trace.ReadAll(context.Background(), m.runsPath)
	if err != nil {
		return err
	}
	m.setRuns(runs)
	m.runsMod = m.runsStamp()
	return nil
}

// setRuns keeps the runs recorded with this flow, newest first; runs of
// other flows in the same place are only counted.
func (m *Model) setRuns(runs []*trace.Run) {
	runs, m.otherRuns = trace.OfFlow(runs, m.path)
	slices.Reverse(runs) // newest first
	prev := len(m.runs)
	m.runs = runs
	m.runCursor = max(min(m.runCursor, len(runs)-1), 0)
	if prev > 0 && len(runs) > prev {
		m.flash = fmt.Sprintf("%d new run(s) recorded", len(runs)-prev)
	}
}

// runsFile reports whether the runs come from a single .jsonl file (cheap
// to check every tick) rather than a directory or S3.
func (m *Model) runsFile() bool {
	st, err := os.Stat(m.runsPath)
	return err == nil && !st.IsDir()
}

func (m *Model) runsStamp() time.Time {
	if m.runsFile() {
		if st, err := os.Stat(m.runsPath); err == nil {
			return st.ModTime()
		}
	}
	return time.Now() // for directories and S3: when they were last listed
}

type runsLoadedMsg struct {
	runs []*trace.Run
	err  error
}

// refreshRuns picks up newly recorded runs: a file when it changes, a
// directory or S3 every 10s, listed in the background so the UI never waits.
func (m *Model) refreshRuns() tea.Cmd {
	if m.runsPath == "" || m.runsLoading {
		return nil
	}
	if m.runsFile() {
		if !m.runsStamp().Equal(m.runsMod) {
			m.loadRuns()
		}
		return nil
	}
	if time.Since(m.runsMod) < 10*time.Second {
		return nil
	}
	m.runsLoading = true
	path := m.runsPath
	return func() tea.Msg {
		runs, err := trace.ReadAll(context.Background(), path)
		return runsLoadedMsg{runs, err}
	}
}

// openRun loads a recorded run: its input into the Inputs pane and all its
// steps into the views.
func (m *Model) openRun(i int) {
	if m.cancel != nil {
		m.cancel()
	}
	tr := m.runs[i]
	m.inputs = nil
	for _, k := range slices.Sorted(maps.Keys(tr.Input)) {
		m.inputs = append(m.inputs, input{k, tmpl.Format(tr.Input[k])})
	}
	m.active, m.dirty = -1, false
	if m.run != nil && m.run.done && m.run.err == nil {
		m.prev = m.run
	}
	m.runID++ // ignore messages from any live run
	m.replay, m.replayK = tr, len(tr.Steps)
	m.run = replayRun(tr, m.replayK)
	m.flash = "opened run " + tr.ID
	if m.oldVersion(tr) {
		m.flash = "recorded with an older version of this flow"
	}
}

// oldVersion reports whether a run of this flow went through steps the flow
// no longer has, i.e. it was recorded before the flow was edited.
func (m *Model) oldVersion(tr *trace.Run) bool {
	if m.flow == nil {
		return false
	}
	for _, ev := range tr.Steps {
		if m.flow.Node(ev.Node) == nil {
			return true
		}
	}
	for _, id := range tr.Path {
		if m.flow.Node(id) == nil {
			return true
		}
	}
	return false
}

// oldVersionLines explains, above the graph, why parts of an older run
// don't show.
func (m *Model) oldVersionLines() []string {
	if m.replay == nil || !m.oldVersion(m.replay) {
		return nil
	}
	return []string{
		sYellow.Render("⚠ This run was recorded with an older version of this flow."),
		sYellow.Render("  Steps that no longer exist aren't shown; r runs its input on the flow as it is now."),
		"",
	}
}

// otherRunsLines says which runs in the same place belong to other flows,
// and how to open them.
func (m *Model) otherRunsLines() []string {
	var lines []string
	for _, flow := range slices.Sorted(maps.Keys(m.otherRuns)) {
		lines = append(lines,
			sDim.Render(fmt.Sprintf("%d run(s) of %s hidden; open them with:", m.otherRuns[flow], flow)),
			sDim.Render("  hunch tui "+flow+" --runs "+m.runsPath))
	}
	return lines
}

// replaying reports whether the views show a recorded run.
func (m *Model) replaying() bool { return m.replay != nil }

// stepReplay shows one more recorded step; from the end it starts over.
func (m *Model) stepReplay() {
	if m.replayK >= len(m.replay.Steps) {
		m.replayK = 0
	}
	m.replayK++
	m.run = replayRun(m.replay, m.replayK)
	if m.replayK <= len(m.replay.Steps) {
		m.selectNode(m.replay.Steps[m.replayK-1].Node)
	}
}

// replayRun rebuilds what the views need from the first k recorded steps.
func replayRun(tr *trace.Run, k int) *run {
	r := &run{events: map[string]runner.Event{}, caseIdx: -1, inputs: tr.Input, started: tr.Time}
	for _, ev := range tr.Steps[:k] {
		r.events[ev.Node] = ev
		r.path = append(r.path, ev.Node)
		r.cost += ev.CostUSD
		r.elapsed += ev.Latency
	}
	if k < len(tr.Steps) {
		r.paused = tr.Steps[k].Node // the next recorded step
		return r
	}
	r.done = true
	if tr.Error != "" && len(tr.Path) > 0 {
		// The failing node started but never recorded a step.
		r.path = tr.Path
		r.errNode = tr.Path[len(tr.Path)-1]
		r.err = errors.New(tr.Error)
	}
	return r
}

func (m *Model) runLines(w, h int) []string {
	if len(m.runs) == 0 {
		return append([]string{sDim.Render("no runs of this flow in " + m.runsPath + " yet")}, m.otherRunsLines()...)
	}
	var lines []string
	for i, tr := range m.runs {
		icon := sGreen.Render("✓")
		switch {
		case !tr.Done:
			icon = sYellow.Render("◐")
		case tr.Error != "":
			icon = sRed.Render("✗")
		}
		marker := " "
		if i == m.runCursor && m.focus == focusTests {
			marker = sTitle.Render("▸")
		}
		when := tr.Time.Local().Format("Jan 2 15:04:05")
		if time.Since(tr.Time) < 24*time.Hour {
			when = tr.Time.Local().Format("15:04:05")
		}
		end := ""
		if len(tr.Path) > 0 {
			end = "→ " + tr.Path[len(tr.Path)-1]
		}
		if m.oldVersion(tr) {
			icon = sYellow.Render("⚠")
			end += "  (older version of this flow)"
		}
		line := fmt.Sprintf("%s%s %s  %s  %s", marker, icon, sDim.Render(when), inputSummary(tr.Input), sDim.Render(end))
		if m.replay == tr {
			line += sTitle.Render("  ◀")
		}
		lines = append(lines, line)
	}
	if top := m.runCursor - h + 1; top > 0 {
		lines = lines[top:]
	}
	if others := m.otherRunsLines(); len(others) > 0 {
		lines = append(append(lines, ""), others...)
	}
	return lines
}

// inputSummary is a short one-line view of a run's input.
func inputSummary(in map[string]any) string {
	parts := make([]string, 0, len(in))
	for _, k := range slices.Sorted(maps.Keys(in)) {
		v := tmpl.Format(in[k])
		if len(v) > 24 {
			v = v[:24] + "…"
		}
		parts = append(parts, k+"="+strings.ReplaceAll(v, "\n", " "))
	}
	return strings.Join(parts, " ")
}
