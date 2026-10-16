package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bcherb2/herdr-routr/internal/hook"
	"github.com/bcherb2/herdr-routr/internal/route"
)

// Permission modes (skills/routr/cli.md, "Permissions").
const (
	PermDefault = "default"
	PermAuto    = "auto"
	PermBypass  = "bypass"
)

// argv is the harness command line after `--`: model and effort, the
// permission flags, Claude's --no-chrome, mcpArgs, and the resume command for
// s.Session. A new Claude session gets its id up front (--session-id), so
// its transcript is known before the harness reports it. session is the id
// the worker starts with.
func (d *herdrDriver) argv(ctx context.Context, s Spec) (args []string, session string, err error) {
	var h *route.Harness
	for _, x := range route.Defaults() {
		if x.Kind == s.Harness {
			h = &x
			break
		}
	}
	if h == nil {
		return nil, "", fmt.Errorf("worker: unknown harness %q", s.Harness)
	}
	args = h.Args(s.Model, s.Effort)
	perm, err := d.permissionArgs(ctx, s.Harness, s.Model, s.Permissions)
	if err != nil {
		return nil, "", err
	}
	args = append(args, perm...)
	if s.Harness == "claude" {
		// Claude in Chrome drives the user's own browser, signed in to
		// everything, and the user's ~/.claude.json can turn it on for every
		// interactive session (claudeInChromeDefaultEnabled): a Haiku
		// fact-checker asked to open a window. [harness.claude] args
		// can give it back with --chrome, which comes later and wins.
		args = append(args, "--no-chrome")
	}
	mcp, err := d.mcpArgs(ctx, s)
	if err != nil {
		return nil, "", err
	}
	args = append(args, mcp...)
	if allow := allowArgs(s, perm); allow != nil {
		args = append(args, allow...)
	}
	args = append(args, s.Args...)
	switch {
	case s.Session != "":
		args, err = route.ResumeArgs(s.Harness, s.Session, args)
		return args, s.Session, err
	case s.Harness == "claude", s.Harness == "pi" && strings.Contains(d.helpText(ctx, s.Harness), "--session-id"):
		// Pi 0.84 takes it too ("creating it if missing"), and herdr does
		// not always report a Pi session: without the id its transcript,
		// and so its final message, could not be found.
		session = d.newID()
		return append(args, "--session-id", session), session, nil
	}
	return args, "", nil
}

