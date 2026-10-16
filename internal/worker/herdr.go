package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/menu"
	"github.com/bcherb2/herdr-routr/internal/transcript"
)

// api is the part of the herdr client the driver uses; *herdr.Client
// satisfies it.
type api interface {
	Socket() string
	ListWorkspaces(ctx context.Context) ([]herdr.Workspace, error)
	CreateWorkspace(ctx context.Context, o herdr.WorkspaceOptions) (herdr.Pane, error)
	CreateTab(ctx context.Context, o herdr.TabOptions) (herdr.Pane, error)
	GetTab(ctx context.Context, tabID string) (herdr.Tab, error)
	RenameTab(ctx context.Context, tabID, label string) error
	GetPane(ctx context.Context, paneID string) (herdr.Pane, error)
	ListPanes(ctx context.Context, workspaceID string) ([]herdr.Pane, error)
	RenamePane(ctx context.Context, paneID, label string) error
	ReadPane(ctx context.Context, paneID string, src herdr.ReadSource, lines int) (string, error)
	MovePane(ctx context.Context, paneID string, to herdr.MoveDestination, focus bool) (herdr.Pane, error)
	FocusPane(ctx context.Context, paneID string) error
	ClosePane(ctx context.Context, paneID string) error
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	StartAgent(ctx context.Context, name, kind, paneID string, args []string, timeout time.Duration) (herdr.Agent, error)
	GetAgent(ctx context.Context, target string) (herdr.Agent, error)
	Prompt(ctx context.Context, target, text string, wait bool, until ...herdr.AgentStatus) (herdr.Agent, error)
	ReadAgent(ctx context.Context, target string, src herdr.ReadSource, lines int) (string, error)
	SendAgentKeys(ctx context.Context, target string, keys ...string) error
}

// Workspace is the label of the workspace that holds workers when Spec
// names none.
const Workspace = "routr"

// Timeouts for the steps of Start and Prompt.
const (
	shellTimeout  = 10 * time.Second // a new pane's shell drawing its prompt
	agentTimeout  = 60 * time.Second // agent.start's readiness handshake
	readyTimeout  = 2 * time.Minute  // startup dialogs answered and the harness idle
	promptTimeout = time.Minute      // the harness picking up a prompt
)

// A pane herdr calls busy (agent_pane_busy: the shell had a child in the
// foreground when agent.start came) is tried again in place, this many
// times, the waits doubling from busyWait. Starts are spaced startGap
// apart: a fan-out that starts every harness at once is what keeps shells
// busy.
const (
	busyTries = 3
	busyWait  = time.Second
	startGap  = 500 * time.Millisecond
)

// herdrDriver runs workers in herdr tabs. It keeps no state about workers:
// everything it needs is in the Handle, and herdr is asked afresh each call.
// Agent calls target the pane id, which stays valid for the pane's life
// (moves within a workspace keep it), unlike the agent name, which herdr
// drops when the harness exits.
type herdrDriver struct {
	c      api
	settle time.Duration // how long a startup status must hold; key gaps scale with it
	help   func(ctx context.Context, harness string) string
	newID  func() string // session id for a new Claude session
	busy   time.Duration // the first wait before agent.start again on a busy pane; it doubles
	gap    time.Duration // the least time between two agent.start calls

	// mcpServers are the MCP servers Codex would start for s (mcpArgs).
	mcpServers func(ctx context.Context, s Spec) ([]string, error)

	mu    sync.Mutex
	helps map[string]string // harness → its --help text

	startMu   sync.Mutex // held across the gap and agent.start
	lastStart time.Time
}

// NewHerdr returns a Driver that runs workers in tabs of the herdr server c
// talks to.
func NewHerdr(c *herdr.Client) Driver {
	return &herdrDriver{c: c, settle: 800 * time.Millisecond, help: harnessHelp, mcpServers: codexMCPServers, newID: newUUID, busy: busyWait, gap: startGap}
}

