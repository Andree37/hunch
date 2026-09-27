package tui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/trace"
)

// drive feeds cmd results back into the model until the run finishes.
func drive(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cmd != nil && time.Now().Before(deadline) {
		msg := cmd()
		if msg == nil {
			break
		}
		_, cmd = m.Update(msg)
		if m.run != nil && m.run.done {
			return
		}
	}
	if m.run == nil || !m.run.done {
		t.Fatal("run did not finish")
	}
}

func press(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestRunAndPoke(t *testing.T) {
	m, err := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 34})

	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	if m.run.err != nil {
		t.Fatal(m.run.err)
	}
	screen := ansi.Strip(m.View())
	for _, want := range []string{"●  YES/NO  worth_it", "YES 95%", "CHOICE  intent", "[favor]", "▰▰▰▱▱ 3.28", "5 steps", "confidence 0.95"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen missing %q:\n%s", want, screen)
		}
	}

	// Edit sender and save: that re-runs and marks what changed.
	m.Update(press("j")) // select intent
	m.Update(press("4")) // inputs pane
	m.Update(press("j")) // sender
	m.Update(press("enter"))
	m.editor.SetValue("boss")
	if _, cmd = m.Update(press("enter")); cmd != nil {
		t.Fatal("saving an input should not start a run")
	}
	_, cmd = m.Update(press("r"))
	drive(t, m, cmd)
	screen = ansi.Strip(m.View())
	if !strings.Contains(screen, "previous run: favor") || !strings.Contains(screen, "← changed") {
		t.Errorf("want previous-run diff on intent:\n%s", screen)
	}
}

func TestTypeSaveAsCaseAndCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	os.WriteFile(path, []byte(`backends: {m: {kind: mock, answers: {a: yes}}}
nodes:
  a: {bool: "Reply to {{who}}?", then: {yes: b, no: b}}
  b: {action: log, message: "{{a.answer}}"}
`), 0o644)

	m, err := New(path, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if len(m.inputs) != 1 || m.inputs[0].key != "who" || m.focus != focusInputs || m.active != -1 {
		t.Fatalf("want discovered empty input focused, got %+v focus=%v", m.inputs, m.focus)
	}
	if len(m.issues) != 0 {
		t.Errorf("inputs should satisfy refs, got %v", m.issues)
	}

	// Type an input, run it, save it as a new test.
	m.Update(press("enter"))
	m.editor.SetValue("ann")
	m.Update(press("enter"))
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	m.Update(press("n"))
	m.keyEditor.SetValue("ann-asks")
	m.Update(press("enter"))

	// The run on screen used these inputs, so its answer is kept too.
	casePath := filepath.Join(dir, "f.tests", "ann-asks.yaml")
	saved, err := os.ReadFile(casePath)
	if err != nil || !strings.Contains(string(saved), "input:\n  who: ann\nexpect:\n  a: yes\n") {
		t.Fatalf("saved case = %q, %v", saved, err)
	}
	if m.active != 0 || m.dirty || !strings.Contains(ansi.Strip(m.View()), "Inputs · test ann-asks") {
		t.Errorf("new case should be active: active=%d dirty=%v", m.active, m.dirty)
	}

	// Correct the expectation by hand, as a user would; the TUI picks it up.
	os.WriteFile(casePath, []byte(strings.Replace(string(saved), "a: yes", "a: no", 1)), 0o644)
	m.Update(tickMsg{})
	m.focus = focusTests
	if _, cmd = m.Update(press("enter")); cmd != nil {
		t.Fatal("loading a case should not start a run")
	}
	_, cmd = m.Update(press("r"))
	drive(t, m, cmd)
	screen := ansi.Strip(m.View())
	if !strings.Contains(screen, "✗ a = no got yes") || !strings.Contains(screen, "✗ ann-asks") {
		t.Errorf("want failed expectation shown:\n%s", screen)
	}

	// Editing marks the case dirty and hides stale results.
	m.focus = focusInputs
	m.Update(press("enter"))
	m.editor.SetValue("bob")
	m.Update(press("enter"))
	if !m.dirty || !strings.Contains(ansi.Strip(m.View()), "test ann-asks*") {
		t.Error("edited case should show as dirty")
	}
}

