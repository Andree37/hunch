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

func (m *Model) loadRuns() error {
	runs, err := trace.ReadAll(context.Background(), m.runsPath)
	if err != nil {
		return err
	}
	m.setRuns(runs)
	m.runsMod = m.runsStamp()
	return nil
}

func (m *Model) setRuns(runs []*trace.Run) {
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
	if m.otherFlow(tr) {
		m.flash = "this run was recorded with " + flowName(tr) + ", not this flow"
	}
}

// otherFlow reports whether a recorded run came from a different flow than
// the one open: it went through steps this flow doesn't have.
func (m *Model) otherFlow(tr *trace.Run) bool {
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

func flowName(tr *trace.Run) string {
	if tr.Flow == "" {
		return "another flow"
	}
	return tr.Flow
}

// otherFlowLines explains, above the graph, why a run from another flow
// shows nothing, and how to open it properly.
func (m *Model) otherFlowLines() []string {
	if m.replay == nil || !m.otherFlow(m.replay) {
		return nil
	}
	return []string{
		sYellow.Render("⚠ This run was recorded with " + flowName(m.replay) + ", not " + m.path + "."),
		sYellow.Render("  Its steps don't exist here, so nothing below shows as run. Open it with:"),
		"  " + sBold.Render("hunch tui "+flowName(m.replay)+" --runs "+m.runsPath),
		"",
	}
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
		return []string{sDim.Render("no runs recorded in " + m.runsPath + " yet")}
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
		if m.otherFlow(tr) {
			icon = sYellow.Render("⚠")
			end = "other flow: " + flowName(tr)
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