// Start opens a labeled tab, launches the harness (resuming s.Session if
// set), answers its startup dialogs and returns once it is ready for a
// prompt. A Handle with a Pane is returned whenever the tab was opened, even
// with an error, so the caller can show or close it. Its Session is one the
// harness confirmed only when Start succeeds: on an error it may name a
// session that was chosen (Claude's --session-id) but never created.
func (d *herdrDriver) Start(ctx context.Context, s Spec) (Handle, error) {
	if err := validate(s); err != nil {
		return Handle{}, err
	}
	if s.Label == "" {
		s.Label = fmt.Sprintf("%d %s", s.ID, s.Harness)
	}
	env := expandHome(s.Env)
	args, session, err := d.argv(ctx, s)
	if err != nil {
		return Handle{}, err
	}
	pane, err := d.place(ctx, s, env)
	if err != nil {
		return Handle{}, fmt.Errorf("worker: open tab: %w", err)
	}
	h := Handle{
		Socket: d.c.Socket(), Pane: pane.PaneID, Tab: pane.TabID, Harness: s.Harness,
		Agent: fmt.Sprintf("routr-%d-%s", s.ID, randomHex(3)), Session: session, Cwd: s.Cwd, Env: s.Env,
	}
	// The pane label is where Hide finds the tab name again.
	if err := d.c.RenamePane(ctx, h.Pane, s.Label); err != nil {
		return h, fmt.Errorf("worker: label pane: %w", err)
	}
	if err := d.startAgent(ctx, h, args); err != nil {
		return h, err
	}
	if err := d.ensureReady(ctx, h); err != nil {
		return h, err
	}
	if ag, err := d.c.GetAgent(ctx, h.Pane); err == nil {
		h.Session, h.Transcript = d.session(ctx, ag, h)
	}
	return h, nil
}

func validate(s Spec) error {
	switch s.Harness {
	case "claude", "codex", "pi":
	default:
		return fmt.Errorf("worker: harness must be claude, codex or pi, got %q", s.Harness)
	}
	if !filepath.IsAbs(s.Cwd) {
		return fmt.Errorf("worker: cwd must be an absolute path, got %q", s.Cwd)
	}
	if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
		return fmt.Errorf("worker: cwd %s is not a directory", s.Cwd)
	}
	return nil
}

// place opens the worker's tab in s.Workspace, or in the "routr" workspace
// (created on first use) when s names none or it is gone.
func (d *herdrDriver) place(ctx context.Context, s Spec, env map[string]string) (herdr.Pane, error) {
	tab := herdr.TabOptions{Workspace: s.Workspace, Label: s.Label, Cwd: s.Cwd, Env: env}
	if s.Workspace != "" {
		p, err := d.c.CreateTab(ctx, tab)
		if !herdr.IsCode(err, "workspace_not_found") {
			return p, err
		}
	}
	all, err := d.c.ListWorkspaces(ctx)
	if err != nil {
		return herdr.Pane{}, err
	}
	for _, w := range all {
		if w.Label == Workspace {
			tab.Workspace = w.WorkspaceID
			return d.c.CreateTab(ctx, tab)
		}
	}
	root, err := d.c.CreateWorkspace(ctx, herdr.WorkspaceOptions{Label: Workspace, Cwd: s.Cwd, Env: env})
	if err != nil {
		return root, err
	}
	return root, d.c.RenameTab(ctx, root.TabID, s.Label)
}

// startAgent launches the harness in h's pane once its shell is free. A
// pane herdr calls busy is waited for and tried again, in place, up to
// busyTries times. The error carries no screen: no harness ran in the pane,
// so all it would show is the shell prompt.
func (d *herdrDriver) startAgent(ctx context.Context, h Handle, args []string) error {
	wait := d.busy
	for try := 1; ; try++ {
		if err := d.waitShell(ctx, h.Pane); err != nil {
			return err
		}
		err := d.spaced(ctx, func() error {
			_, err := d.c.StartAgent(ctx, h.Agent, h.Harness, h.Pane, args, agentTimeout)
			return err
		})
		switch {
		case err == nil || herdr.IsCode(err, "agent_not_ready"):
			return nil
		case !herdr.IsCode(err, "agent_pane_busy") || try == busyTries:
			return fmt.Errorf("worker: start %s: %w", h.Harness, err)
		}
		if !sleep(ctx, wait) {
			return fmt.Errorf("worker: start %s: %w", h.Harness, err)
		}
		wait *= 2
	}
}

// spaced runs start at least d.gap after the last one began, one at a time.
func (d *herdrDriver) spaced(ctx context.Context, start func() error) error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	if wait := d.gap - time.Since(d.lastStart); wait > 0 && !sleep(ctx, wait) {
		return ctx.Err()
	}
	d.lastStart = time.Now()
	return start()
}