// pump processes run messages until the run pauses or finishes.
func pump(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cmd != nil && time.Now().Before(deadline) {
		msg := cmd()
		if msg == nil {
			return
		}
		_, cmd = m.Update(msg)
		if m.run.done || m.run.paused != "" {
			return
		}
	}
}

func TestStepThroughRun(t *testing.T) {
	m, err := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 34})

	_, cmd := m.Update(press("s"))
	pump(t, m, cmd)
	if len(m.run.events) != 1 || m.run.paused != "intent" {
		t.Fatalf("after first step: events=%d paused=%q", len(m.run.events), m.run.paused)
	}
	if screen := ansi.Strip(m.View()); !strings.Contains(screen, "●  YES/NO  worth_it") || !strings.Contains(screen, "⏸  CHOICE  intent") || !strings.Contains(screen, "next: intent") {
		t.Errorf("want paused marker:\n%s", screen)
	}

	m.Update(press("s"))
	pump(t, m, m.next())
	if len(m.run.events) != 2 || m.run.paused != "tone" || m.selected().ID != "intent" {
		t.Fatalf("after second step: events=%d paused=%q selected=%q", len(m.run.events), m.run.paused, m.selected().ID)
	}

	m.Update(press("r")) // run the rest
	pump(t, m, m.next())
	if !m.run.done || m.run.err != nil || len(m.run.path) != 5 {
		t.Fatalf("run to end: done=%v err=%v path=%v", m.run.done, m.run.err, m.run.path)
	}

	m.Update(press("3"))
	if m.focus != focusNode {
		t.Errorf("3 should focus the node pane, got %v", m.focus)
	}
}

func TestStopPausedRun(t *testing.T) {
	m, _ := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 34})
	_, cmd := m.Update(press("s"))
	pump(t, m, cmd)
	m.Update(press("x"))
	if !m.run.done || m.run.paused != "" || m.pausedRun() {
		t.Errorf("x should stop: done=%v paused=%q", m.run.done, m.run.paused)
	}
}

func TestTypedInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(path, []byte(`inputs:
  channel: [email, slack, sms]
  vip: {type: bool}
  count: {type: number, min: 1, max: 5}
nodes:
  a: {bool: "{{channel}} {{vip}} {{count}}?", then: {yes: b, no: b}}
  b: {action: log, message: "{{a.answer}}"}
`), 0o644)
	m, err := New(path, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})

	// Empty inputs block the run and point at the first one.
	if _, cmd := m.Update(press("r")); cmd != nil || m.flash != "fill in channel first" {
		t.Fatalf("run with empty inputs: cmd=%v flash=%q", cmd != nil, m.flash)
	}

	// Choice: enter opens a picker; j moves; enter picks.
	m.Update(press("enter"))
	if m.editing != editPick || len(m.pickOptions) != 3 {
		t.Fatalf("want picker with 3 options, got editing=%v %v", m.editing, m.pickOptions)
	}
	if screen := ansi.Strip(m.View()); !strings.Contains(screen, "▸ ○ email") {
		t.Errorf("want picker shown:\n%s", screen)
	}
	m.Update(press("j"))
	m.Update(press("enter"))
	if m.inputs[0].value != "slack" {
		t.Errorf("channel = %q", m.inputs[0].value)
	}

	// Bool: picker of yes / no, stored typed.
	m.Update(press("j"))
	m.Update(press("enter"))
	if !slices.Equal(m.pickOptions, []string{"yes", "no"}) {
		t.Fatalf("bool picker = %v", m.pickOptions)
	}
	m.Update(press("enter"))
	if m.state()["vip"] != true {
		t.Errorf("vip = %#v, want true", m.state()["vip"])
	}

	// Number: out of range is refused and the editor stays open.
	m.Update(press("j"))
	m.Update(press("enter"))
	m.editor.SetValue("9")
	m.Update(press("enter"))
	if m.editing != editValue || !strings.Contains(m.flash, "above the maximum") {
		t.Errorf("want number refused: editing=%v flash=%q", m.editing, m.flash)
	}
	m.editor.SetValue("3")
	m.Update(press("enter"))
	if m.editing != editNone || m.state()["count"] != 3.0 {
		t.Errorf("count = %#v", m.state()["count"])
	}

	screen := ansi.Strip(m.View())
	for _, want := range []string{"slack ▾", "YES", "3  1–5"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen missing %q:\n%s", want, screen)
		}
	}
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	if m.run.err != nil {
		t.Errorf("run: %v", m.run.err)
	}
}

