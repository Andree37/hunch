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

var sSlot = lipgloss.NewStyle().Foreground(lilac).Italic(true)

// kindName says what sort of node this is, in the words a flow uses.
func kindName(n *flow.Node) string {
	switch n.Kind {
	case flow.Bool:
		return "yes/no question"
	case flow.Choice:
		return "choice"
	case flow.Score:
		return "score"
	case flow.Questions:
		return "several questions"
	case flow.Switch:
		return "rule"
	}
	switch n.Action.Type {
	case flow.ActShell:
		return "shell command"
	case flow.ActLLM:
		return "model writes"
	case flow.ActHTTP:
		return "http call"
	case flow.ActOutput:
		return "output"
	}
	return "message"
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
			s = sAnswer.Render(tmpl.Format(out["value"]))
		} else {
			s = sDim.Render("on ") + slots(n.Switch)
		}
	}
	if ran && ev.Branch == flow.Unsure {
		s += sOrange.Render("  not sure enough")
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
	case flow.Bool, flow.Choice:
		return sAnswer.Render(answerLabel(q, d)) + sDim.Render(fmt.Sprintf(" %.0f%%", d.Confidence*100))
	case flow.Score:
		v, _ := d.Answer.(float64)
		return sCyan.Render(meter(q.Scale, v)) + sAnswer.Render(fmt.Sprintf(" %.1f", v))
	}
	return ""
}

// compactAnswer fits one answer of several on a line.
func compactAnswer(q flow.Question, d backend.Decision) string {
	return sAnswer.Render(answerLabel(q, d))
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
		if out["dry_run"] == true {
			return sYellow.Render("DRY RUN ") + sDim.Render(fmt.Sprintf("$ %v", out["command"]))
		}
		code := asInt(out["exit_code"])
		result := firstLine(tmpl.Format(out["stdout"]))
		if code != 0 {
			return sRed.Render(fmt.Sprintf("exit %d ", code)) + result
		}
		return sGreen.Render("✓ ") + result
	case flow.ActLLM:
		if !ran {
			return slots(a.Prompt)
		}
		return sDim.Render("✎ ") + firstLine(tmpl.Format(out["text"]))
	case flow.ActHTTP:
		if !ran {
			return sDim.Render(a.Method+" ") + slots(a.URL)
		}
		if out["dry_run"] == true {
			return sYellow.Render("DRY RUN ") + sDim.Render(fmt.Sprintf("%v %v", out["method"], out["url"]))
		}
		if out["faked"] == true {
			return sYellow.Render(fmt.Sprintf("FAKED %v", out["status"])) + sDim.Render(" from the test case")
		}
		status := asInt(out["status"])
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

// asInt reads a number that may have been through JSON (a recorded run),
// where every number comes back as float64.
func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}

func firstLine(s string) string {
	if first, _, more := strings.Cut(s, "\n"); more {
		return first + " …"
	}
	return s
}

// slots renders a template dim, with each {{ref}} shown as a ⟨ref⟩ slot.
func slots(s string) string {
	s = strings.Join(strings.Fields(s), " ") // one line, wherever it's shown
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