// waitShell waits for a new pane's shell to draw its prompt and hold the
// foreground (herdr's own test before agent.start types the harness command
// into it) on two checks in a row: a prompt such as powerline's is drawn
// before the shell's startup files have finished running.
func (d *herdrDriver) waitShell(ctx context.Context, pane string) error {
	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	calm := 0
	for {
		if d.atPrompt(ctx, pane) {
			if calm++; calm >= 2 {
				return nil
			}
		} else {
			calm = 0
		}
		if !sleep(ctx, d.settle/8) {
			return fmt.Errorf("worker: the shell in %s never sat at its prompt with nothing in the foreground: %w", pane, ctx.Err())
		}
	}
}

// atPrompt reports whether pane shows text and its shell holds the
// foreground.
func (d *herdrDriver) atPrompt(ctx context.Context, pane string) bool {
	text, err := d.c.ReadPane(ctx, pane, herdr.Visible, 0)
	if err != nil || strings.TrimSpace(text) == "" {
		return false
	}
	info, err := d.c.PaneProcessInfo(ctx, pane)
	return err == nil && info.AtShell()
}

// ErrStartupDialog is returned by Start when the harness shows a startup
// dialog it cannot answer on its own; the dialog is in the error text.
var ErrStartupDialog = errors.New("the harness is waiting on a startup dialog")

// ensureReady waits until the harness has launched and sits idle, answering
// startup dialogs: folder trust (Claude reports it blocked, Codex idle),
// "update available" (skipped), permission-mode confirmations, and Pi's
// "Press any key to continue" notice, which would swallow the first prompt,
// and Codex's hooks review, answered by continuing without trusting them.
// Dialogs can render after agent.start returns, so idle must hold across two
// checks.
func (d *herdrDriver) ensureReady(ctx context.Context, h Handle) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	calm := 0
	for {
		ag, err := d.c.GetAgent(ctx, h.Pane)
		if herdr.IsCode(err, "agent_not_found") {
			if info, perr := d.c.PaneProcessInfo(ctx, h.Pane); perr == nil && info.AtShell() {
				return fmt.Errorf("worker: %s exited during startup:\n%s", h.Harness, d.tail(ctx, h.Pane))
			}
		} else if err != nil {
			return err
		}
		screen, err := d.c.ReadAgent(ctx, h.Pane, herdr.Visible, 0)
		if err != nil && !herdr.IsCode(err, "agent_not_found") {
			return err
		}
		keys, dialog := startupKeys(ag.AgentStatus, screen)
		switch {
		case dialog:
			calm = 0
			if err := d.answerStartup(ctx, h, ag.AgentStatus); err != nil {
				return err
			}
		case keys != nil: // a notice that any key dismisses
			if err := d.press(ctx, h.Pane, keys); err != nil {
				return err
			}
			calm = 0
		case err == nil && !ag.LaunchPending && ag.AgentStatus.Finished():
			if calm++; calm >= 2 {
				return nil
			}
		default:
			calm = 0
		}
		if !sleep(ctx, d.settle/2) {
			return fmt.Errorf("worker: %s never became ready: %w", h.Harness, ctx.Err())
		}
	}
}

// answerStartup answers a startup dialog once it has finished rendering, and
// waits for the answer to take effect.
func (d *herdrDriver) answerStartup(ctx context.Context, h Handle, status herdr.AgentStatus) error {
	if !sleep(ctx, d.settle) {
		return ctx.Err()
	}
	screen, err := d.c.ReadAgent(ctx, h.Pane, herdr.Visible, 0)
	if err != nil {
		return err
	}
	keys, dialog := startupKeys(status, screen)
	if !dialog {
		return nil // gone meanwhile (a flap, or answered in the pane)
	}
	if keys == nil {
		q, _, _ := question(screen)
		return fmt.Errorf("worker: %w:\n%s", ErrStartupDialog, q)
	}
	if err := d.press(ctx, h.Pane, keys); err != nil {
		return err
	}
	if !sleep(ctx, 2*d.settle) {
		return ctx.Err()
	}
	return nil
}

// tail is the last lines of a pane, for errors.
func (d *herdrDriver) tail(ctx context.Context, pane string) string {
	text, _ := d.c.ReadPane(ctx, pane, herdr.Recent, 12)
	return strings.TrimSpace(text)
}

// Prompt sends text and returns once the harness has picked it up, not when
// the turn ends. Waiting for pickup means a Poll right after cannot see the
// stale idle from before the prompt: herdr's agent.wait right after
// agent.prompt can return that old idle.
func (d *herdrDriver) Prompt(ctx context.Context, h Handle, text string) error {
	before, err := d.c.GetAgent(ctx, h.Pane)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, promptTimeout)
	defer cancel()
	_, err = d.c.Prompt(pctx, h.Pane, text, true, herdr.Working, herdr.Blocked, herdr.Idle, herdr.Done)
	if herdr.IsCode(err, "agent_prompt_stalled") {
		// A turn fast enough to finish between herdr's samples still moves
		// the state sequence.
		if after, gerr := d.c.GetAgent(ctx, h.Pane); gerr == nil && after.StateChangeSeq != before.StateChangeSeq {
			return nil
		}
	}
	return err
}

