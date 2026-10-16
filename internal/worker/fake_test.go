package worker

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
)

// fakeHerdr is an in-memory herdr: workspaces, tabs and panes, and one
// scripted agent per pane. Tests set agent status and screens and react to
// keys and prompts through hooks.
type fakeHerdr struct {
	mu         sync.Mutex
	workspaces []herdr.Workspace
	panes      map[string]*herdr.Pane
	agents     map[string]*herdr.Agent // pane id → agent; absent: none detected
	screens    map[string]string       // pane id → visible text
	atShell    map[string]bool         // pane id → shell in the foreground
	next       int
	calls      []string
	keys       []string
	startArgs  []string
	startKind  string
	tabEnv     map[string]string
	startErrs  []error // agent.start returns these first, one per call, and starts nothing
	starts     int     // agent.start calls
	busyFor    int     // process_info calls that still find a child in a pane's foreground
	shellPID   int     // process_info's shell pid for every pane; 0 = this process, whose environment the tab then inherited
	startBusy  bool    // agent.start came while a child held the foreground

	onStart  func(f *fakeHerdr, pane string) // default: an idle agent
	onKey    func(f *fakeHerdr, pane, key string)
	onPrompt func(f *fakeHerdr, pane, text string) error
}

func newFake() *fakeHerdr {
	return &fakeHerdr{panes: map[string]*herdr.Pane{}, agents: map[string]*herdr.Agent{},
		screens: map[string]string{}, atShell: map[string]bool{}}
}

func (f *fakeHerdr) record(format string, a ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *fakeHerdr) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeHerdr) pressed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

// addPane puts a new shell pane in a new tab of ws.
func (f *fakeHerdr) addPane(ws, label string) *herdr.Pane {
	f.next++
	p := &herdr.Pane{PaneID: fmt.Sprintf("%s:p%d", ws, f.next), WorkspaceID: ws, TabID: fmt.Sprintf("%s:t%d", ws, f.next), Label: label}
	f.panes[p.PaneID] = p
	f.screens[p.PaneID] = "$ "
	f.atShell[p.PaneID] = true
	return p
}

// setAgent scripts pane's agent; the state sequence moves with every change.
func (f *fakeHerdr) setAgent(pane string, st herdr.AgentStatus, screen string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[pane]
	if a == nil {
		a = &herdr.Agent{Name: "a", PaneID: pane}
		f.agents[pane] = a
	}
	if a.AgentStatus != st {
		a.StateChangeSeq++
	}
	a.AgentStatus, a.LaunchPending = st, false
	f.screens[pane] = screen
	f.atShell[pane] = false
}

func (f *fakeHerdr) Socket() string { return "/tmp/fake.sock" }

func (f *fakeHerdr) ListWorkspaces(context.Context) ([]herdr.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.workspaces), nil
}

func (f *fakeHerdr) CreateWorkspace(_ context.Context, o herdr.WorkspaceOptions) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("w%d", len(f.workspaces)+1)
	f.workspaces = append(f.workspaces, herdr.Workspace{WorkspaceID: id, Label: o.Label})
	f.record("workspace.create %s focus=%v", o.Label, o.Focus)
	f.tabEnv = o.Env
	return *f.addPane(id, ""), nil
}

func (f *fakeHerdr) CreateTab(_ context.Context, o herdr.TabOptions) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.ContainsFunc(f.workspaces, func(w herdr.Workspace) bool { return w.WorkspaceID == o.Workspace }) {
		return herdr.Pane{}, &herdr.APIError{Code: "workspace_not_found"}
	}
	f.record("tab.create %s %s focus=%v", o.Workspace, o.Label, o.Focus)
	f.tabEnv = o.Env
	return *f.addPane(o.Workspace, ""), nil
}

func (f *fakeHerdr) GetTab(_ context.Context, tab string) (herdr.Tab, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := herdr.Tab{TabID: tab}
	for _, p := range f.panes {
		if p.TabID == tab {
			t.PaneCount++
		}
	}
	return t, nil
}

func (f *fakeHerdr) RenameTab(_ context.Context, tab, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("tab.rename %s %s", tab, label)
	return nil
}

func (f *fakeHerdr) GetPane(_ context.Context, id string) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.panes[id]
	if !ok {
		return herdr.Pane{}, &herdr.APIError{Code: "pane_not_found"}
	}
	return *p, nil
}

