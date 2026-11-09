package tui

import (
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/runner"
	"github.com/Andree37/hunch/internal/tmpl"
	"github.com/Andree37/hunch/internal/validate"
)

// Colours mean one thing each: green is the way the run went (or a pass),
// red a failure, yellow a close call or something to watch, orange "not
// sure enough", cyan what the model answered. Everything else is neutral.
var (
	purple = lipgloss.Color("#7C6CF0")
	lilac  = lipgloss.Color("#A78BFA")
	green  = lipgloss.Color("#34D399")
	red    = lipgloss.Color("#F87171")
	yellow = lipgloss.Color("#FCD34D")
	orange = lipgloss.Color("#FB923C")
	cyan   = lipgloss.Color("#5EEAD4")
	dim    = lipgloss.Color("#7A8194")
	faint  = lipgloss.Color("#3F4454")

	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(lilac)
	sDim    = lipgloss.NewStyle().Foreground(dim)
	sGreen  = lipgloss.NewStyle().Foreground(green)
	sRed    = lipgloss.NewStyle().Foreground(red)
	sYellow = lipgloss.NewStyle().Foreground(yellow)
	sOrange = lipgloss.NewStyle().Foreground(orange)
	sCyan   = lipgloss.NewStyle().Foreground(cyan)
	sAnswer = lipgloss.NewStyle().Foreground(cyan).Bold(true)
	sBold   = lipgloss.NewStyle().Bold(true)
	sQuote  = lipgloss.NewStyle().Foreground(lipgloss.Color("#E2E8F0")).Italic(true)
	sStatus = lipgloss.NewStyle().Foreground(lipgloss.Color("#CBD5E1")).Background(lipgloss.Color("#1E2130"))
)

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	bodyH := max(m.height-2, 6)
	var body string
	switch {
	case m.help:
		body = m.helpView(bodyH)
	case m.detail:
		body = pane(m.stepTitle(), m.scrolledStepLines(m.width-4, bodyH-3), m.width, bodyH, true)
	case m.editing == editValue:
		body = m.editorView(bodyH)
	default:
		body = m.panes(bodyH)
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerBar(), body, m.footerBar())
}

// panes lays the screen out in reading order: what goes in (cases and
// inputs) on the left, what happened (the run, or the map of the flow) in
// the middle, and why (the selected step) on the right. Narrow terminals
// stack the run above the step.
func (m *Model) panes(bodyH int) string {
	wide := m.width >= 120
	leftW := m.width * 38 / 100
	if wide {
		leftW = min(max(m.width*25/100, 30), 46)
	}
	restW := m.width - leftW

	listLen := len(m.pane2Lines(leftW-4, bodyH))
	casesH := min(max(listLen+3, 5), bodyH/2)
	inputsH := bodyH - casesH
	inputLines := m.inputLines(leftW - 4)
	left := lipgloss.JoinVertical(lipgloss.Left,
		pane(m.casesTitle(), m.pane2Lines(leftW-4, casesH-3), leftW, casesH, m.focus == focusTests),
		pane(m.inputsTitle(), inputLines, leftW, inputsH, m.focus == focusInputs),
	)

	if m.flowView == viewGrid && m.grid != nil {
		return lipgloss.JoinHorizontal(lipgloss.Top, left,
			pane(m.centerTitle(), m.centerLines(restW-4, bodyH-3), restW, bodyH, m.focus == focusFlow))
	}
	if wide {
		centerW := restW * 52 / 100
		stepW := restW - centerW
		return lipgloss.JoinHorizontal(lipgloss.Top, left,
			pane(m.centerTitle(), m.centerLines(centerW-4, bodyH-3), centerW, bodyH, m.focus == focusFlow),
			pane(m.stepTitle(), m.scrolledStepLines(stepW-4, bodyH-3), stepW, bodyH, m.focus == focusNode),
		)
	}
	centerH := bodyH * 55 / 100
	stepH := bodyH - centerH
	return lipgloss.JoinHorizontal(lipgloss.Top, left, lipgloss.JoinVertical(lipgloss.Left,
		pane(m.centerTitle(), m.centerLines(restW-4, centerH-3), restW, centerH, m.focus == focusFlow),
		pane(m.stepTitle(), m.scrolledStepLines(restW-4, stepH-3), restW, stepH, m.focus == focusNode),
	))
}

// pane draws a bordered box of exactly w×h cells.
func pane(title string, lines []string, w, h int, active bool) string {
	border := faint
	if active {
		border = purple
	}
	innerW, innerH := w-4, h-3
	if len(lines) > innerH {
		lines = lines[:max(innerH, 0)]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, innerW, "…")
	}
	body := ansi.Truncate(title, innerW, "…") + "\n" + strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(w - 2).
		Height(h - 2).
		Render(body)
}

// paneTitle is "n Name": the number is the key that jumps to the pane.
func paneTitle(n int, name, hint string) string {
	t := sDim.Render(strconv.Itoa(n)+" ") + sTitle.Render(name)
	if hint != "" {
		t += sDim.Render("  " + hint)
	}
	return t
}

// --- header and footer ---