func TestEditedInputsRestartPausedRun(t *testing.T) {
	m, _ := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 34})
	_, cmd := m.Update(press("s"))
	pump(t, m, cmd)
	paused := m.run

	m.inputs[1].value = "boss" // sender
	_, cmd = m.Update(press("r"))
	if m.run == paused || cmd == nil {
		t.Fatal("r after editing inputs should start a fresh run, not resume")
	}
	drive(t, m, cmd)
	if m.run.inputs["sender"] != "boss" {
		t.Errorf("fresh run inputs = %v", m.run.inputs)
	}
}

func TestSlowNodeShowsTimerAndRetries(t *testing.T) {
	m, _ := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 34})
	_, cmd := m.Update(press("s"))
	pump(t, m, cmd)
	// Simulate the next node being slow and retried.
	m.Update(startMsg{m.runID, "intent"})
	m.run.nodeAt = time.Now().Add(-12 * time.Second)
	m.Update(statusMsg{m.runID, "Jev overloaded (529), retrying in 2s (attempt 3/4)"})
	bar := ansi.Strip(m.statusBar())
	for _, want := range []string{"x stop", "running intent… 12.", "Jev overloaded (529), retrying in 2s (attempt 3/4)"} {
		if !strings.Contains(bar, want) {
			t.Errorf("status bar missing %q: %s", want, bar)
		}
	}
	m.Update(press("x"))
}

func TestActionRowsShowSlotsThenOutput(t *testing.T) {
	m, _ := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	screen := ansi.Strip(m.View())
	if !strings.Contains(screen, "Reply now: ⟨intent") || !strings.Contains(screen, "├─ no ▶ ○  SAY     archive ↑") {
		t.Errorf("want template slots before running:\n%s", screen)
	}
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	screen = ansi.Strip(m.View())
	if !strings.Contains(screen, "✓ Block time to answer carol: hi") || !strings.Contains(screen, "Outcome: schedule") {
		t.Errorf("want shell output after running:\n%s", screen)
	}
}

func TestPathView(t *testing.T) {
	m, _ := New("testdata/inbox.yaml", map[string]any{"sender": "carol", "message": "hi"}, "mock", false)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.Update(press("v")); m.flowView != viewGraph || m.flash != "run first to see its path" {
		t.Fatalf("v before a run: view=%v flash=%q", m.flowView, m.flash)
	}
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	m.Update(press("j")) // intent, in the tree
	m.Update(press("v"))
	if m.flowView != viewPath || m.selected().ID != "intent" {
		t.Fatalf("path view should keep intent selected, got view=%v sel=%q", m.flowView, m.selected().ID)
	}
	screen := ansi.Strip(m.View())
	for _, want := range []string{
		"Flow · path", "Result  schedule", "✓ Block time to answer carol: hi",
		" 1 ●  YES/NO  worth_it", "Does this message from carol need a response from me?",
		" 5 ●  SHELL   schedule", "CHOICE  intent       favor 48%",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("path view missing %q:\n%s", want, screen)
		}
	}
	// The skipped endings aren't in the path.
	if strings.Contains(screen, "ask_me") {
		t.Errorf("path view should only show what ran:\n%s", screen)
	}
	m.Update(press("v"))
	if m.flowView != viewGraph || m.selected().ID != "intent" {
		t.Errorf("back to graph should keep selection, got %q", m.selected().ID)
	}
}

func TestActionNodesAndDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(path, []byte(`backends: {m: {kind: mock, answers: {ok: yes}}}
nodes:
  draft: {action: llm, prompt: "Reply to {{who}}", then: ok}
  ok: {bool: "Send it?", then: {yes: send, no: result}}
  send: {action: http, url: "https://example.invalid/post", body: {text: "{{draft.text}}"}, then: result}
  result: {action: output, set: {action: sent, to: "{{who}}"}}
`), 0o644)
	m, err := New(path, map[string]any{"who": "ann"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	if bar := ansi.Strip(m.statusBar()); !strings.Contains(bar, "dry run") {
		t.Errorf("default should be dry run: %s", bar)
	}
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	if m.run.err != nil {
		t.Fatal(m.run.err)
	}
	screen := ansi.Strip(m.View())
	for _, want := range []string{"LLM     draft", "✎ [mock text", "HTTP    send", "DRY RUN POST https://example.invalid", "OUTPUT  result", "action=sent · to=ann"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen missing %q:\n%s", want, screen)
		}
	}
	m.Update(press("L"))
	if !m.live || !strings.Contains(ansi.Strip(m.statusBar()), "LIVE") {
		t.Error("L should switch to live")
	}
}

func TestSwitchNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(path, []byte(`nodes:
  scope: {switch: "{{format}}", then: {pdf: in, _: out}}
  in: {action: output, set: {scope: in}}
  out: {action: output, set: {scope: out}}
`), 0o644)
	m, err := New(path, map[string]any{"format": "pdf"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	if screen := ansi.Strip(m.View()); !strings.Contains(screen, "RULE    scope  on ⟨format⟩") {
		t.Errorf("want rule node before run:\n%s", screen)
	}
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	screen := ansi.Strip(m.View())
	for _, want := range []string{"RULE    scope  pdf", "pdf ▶ ●  OUTPUT  in", "value  pdf", "pdf ▶ → in"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen missing %q:\n%s", want, screen)
		}
	}
}

// recordRuns runs the test flow for each input and records it like serve does.
func recordRuns(t *testing.T, inputs ...map[string]any) string {
	t.Helper()
	return recordRunsFor(t, "testdata/inbox.yaml", inputs...)
}

func recordRunsFor(t *testing.T, flowPath string, inputs ...map[string]any) string {
	t.Helper()
	f, err := flow.Load(flowPath)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := backend.New(f.Backends["mock"])
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	file, _ := os.Create(path)
	defer file.Close()
	rec := trace.NewWriter(file)
	for _, in := range inputs {
		id := trace.NewID()
		rec.Start(id, flowPath, "mock", in)
		res, err := runner.Run(context.Background(), f, in, runner.Options{
			Backends: map[string]backend.Backend{"mock": b},
			OnEvent:  func(ev runner.Event) { rec.Step(id, ev) },
		})
		rec.End(id, res, err)
	}
	return path
}

