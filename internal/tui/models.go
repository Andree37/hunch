package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
)

// b lists the models in hunch.yaml: enter decides with one, w writes with
// one (for llm nodes). The next run, test or grid uses them. Opening the
// list asks each model one small question, to show which ones answer.

type modelCheck struct {
	done bool
	took time.Duration
	err  string
}

type checkMsg struct {
	stamp, name string
	took        time.Duration
	err         error
}

// checkTimeout is how long a model gets to answer the check.
const checkTimeout = 15 * time.Second

// checkModels asks every model one yes/no question, unless they were
// checked against this hunch.yaml already.
func (m *Model) checkModels() tea.Cmd {
	stamp := m.projStamp
	if m.checks != nil && m.checksFor == stamp {
		return nil
	}
	m.checks, m.checksFor = map[string]*modelCheck{}, stamp
	var cmds []tea.Cmd
	for _, name := range m.modelNames() {
		cfg := m.flow.Backends[name]
		cfg.Options = maps.Clone(cfg.Options)
		cfg.Options["max_retries"] = 0 // a check says what's wrong, it doesn't retry
		m.checks[name] = &modelCheck{}
		cmds = append(cmds, func() tea.Msg {
			start := time.Now()
			b, err := backend.New(cfg)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
				defer cancel()
				_, err = b.Decide(ctx, map[string]any{"message": "Hello, is this thing on?"},
					[]flow.Question{{ID: "check", Name: "check", Kind: flow.Bool, Text: "Is this a greeting?"}})
			}
			return checkMsg{stamp, name, time.Since(start), err}
		})
	}
	return tea.Batch(append(cmds, m.kick())...)
}

func (m *Model) checking() bool {
	for _, c := range m.checks {
		if !c.done {
			return true
		}
	}
	return false
}

func (m *Model) gotCheck(msg checkMsg) {
	c, ok := m.checks[msg.name]
	if !ok || msg.stamp != m.checksFor {
		return
	}
	c.done, c.took = true, msg.took
	if msg.err != nil {
		c.err = checkError(msg.err)
	}
}

// checkError says in a few words why a model didn't answer.
func checkError(err error) string {
	e := err.Error()
	low := strings.ToLower(e)
	switch {
	case strings.Contains(e, "401") || strings.Contains(e, "403") || strings.Contains(low, "unauthorized") || strings.Contains(low, "no key"):
		return "the key is wrong or missing"
	case strings.Contains(e, "404"):
		return "nothing at that URL path"
	case strings.Contains(low, "connection refused"):
		return "can't reach it: is it running?"
	case strings.Contains(low, "no such host"):
		return "can't reach it: no such host"
	case strings.Contains(low, "deadline exceeded") || strings.Contains(low, "timeout"):
		return fmt.Sprintf("no answer in %s", checkTimeout)
	}
	if first, _, _ := strings.Cut(e, "\n"); len(first) > 80 {
		return first[:79] + "…"
	} else {
		return first
	}
}

func (m *Model) modelNames() []string {
	if m.flow.Project != nil {
		return m.flow.Project.Order
	}
	return []string{"mock"}
}

func (m *Model) openModels() tea.Cmd {
	names := m.modelNames()
	m.picking = true
	m.modelCursor = max(slices.Index(names, m.backend), 0)
	return m.checkModels()
}

func (m *Model) updateModels(key string) tea.Cmd {
	names := m.modelNames()
	m.modelCursor = max(min(m.modelCursor, len(names)-1), 0) // hunch.yaml may have changed
	switch key {
	case "j", "down":
		m.modelCursor = min(m.modelCursor+1, len(names)-1)
	case "k", "up":
		m.modelCursor = max(m.modelCursor-1, 0)
	case "enter":
		m.backend, m.pickedModel = names[m.modelCursor], true
		m.picking = false
		m.revalidate()
		m.flash = "deciding with " + m.backend + " · r or R to run"
	case "w":
		name := names[m.modelCursor]
		if !backend.CanWrite(m.flow.Backends[name].Kind) {
			m.flash = name + " only decides; pick a chat model to write"
			return nil
		}
		m.writer = name
		m.picking = false
		m.revalidate()
		m.flash = "writing with " + name
	case "c":
		m.checks = nil
		return m.checkModels()
	case "esc", "q", "b":
		m.picking = false
	}
	return nil
}

func (m *Model) modelsView(h int) string {
	names := m.modelNames()
	lines := []string{}
	nameW := 4
	for _, n := range names {
		nameW = max(nameW, lipgloss.Width(n))
	}
	writer := m.writer
	if writer == "" {
		writer = m.flow.WriterBackend
	}
	for i, n := range names {
		cfg := m.flow.Backends[n]
		marker := " "
		name := fmt.Sprintf("%-*s", nameW, n)
		if i == m.modelCursor {
			marker, name = sTitle.Render("▸"), sBold.Render(name)
		}
		where, _ := cfg.Options["endpoint_url"].(string)
		if cfg.Kind == "mock" {
			where = "offline, made-up answers"
		}
		if model, ok := cfg.Options["model"].(string); ok && model != "" {
			where += " · " + model
		}
		var roles []string
		if n == m.backend {
			roles = append(roles, sGreen.Render("decides"))
		}
		if n == writer {
			roles = append(roles, sAnswer.Render("writes"))
		}
		role := ""
		for _, r := range roles {
			role += "  " + r
		}
		status := ""
		switch c := m.checks[n]; {
		case c == nil:
		case !c.done:
			status = sYellow.Render(m.spinner() + " checking")
		case c.err != "":
			status = sRed.Render("✗ " + c.err)
		default:
			status = sGreen.Render(fmt.Sprintf("✓ %dms", c.took.Milliseconds()))
		}
		lines = append(lines, marker+" "+name+"  "+pad(status, 40)+"  "+sDim.Render(kindLabel(cfg.Kind)+"  "+where)+role)
	}
	from := "only mock: add a hunch.yaml to use real models"
	if m.flow.Project != nil {
		from = "from " + m.flow.Project.Path
	}
	lines = append(lines, "", sDim.Render(from), sDim.Render("Each model is asked one yes/no question when the list opens; c asks again."))
	return pane(paneTitle(1, "Models", "enter decide with it · w write with it · c check again · esc back"), lines, m.width, h, true)
}

func pad(s string, w int) string {
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

func kindLabel(kind string) string {
	switch kind {
	case "jev":
		return "decision model"
	case "openai", "anthropic", "bedrock":
		return "chat model    "
	case "mock":
		return "mock          "
	}
	return kind
}