// headerBar says what is on screen (flow, input, backend, dry or live) and,
// on the right, how the run went.
func (m *Model) headerBar() string {
	name := filepath.Base(m.path)
	if m.flow != nil && m.flow.Name != "" {
		name = m.flow.Name
	}
	sep := sDim.Render(" · ")
	be := m.backend
	if m.mock() {
		be += sYellow.Render(" (made-up answers)")
	}
	left := " " + sTitle.Render(name) + sep + m.sourceLabel() + sep + be + sep + m.modeLabel()
	if m.shiftSteps != 0 {
		left += sep + sYellow.Render("trying "+m.thresholdText(m.shiftSteps))
	}

	var right string
	if r := m.run; r != nil {
		switch {
		case m.replaying():
			right = sTitle.Render(fmt.Sprintf("replay %d/%d", m.replayK, len(m.replay.Steps))) +
				sDim.Render(" · recorded "+m.replay.Time.Local().Format("Jan 2 15:04"))
			if m.replay.Backend != "" {
				right += sDim.Render(" · " + m.replay.Backend)
			}
		case r.paused != "":
			right = sYellow.Render("⏸ paused") + " before " + r.paused + sDim.Render(fmt.Sprintf(" · %d steps", len(r.path)))
		case !r.done:
			right = sYellow.Render(fmt.Sprintf("%s running %s %.1fs", m.spinner(), r.current, time.Since(r.nodeAt).Seconds()))
			if r.status != "" {
				right += sDim.Render(" · ") + sYellow.Render(r.status)
			}
		case r.err != nil:
			right = sRed.Render("✗ failed at " + r.errNode)
		default:
			right = m.expectSummary() + sDim.Render(fmt.Sprintf("%d steps · $%.6f · %s", len(r.path), r.cost, r.elapsed.Round(time.Millisecond)))
		}
	}
	return bar(left, right+" ", m.width)
}

// mock reports whether the backend picked for this run makes its answers up.
func (m *Model) mock() bool {
	if m.flow == nil {
		return false
	}
	cfg, ok := m.flow.Backends[m.backend]
	return ok && cfg.Kind == "mock"
}

// expectSummary is "✓ 2/2 expected · " for a checked test case.
func (m *Model) expectSummary() string {
	rs := m.activeResults()
	if len(rs) == 0 {
		return ""
	}
	ok := 0
	for _, r := range rs {
		if r.OK {
			ok++
		}
	}
	s := fmt.Sprintf("%d/%d expected", ok, len(rs))
	if ok == len(rs) {
		return sGreen.Render("✓ "+s) + sDim.Render(" · ")
	}
	return sRed.Render("✗ "+s) + sDim.Render(" · ")
}

// sourceLabel says where the input on screen came from.
func (m *Model) sourceLabel() string {
	switch {
	case m.replay != nil:
		return "run " + m.replay.ID
	case m.active >= 0:
		s := "test " + m.cases[m.active].Name
		if m.dirty {
			s += "*"
		}
		return s
	}
	return "typed input"
}

// modeLabel says whether http nodes would really send.
func (m *Model) modeLabel() string {
	if m.live {
		return sRed.Bold(true).Render("LIVE")
	}
	return sDim.Render("dry run")
}

// footerBar lists the few keys that matter here; ? shows them all.
func (m *Model) footerBar() string {
	var keys string
	switch {
	case m.help:
		keys = "any key closes"
	case m.detail:
		keys = "j/k scroll · esc back"
	case m.editing == editKey || m.editing == editCaseName || m.editing == editRunsPath:
		keys = "enter ok · esc cancel"
	case m.editing == editPick:
		keys = "j/k choose · enter select · esc cancel"
	case m.editing == editValue:
		keys = "enter save · alt+enter new line · esc cancel"
	case m.replaying():
		keys = "esc close run · s step · r run again live · n save as test · m map · ? keys"
	case m.run != nil && !m.run.done && m.run.paused == "":
		keys = "x stop · ? keys"
	case m.pausedRun():
		keys = "s next step · r run to end · x stop · ? keys"
	case m.flowView == viewGrid && m.grid != nil && m.focus == focusFlow:
		keys = "j/k select · enter open its run · [ ] threshold · R run all again · m map · ? keys"
		if slices.ContainsFunc(m.grid.rows, func(r *gridRow) bool { return r.needs != "" }) {
			keys = "j/k select · enter open its run · [ ] threshold · A ask what ? tests need · ? keys"
		}
	case m.focus == focusInputs:
		keys = "enter edit · a add · d delete · w save test · r run · ? keys"
	case m.focus == focusTests:
		keys = "r run this · s step through it · enter load · t tests/runs · ? keys"
	case m.focus == focusNode:
		keys = "j/k scroll · r run · ? keys"
	default:
		keys = "j/k select · enter open step · r run · R run all tests · m " + m.otherView() + " · ? keys"
		if m.grid != nil {
			keys = "j/k select · enter open step · r run · g all tests · m " + m.otherView() + " · ? keys"
		}
	}
	return bar(" "+keys, m.flash+" ", m.width)
}

func (m *Model) otherView() string {
	if m.showingRun() {
		return "map"
	}
	return "run"
}

// bar puts left and right on one line of exactly width cells; when they
// don't fit, the left side gives way.
func bar(left, right string, width int) string {
	rw := lipgloss.Width(right)
	left = ansi.Truncate(left, max(width-rw-1, 0), "…")
	gap := max(width-lipgloss.Width(left)-rw, 0)
	return sStatus.Width(width).Render(ansi.Truncate(left+strings.Repeat(" ", gap)+right, width, "…"))
}