func TestReplayRecordedRuns(t *testing.T) {
	runs := recordRuns(t,
		map[string]any{"sender": "carol", "message": "hi"},
		map[string]any{"sender": "boss", "message": "urgent"},
	)
	m, err := New("testdata/inbox.yaml", nil, "mock", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetRuns(runs); err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	screen := ansi.Strip(m.View())
	if !strings.Contains(screen, "[2] Runs · 2 of this flow") || !strings.Contains(screen, "message=urgent sender=boss") {
		t.Fatalf("want runs listed, newest first:\n%s", screen)
	}

	// Open the older run (carol), which went through 5 nodes.
	m.Update(press("2"))
	m.Update(press("j"))
	if _, cmd := m.Update(press("enter")); cmd != nil {
		t.Fatal("opening a run must not start a live one")
	}
	screen = ansi.Strip(m.View())
	for _, want := range []string{"Inputs · from run", "sender   carol", "replay 5/5", "●  SHELL   schedule"} {
		if !strings.Contains(screen, want) {
			t.Errorf("opened run: missing %q:\n%s", want, screen)
		}
	}

	// s restarts the replay and walks it one step at a time.
	m.Update(press("s"))
	if m.replayK != 1 || m.run.paused != "intent" || len(m.run.path) != 1 {
		t.Fatalf("after s: k=%d paused=%q path=%v", m.replayK, m.run.paused, m.run.path)
	}
	if bar := ansi.Strip(m.statusBar()); !strings.Contains(bar, "replay 1/5") {
		t.Errorf("status: %s", bar)
	}
	m.Update(press("s"))
	if m.selected().ID != "intent" || m.replayK != 2 {
		t.Errorf("replay should follow the step: sel=%q k=%d", m.selected().ID, m.replayK)
	}

	// r runs the same input again, live.
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	if m.replaying() || m.run.inputs["sender"] != "carol" || !m.run.done {
		t.Errorf("r should re-run the recorded input live: replaying=%v inputs=%v", m.replaying(), m.run.inputs)
	}
}

func TestReplayShowsRecordedFailure(t *testing.T) {
	runs := recordRuns(t, map[string]any{"sender": "carol"}) // no message: fails at worth_it
	m, _ := New("testdata/inbox.yaml", nil, "mock", false)
	m.SetRuns(runs)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m.Update(press("2"))
	m.Update(press("enter"))
	if m.run.errNode != "worth_it" || !strings.Contains(ansi.Strip(m.View()), "✗  YES/NO  worth_it") {
		t.Errorf("errNode=%q\n%s", m.run.errNode, ansi.Strip(m.View()))
	}
}

func TestReplayShowsRecordedBackend(t *testing.T) {
	runs := recordRuns(t, map[string]any{"sender": "carol", "message": "hi"})
	// Pretend it was recorded with another backend than the TUI's pick.
	data, _ := os.ReadFile(runs)
	os.WriteFile(runs, []byte(strings.ReplaceAll(string(data), `"backend":"mock"`, `"backend":"jev"`)), 0o644)
	m, _ := New("testdata/inbox.yaml", nil, "mock", false)
	m.SetRuns(runs)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m.Update(press("2"))
	m.Update(press("enter"))
	if title := ansi.Strip(m.nodeTitle()); !strings.Contains(title, "worth_it · jev") {
		t.Errorf("title = %q, want the recorded backend", title)
	}
}

func TestSaveReplayedRunAsTest(t *testing.T) {
	dir := t.TempDir()
	flowPath := filepath.Join(dir, "inbox.yaml")
	src, _ := os.ReadFile("testdata/inbox.yaml")
	os.WriteFile(flowPath, src, 0o644)
	runs := recordRunsFor(t, flowPath, map[string]any{"sender": "carol", "message": "hi"})

	m, err := New(flowPath, nil, "mock", false)
	if err != nil {
		t.Fatal(err)
	}
	m.SetRuns(runs)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	m.Update(press("2"))
	m.Update(press("enter"))
	m.Update(press("n"))
	m.keyEditor.SetValue("carol-says-hi")
	m.Update(press("enter"))

	saved, err := os.ReadFile(filepath.Join(dir, "inbox.tests", "carol-says-hi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"description: From run", "sender: carol", "expect:", "worth_it: yes", "intent: favor", "tone.urgency:", "effort:"} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("saved case missing %q:\n%s", want, saved)
		}
	}
	if !strings.Contains(m.flash, "expectations from the run") || m.active < 0 {
		t.Errorf("flash=%q active=%d", m.flash, m.active)
	}
	// Re-running it live against the saved expectations passes.
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	if rs := m.results["carol-says-hi"]; len(rs) == 0 || !cases.Passed(rs) {
		t.Errorf("results = %+v", rs)
	}
}

func TestRunsIncludeRolledOverFiles(t *testing.T) {
	older := recordRuns(t, map[string]any{"sender": "old", "message": "hi"})
	newer := recordRuns(t, map[string]any{"sender": "new", "message": "hi"})
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	data, _ := os.ReadFile(older)
	os.WriteFile(path+".1", data, 0o644)
	data, _ = os.ReadFile(newer)
	os.WriteFile(path, data, 0o644)

	m, _ := New("testdata/inbox.yaml", nil, "mock", false)
	if err := m.SetRuns(path); err != nil {
		t.Fatal(err)
	}
	if len(m.runs) != 2 || m.runs[0].Input["sender"] != "new" || m.runs[1].Input["sender"] != "old" {
		t.Errorf("runs = %d, first=%v", len(m.runs), m.runs[0].Input)
	}
}

func TestRunsFromADirectory(t *testing.T) {
	dir := t.TempDir()
	f, _ := flow.Load("testdata/inbox.yaml")
	b, _ := backend.New(f.Backends["mock"])
	w, _, err := trace.Open(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	record := func(sender string) {
		id := trace.NewID()
		in := map[string]any{"sender": sender, "message": "hi"}
		w.Start(id, "testdata/inbox.yaml", "mock", in)
		res, err := runner.Run(context.Background(), f, in, runner.Options{
			Backends: map[string]backend.Backend{"mock": b},
			OnEvent:  func(ev runner.Event) { w.Step(id, ev) },
		})
		w.End(id, res, err)
	}
	record("ann")

	m, _ := New("testdata/inbox.yaml", nil, "mock", false)
	if err := m.SetRuns(dir); err != nil {
		t.Fatal(err)
	}
	if len(m.runs) != 1 {
		t.Fatalf("runs = %d", len(m.runs))
	}

	// A run recorded later shows up on the next background listing.
	time.Sleep(5 * time.Millisecond) // a later start time sorts after
	record("bob")
	m.runsMod = time.Now().Add(-11 * time.Second)
	cmd := m.refreshRuns()
	if cmd == nil {
		t.Fatal("a directory should be re-listed in the background")
	}
	m.Update(cmd())
	if len(m.runs) != 2 || m.runs[0].Input["sender"] != "bob" || !strings.Contains(m.flash, "1 new run") {
		t.Errorf("runs=%d first=%v flash=%q", len(m.runs), m.runs[0].Input, m.flash)
	}
	if m.refreshRuns() != nil {
		t.Error("should not re-list again within 10s")
	}
}

func TestReplayedActionsShowTheirResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(path, []byte(`nodes:
  post: {action: http, url: "https://example.invalid", then: done}
  done: {action: output, set: {action: sent}}
`), 0o644)
	f, _ := flow.Load(path)
	runsPath := filepath.Join(t.TempDir(), "runs.jsonl")
	file, _ := os.Create(runsPath)
	rec := trace.NewWriter(file)
	rec.Start("r1", path, "mock", nil)
	res, err := runner.Run(context.Background(), f, nil, runner.Options{
		Fake:    map[string]runner.FakeResponse{"post": {Status: 201}},
		OnEvent: func(ev runner.Event) { rec.Step("r1", ev) },
	})
	rec.End("r1", res, err)
	file.Close()
	// Make the http output look like a real (not faked) response.
	data, _ := os.ReadFile(runsPath)
	os.WriteFile(runsPath, []byte(strings.ReplaceAll(string(data), `"faked":true`, `"faked":false`)), 0o644)

	m, _ := New(path, nil, "", false)
	m.SetRuns(runsPath)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m.Update(press("2"))
	m.Update(press("enter"))
	m.Update(press("1"))
	m.Update(press("v"))
	screen := ansi.Strip(m.View())
	for _, want := range []string{"HTTP    post         ✓ 201", "OUTPUT  done         action=sent"} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q:\n%s", want, screen)
		}
	}
}

