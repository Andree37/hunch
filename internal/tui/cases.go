package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/tmpl"
)

// loadCases (re)reads the flow's test cases, keeping the active one by name.
func (m *Model) loadCases() error {
	var activeName string
	if m.active >= 0 {
		activeName = m.cases[m.active].Name
	}
	cs, err := cases.Load(cases.Dir(m.path))
	m.casesStamp = casesStamp(m.path)
	if err != nil {
		return err
	}
	m.cases = cs
	m.active = slices.IndexFunc(cs, func(c *cases.Case) bool { return c.Name == activeName })
	if activeName == "" {
		m.active = -1
	}
	m.caseCursor = max(min(m.caseCursor, len(cs)-1), 0)
	return nil
}

// casesStamp fingerprints the cases dir so edits made outside the TUI show up.
func casesStamp(flowPath string) string {
	paths, _ := filepath.Glob(filepath.Join(cases.Dir(flowPath), "*.yaml"))
	var b strings.Builder
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", p, st.ModTime().UnixNano(), st.Size())
		}
	}
	return b.String()
}

// useCase replaces the inputs with case i's, plus any other inputs the flow
// reads, left empty.
func (m *Model) useCase(i int) {
	c := m.cases[i]
	m.inputs = nil
	for _, k := range c.Keys {
		m.inputs = append(m.inputs, input{k, tmpl.Format(c.Input[k])})
	}
	if m.flow != nil {
		for _, k := range m.flow.Inputs() {
			if !slices.Contains(c.Keys, k) {
				m.inputs = append(m.inputs, input{k, ""})
			}
		}
		m.revalidate()
	}
	m.active, m.caseCursor, m.dirty = i, i, false
	m.inputCursor = 0
	m.flash = "test: " + c.Name
}

func (m *Model) saveCase(c *cases.Case) {
	state := m.state()
	c.Keys, c.Input = nil, map[string]any{}
	for _, in := range m.inputs {
		c.Keys = append(c.Keys, in.key)
		c.Input[in.key] = state[in.key]
	}
	if err := c.Save(); err != nil {
		m.flash = "save failed: " + err.Error()
		return
	}
	m.dirty = false
	m.flash = "saved " + c.Path
	m.loadCases()
}

func (m *Model) nameCase() tea.Cmd {
	m.editing = editCaseName
	m.keyEditor.Prompt = "new test name: "
	m.keyEditor.SetValue("")
	return m.keyEditor.Focus()
}

func (m *Model) updateCaseNameEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = editNone
		m.keyEditor.Blur()
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.keyEditor.Value())
		if !cases.ValidName(name) {
			m.flash = "not a usable file name"
			return m, nil
		}
		path := filepath.Join(cases.Dir(m.path), name+".yaml")
		if _, err := os.Stat(path); err == nil {
			m.flash = name + " already exists"
			return m, nil
		}
		m.editing = editNone
		m.keyEditor.Blur()
		m.saveCase(&cases.Case{Name: name, Path: path})
		m.active = slices.IndexFunc(m.cases, func(c *cases.Case) bool { return c.Name == name })
		m.caseCursor = max(m.active, 0)
		// The run that just happened was on these exact inputs.
		if m.run != nil && m.run.done && m.run.caseIdx < 0 {
			m.run.caseIdx = m.active
			m.checkRun()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.keyEditor, cmd = m.keyEditor.Update(msg)
	return m, cmd
}

// checkRun scores the finished run against its test case's expectations.
func (m *Model) checkRun() {
	r := m.run
	if r == nil || !r.done || r.err != nil || r.caseIdx < 0 || r.caseIdx >= len(m.cases) || m.flow == nil {
		return
	}
	c := m.cases[r.caseIdx]
	m.results[c.Name] = c.Check(m.flow, r.state)
}
