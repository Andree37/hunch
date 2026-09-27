package tui

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Andree37/hunch/internal/cases"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/store"
)

// FlowInfo describes a flow file found in the project, for the picker.
type FlowInfo struct {
	Path  string
	Name  string
	About string // first comment line of the file
	Nodes int
	Tests int
	Runs  int // recorded runs on disk; -1 when they live in S3
	Err   error
}

// skipDirs are never searched for flows.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "testdata": true}

// FindFlows lists the flow files under root: YAML files that parse as a flow
// with at least one node. Test case and run folders are skipped.
func FindFlows(root string) []FlowInfo {
	var out []FlowInfo
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			depth := strings.Count(filepath.ToSlash(strings.TrimPrefix(p, root)), "/")
			if p != root && (skipDirs[name] || strings.HasPrefix(name, ".") ||
				strings.HasSuffix(name, ".tests") || strings.HasSuffix(name, ".runs") || depth > 4) {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		f, err := flow.Load(p)
		if err != nil || len(f.Nodes) == 0 {
			return nil // not a flow (or not one yet)
		}
		rel, _ := filepath.Rel(root, p)
		info := FlowInfo{Path: rel, Name: f.Name, About: firstComment(p), Nodes: len(f.Nodes)}
		if info.Name == "" {
			info.Name = strings.TrimSuffix(name, filepath.Ext(name))
		}
		if cs, err := cases.Load(cases.Dir(p)); err == nil {
			info.Tests = len(cs)
		}
		switch loc := f.RunsLocation(); {
		case store.IsRemote(loc):
			info.Runs = -1
		case loc != "":
			if keys, err := store.Dir(loc).List(context.Background(), ""); err == nil {
				info.Runs = len(keys)
			}
		}
		out = append(out, info)
		return nil
	})
	return out
}

// firstComment returns the first line of a file's leading comment.
func firstComment(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == "#" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return ""
		}
		return strings.TrimSpace(strings.TrimLeft(line, "#"))
	}
	return ""
}

// OpenOptions are what a flow is opened with, from the command line.
type OpenOptions struct {
	State    map[string]any
	Backend  string
	Writer   string
	Runs     string
	FromCase bool
}

// App is the whole TUI: the flow picker, and the flow that's open.
type App struct {
	root   string
	opts   OpenOptions
	flows  []FlowInfo
	cursor int
	main   *Model
	err    string

	width, height int
}

// NewApp starts on the picker, listing the flows under root.
func NewApp(root string, opts OpenOptions) *App {
	return &App{root: root, opts: opts, flows: FindFlows(root)}
}

// OpenFlow starts the app with a flow already open.
func OpenFlow(root, path string, opts OpenOptions) (*App, error) {
	a := NewApp(root, opts)
	if err := a.open(path); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) open(path string) error {
	m, err := New(path, a.opts.State, a.opts.Backend, a.opts.FromCase)
	if err != nil {
		return err
	}
	if a.opts.Runs != "" {
		if err := m.SetRuns(a.opts.Runs); err != nil {
			return err
		}
	}
	if err := m.SetWriter(a.opts.Writer); err != nil {
		return err
	}
	a.main = m
	return nil
}

// backToPickerMsg is sent by the open flow when F is pressed.
type backToPickerMsg struct{}

func (a *App) Init() tea.Cmd {
	if a.main != nil {
		return a.main.Init()
	}
	return nil
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		a.width, a.height = ws.Width, ws.Height
	}
	if a.main != nil {
		if _, ok := msg.(backToPickerMsg); ok {
			if a.main.cancel != nil {
				a.main.cancel()
			}
			a.main, a.err = nil, ""
			a.flows = FindFlows(a.root) // counts may have changed
			return a, nil
		}
		_, cmd := a.main.Update(msg)
		return a, cmd
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return a, nil
	}
	switch key.String() {
	case "q", "ctrl+c", "esc":
		return a, tea.Quit
	case "j", "down":
		a.cursor = min(a.cursor+1, len(a.flows)-1)
	case "k", "up":
		a.cursor = max(a.cursor-1, 0)
	case "enter":
		if len(a.flows) == 0 {
			return a, nil
		}
		if err := a.open(filepath.Join(a.root, a.flows[a.cursor].Path)); err != nil {
			a.err = err.Error()
			return a, nil
		}
		// The flow's model hasn't seen the window yet.
		a.main.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
		return a, a.main.Init()
	}
	return a, nil
}

func (a *App) View() string {
	if a.main != nil {
		return a.main.View()
	}
	if a.width == 0 {
		return ""
	}
	w := a.width - 4
	var lines []string
	if len(a.flows) == 0 {
		lines = append(lines, sDim.Render("No flows found under "+a.root+"."),
			sDim.Render("A flow is a .yaml file with nodes:, e.g. examples/triage.yaml. Run hunch tui from the project folder."))
	}
	for i, fi := range a.flows {
		marker, name := " ", fi.Name
		if i == a.cursor {
			marker, name = sTitle.Render("▸"), sBold.Render(fi.Name)
		}
		runs := fmt.Sprintf("%d runs", fi.Runs)
		if fi.Runs < 0 {
			runs = "runs in S3"
		}
		lines = append(lines,
			fmt.Sprintf("%s %s  %s", marker, name, sDim.Render(fi.Path)),
			"    "+sDim.Render(fmt.Sprintf("%d steps · %d tests · %s", fi.Nodes, fi.Tests, runs)))
		if fi.About != "" {
			lines = append(lines, "    "+fi.About)
		}
		lines = append(lines, "")
	}
	if a.err != "" {
		lines = append(lines, sRed.Render("can't open: "+a.err))
	}
	body := pane("Pick a flow · "+fmt.Sprint(len(a.flows))+" found", lines, a.width, a.height-1, true)
	bar := sStatus.Width(a.width).Render(" j/k move · enter open · q quit" + strings.Repeat(" ", max(w-30, 0)))
	return body + "\n" + bar
}

// RunApp runs the TUI until it quits.
func RunApp(a *App) error {
	_, err := tea.NewProgram(a, tea.WithAltScreen()).Run()
	return err
}