func TestRunsOfOtherFlowsAreHidden(t *testing.T) {
	runs := recordRuns(t, map[string]any{"sender": "carol", "message": "hi"}) // recorded with testdata/inbox.yaml
	other := filepath.Join(t.TempDir(), "other.yaml")
	os.WriteFile(other, []byte(`nodes: {fetch: {action: log, message: hi}}`), 0o644)

	m, _ := New(other, nil, "", false)
	m.SetRuns(runs)
	m.Update(tea.WindowSizeMsg{Width: 170, Height: 40})
	screen := ansi.Strip(m.View())
	if len(m.runs) != 0 || !strings.Contains(screen, "Runs · 0 of this flow") ||
		!strings.Contains(screen, "1 run(s) of testdata/inbox.yaml hidden") || !strings.Contains(screen, "hunch tui testdata/inbox.yaml --runs") {
		t.Errorf("runs of another flow should be hidden and pointed to:\n%s", screen)
	}

	m2, _ := New("testdata/inbox.yaml", nil, "mock", false)
	m2.SetRuns(runs)
	if len(m2.runs) != 1 || len(m2.otherRuns) != 0 {
		t.Errorf("the right flow lists its run: runs=%d others=%v", len(m2.runs), m2.otherRuns)
	}
}

func TestRunOfOlderVersionIsMarked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	os.WriteFile(path, []byte(`nodes: {a: {action: log, message: one, then: b}, b: {action: log, message: two}}`), 0o644)
	f, _ := flow.Load(path)
	runsPath := filepath.Join(dir, "runs.jsonl")
	file, _ := os.Create(runsPath)
	rec := trace.NewWriter(file)
	rec.Start("r1", path, "", nil)
	res, err := runner.Run(context.Background(), f, nil, runner.Options{OnEvent: func(ev runner.Event) { rec.Step("r1", ev) }})
	rec.End("r1", res, err)
	file.Close()

	os.WriteFile(path, []byte(`nodes: {a: {action: log, message: one}}`), 0o644) // b removed
	m, _ := New(path, nil, "", false)
	m.SetRuns(runsPath)
	m.Update(tea.WindowSizeMsg{Width: 170, Height: 40})
	m.Update(press("2"))
	m.Update(press("enter"))
	screen := ansi.Strip(m.View())
	if !strings.Contains(screen, "older version of this flow") {
		t.Errorf("want older-version note:\n%s", screen)
	}
}

func TestFlowRunsLoadWithoutFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	src, _ := os.ReadFile("testdata/inbox.yaml")
	os.WriteFile(path, src, 0o644)

	// Nothing recorded yet: no runs, and reading doesn't create the folder.
	m, err := New(path, nil, "mock", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.showRuns || m.runsPath != filepath.Join(dir, "f.runs") {
		t.Errorf("runsPath=%q showRuns=%v", m.runsPath, m.showRuns)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.runs")); !os.IsNotExist(err) {
		t.Error("reading runs must not create the folder")
	}

	// Runs recorded into the flow's runs folder show up on their own.
	f, _ := flow.Load(path)
	b, _ := backend.New(f.Backends["mock"])
	w, _, _ := trace.Open(context.Background(), f.RunsLocation(), 0)
	w.Start("r1", path, "mock", map[string]any{"sender": "ann", "message": "hi"})
	res, rerr := runner.Run(context.Background(), f, map[string]any{"sender": "ann", "message": "hi"},
		runner.Options{Backends: map[string]backend.Backend{"mock": b}, OnEvent: func(ev runner.Event) { w.Step("r1", ev) }})
	w.End("r1", res, rerr)
	m2, _ := New(path, nil, "mock", false)
	if !m2.showRuns || len(m2.runs) != 1 {
		t.Errorf("want the flow's run listed: showRuns=%v runs=%d", m2.showRuns, len(m2.runs))
	}

	// o opens another place from inside the TUI.
	other := recordRunsFor(t, path, map[string]any{"sender": "bob", "message": "x"}, map[string]any{"sender": "cy", "message": "y"})
	m2.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m2.Update(press("2"))
	m2.Update(press("o"))
	if m2.editing != editRunsPath {
		t.Fatal("o should prompt for a location")
	}
	m2.keyEditor.SetValue(other)
	m2.Update(press("enter"))
	if m2.runsPath != other || len(m2.runs) != 2 || m2.editing != editNone {
		t.Errorf("after o: path=%q runs=%d", m2.runsPath, len(m2.runs))
	}
}