// Poll reads the worker's phase. A blocked harness, or an idle one showing a
// startup dialog or a numbered menu (idleMenu), is Asking, with the question
// read from the screen.
func (d *herdrDriver) Poll(ctx context.Context, h Handle) (Status, error) {
	if _, err := d.c.GetPane(ctx, h.Pane); herdr.IsCode(err, "pane_not_found") {
		return Status{Phase: Gone, Session: h.Session, Transcript: h.Transcript}, nil
	} else if err != nil {
		return Status{}, err
	}
	ag, err := d.c.GetAgent(ctx, h.Pane)
	if herdr.IsCode(err, "agent_not_found") {
		st := Status{Phase: Working, Session: h.Session, Transcript: h.Transcript}
		info, err := d.c.PaneProcessInfo(ctx, h.Pane)
		if err != nil {
			return Status{}, err
		}
		if info.AtShell() {
			st.Phase = Exited
		}
		return st, nil
	}
	if err != nil {
		return Status{}, err
	}
	st := Status{Phase: Working, Seq: ag.StateChangeSeq}
	st.Session, st.Transcript = d.session(ctx, ag, h)
	if ag.AgentStatus != herdr.Blocked && !(ag.AgentStatus.Finished() && !ag.LaunchPending) {
		return st, nil
	}
	screen, err := d.c.ReadAgent(ctx, h.Pane, herdr.Visible, 0)
	if err != nil {
		return Status{}, err
	}
	if _, dialog := startupKeys(ag.AgentStatus, screen); ag.AgentStatus == herdr.Blocked || dialog || idleMenu(screen) {
		st.Phase = Asking
		st.Question, st.Options, st.Fingerprint = question(screen)
		return st, nil
	}
	st.Phase = Idle
	return st, nil
}

// session reads the harness session from herdr's agent record, falling back
// to what h already knows. herdr reports either the session id or, for Pi
// (and Claude when its hook sends one), the session file's path; for an id,
// locate finds the file. One not found yet is looked for again next Poll.
func (d *herdrDriver) session(ctx context.Context, ag herdr.Agent, h Handle) (session, file string) {
	session, file = h.Session, h.Transcript
	if ag.Session != nil && ag.Session.Value != "" {
		session, file = sessionRef(h.Harness, *ag.Session)
		if file == "" && session == h.Session {
			file = h.Transcript
		}
	}
	if file == "" && session != "" {
		file = d.locate(ctx, h, session)
	}
	return session, file
}

// locate is the file of session, found with the tab's environment (tabEnv).
// Where that cannot be read (macOS shows no other process's environment),
// the session id, which is unique, is looked for with routr's env for the
// tab over this process's environment, then over the harness defaults, and
// only a file that exists is taken: a wrong place finds nothing, not the
// wrong file.
func (d *herdrDriver) locate(ctx context.Context, h Handle, session string) string {
	if getenv, err := d.tabEnv(ctx, h); err == nil {
		file, _ := transcript.LocateEnv(h.Harness, session, h.Cwd, getenv)
		return file
	}
	env := expandHome(h.Env)
	for _, base := range []func(string) string{os.Getenv, func(string) string { return "" }} {
		getenv := func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return base(k)
		}
		file, err := transcript.LocateEnv(h.Harness, session, h.Cwd, getenv)
		if err != nil {
			continue
		}
		if _, err := os.Stat(file); err == nil {
			return file
		}
	}
	return ""
}

// tabEnv reads the environment h's tab received: h.Env, over what its shell
// inherited from the herdr server. That, not this process's environment,
// says where the harness keeps its sessions (CLAUDE_CONFIG_DIR, CODEX_HOME):
// the watcher need not run with the herdr server's environment.
func (d *herdrDriver) tabEnv(ctx context.Context, h Handle) (func(string) string, error) {
	info, err := d.c.PaneProcessInfo(ctx, h.Pane)
	if err != nil {
		return nil, err
	}
	if info.ShellPID == 0 {
		return nil, fmt.Errorf("worker: pane %s has no shell", h.Pane)
	}
	shell, err := procEnv(info.ShellPID)
	if err != nil {
		return nil, err
	}
	env := expandHome(h.Env)
	return func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return shell[k]
	}, nil
}

