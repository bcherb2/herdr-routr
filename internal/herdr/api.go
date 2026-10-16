package herdr

import (
	"context"
	"time"
)

// AgentStatus is herdr's lifecycle label for an agent pane.
// Note: herdr reports Done only when the finished pane is unfocused; a focused
// pane finishes as Idle, so "turn finished" means Idle or Done.
type AgentStatus string

const (
	Idle    AgentStatus = "idle"
	Working AgentStatus = "working"
	Blocked AgentStatus = "blocked"
	Done    AgentStatus = "done"
	Unknown AgentStatus = "unknown"
)

// Finished reports whether s ends a turn.
func (s AgentStatus) Finished() bool { return s == Idle || s == Done }

// ReadSource selects which terminal snapshot a read returns.
type ReadSource string

const (
	Visible         ReadSource = "visible"
	Recent          ReadSource = "recent"
	RecentUnwrapped ReadSource = "recent_unwrapped"
	Detection       ReadSource = "detection"
)

// Pane is the subset of herdr's pane record routr uses.
type Pane struct {
	PaneID      string      `json:"pane_id"`
	WorkspaceID string      `json:"workspace_id"`
	TabID       string      `json:"tab_id"`
	Cwd         string      `json:"cwd"`
	Label       string      `json:"label"`
	Agent       string      `json:"agent"`
	AgentStatus AgentStatus `json:"agent_status"`
	Focused     bool        `json:"focused"`
}

// AgentSession identifies the harness's own session; for Claude and Codex the
// value is the transcript id.
type AgentSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

// Agent is the subset of herdr's agent record routr uses.
type Agent struct {
	Name        string        `json:"name"`
	Agent       string        `json:"agent"`
	PaneID      string        `json:"pane_id"`
	WorkspaceID string        `json:"workspace_id"`
	AgentStatus AgentStatus   `json:"agent_status"`
	Focused     bool          `json:"focused"`
	Session     *AgentSession `json:"agent_session"`
	// LaunchPending is true until agent.start's readiness handshake completes
	// (e.g. while a startup dialog is up); prompts are rejected until then.
	LaunchPending bool   `json:"launch_pending"`
	TerminalName  string `json:"terminal_title_stripped"`
	// StateChangeSeq increases with every status change herdr observes, so
	// an unchanged value means the agent has not moved since it was last seen.
	StateChangeSeq uint64 `json:"state_change_seq"`
}

// Workspace is the subset of herdr's workspace record routr uses.
type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// ListWorkspaces returns all workspaces.
func (c *Client) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	err := c.call(ctx, "workspace.list", nil, &out)
	return out.Workspaces, err
}

// WorkspaceOptions configures workspace.create.
type WorkspaceOptions struct {
	Label string            `json:"label,omitempty"`
	Cwd   string            `json:"cwd,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	Focus bool              `json:"focus"`
}

// CreateWorkspace creates a workspace and returns its root shell pane.
func (c *Client) CreateWorkspace(ctx context.Context, o WorkspaceOptions) (Pane, error) {
	var out struct {
		RootPane Pane `json:"root_pane"`
	}
	err := c.call(ctx, "workspace.create", o, &out)
	return out.RootPane, err
}

// SplitOptions configures pane.split.
type SplitOptions struct {
	Target    string            `json:"target_pane_id,omitempty"`
	Direction string            `json:"direction"` // "right" or "down"
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Focus     bool              `json:"focus"`
}

// SplitPane splits Target and returns the new shell pane.
func (c *Client) SplitPane(ctx context.Context, o SplitOptions) (Pane, error) {
	if o.Direction == "" {
		o.Direction = "right"
	}
	var out struct {
		Pane Pane `json:"pane"`
	}
	err := c.call(ctx, "pane.split", o, &out)
	return out.Pane, err
}

// TabOptions configures tab.create.
type TabOptions struct {
	Workspace string            `json:"workspace_id,omitempty"`
	Label     string            `json:"label,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Focus     bool              `json:"focus"`
}

// Tab is the subset of herdr's tab record routr uses.
type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
}

// CreateTab creates a tab and returns its root shell pane.
func (c *Client) CreateTab(ctx context.Context, o TabOptions) (Pane, error) {
	var out struct {
		RootPane Pane `json:"root_pane"`
	}
	err := c.call(ctx, "tab.create", o, &out)
	return out.RootPane, err
}

// GetTab returns one tab.
func (c *Client) GetTab(ctx context.Context, tabID string) (Tab, error) {
	var out struct {
		Tab Tab `json:"tab"`
	}
	err := c.call(ctx, "tab.get", map[string]string{"tab_id": tabID}, &out)
	return out.Tab, err
}

