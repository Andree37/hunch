package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
)

// Each node kind gets its own colour and badge, so the flow reads at a glance.
var (
	pink   = lipgloss.Color("#FF69B4")
	orange = lipgloss.Color("#FFA07A")
	blue   = lipgloss.Color("#74B9FF")
	violet = lipgloss.Color("#9B8CFF")
	white  = lipgloss.Color("#DDD6FE")
	slate  = lipgloss.Color("#A0AEC0")

	sInk  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1A202C")).Bold(true)
	sSlot = lipgloss.NewStyle().Foreground(lilac).Italic(true)
)

func kindStyle(n *flow.Node) (label string, c lipgloss.Color) {
	switch n.Kind {
	case flow.Bool:
		return "YES/NO", cyan
	case flow.Choice:
		return "CHOICE", pink
	case flow.Score:
		return "SCORE", orange
	case flow.Questions:
		return "MULTI", blue
	case flow.Switch:
		return "RULE", slate
	}
	switch n.Action.Type {
	case flow.ActShell:
		return "SHELL", lilac
	case flow.ActLLM:
		return "LLM", violet
	case flow.ActHTTP:
		return "HTTP", yellow
	case flow.ActOutput:
		return "OUTPUT", white
	}
	return "SAY", green
}

func badge(n *flow.Node) string {
	label, c := kindStyle(n)
	return sInk.Background(c).Render(fmt.Sprintf(" %-6s ", label))
}

// nodeDetail is the kind-specific part of a flow row: what the node can
// answer before it runs, and what it answered after.
func nodeDetail(n *flow.Node, ev runner.Event, ran bool) string {
	var s string
	switch n.Kind {
	case flow.Bool, flow.Choice, flow.Score:
		q := n.Questions[0]
		if ran && len(ev.Decisions) > 0 {
			s = answerView(q, ev.Decisions[0])
		} else {
			s = emptyView(q)
		}
	case flow.Questions:
		parts := make([]string, len(n.Questions))
		for i, q := range n.Questions {
			v := sDim.Render("?")
			if ran && i < len(ev.Decisions) {
				v = compactAnswer(q, ev.Decisions[i])
			}
			parts[i] = sDim.Render(q.Name+" ") + v
		}
		s = strings.Join(parts, sDim.Render(" · "))
	case flow.Action:
		s = actionView(n.Action, ev, ran)
	case flow.Switch:
		if ran {
			out, _ := ev.Output.(map[string]any)
			s = lipgloss.NewStyle().Foreground(slate).Bold(true).Render(tmpl.Format(out["value"]))
		} else {
			s = sDim.Render("on ") + slots(n.Switch)
		}
	}
	if ran && ev.Branch == flow.Unsure {
		s += sYellow.Render(" unsure")
	}
	return s
}

func emptyView(q flow.Question) string {
	switch q.Kind {
	case flow.Bool:
		return sDim.Render("yes / no")
	case flow.Choice:
		return sDim.Render(strings.Join(q.Options, " · "))
	case flow.Score:
		return sDim.Render(meter(q.Scale, math.NaN()) + " " + q.Scale.String())
	}
	return ""
}

func answerView(q flow.Question, d backend.Decision) string {
	switch q.Kind {
	case flow.Bool:
		return yesNo(d) + sDim.Render(fmt.Sprintf(" %.0f%%", d.Confidence*100))
	case flow.Choice:
		chosen := tmpl.Format(d.Answer)
		parts := make([]string, len(q.Options))
		for i, o := range q.Options {
			if o == chosen {
				parts[i] = lipgloss.NewStyle().Foreground(pink).Bold(true).Render("[" + o + "]")
			} else {
				parts[i] = sDim.Render(o)
			}
		}
		return strings.Join(parts, " ")
	case flow.Score:
		v, _ := d.Answer.(float64)
		return lipgloss.NewStyle().Foreground(orange).Render(meter(q.Scale, v)) + fmt.Sprintf(" %.2f", v)
	}
	return ""
}

// compactAnswer fits one answer inside a MULTI row.
func compactAnswer(q flow.Question, d backend.Decision) string {
	switch q.Kind {
	case flow.Bool:
		return yesNo(d)
	case flow.Score:
		v, _ := d.Answer.(float64)
		return lipgloss.NewStyle().Foreground(orange).Render(fmt.Sprintf("%.1f", v))
	}
	return lipgloss.NewStyle().Foreground(pink).Bold(true).Render(tmpl.Format(d.Answer))
}

func yesNo(d backend.Decision) string {
	if yes, _ := d.Answer.(bool); yes {
		return sGreen.Bold(true).Render("YES")
	}
	return sRed.Bold(true).Render("NO")
}

// meter draws one cell per scale level, filled up to the rounded value.
// NaN draws it empty.
func meter(sc flow.Scale, v float64) string {
	levels := sc.Max - sc.Min + 1
	filled := 0
	if !math.IsNaN(v) {
		filled = max(min(int(math.Round(v))-sc.Min+1, levels), 0)
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", levels-filled)
}

// actionView shows what an action produced once it ran, and before that
// its template with the {{placeholders}} marked as slots to be filled.
func actionView(a *flow.ActionSpec, ev runner.Event, ran bool) string {
	out, _ := ev.Output.(map[string]any)
	switch a.Type {
	case flow.ActShell:
		if !ran {
			return sDim.Render("$ ") + slots(a.Run)
		}
		code, _ := out["exit_code"].(int)
		result := firstLine(tmpl.Format(out["stdout"]))
		if code != 0 {
			return sRed.Render(fmt.Sprintf("exit %d ", code)) + result
		}
		return sGreen.Render("✓ ") + result
	case flow.ActLLM:
		if !ran {
			return slots(a.Prompt)
		}
		return lipgloss.NewStyle().Foreground(violet).Render("✎ ") + firstLine(tmpl.Format(out["text"]))
	case flow.ActHTTP:
		if !ran {
			return sDim.Render(a.Method+" ") + slots(a.URL)
		}
		if out["dry_run"] == true {
			return sYellow.Render("DRY RUN ") + sDim.Render(fmt.Sprintf("%v %v", out["method"], out["url"]))
		}
		status, _ := out["status"].(int)
		if status >= 300 {
			return sRed.Render(fmt.Sprintf("✗ %d", status))
		}
		return sGreen.Render(fmt.Sprintf("✓ %d ", status)) + sDim.Render(a.Method)
	case flow.ActOutput:
		parts := make([]string, len(a.Set))
		for i, kv := range a.Set {
			v := slots(kv.Value)
			if ran {
				v = tmpl.Format(out[kv.Key])
			}
			parts[i] = sDim.Render(kv.Key+"=") + v
		}
		return strings.Join(parts, sDim.Render(" · "))
	}
	if ran {
		return fmt.Sprintf("%q", tmpl.Format(out["message"]))
	}
	return slots(a.Message)
}

func firstLine(s string) string {
	if first, _, more := strings.Cut(s, "\n"); more {
		return first + " …"
	}
	return s
}

// slots renders a template dim, with each {{ref}} shown as a ⟨ref⟩ slot.
func slots(s string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "{{")
		j := strings.Index(rest, "}}")
		if i < 0 || j < i {
			b.WriteString(sDim.Render(rest))
			return b.String()
		}
		b.WriteString(sDim.Render(rest[:i]))
		ref := strings.TrimSpace(rest[i+2 : j])
		b.WriteString(sSlot.Render("⟨" + ref + "⟩"))
		rest = rest[j+2:]
	}
}