func TestFindFlows(t *testing.T) {
	flows := FindFlows("../../examples")
	var names []string
	for _, f := range flows {
		names = append(names, f.Path)
	}
	for _, want := range []string{"inbox.yaml", "respond.yaml", "triage.yaml"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	for _, f := range flows {
		if strings.Contains(f.Path, ".tests") {
			t.Errorf("test cases aren't flows: %s", f.Path)
		}
		if f.Path == "triage.yaml" && (f.Name != "network-triage" || f.Tests != 12 || f.Nodes < 20 || !strings.HasPrefix(f.About, "Triage for a network team")) {
			t.Errorf("triage info = %+v", f)
		}
	}
}

func TestPickerOpensAFlowAndComesBack(t *testing.T) {
	dir := t.TempDir()
	src, _ := os.ReadFile("testdata/inbox.yaml")
	os.WriteFile(filepath.Join(dir, "a.yaml"), src, 0o644)
	os.WriteFile(filepath.Join(dir, "not-a-flow.yaml"), []byte("hello: world\n"), 0o644)

	a := NewApp(dir, OpenOptions{FromCase: true})
	a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if len(a.flows) != 1 || !strings.Contains(ansi.Strip(a.View()), "Pick a flow · 1 found") {
		t.Fatalf("flows = %+v\n%s", a.flows, ansi.Strip(a.View()))
	}
	a.Update(press("enter"))
	if a.main == nil || !strings.Contains(ansi.Strip(a.View()), "[1] Flow") {
		t.Fatalf("enter should open the flow:\n%s", ansi.Strip(a.View()))
	}
	_, cmd := a.Update(press("F"))
	a.Update(cmd())
	if a.main != nil || !strings.Contains(ansi.Strip(a.View()), "Pick a flow") {
		t.Errorf("F should go back to the picker")
	}
}

func TestCloseARun(t *testing.T) {
	runs := recordRuns(t, map[string]any{"sender": "carol", "message": "hi"}, map[string]any{"sender": "bob", "message": "yo"})
	m, _ := New("testdata/inbox.yaml", map[string]any{"sender": "me", "message": "typed"}, "mock", false)
	m.SetRuns(runs)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	m.Update(press("2"))

	m.Update(press("enter"))
	if !m.replaying() || m.inputs[1].value != "bob" {
		t.Fatalf("open: replaying=%v inputs=%v", m.replaying(), m.inputs)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.replaying() || m.run != nil || m.inputs[0].value != "typed" || m.inputs[1].value != "me" {
		t.Errorf("esc should close and restore: replaying=%v run=%v inputs=%v", m.replaying(), m.run, m.inputs)
	}
	if strings.Contains(ansi.Strip(m.View()), "replay ") {
		t.Error("status bar still shows a replay")
	}

	// Enter on the open run closes it; on another run switches to it.
	m.Update(press("enter"))
	m.Update(press("j"))
	m.Update(press("enter"))
	if !m.replaying() || m.inputs[1].value != "carol" {
		t.Fatalf("switch: inputs=%v", m.inputs)
	}
	m.Update(press("enter"))
	if m.replaying() || m.inputs[1].value != "me" {
		t.Errorf("enter on the open run should close it: replaying=%v inputs=%v", m.replaying(), m.inputs)
	}
}
