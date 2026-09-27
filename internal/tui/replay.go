package tui

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

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
	f, err := os.Open(m.runsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	runs, err := trace.Read(f)
	if err != nil {
		return fmt.Errorf("%s: %w", m.runsPath, err)
	}
	slices.Reverse(runs) // newest first
	m.runs, m.runsMod = runs, st.ModTime()
	m.runCursor = max(min(m.runCursor, len(runs)-1), 0)
	return nil
}

// refreshRuns rereads the runs file if it grew, e.g. while serve records.
func (m *Model) refreshRuns() {
	if m.runsPath == "" {
		return
	}
	if st, err := os.Stat(m.runsPath); err == nil && !st.ModTime().Equal(m.runsMod) {
		prev := len(m.runs)
		if m.loadRuns() == nil && len(m.runs) > prev {
			m.flash = fmt.Sprintf("%d new run(s) recorded", len(m.runs)-prev)
		}
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