// MoveDestination is where pane.move puts a pane; see ToTab and ToNewTab.
type MoveDestination struct {
	Type      string `json:"type"`
	TabID     string `json:"tab_id,omitempty"`
	Target    string `json:"target_pane_id,omitempty"`
	Split     string `json:"split,omitempty"`
	Workspace string `json:"workspace_id,omitempty"`
	Label     string `json:"label,omitempty"`
}

// ToTab puts the pane in tab by splitting target in direction ("right" or "down").
func ToTab(tab, target, direction string) MoveDestination {
	return MoveDestination{Type: "tab", TabID: tab, Target: target, Split: direction}
}

// ToNewTab puts the pane alone in a new, labeled tab of workspace.
func ToNewTab(workspace, label string) MoveDestination {
	return MoveDestination{Type: "new_tab", Workspace: workspace, Label: label}
}

// MovePane moves a pane, keeping its process, and returns it as it is now (its
// id may change). Emptying a tab closes that tab.
func (c *Client) MovePane(ctx context.Context, paneID string, to MoveDestination, focus bool) (Pane, error) {
	var out struct {
		MoveResult struct {
			Pane Pane `json:"pane"`
		} `json:"move_result"`
	}
	err := c.call(ctx, "pane.move", map[string]any{"pane_id": paneID, "destination": to, "focus": focus}, &out)
	return out.MoveResult.Pane, err
}

// FocusPane focuses a pane, switching to its tab.
func (c *Client) FocusPane(ctx context.Context, paneID string) error {
	return c.call(ctx, "pane.focus", map[string]string{"pane_id": paneID}, nil)
}

// GetPane returns one pane.
func (c *Client) GetPane(ctx context.Context, paneID string) (Pane, error) {
	var out struct {
		Pane Pane `json:"pane"`
	}
	err := c.call(ctx, "pane.get", map[string]string{"pane_id": paneID}, &out)
	return out.Pane, err
}

// ClosePane closes a pane and ends its process. Closing a tab's last pane
// closes the tab, and a workspace's last pane the workspace.
func (c *Client) ClosePane(ctx context.Context, paneID string) error {
	return c.call(ctx, "pane.close", map[string]string{"pane_id": paneID}, nil)
}

// ListPanes returns the panes of one workspace, or of all when workspaceID is "".
func (c *Client) ListPanes(ctx context.Context, workspaceID string) ([]Pane, error) {
	p := map[string]string{}
	if workspaceID != "" {
		p["workspace_id"] = workspaceID
	}
	var out struct {
		Panes []Pane `json:"panes"`
	}
	err := c.call(ctx, "pane.list", p, &out)
	return out.Panes, err
}

// ProcessInfo is what runs in a pane's terminal.
type ProcessInfo struct {
	ShellPID     int `json:"shell_pid"`
	ForegroundPG int `json:"foreground_process_group_id"`
}

// AtShell reports whether the pane's shell is in the foreground: at its
// prompt, with no command (such as a harness) running.
func (p ProcessInfo) AtShell() bool { return p.ShellPID != 0 && p.ForegroundPG == p.ShellPID }

// PaneProcessInfo returns a pane's shell and foreground process group.
func (c *Client) PaneProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	var out struct {
		ProcessInfo ProcessInfo `json:"process_info"`
	}
	err := c.call(ctx, "pane.process_info", map[string]string{"pane_id": paneID}, &out)
	return out.ProcessInfo, err
}

// RenamePane sets the pane label shown in herdr.
func (c *Client) RenamePane(ctx context.Context, paneID, label string) error {
	return c.call(ctx, "pane.rename", map[string]string{"pane_id": paneID, "label": label}, nil)
}

type readResult struct {
	Read struct {
		Text string `json:"text"`
	} `json:"read"`
}

type readParams struct {
	PaneID string     `json:"pane_id,omitempty"`
	Target string     `json:"target,omitempty"`
	Source ReadSource `json:"source"`
	Lines  int        `json:"lines,omitempty"`
	Format string     `json:"format"`
}

// ReadPane returns rendered (post-VT) text. lines <= 0 uses herdr's default.
func (c *Client) ReadPane(ctx context.Context, paneID string, src ReadSource, lines int) (string, error) {
	var out readResult
	err := c.call(ctx, "pane.read", readParams{PaneID: paneID, Source: src, Lines: lines, Format: "text"}, &out)
	return out.Read.Text, err
}