// procEnv is the environment process pid started with, from /proc.
func procEnv(pid int) (map[string]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, fmt.Errorf("worker: environment of process %d: %w", pid, err)
	}
	env := map[string]string{}
	for kv := range strings.SplitSeq(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

// sessionRef splits herdr's agent_session into the harness's resume key and
// its session file, if herdr gave a path. Pi resumes from the path itself;
// Claude's file is <id>.jsonl and Codex's rollout-<time>-<id>.jsonl.
func sessionRef(harness string, s herdr.AgentSession) (session, file string) {
	if s.Kind != "path" {
		return s.Value, ""
	}
	base := strings.TrimSuffix(filepath.Base(s.Value), ".jsonl")
	switch harness {
	case "claude":
		return base, s.Value
	case "codex":
		if len(base) >= 36 {
			return base[len(base)-36:], s.Value
		}
	}
	return s.Value, s.Value
}

// LastReply is the final message of the worker's last turn, read from its
// transcript.
func (d *herdrDriver) LastReply(ctx context.Context, h Handle) (string, error) {
	file := h.Transcript
	if file == "" {
		st, err := d.Poll(ctx, h)
		if err != nil {
			return "", err
		}
		file = st.Transcript
	}
	if file == "" {
		return "", fmt.Errorf("worker: no transcript for %s pane %s yet", h.Harness, h.Pane)
	}
	parse, err := transcript.ForKind(h.Harness)
	if err != nil {
		return "", err
	}
	return transcript.LastReply(file, parse)
}

// Answer picks option (1-based) of the menu on screen: with expect, only if
// its fingerprint is expect. The arrow keys only move the highlight; enter
// goes once the screen shows it on the option, in the same menu (a key can
// land late, and another question can come meanwhile). Then it waits for
// the question to leave the screen, or another to take its place; herdr
// goes on reporting Claude blocked for a moment after the keys land,
// so the screen tells, not the status. With once, an option that decides
// more than this call (menu.Lasting) is refused before any key goes.
func (d *herdrDriver) Answer(ctx context.Context, h Handle, option int, expect string, once bool) (string, error) {
	screen, err := d.c.ReadAgent(ctx, h.Pane, herdr.Visible, 0)
	if err != nil {
		return "", err
	}
	m, ok := menu.Parse(screen)
	fp := ""
	if ok {
		fp = fingerprint(screen, m)
	}
	if expect != "" && fp != expect {
		return "", changed(screen)
	}
	if !ok {
		return "", fmt.Errorf("worker: no menu on the %s screen to answer", h.Harness)
	}
	if option < 1 || option > len(m.Options) {
		return "", fmt.Errorf("worker: no option %d; pick 1-%d", option, len(m.Options))
	}
	if once && menu.Lasting(m.Options[option-1]) {
		return "", &LastingError{Option: option, Options: m.Options}
	}
	keys := m.Keys(option - 1)
	if err := d.press(ctx, h.Pane, keys[:len(keys)-1]); err != nil {
		return "", err
	}
	if err := d.highlighted(ctx, h, fp, option-1); err != nil {
		return "", err
	}
	if err := d.c.SendAgentKeys(ctx, h.Pane, "enter"); err != nil {
		return "", err
	}
	return m.Options[option-1], d.cleared(ctx, h, fp)
}

// changed is Answer's error for a screen that does not show the menu it
// expected.
func changed(screen string) *ChangedError {
	e := &ChangedError{}
	if _, ok := menu.Parse(screen); ok {
		e.Question, e.Options, e.Fingerprint = question(screen)
	}
	return e
}

// highlighted waits, for up to 2 settle times, until the menu with
// fingerprint fp shows option i highlighted.
func (d *herdrDriver) highlighted(ctx context.Context, h Handle, fp string, i int) error {
	wait, cancel := context.WithTimeout(ctx, 2*d.settle)
	defer cancel()
	for {
		screen, err := d.c.ReadAgent(ctx, h.Pane, herdr.Visible, 0)
		if err != nil {
			return err
		}
		m, ok := menu.Parse(screen)
		if !ok || fingerprint(screen, m) != fp {
			return changed(screen)
		}
		if m.Selected == i {
			return nil
		}
		if !sleep(wait, d.settle/8) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("worker: the highlight did not reach option %d; no option was chosen", i+1)
		}
	}
}

