package tui

import (
	"fmt"
	"slices"

	"github.com/charmbracelet/lipgloss"

	"github.com/Andree37/hunch/internal/backend"
)

// b lists the models in hunch.yaml: enter decides with one, w writes with
// one (for llm nodes). The next run, test or grid uses them.

func (m *Model) modelNames() []string {
	if m.flow.Project != nil {
		return m.flow.Project.Order
	}
	return []string{"mock"}
}

func (m *Model) openModels() {
	names := m.modelNames()
	m.picking = true
	m.modelCursor = max(slices.Index(names, m.backend), 0)
}

func (m *Model) updateModels(key string) {
	names := m.modelNames()
	switch key {
	case "j", "down":
		m.modelCursor = min(m.modelCursor+1, len(names)-1)
	case "k", "up":
		m.modelCursor = max(m.modelCursor-1, 0)
	case "enter":
		m.backend = names[m.modelCursor]
		m.picking = false
		m.flash = "deciding with " + m.backend + " · r or R to run"
	case "w":
		name := names[m.modelCursor]
		if !backend.CanWrite(m.flow.Backends[name].Kind) {
			m.flash = name + " only decides; pick a chat model to write"
			return
		}
		m.writer = name
		m.picking = false
		m.flash = "writing with " + name
	case "esc", "q", "b":
		m.picking = false
	}
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
		lines = append(lines, marker+" "+name+"  "+sDim.Render(kindLabel(cfg.Kind)+"  "+where)+role)
	}
	from := "only mock: add a hunch.yaml to use real models"
	if m.flow.Project != nil {
		from = "from " + m.flow.Project.Path
	}
	lines = append(lines, "", sDim.Render(from))
	return pane(paneTitle(1, "Models", "enter decide with it · w write with it · esc back"), lines, m.width, h, true)
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