// SendText types literal text into a pane without pressing Enter.
func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	return c.call(ctx, "pane.send_text", map[string]string{"pane_id": paneID, "text": text}, nil)
}

// SendKeys sends logical keys ("enter", "esc", "down", "ctrl+c") to a pane.
func (c *Client) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	return c.call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

type agentResult struct {
	Agent Agent `json:"agent"`
}

// StartAgent launches a supported harness (kind: claude, codex, gemini, …) in a
// pane that is at an interactive shell prompt, passing args to the harness.
// It returns once herdr detects the agent is ready; a harness stuck on a
// startup dialog yields an APIError with code agent_not_ready.
func (c *Client) StartAgent(ctx context.Context, name, kind, paneID string, args []string, timeout time.Duration) (Agent, error) {
	if args == nil {
		args = []string{}
	}
	p := map[string]any{"name": name, "kind": kind, "pane_id": paneID, "args": args}
	if timeout > 0 {
		p["timeout_ms"] = timeout.Milliseconds()
	}
	var out agentResult
	err := c.callWithin(ctx, timeout+c.timeout, "agent.start", p, &out)
	return out.Agent, err
}

// GetAgent returns an agent by name, pane id, or terminal id.
func (c *Client) GetAgent(ctx context.Context, target string) (Agent, error) {
	var out agentResult
	err := c.call(ctx, "agent.get", map[string]string{"target": target}, &out)
	return out.Agent, err
}

// Prompt submits text and, with wait, blocks until the agent reaches one of
// until (default: idle, done, blocked) after first being seen working or
// blocked — so a stale pre-prompt idle cannot match.
func (c *Client) Prompt(ctx context.Context, target, text string, wait bool, until ...AgentStatus) (Agent, error) {
	p := map[string]any{"target": target, "text": text}
	limit := c.timeout
	if wait {
		w := map[string]any{}
		if len(until) > 0 {
			w["until"] = until
		}
		if dl, ok := ctx.Deadline(); ok {
			w["timeout_ms"] = time.Until(dl).Milliseconds()
		}
		p["wait"], limit = w, 0 // bounded by ctx, if at all
	}
	var out agentResult
	err := c.callWithin(ctx, limit, "agent.prompt", p, &out)
	return out.Agent, err
}

// WaitAgent blocks until the agent's status is one of until (default: idle,
// done, blocked). It matches the current status immediately if it qualifies.
func (c *Client) WaitAgent(ctx context.Context, target string, until ...AgentStatus) (Agent, error) {
	p := map[string]any{"target": target}
	if len(until) > 0 {
		p["until"] = until
	}
	if dl, ok := ctx.Deadline(); ok {
		p["timeout_ms"] = time.Until(dl).Milliseconds()
	}
	var out agentResult
	err := c.callWithin(ctx, 0, "agent.wait", p, &out) // bounded by ctx, if at all
	return out.Agent, err
}

// ReadAgent returns the agent pane's rendered text.
func (c *Client) ReadAgent(ctx context.Context, target string, src ReadSource, lines int) (string, error) {
	var out readResult
	err := c.call(ctx, "agent.read", readParams{Target: target, Source: src, Lines: lines, Format: "text"}, &out)
	return out.Read.Text, err
}

// SendAgentKeys sends logical keys to an agent's pane.
func (c *Client) SendAgentKeys(ctx context.Context, target string, keys ...string) error {
	return c.call(ctx, "agent.send_keys", map[string]any{"target": target, "keys": keys}, nil)
}

// PluginPaneOptions configures plugin.pane.open.
type PluginPaneOptions struct {
	PluginID   string `json:"plugin_id"`
	Entrypoint string `json:"entrypoint"`
	Placement  string `json:"placement,omitempty"` // overlay, split, tab, zoomed
	Target     string `json:"target_pane_id,omitempty"`
	Direction  string `json:"direction,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Focus      bool   `json:"focus"`
}

// OpenPluginPane opens a plugin's pane entrypoint and returns the new pane.
func (c *Client) OpenPluginPane(ctx context.Context, o PluginPaneOptions) (Pane, error) {
	var out struct {
		PluginPane struct {
			Pane Pane `json:"pane"`
		} `json:"plugin_pane"`
	}
	err := c.call(ctx, "plugin.pane.open", o, &out)
	return out.PluginPane.Pane, err
}

// RenameTab sets a tab's label.
func (c *Client) RenameTab(ctx context.Context, tabID, label string) error {
	return c.call(ctx, "tab.rename", map[string]string{"tab_id": tabID, "label": label}, nil)
}