// helpView lists every key.
func (m *Model) helpView(h int) string {
	groups := []struct {
		title string
		keys  [][2]string
	}{
		{"Run", [][2]string{
			{"r", "run the flow; on a highlighted test or recorded run, run that"},
			{"R", "run every test, side by side; saving the flow or a test runs them again"},
			{"g", "back to every test's results"},
			{"[ ]", "try a lower or higher threshold on every test's answers, with no model calls; set it in the flow to keep it"},
			{"A", "ask the model only what the ? tests need at the threshold being tried"}, {"s", "run one step at a time"}, {"x", "stop"},
			{"m", "switch between the run and the map of the flow"},
			{"b", "switch backend"}, {"L", "live or dry run: dry runs don't run shell or send http writes"},
		}},
		{"Move", [][2]string{
			{"1 2 3 4", "run · tests · step · input"}, {"tab", "next pane"},
			{"j k", "move, or scroll the step"}, {"enter", "load, open or edit what's selected"},
		}},
		{"Input and tests", [][2]string{
			{"a", "add an input"}, {"d", "delete an input"},
			{"w", "save the input to this test"}, {"n", "save it (or the run) as a new test"},
			{"t", "show tests or recorded runs"}, {"o", "read runs from another folder or file"},
			{"esc", "close a recorded run"},
		}},
		{"Other", [][2]string{{"F", "back to the list of flows"}, {"?", "this help"}, {"q", "quit"}}},
	}
	var lines []string
	for _, g := range groups {
		lines = append(lines, sTitle.Render(g.title))
		for _, k := range g.keys {
			lines = append(lines, fmt.Sprintf("  %s  %s", sCyan.Render(fmt.Sprintf("%-8s", k[0])), k[1]))
		}
		lines = append(lines, "")
	}
	return pane(sTitle.Render("Keys"), lines, m.width, h, true)
}

// editorView gives a text input the whole screen, so a pasted email reads
// as it is.
func (m *Model) editorView(h int) string {
	in := m.inputs[m.inputCursor]
	var lines []string
	if d := m.spec(in.key).Description; d != "" {
		lines = append(lines, sDim.Render(d), "")
	}
	lines = append(lines, strings.Split(m.editor.View(), "\n")...)
	return pane(sTitle.Render("Edit "+in.key), lines, m.width, h, true)
}

// --- the middle: the run, or the map of the flow ---

// showingRun reports whether the middle pane tells the run rather than
// drawing the map.
func (m *Model) showingRun() bool {
	return m.flowView == viewPath && m.run != nil && len(m.run.path) > 0
}

func (m *Model) centerTitle() string {
	if m.flowView == viewGrid && m.grid != nil {
		return paneTitle(1, "All tests", "saving the flow or a test runs them again · m map")
	}
	if m.showingRun() {
		return paneTitle(1, "Run", "m map")
	}
	if m.run != nil && len(m.run.path) > 0 {
		return paneTitle(1, "Map", "m run")
	}
	return paneTitle(1, "Map", "")
}

func (m *Model) centerLines(w, h int) []string {
	if m.flow == nil {
		return []string{sRed.Render(fmt.Sprint(m.loadErr))}
	}
	var top []string
	if m.loadErr != nil {
		top = append(top, sRed.Render("reload failed: "+m.loadErr.Error()), "")
	}
	top = append(top, m.oldVersionLines()...)
	for _, is := range m.issues {
		if is.Severity == validate.Error {
			top = append(top, sRed.Render("✗ "+is.String()))
		} else if !m.showingRun() {
			top = append(top, sYellow.Render("⚠ "+is.String()))
		}
	}
	if len(m.issues) > 0 && len(top) > 0 && top[len(top)-1] != "" {
		top = append(top, "")
	}

	if m.flowView == viewGrid && m.grid != nil {
		return append(top, m.gridLines(w, h-len(top))...)
	}
	var lines []string
	var rowLine []int
	if m.showingRun() {
		lines, rowLine = m.storyLines(w)
	} else {
		lines, rowLine = m.mapLines(w)
	}
	lines = append(top, lines...)
	// Keep the selected row in view.
	if c := min(m.cursor, len(rowLine)-1); c >= 0 {
		if first := len(top) + rowLine[c] - h + 3; first > 0 {
			lines = lines[first:]
		}
	}
	return lines
}