func (f *fakeHerdr) ListPanes(_ context.Context, ws string) ([]herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []herdr.Pane
	for _, p := range f.panes {
		if p.WorkspaceID == ws {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b herdr.Pane) int { return strings.Compare(a.PaneID, b.PaneID) })
	return out, nil
}

func (f *fakeHerdr) RenamePane(_ context.Context, id, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[id].Label = label
	return nil
}

func (f *fakeHerdr) ReadPane(_ context.Context, id string, _ herdr.ReadSource, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screens[id], nil
}

func (f *fakeHerdr) MovePane(_ context.Context, id string, to herdr.MoveDestination, focus bool) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.panes[id]
	switch to.Type {
	case "tab":
		p.TabID = to.TabID
		f.record("move %s beside %s in %s", id, to.Target, to.TabID)
	case "new_tab":
		f.next++
		p.TabID = fmt.Sprintf("%s:t%d", to.Workspace, f.next)
		f.record("move %s to new tab %q", id, to.Label)
	}
	return *p, nil
}

func (f *fakeHerdr) FocusPane(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.panes {
		p.Focused = p.PaneID == id
	}
	f.record("focus %s", id)
	return nil
}

func (f *fakeHerdr) ClosePane(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.panes[id]; !ok {
		return &herdr.APIError{Code: "pane_not_found"}
	}
	delete(f.panes, id)
	delete(f.agents, id)
	f.record("close %s", id)
	return nil
}

func (f *fakeHerdr) PaneProcessInfo(_ context.Context, id string) (herdr.ProcessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	shell := cmp.Or(f.shellPID, os.Getpid())
	if f.busyFor > 0 {
		f.busyFor--
		return herdr.ProcessInfo{ShellPID: shell, ForegroundPG: shell + 1}, nil
	}
	if f.atShell[id] {
		return herdr.ProcessInfo{ShellPID: shell, ForegroundPG: shell}, nil
	}
	return herdr.ProcessInfo{ShellPID: shell, ForegroundPG: shell + 2}, nil
}

func (f *fakeHerdr) StartAgent(_ context.Context, name, kind, pane string, args []string, _ time.Duration) (herdr.Agent, error) {
	f.mu.Lock()
	f.startKind, f.startArgs = kind, args
	f.starts++
	f.startBusy = f.startBusy || f.busyFor > 0
	if len(f.startErrs) > 0 {
		err := f.startErrs[0]
		f.startErrs = f.startErrs[1:]
		f.mu.Unlock()
		return herdr.Agent{}, err
	}
	f.mu.Unlock()
	if f.onStart != nil {
		f.onStart(f, pane)
	} else {
		f.setAgent(pane, herdr.Idle, "❯ ")
	}
	a, err := f.agent(pane)
	if err != nil {
		return a, &herdr.APIError{Code: "agent_not_ready"}
	}
	return a, nil
}

func (f *fakeHerdr) agent(pane string) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[pane]
	if !ok {
		return herdr.Agent{}, &herdr.APIError{Code: "agent_not_found"}
	}
	return *a, nil
}

func (f *fakeHerdr) GetAgent(_ context.Context, target string) (herdr.Agent, error) {
	return f.agent(target)
}

func (f *fakeHerdr) Prompt(_ context.Context, target, text string, wait bool, until ...herdr.AgentStatus) (herdr.Agent, error) {
	if f.onPrompt != nil {
		if err := f.onPrompt(f, target, text); err != nil {
			return herdr.Agent{}, err
		}
	}
	return f.agent(target)
}

func (f *fakeHerdr) ReadAgent(_ context.Context, target string, _ herdr.ReadSource, _ int) (string, error) {
	if _, err := f.agent(target); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screens[target], nil
}

func (f *fakeHerdr) SendAgentKeys(_ context.Context, target string, keys ...string) error {
	f.mu.Lock()
	f.keys = append(f.keys, keys...)
	f.mu.Unlock()
	if f.onKey != nil {
		for _, k := range keys {
			f.onKey(f, target, k)
		}
	}
	return nil
}

// newDriver returns a driver over f with fast timings and fixed probes.
func newDriver(f *fakeHerdr, help map[string]string) *herdrDriver {
	return &herdrDriver{c: f, settle: 10 * time.Millisecond, busy: time.Millisecond,
		help:       func(_ context.Context, h string) string { return help[h] },
		mcpServers: func(context.Context, Spec) ([]string, error) { return nil, nil },
		newID:      func() string { return "00000000-0000-4000-8000-000000000001" }}
}