// DoneCommand is how a worker's brief spells `routr done`: the binary's
// path, not "$ROUTR_BIN". Claude asks before running any command with a
// variable in it ("Contains simple_expansion"), whatever its rules allow,
// and allowArgs's rule matches this text. "" falls back to the variable.
func DoneCommand(bin string) string {
	if bin == "" {
		return `"$ROUTR_BIN" done`
	}
	return shellQuote(bin) + " done"
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./+-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// autoTools are what a Claude worker under "auto" may use without asking
// when it runs in acceptEdits instead (autoModeless): the read-only web
// tools, which auto mode allows. Without them a research worker (Haiku by
// default) stops at its first web search.
var autoTools = []string{"WebSearch", "WebFetch"}

// allowArgs lets a worker run `routr done` without asking, whatever its
// permission mode: a per-launch allow rule for Claude (--settings merges it
// with the user's own), and for Codex its run dir as a writable root, since
// the run dir is outside the sandbox's workspace when the worker's cwd is a
// subdirectory or another worktree of the repo. s.Dirs (the chain's work
// directory, outside a worktree worker's cwd) are writable roots for Codex
// too, and added directories for Claude (--add-dir), so that it can write
// its output there without asking; Pi has no sandbox. perm is the
// permission flags already chosen. Claude's --settings also carries its
// PermissionRequest hook, in every mode (claudeSettings).
func allowArgs(s Spec, perm []string) []string {
	var args []string
	switch s.Harness {
	case "claude":
		var rules []string
		if bin := s.Env["ROUTR_BIN"]; bin != "" && s.Permissions != PermBypass {
			rules = append(rules, "Bash("+DoneCommand(bin)+":*)")
		}
		if s.Permissions == PermAuto && slices.Contains(perm, "acceptEdits") {
			rules = append(rules, autoTools...)
		}
		if settings := claudeSettings(s, rules); settings != "" {
			args = []string{"--settings", settings}
		}
	case "codex":
		if dir := s.Env["ROUTR_RUN_DIR"]; dir != "" && s.Permissions != PermBypass {
			args = []string{"--add-dir", dir}
		}
	default:
		return nil
	}
	if s.Permissions == PermBypass {
		return args
	}
	for _, d := range s.Dirs {
		args = append(args, "--add-dir", d)
	}
	return args
}

// claudeSettings is Claude's per-launch --settings JSON: the allow rules,
// and a PermissionRequest hook that appends each prompt's tool call to
// s.HookLog, so the watcher can say what a question is about (screens say
// "Do you want to proceed?"). "" when there is neither.
func claudeSettings(s Spec, rules []string) string {
	settings := map[string]any{}
	if bin := s.Env["ROUTR_BIN"]; bin != "" && s.HookLog != "" {
		settings["hooks"] = hook.Hooks(bin, s.HookLog, hook.PermissionRequest)
	}
	if len(rules) > 0 {
		settings["permissions"] = map[string]any{"allow": rules}
	}
	if len(settings) == 0 {
		return ""
	}
	b, _ := json.Marshal(settings)
	return string(b)
}

// autoModeless reports a Claude model that has no auto permission mode:
// Claude Code 2.1.283 starts Haiku with `--permission-mode auto` in manual
// mode ("⏸ manual mode on"), asking before every edit.
func autoModeless(model string) bool {
	return strings.Contains(strings.ToLower(model), "haiku")
}

// permissionArgs maps a permission mode to harness flags. "auto" uses the
// harness's own automatic approval where its --help lists one: Claude's
// `--permission-mode auto` (else, or for a model without it, acceptEdits),
// Codex's --approve-for-me (else the older --full-auto, else a
// workspace-write sandbox). Pi has no permission prompts, so every mode is
// the same there.
func (d *herdrDriver) permissionArgs(ctx context.Context, harness, model, mode string) ([]string, error) {
	switch mode {
	case "", PermDefault:
		return nil, nil
	case PermAuto, PermBypass:
	default:
		return nil, fmt.Errorf("worker: permissions must be default, auto or bypass, got %q", mode)
	}
	switch harness + "/" + mode {
	case "claude/" + PermBypass:
		return []string{"--dangerously-skip-permissions"}, nil
	case "codex/" + PermBypass:
		return []string{"--dangerously-bypass-approvals-and-sandbox"}, nil
	case "claude/" + PermAuto:
		if !autoModeless(model) && strings.Contains(d.helpText(ctx, harness), `"auto"`) {
			return []string{"--permission-mode", "auto"}, nil
		}
		return []string{"--permission-mode", "acceptEdits"}, nil
	case "codex/" + PermAuto:
		help := d.helpText(ctx, harness)
		switch {
		case strings.Contains(help, "--approve-for-me"):
			return []string{"--approve-for-me"}, nil
		case strings.Contains(help, "--full-auto"):
			return []string{"--full-auto"}, nil
		}
		return []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request"}, nil
	}
	return nil, nil
}

// mcpArgs keep the user's MCP servers and claude.ai connectors out of a
// worker unless config start_mcp_servers lets them in (s.MCP): they reach
// the user's own accounts, a bank or a signed-in browser among them,
// and under bypass nothing asks before their tools run. Claude's
// --strict-mcp-config drops the connectors too. Pi's --no-mcp is in its
// --help from 1.0, the first Pi with MCP; it turns off the built-in only,
// not an MCP extension (pi-mcp-adapter). Codex has no switch for all of
// them: its apps feature (its own connectors, as codex_apps) goes off by
// key, which a Codex without it ignores, and each server it lists by
// name; a list that fails fails the start rather than let them in.
func (d *herdrDriver) mcpArgs(ctx context.Context, s Spec) ([]string, error) {
	if s.MCP {
		return nil, nil
	}
	switch s.Harness {
	case "claude":
		return []string{"--strict-mcp-config"}, nil
	case "pi":
		if strings.Contains(d.helpText(ctx, s.Harness), "--no-mcp") {
			return []string{"--no-mcp"}, nil
		}
	case "codex":
		names, err := d.mcpServers(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("worker: listing Codex's MCP servers, to start it without them: %w (config start_mcp_servers = true starts workers with them)", err)
		}
		args := []string{"-c", "features.apps=false"}
		for _, n := range names {
			if !mcpName.MatchString(n) {
				return nil, fmt.Errorf("worker: Codex MCP server %q: routr can only turn off servers named with letters, digits, - and _ (config start_mcp_servers = true starts workers with them)", n)
			}
			args = append(args, "-c", "mcp_servers."+n+".enabled=false")
		}
		return args, nil
	}
	return nil, nil
}

// mcpName is a server name that is a bare key in Codex's -c override.
var mcpName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// codexMCPServers runs `codex mcp list --json` the way the worker's tab
// will start Codex (its cwd, its env over the watcher's) and returns the
// servers it would start.
func codexMCPServers(ctx context.Context, s Spec) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "mcp", "list", "--json")
	cmd.Dir = s.Cwd
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("codex mcp list: %v: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("codex mcp list: %w", err)
	}
	var servers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(out, &servers); err != nil {
		return nil, fmt.Errorf("codex mcp list --json: %w", err)
	}
	var names []string
	for _, x := range servers {
		if x.Enabled {
			names = append(names, x.Name)
		}
	}
	return names, nil
}

// helpText is the harness's --help output, read once per driver.
func (d *herdrDriver) helpText(ctx context.Context, harness string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.helps[harness]; ok {
		return t
	}
	if d.helps == nil {
		d.helps = map[string]string{}
	}
	t := d.help(ctx, harness)
	d.helps[harness] = t
	return t
}

// harnessHelp runs `harness --help`; "" if it fails.
func harnessHelp(ctx context.Context, harness string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, harness, "--help").CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}