// cleared waits for the menu with fingerprint fp to leave the screen (or the
// harness to exit) after its answer, for up to 6 settle times (4.8 s).
func (d *herdrDriver) cleared(ctx context.Context, h Handle, fp string) error {
	wait, cancel := context.WithTimeout(ctx, 6*d.settle)
	defer cancel()
	for {
		if !sleep(wait, d.settle/8) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("worker: the keys went, but %w %s later", ErrStillAsking, 6*d.settle)
		}
		screen, err := d.c.ReadAgent(ctx, h.Pane, herdr.Visible, 0)
		if herdr.IsCode(err, "agent_not_found") {
			return nil
		} else if err != nil {
			return err
		}
		if m, ok := menu.Parse(screen); !ok || fingerprint(screen, m) != fp {
			return nil
		}
	}
}

// press sends one key per call with a short gap; batched keys can land before
// the TUI has redrawn.
func (d *herdrDriver) press(ctx context.Context, pane string, keys []string) error {
	for i, k := range keys {
		if i > 0 && !sleep(ctx, d.settle/5) {
			return ctx.Err()
		}
		if err := d.c.SendAgentKeys(ctx, pane, k); err != nil {
			return err
		}
	}
	return nil
}

// ErrOtherWorkspace is returned by Show without focus when the worker is in
// another workspace than besidePane.
var ErrOtherWorkspace = errors.New("the worker is in another workspace")

// Show puts the worker's pane beside besidePane (to its right) and, with
// focus, moves the user into it. A pane in another workspace is not moved,
// since that would change its id; with focus its tab is switched to instead.
func (d *herdrDriver) Show(ctx context.Context, h Handle, besidePane string, focus bool) error {
	p, err := d.c.GetPane(ctx, h.Pane)
	if err != nil {
		return err
	}
	b, err := d.c.GetPane(ctx, besidePane)
	if err != nil {
		return err
	}
	switch {
	case p.TabID == b.TabID: // already beside it
	case p.WorkspaceID != b.WorkspaceID:
		if !focus {
			return fmt.Errorf("worker: show %s beside %s: %w", h.Pane, besidePane, ErrOtherWorkspace)
		}
	default:
		// Focus separately: herdr 0.9.0's pane.move focus did not move clients.
		if _, err := d.c.MovePane(ctx, h.Pane, herdr.ToTab(b.TabID, besidePane, "right"), false); err != nil {
			return err
		}
	}
	if focus {
		return d.c.FocusPane(ctx, h.Pane)
	}
	return nil
}

// Hide moves the worker's pane from beside another pane back into a tab of
// its own, named by the pane's label. A pane already alone, or gone, is left.
func (d *herdrDriver) Hide(ctx context.Context, h Handle) error {
	p, err := d.c.GetPane(ctx, h.Pane)
	if herdr.IsCode(err, "pane_not_found") {
		return nil
	} else if err != nil {
		return err
	}
	tab, err := d.c.GetTab(ctx, p.TabID)
	if err != nil || tab.PaneCount <= 1 {
		return err
	}
	if p.Focused {
		// Focus a pane that stays, so the client never follows the worker
		// into a background tab.
		if err := d.focusOther(ctx, p); err != nil {
			return err
		}
	}
	label := p.Label
	if label == "" {
		label = h.Harness
	}
	_, err = d.c.MovePane(ctx, h.Pane, herdr.ToNewTab(p.WorkspaceID, label), false)
	return err
}

func (d *herdrDriver) focusOther(ctx context.Context, p herdr.Pane) error {
	panes, err := d.c.ListPanes(ctx, p.WorkspaceID)
	if err != nil {
		return err
	}
	for _, o := range panes {
		if o.TabID == p.TabID && o.PaneID != p.PaneID {
			return d.c.FocusPane(ctx, o.PaneID)
		}
	}
	return nil
}

// Close closes the worker's pane, which ends the harness and closes its tab.
// The harness session stays resumable. A pane already gone is not an error.
func (d *herdrDriver) Close(ctx context.Context, h Handle) error {
	if err := d.c.ClosePane(ctx, h.Pane); err != nil && !herdr.IsCode(err, "pane_not_found") {
		return err
	}
	return nil
}

// expandHome returns env with a leading ~ in values expanded, since herdr
// sets the variables directly rather than through a shell.
func expandHome(env map[string]string) map[string]string {
	home, err := os.UserHomeDir()
	if len(env) == 0 || err != nil {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == "~" || strings.HasPrefix(v, "~/") {
			v = home + v[1:]
		}
		out[k] = v
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