// storyLines tell the run in order: the input, then each step with what it
// asked, what came back, how sure, and where it went, then how it ended.
func (m *Model) storyLines(w int) (lines []string, rowLine []int) {
	r := m.run
	lines = append(lines, m.inputQuote(w)...)
	lines = append(lines, "")

	nameW := 0
	for _, id := range r.path {
		nameW = max(nameW, len(id))
	}
	nameW = min(nameW, 18)
	indent := strings.Repeat(" ", 6)

	shown := m.shown()
	for i, id := range r.path[:shown] {
		rowLine = append(rowLine, len(lines))
		n := m.flow.Node(id)
		marker := " "
		name := fmt.Sprintf("%-*s", nameW, id)
		if i == m.cursor {
			marker, name = sTitle.Render("▸"), sBold.Render(name)
		}
		head := fmt.Sprintf("%s%2d %s %s", marker, i+1, m.stepIcon(id), name)
		if n == nil {
			lines = append(lines, head+sDim.Render("  (no longer in the flow)"))
			continue
		}
		ev, ran := m.eventFor(id)
		switch {
		case n.Kind == flow.Switch:
			detail := nodeDetail(n, ev, ran)
			if ran && ev.Next != "" {
				detail += m.nextMark(ev)
			}
			lines = append(lines, head+"  "+detail+m.expectMark(id))
		case n.Kind == flow.Action:
			detail := nodeDetail(n, ev, ran)
			full := actionText(n, ev, ran)
			if lipgloss.Width(head)+2+lipgloss.Width(detail) <= w && !strings.Contains(strings.TrimSpace(full), "\n") {
				lines = append(lines, head+"  "+detail+m.expectMark(id))
				break
			}
			// Too long for one line: the output goes underneath, wrapped.
			lines = append(lines, head+m.expectMark(id))
			if full == "" {
				full = detail
			}
			lines = append(lines, clip(wrapAll(full, w-len(indent)), 4, indent)...)
		case n.Kind == flow.Questions:
			lines = append(lines, head+m.nextMark(ev)+m.expectMark(id))
			lines = append(lines, indent+sDim.Render(fmt.Sprintf("%d questions in one call", len(n.Questions))))
			if ran {
				lines = append(lines, indent+nodeDetail(n, ev, true))
			}
		default:
			q := n.Questions[0]
			text := q.Text
			if ran && len(ev.Asked) > 0 {
				text = ev.Asked[0]
			}
			lines = append(lines, head+m.nextMark(ev)+m.expectMark(id))
			for _, l := range clip(wrap(strings.Join(strings.Fields(text), " "), w-len(indent)), 2, indent) {
				lines = append(lines, sDim.Render(l))
			}
			if ran && len(ev.Decisions) > 0 {
				lines = append(lines, indent+m.verdict(n, q, ev.Decisions[0], ev, w-len(indent), m.fill(i)))
			}
		}
		switch {
		case r.errNode == id:
			for _, l := range wrap("✗ "+r.err.Error(), w-len(indent)) {
				lines = append(lines, indent+sRed.Render(l))
			}
		case r.current == id && !ran:
			lines = append(lines, indent+sYellow.Render(fmt.Sprintf("%s asking… %.1fs", m.spinner(), time.Since(r.nodeAt).Seconds())))
		}
		if n.Kind != flow.Switch {
			lines = append(lines, "")
		}
	}
	if shown < len(r.path) {
		return lines, rowLine // the rest is still appearing
	}
	if r.paused != "" {
		rowLine = append(rowLine, len(lines))
		lines = append(lines, fmt.Sprintf(" %2d %s %s", len(r.path)+1, sYellow.Render("⏸"), r.paused+sDim.Render("  next · s runs it")))
	}

	lines = append(lines, "")
	switch {
	case r.err != nil:
		lines = append(lines, sRed.Render("✗ failed at "+r.errNode))
	case !r.done && r.paused == "":
		lines = append(lines, sYellow.Render("◐ running…"))
	case r.done:
		lines = append(lines, sGreen.Render("■ ended at ")+sBold.Render(r.path[len(r.path)-1]))
	}
	// Expectations about nodes the run never reached.
	for _, res := range m.activeResults() {
		if !res.OK && !slices.Contains(r.path, resultNode(res)) {
			lines = append(lines, sRed.Render(fmt.Sprintf("✗ the test expects %s = %s, but %s", res.Target, res.Want, res.Got)))
		}
	}
	if m.mock() && slices.ContainsFunc(m.activeResults(), func(r cases.Result) bool { return !r.OK }) {
		lines = append(lines, "", sDim.Render("mock makes its answers up, so tests fail on it. b switches backend."))
	}
	return lines, rowLine
}

// actionText is the whole of what an action produced, when it is text
// worth reading in full: a message, written text or command output.
func actionText(n *flow.Node, ev runner.Event, ran bool) string {
	if !ran {
		return ""
	}
	out, _ := ev.Output.(map[string]any)
	switch n.Action.Type {
	case flow.ActLog:
		return tmpl.Format(out["message"])
	case flow.ActLLM:
		return "✎ " + tmpl.Format(out["text"])
	case flow.ActShell:
		if out["dry_run"] == true {
			return "DRY RUN $ " + tmpl.Format(out["command"])
		}
		if asInt(out["exit_code"]) != 0 {
			return ""
		}
		return "✓ " + tmpl.Format(out["stdout"])
	}
	return ""
}

// wrapAll wraps text to w, keeping its own line breaks.
func wrapAll(s string, w int) []string {
	var lines []string
	for _, para := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		lines = append(lines, wrap(para, w)...)
	}
	return lines
}

// clip keeps the first n lines, indented, and says how to see the rest.
func clip(lines []string, n int, indent string) []string {
	out := make([]string, 0, n+1)
	for i, l := range lines {
		if i == n {
			out = append(out, indent+sDim.Render(fmt.Sprintf("+%d more lines · enter shows all", len(lines)-n)))
			break
		}
		out = append(out, indent+l)
	}
	return out
}

// inputQuote shows the run's input at the top of the story: a single text
// input as a quote, several as name: value lines.
func (m *Model) inputQuote(w int) []string {
	in := m.run.inputs
	if len(in) == 0 {
		in = m.state()
	}
	keys := slices.Sorted(maps.Keys(in))
	if len(keys) == 1 {
		if s, ok := in[keys[0]].(string); ok {
			lines := wrap("“"+strings.Join(strings.Fields(s), " ")+"”", w)
			if len(lines) > 3 {
				lines = append(lines[:3], "…")
			}
			for i, l := range lines {
				lines[i] = sQuote.Render(l)
			}
			return lines
		}
	}
	var lines []string
	for _, k := range keys {
		v := strings.Join(strings.Fields(tmpl.Format(in[k])), " ")
		lines = append(lines, sDim.Render(k+": ")+v)
	}
	if len(lines) > 4 {
		lines = append(lines[:4], sDim.Render(fmt.Sprintf("+%d more", len(keys)-4)))
	}
	return lines
}

