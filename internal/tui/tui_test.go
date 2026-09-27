package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
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

	casePath := filepath.Join(dir, "f.tests", "ann-asks.yaml")
	saved, err := os.ReadFile(casePath)
	if err != nil || string(saved) != "input:\n  who: ann\n" {
		t.Fatalf("saved case = %q, %v", saved, err)
	}
	if m.active != 0 || m.dirty || !strings.Contains(ansi.Strip(m.View()), "Inputs · test ann-asks") {
		t.Errorf("new case should be active: active=%d dirty=%v", m.active, m.dirty)
	}

	// Add an expectation by hand, as a user would; the TUI picks it up.
	os.WriteFile(casePath, append(saved, "expect:\n  a: no\n"...), 0o644)
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
  scope: {switch: "{{sev}}", then: {sev2: in, _: out}}
  in: {action: output, set: {scope: in}}
  out: {action: output, set: {scope: out}}
`), 0o644)
	m, err := New(path, map[string]any{"sev": "sev2"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	if screen := ansi.Strip(m.View()); !strings.Contains(screen, "RULE    scope  on ⟨sev⟩") {
		t.Errorf("want rule node before run:\n%s", screen)
	}
	_, cmd := m.Update(press("r"))
	drive(t, m, cmd)
	screen := ansi.Strip(m.View())
	for _, want := range []string{"RULE    scope  sev2", "sev2 ▶ ●  OUTPUT  in", "value  sev2", "sev2 ▶ → in"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen missing %q:\n%s", want, screen)
		}
	}
}