// verdict is one answer with how sure the model was against the threshold,
// fitted to w cells: "yes  ━━━━━━━┃━━──  90%".
func (m *Model) verdict(n *flow.Node, q flow.Question, d backend.Decision, ev runner.Event, w int, frac float64) string {
	th := m.threshold(n)
	style := sAnswer
	note := ""
	switch {
	case ev.Branch == flow.Unsure:
		style = sOrange.Bold(true)
		note = sOrange.Render("  not sure enough")
	case d.Confidence < th:
		style = sYellow.Bold(true)
		note = sYellow.Render("  under threshold")
	case m.closeCall(n, d.Confidence):
		style = sYellow.Bold(true)
		note = sYellow.Render("  close call")
	}
	label := fmt.Sprintf("%-8s", answerLabel(q, d))
	barW := min(max(w-len(label)-6-lipgloss.Width(note), 6), 30)
	if frac < 1 { // still filling in
		note = ""
	}
	conf := d.Confidence * frac
	return style.Render(label) + " " + confBar(conf, th, barW, style) +
		sDim.Render(fmt.Sprintf(" %3.0f%%", conf*100)) + note
}

// confBar is a slider filled up to p, with a tick where the threshold
// sits: ━━━━━━┃━━──────
func confBar(p, th float64, w int, style lipgloss.Style) string {
	filled := int(p*float64(w) + 0.5)
	tick := min(int(th*float64(w)+0.5), w-1)
	empty := lipgloss.NewStyle().Foreground(faint)
	var b strings.Builder
	for i := 0; i < w; i++ {
		switch {
		case i == tick:
			b.WriteString(sBold.Foreground(lilac).Render("┃"))
		case i < filled:
			b.WriteString(style.Render("━"))
		default:
			b.WriteString(empty.Render("─"))
		}
	}
	return b.String()
}

// answerLabel is an answer the way the flow names it: yes, an option, or a
// score.
func answerLabel(q flow.Question, d backend.Decision) string {
	switch q.Kind {
	case flow.Bool:
		if yes, _ := d.Answer.(bool); yes {
			return "yes"
		}
		return "no"
	case flow.Score:
		v, _ := d.Answer.(float64)
		return fmt.Sprintf("%.1f", v)
	}
	return tmpl.Format(d.Answer)
}

// nextMark is "  → intent" for where a step went.
func (m *Model) nextMark(ev runner.Event) string {
	if ev.Next == "" {
		return ""
	}
	if ev.Branch == flow.Unsure {
		return sOrange.Render("  → " + ev.Next)
	}
	return sDim.Render("  → ") + ev.Next
}

// expectMark is a ✓ or ✗ for the test's expectations about this node.
func (m *Model) expectMark(node string) string {
	var s string
	for _, res := range m.activeResults() {
		if resultNode(res) != node {
			continue
		}
		if res.OK {
			s += sGreen.Render("  ✓")
		} else {
			s += sRed.Render("  ✗ test expects " + res.Want)
		}
	}
	return s
}

// resultNode is the node an expectation is about: intent for "intent" and
// for "tone.urgency".
func resultNode(r cases.Result) string {
	node, _, _ := strings.Cut(r.Target, ".")
	return node
}

// stepIcon marks a step of the run: done, failed, running or paused.
func (m *Model) stepIcon(node string) string {
	r := m.run
	switch {
	case r.errNode == node:
		return sRed.Render("✗")
	case r.current == node:
		return sYellow.Render(m.spinner())
	}
	return sGreen.Render("●")
}

// mapLines draw the flow as a tree from the start node. The branches the
// run took are green, so a node reachable from several places shows which
// way it was reached.
func (m *Model) mapLines(w int) (lines []string, rowLine []int) {
	for i, r := range m.layout() {
		rowLine = append(rowLine, len(lines))
		lines = append(lines, m.rowLine(r, i == m.cursor))
	}
	if r := m.run; r != nil && r.done && r.err == nil && len(r.path) > 0 {
		lines = append(lines, "", sGreen.Render("■ ended at ")+sBold.Render(r.path[len(r.path)-1])+sDim.Render("   m shows the run"))
	}
	return lines, rowLine
}

// --- the right: the selected step, and why it went the way it did ---

func (m *Model) stepTitle() string {
	n := m.selected()
	if n == nil {
		return paneTitle(3, "Step", "")
	}
	hint := kindName(n)
	if n.Kind.IsDecision() {
		hint += fmt.Sprintf(" · %s · threshold %.2f", m.backendOf(n), m.threshold(n))
	}
	return paneTitle(3, n.ID, hint)
}

// scrolledStepLines applies the step pane's scroll, keeping a marker when
// there is more above or below.
func (m *Model) scrolledStepLines(w, h int) []string {
	lines := m.stepLines(w)
	m.nodeScroll = max(min(m.nodeScroll, len(lines)-h), 0)
	lines = lines[m.nodeScroll:]
	if m.nodeScroll > 0 {
		lines = append([]string{sDim.Render(fmt.Sprintf("↑ %d more", m.nodeScroll))}, lines...)
	}
	if len(lines) > h && h > 0 {
		more := len(lines) - h + 1
		lines = append(lines[:h-1], sDim.Render(fmt.Sprintf("↓ %d more", more)))
	}
	return lines
}

func (m *Model) stepLines(w int) []string {
	n := m.selected()
	if n == nil {
		return nil
	}
	ev, ran := m.eventFor(n.ID)
	var lines []string
	if m.run != nil && m.run.errNode == n.ID {
		lines = append(lines, wrap(sRed.Render("✗ "+m.run.err.Error()), w)...)
		lines = append(lines, "")
	}

	switch n.Kind {
	case flow.Action:
		lines = append(lines, m.actionLines(n, ev, ran, w)...)
	case flow.Switch:
		lines = append(lines, sDim.Render("a rule: routes on ")+slots(n.Switch), "")
		if ran {
			out, _ := ev.Output.(map[string]any)
			lines = append(lines, sBold.Render("value")+"  "+tmpl.Format(out["value"]), "")
		}
	default:
		lines = append(lines, m.decisionLines(n, ev, ran, w)...)
	}
	lines = append(lines, m.routeLines(n, ev, ran)...)
	if exp := m.expectLines(n.ID); len(exp) > 0 {
		lines = append(lines, "")
		lines = append(lines, exp...)
	}
	if ran && ev.CostUSD > 0 {
		lines = append(lines, "", sDim.Render(fmt.Sprintf("%s · $%.6f · %s", m.backendOf(n), ev.CostUSD, ev.Latency.Round(time.Millisecond))))
	}
	return lines
}

// decisionLines show each question as it was asked, how likely each answer
// was, and in plain words why that answer carries the run where it goes.
func (m *Model) decisionLines(n *flow.Node, ev runner.Event, ran bool, w int) []string {
	var lines []string
	lines = append(lines, m.seesLines(n, ev, ran, w)...)

	var prevEv runner.Event
	var hasPrev bool
	if m.prev != nil {
		prevEv, hasPrev = m.prev.events[n.ID]
	}
	for i, q := range n.Questions {
		if len(n.Questions) > 1 {
			lines = append(lines, sBold.Render(q.Name))
		}
		if ran && i < len(ev.Asked) {
			lines = append(lines, wrap(ev.Asked[i], w)...)
		} else {
			lines = append(lines, wrap(slots(q.Text), w)...)
		}
		lines = append(lines, "")
		if !ran || i >= len(ev.Decisions) {
			continue
		}
		d := ev.Decisions[i]
		lines = append(lines, bars(q, d, w)...)
		lines = append(lines, "")
		if len(n.Questions) == 1 {
			lines = append(lines, wrap(m.why(n, q, d, ev), w)...)
		} else {
			lines = append(lines, sDim.Render(fmt.Sprintf("confidence %.2f", d.Confidence)))
		}
		if hasPrev && i < len(prevEv.Decisions) {
			p := prevEv.Decisions[i]
			if changed(p, d) {
				lines = append(lines, sYellow.Render(fmt.Sprintf("previous run: %s %.2f  ← changed", answerLabel(q, p), p.Confidence)))
			} else {
				lines = append(lines, sDim.Render(fmt.Sprintf("previous run: %s %.2f, the same", answerLabel(q, p), p.Confidence)))
			}
		}
		lines = append(lines, "")
	}
	return lines
}

// why says in a sentence how sure the model was and what that did to the
// route.
func (m *Model) why(n *flow.Node, q flow.Question, d backend.Decision, ev runner.Event) string {
	th := m.threshold(n)
	label := answerLabel(q, d)
	_, hasUnsure := n.Then.Get(flow.Unsure)
	var s string
	switch {
	case d.Confidence >= th && m.closeCall(n, d.Confidence):
		s = sYellow.Render(fmt.Sprintf("Only just sure enough: %.2f against a threshold of %.2f, so it took %s. A close call.", d.Confidence, th, label))
	case d.Confidence >= th:
		s = fmt.Sprintf("Sure enough: %.2f against a threshold of %.2f, so it took %s.", d.Confidence, th, sAnswer.Render(label))
	case hasUnsure:
		s = sOrange.Render(fmt.Sprintf("Not sure enough: %.2f is under the %.2f threshold, so it went to unsure instead of %s.", d.Confidence, th, label))
	default:
		s = sYellow.Render(fmt.Sprintf("Under the %.2f threshold (%.2f), but there's no unsure route, so it took %s anyway.", th, d.Confidence, label))
	}
	if cfg, ok := m.flow.Backends[m.backendOf(n)]; ok && backend.SelfReported(cfg.Kind) {
		s += sDim.Render(" The model rates its own confidence.")
	}
	return s
}

// routeLines list where each answer leads, with the one the run took marked.
func (m *Model) routeLines(n *flow.Node, ev runner.Event, ran bool) []string {
	edges := outEdges(n)
	if len(edges) == 0 {
		return nil
	}
	lines := []string{sDim.Render("Routes")}
	for _, e := range edges {
		when := e.When
		switch when {
		case "":
			when = "then"
		case flow.Default:
			when = "else"
		}
		taken := ran && ev.Next == e.To && (e.When == "" || ev.Branch == e.When)
		if taken {
			lines = append(lines, sGreen.Render(fmt.Sprintf("● %s → %s", when, e.To)))
		} else {
			lines = append(lines, sDim.Render(fmt.Sprintf("  %s → %s", when, e.To)))
		}
	}
	return lines
}

// expectLines show what the test case expects of this node.
func (m *Model) expectLines(node string) []string {
	var lines []string
	for _, r := range m.activeResults() {
		if resultNode(r) != node {
			continue
		}
		if r.OK {
			lines = append(lines, sGreen.Render("✓ ")+fmt.Sprintf("expected %s = %s", r.Target, r.Want))
		} else {
			lines = append(lines, sRed.Render("✗ ")+fmt.Sprintf("the test expects %s = %s, ", r.Target, r.Want)+sRed.Render("but got "+r.Got))
		}
	}
	return lines
}

// seesLines say what the model is shown, when the node limits it.
func (m *Model) seesLines(n *flow.Node, ev runner.Event, ran bool, w int) []string {
	if n.Sees == nil {
		return nil
	}
	if len(n.Sees) == 0 {
		return []string{sDim.Render("sees: nothing but the question"), ""}
	}
	lines := []string{sDim.Render("sees: " + strings.Join(n.Sees, ", "))}
	if ran {
		for _, k := range slices.Sorted(maps.Keys(ev.Saw)) {
			lines = append(lines, sDim.Render("  "+k+": ")+ansi.Truncate(tmpl.Format(ev.Saw[k]), max(w-len(k)-4, 10), "…"))
		}
	}
	return append(lines, "")
}

// actionLines show what an action does and, once it ran, everything it
// produced, with long text wrapped.
func (m *Model) actionLines(n *flow.Node, ev runner.Event, ran bool, w int) []string {
	a := n.Action
	var lines []string
	src := func(label, v string) {
		if v != "" {
			lines = append(lines, wrap(sDim.Render(label)+" "+slots(v), w)...)
		}
	}
	switch a.Type {
	case flow.ActLog:
		src("say", a.Message)
	case flow.ActShell:
		src("$", a.Run)
	case flow.ActLLM:
		src("system", a.System)
		src("prompt", a.Prompt)
	case flow.ActHTTP:
		src(a.Method, a.URL)
	case flow.ActOutput:
		for _, kv := range a.Set {
			src(kv.Key+" =", kv.Value)
		}
	}
	lines = append(lines, "")
	if !ran {
		return append(lines, sDim.Render("hasn't run"), "")
	}
	out, _ := ev.Output.(map[string]any)
	if out["dry_run"] == true {
		lines = append(lines, sYellow.Render("dry run: nothing was run or sent"), "")
	}
	for _, k := range slices.Sorted(maps.Keys(out)) {
		if k == "dry_run" {
			continue
		}
		v := tmpl.Format(out[k])
		if len(v) > w-len(k)-2 || strings.Contains(v, "\n") {
			lines = append(lines, sBold.Render(k))
			for _, para := range strings.Split(v, "\n") {
				lines = append(lines, wrap(para, w)...)
			}
			continue
		}
		lines = append(lines, sBold.Render(k)+"  "+v)
	}
	return append(lines, "")
}

// backendOf names the backend behind a node's answer on screen: what the
// shown run actually used, falling back to the node's own or the TUI's pick.
func (m *Model) backendOf(n *flow.Node) string {
	if ev, ok := m.eventFor(n.ID); ok && ev.Backend != "" {
		return ev.Backend
	}
	if n.Backend != "" {
		return n.Backend
	}
	if m.replay != nil && m.replay.Backend != "" {
		return m.replay.Backend
	}
	return m.backend
}

func changed(a, b backend.Decision) bool {
	if a.Kind == flow.Score {
		x, _ := a.Answer.(float64)
		y, _ := b.Answer.(float64)
		return math.Round(x) != math.Round(y)
	}
	return tmpl.Format(a.Answer) != tmpl.Format(b.Answer)
}

// bars draws one probability bar per answer, in the question's own order,
// with the answer it gave highlighted.
func bars(q flow.Question, d backend.Decision, w int) []string {
	var keys []string
	switch q.Kind {
	case flow.Bool:
		keys = []string{"yes", "no"}
	case flow.Choice:
		keys = q.Options
	case flow.Score:
		for l := q.Scale.Min; l <= q.Scale.Max; l++ {
			keys = append(keys, strconv.Itoa(l))
		}
	}
	labelW := 0
	for _, k := range keys {
		labelW = max(labelW, len(k))
	}
	barW := max(min(w-labelW-7, 30), 5)
	chosen := answerLabel(q, d)
	if q.Kind == flow.Score {
		v, _ := d.Answer.(float64)
		chosen = strconv.Itoa(int(math.Round(v)))
	}

	var lines []string
	empty := lipgloss.NewStyle().Foreground(faint)
	for _, k := range keys {
		p := d.Probs[k]
		filled := int(p*float64(barW) + 0.5)
		label := fmt.Sprintf("%-*s", labelW, k)
		fill := sDim.Render(strings.Repeat("━", filled))
		if k == chosen {
			label, fill = sAnswer.Render(label), sCyan.Render(strings.Repeat("━", filled))
		}
		lines = append(lines, fmt.Sprintf("%s %s%s %3.0f%%", label, fill, empty.Render(strings.Repeat("─", barW-filled)), p*100))
	}
	if q.Kind == flow.Score {
		lines = append(lines, sDim.Render(fmt.Sprintf("expected value %s", answerLabel(q, d))))
	}
	return lines
}

// --- the left: tests or recorded runs, and the input ---

func (m *Model) casesTitle() string {
	switch {
	case m.showRuns:
		return paneTitle(2, fmt.Sprintf("Recorded runs (%d)", len(m.runs)), "t tests")
	case m.runsPath != "":
		return paneTitle(2, "Tests", fmt.Sprintf("t recorded runs (%d)", len(m.runs)))
	}
	return paneTitle(2, "Tests", "")
}

func (m *Model) pane2Lines(w, h int) []string {
	if m.editing == editRunsPath {
		return append([]string{m.keyEditor.View(), ""}, m.runLines(w, h-2)...)
	}
	if m.showRuns {
		return m.runLines(w, h)
	}
	return m.caseLines(w, h)
}

func (m *Model) inputsTitle() string {
	switch {
	case m.replay != nil:
		return paneTitle(4, "Input", "from the run")
	case m.active < 0:
		return paneTitle(4, "Input", "not saved")
	case m.dirty:
		return paneTitle(4, "Input", "edited")
	}
	return paneTitle(4, "Input", "")
}

// activeResults are the expectation checks for the input on screen: only
// when it's an unedited test case that has been run.
func (m *Model) activeResults() []cases.Result {
	if m.active < 0 || m.dirty {
		return nil
	}
	return m.results[m.cases[m.active].Name]
}

func (m *Model) caseLines(w, h int) []string {
	if len(m.cases) == 0 {
		return wrap(sDim.Render("No tests yet · n saves the input as one"), w)
	}
	var lines []string
	for i, c := range m.cases {
		icon := sDim.Render("○")
		if rs, ok := m.results[c.Name]; ok {
			if cases.Passed(rs) {
				icon = sGreen.Render("✓")
			} else {
				icon = sRed.Render("✗")
			}
		} else if len(c.Expect) == 0 {
			icon = sDim.Render("·")
		}
		marker := " "
		if i == m.caseCursor && m.focus == focusTests {
			marker = sTitle.Render("▸")
		}
		name := c.Name
		if i == m.active {
			name = sBold.Render(name)
		}
		line := fmt.Sprintf("%s%s %s", marker, icon, name)
		if c.Description != "" {
			line += "  " + sDim.Render(c.Description)
		}
		lines = append(lines, line)
	}
	if top := m.caseCursor - h + 1; top > 0 {
		lines = lines[top:]
	}
	return lines
}

func (m *Model) inputLines(w int) []string {
	var lines []string
	if len(m.inputs) == 0 && m.editing != editKey {
		lines = append(lines, sDim.Render("no inputs · a adds one"))
	}
	keyW := 0
	for _, in := range m.inputs {
		keyW = max(keyW, len(in.key))
	}
	for i, in := range m.inputs {
		spec := m.spec(in.key)
		if i == m.inputCursor && m.editing == editPick {
			lines = append(lines, sTitle.Render(in.key))
			lines = append(lines, m.pickerLines(spec, in.value)...)
			continue
		}
		marker, key := " ", sDim.Render(fmt.Sprintf("%-*s", keyW, in.key))
		if m.focus == focusInputs && i == m.inputCursor && m.editing == editNone {
			marker, key = sTitle.Render("▸"), sBold.Render(fmt.Sprintf("%-*s", keyW, in.key))
		}
		// Long text reads better wrapped under its name.
		if text := in.value; (spec.Type == flow.InputText || spec.Type == "") && m.inputError(in) == nil &&
			(len(text)+keyW+3 > w || strings.Contains(text, "\n")) {
			lines = append(lines, marker+key)
			var body []string
			for _, para := range strings.Split(text, "\n") {
				body = append(body, wrap(para, w-3)...)
			}
			if len(body) > 8 {
				body = append(body[:8], sDim.Render(fmt.Sprintf("+%d more lines · enter shows all", len(body)-8)))
			}
			for _, l := range body {
				lines = append(lines, "   "+l)
			}
			continue
		}
		lines = append(lines, fmt.Sprintf("%s%s  %s", marker, key, m.valueView(spec, in)))
	}
	if m.editing == editKey || m.editing == editCaseName {
		lines = append(lines, m.keyEditor.View())
	}
	return lines
}

// valueView renders an input's value the way its type reads best.
func (m *Model) valueView(s flow.InputSpec, in input) string {
	if in.value == "" {
		return sYellow.Render("(empty)") + sDim.Render(typeHint(s))
	}
	if err := m.inputError(in); err != nil {
		return sRed.Render(in.value) + sRed.Render("  ✗ "+err.Error())
	}
	switch s.Type {
	case flow.InputChoice:
		return sBold.Render(in.value) + sDim.Render(" ▾")
	case flow.InputBool:
		v, _ := s.Parse(in.value)
		if v == true {
			return sBold.Render("yes") + sDim.Render(" ▾")
		}
		return sBold.Render("no") + sDim.Render(" ▾")
	case flow.InputNumber:
		return in.value + sDim.Render(typeHint(s))
	}
	return preview(in.value)
}

// typeHint is a short, dim reminder of what an input accepts.
func typeHint(s flow.InputSpec) string {
	switch s.Type {
	case flow.InputChoice:
		return "  " + strings.Join(s.Options, " / ")
	case flow.InputBool:
		return "  yes / no"
	case flow.InputNumber:
		switch {
		case s.Min != nil && s.Max != nil:
			return fmt.Sprintf("  %g–%g", *s.Min, *s.Max)
		case s.Min != nil:
			return fmt.Sprintf("  ≥ %g", *s.Min)
		case s.Max != nil:
			return fmt.Sprintf("  ≤ %g", *s.Max)
		}
		return "  number"
	}
	return ""
}

func (m *Model) pickerLines(s flow.InputSpec, current string) []string {
	current = displayValue(s, current)
	lines := make([]string, len(m.pickOptions))
	for i, o := range m.pickOptions {
		cursor, dot := "  ", sDim.Render("○")
		if o == current {
			dot = sCyan.Render("●")
		}
		label := o
		if i == m.pickCursor {
			cursor, label = sTitle.Render("▸ "), sBold.Render(o)
		}
		lines[i] = "  " + cursor + dot + " " + label
	}
	return lines
}

// preview shows the first line of a value and how much more there is.
func preview(v string) string {
	if v == "" {
		return sYellow.Render("(empty)")
	}
	first, rest, multi := strings.Cut(v, "\n")
	if !multi {
		return v
	}
	return first + sDim.Render(fmt.Sprintf(" ⏎ +%d lines", strings.Count(rest, "\n")+1))
}

// --- shared ---

func (m *Model) eventFor(node string) (runner.Event, bool) {
	if m.run == nil {
		return runner.Event{}, false
	}
	ev, ok := m.run.events[node]
	return ev, ok
}

// selected is the node on the cursor's row of the middle pane.
func (m *Model) selected() *flow.Node {
	rows := m.rows()
	if len(rows) == 0 || m.flow == nil {
		return nil
	}
	return m.flow.Node(rows[min(m.cursor, len(rows)-1)].node)
}

func wrap(s string, w int) []string {
	if w <= 0 {
		return []string{s}
	}
	return strings.Split(ansi.Wordwrap(s, w, ""), "\n")
}
