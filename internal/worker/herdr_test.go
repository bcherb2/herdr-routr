package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/herdr"
)

// --help excerpts: what the permission probes look for.
var helps = map[string]string{
	"claude": `--permission-mode <mode>  (choices: "acceptEdits", "auto", "bypassPermissions")`,
	"codex":  "--approve-for-me\n--dangerously-bypass-approvals-and-sandbox",
}

func TestArgv(t *testing.T) {
	const pi = "/home/u/.pi/agent/sessions/--w--/t_01a0.jsonl"
	tests := []struct {
		name string
		spec Spec
		help map[string]string
		want string
	}{
		{"claude new", Spec{Harness: "claude", Model: "haiku", Effort: "low"}, helps,
			"--model haiku --effort low --no-chrome --strict-mcp-config --session-id 00000000-0000-4000-8000-000000000001"},
		{"claude auto", Spec{Harness: "claude", Permissions: PermAuto}, helps,
			"--permission-mode auto --no-chrome --strict-mcp-config --session-id 00000000-0000-4000-8000-000000000001"},
		{"claude auto, older claude", Spec{Harness: "claude", Permissions: PermAuto}, nil,
			`--permission-mode acceptEdits --no-chrome --strict-mcp-config --settings {"permissions":{"allow":["WebSearch","WebFetch"]}} --session-id 00000000-0000-4000-8000-000000000001`},
		// Haiku has no auto mode (Claude starts it in manual mode):
		// acceptEdits, plus the web tools a research worker needs, or it
		// stops at its first web search.
		{"claude auto, haiku", Spec{Harness: "claude", Model: "claude-haiku-4-5", Permissions: PermAuto, Env: map[string]string{"ROUTR_BIN": "/opt/routr"}}, helps,
			`--model claude-haiku-4-5 --permission-mode acceptEdits --no-chrome --strict-mcp-config --settings {"permissions":{"allow":["Bash(/opt/routr done:*)","WebSearch","WebFetch"]}} --session-id 00000000-0000-4000-8000-000000000001`},
		// routr done never asks: the rule matches the brief's DoneCommand.
		{"claude may run routr done", Spec{Harness: "claude", Permissions: PermAuto, Env: map[string]string{"ROUTR_BIN": "/opt/routr"}}, helps,
			`--permission-mode auto --no-chrome --strict-mcp-config --settings {"permissions":{"allow":["Bash(/opt/routr done:*)"]}} --session-id 00000000-0000-4000-8000-000000000001`},
		{"claude default may run routr done, resumed", Spec{Harness: "claude", Session: "4f1c", Env: map[string]string{"ROUTR_BIN": "/my tools/routr"}}, helps,
			`--resume 4f1c --no-chrome --strict-mcp-config --settings {"permissions":{"allow":["Bash('/my tools/routr' done:*)"]}}`},
		// A worker in a subdirectory can't otherwise write its run dir.
		{"codex may write its run dir", Spec{Harness: "codex", Permissions: PermAuto, Env: map[string]string{"ROUTR_BIN": "/opt/routr", "ROUTR_RUN_DIR": "/r/.routr/runs/3"}}, helps,
			"--approve-for-me -c features.apps=false --add-dir /r/.routr/runs/3"},
		{"bypass needs no rule", Spec{Harness: "codex", Permissions: PermBypass, Env: map[string]string{"ROUTR_RUN_DIR": "/r/.routr/runs/3"}}, helps,
			"--dangerously-bypass-approvals-and-sandbox -c features.apps=false"},
		// A worker in its own worktree writes its output to the main repo's
		// work directory: a writable root for Codex, an added dir for Claude.
		{"codex in a worktree", Spec{Harness: "codex", Permissions: PermAuto, Dirs: []string{"/r/.routr/work/c1"}, Env: map[string]string{"ROUTR_RUN_DIR": "/r/.routr/runs/3"}}, helps,
			"--approve-for-me -c features.apps=false --add-dir /r/.routr/runs/3 --add-dir /r/.routr/work/c1"},
		{"codex default in a worktree", Spec{Harness: "codex", Dirs: []string{"/r/.routr/work/c1"}}, helps,
			"-c features.apps=false --add-dir /r/.routr/work/c1"},
		{"claude in a worktree", Spec{Harness: "claude", Permissions: PermAuto, Dirs: []string{"/r/.routr/work/c1"}, Env: map[string]string{"ROUTR_BIN": "/opt/routr"}}, helps,
			`--permission-mode auto --no-chrome --strict-mcp-config --settings {"permissions":{"allow":["Bash(/opt/routr done:*)"]}} --add-dir /r/.routr/work/c1 --session-id 00000000-0000-4000-8000-000000000001`},
		{"claude default in a worktree", Spec{Harness: "claude", Dirs: []string{"/r/.routr/work/c1"}}, helps,
			"--no-chrome --strict-mcp-config --add-dir /r/.routr/work/c1 --session-id 00000000-0000-4000-8000-000000000001"},
		{"claude bypass in a worktree", Spec{Harness: "claude", Permissions: PermBypass, Dirs: []string{"/r/.routr/work/c1"}}, helps,
			"--dangerously-skip-permissions --no-chrome --strict-mcp-config --session-id 00000000-0000-4000-8000-000000000001"},
		// Claude's PermissionRequest hook says what each prompt is about, in
		// every mode, in the one --settings with the allow rules.
		{"claude hook", Spec{Harness: "claude", HookLog: "/r/.routr/runs/3/hooks.jsonl", Env: map[string]string{"ROUTR_BIN": "/opt/routr"}}, helps,
			`--no-chrome --strict-mcp-config --settings {"hooks":{"PermissionRequest":[{"hooks":[{"command":"'/opt/routr' hook --log '/r/.routr/runs/3/hooks.jsonl' PermissionRequest","type":"command"}],"matcher":""}]},"permissions":{"allow":["Bash(/opt/routr done:*)"]}} --session-id 00000000-0000-4000-8000-000000000001`},
		{"claude hook, bypass", Spec{Harness: "claude", Permissions: PermBypass, HookLog: "/r/.routr/runs/3/hooks.jsonl", Dirs: []string{"/r/.routr/work/c1"}, Env: map[string]string{"ROUTR_BIN": "/opt/routr"}}, helps,
			`--dangerously-skip-permissions --no-chrome --strict-mcp-config --settings {"hooks":{"PermissionRequest":[{"hooks":[{"command":"'/opt/routr' hook --log '/r/.routr/runs/3/hooks.jsonl' PermissionRequest","type":"command"}],"matcher":""}]}} --session-id 00000000-0000-4000-8000-000000000001`},
		{"codex has no hook", Spec{Harness: "codex", HookLog: "/r/.routr/runs/3/hooks.jsonl", Env: map[string]string{"ROUTR_BIN": "/opt/routr"}}, helps, "-c features.apps=false"},
		{"pi in a worktree", Spec{Harness: "pi", Dirs: []string{"/r/.routr/work/c1"}}, helps, ""},
		{"claude bypass, resumed", Spec{Harness: "claude", Model: "haiku", Permissions: PermBypass, Session: "4f1c"}, helps,
			"--resume 4f1c --model haiku --dangerously-skip-permissions --no-chrome --strict-mcp-config"},
		{"codex default", Spec{Harness: "codex", Effort: "low", Permissions: PermDefault}, helps,
			`-c model_reasoning_effort="low" -c features.apps=false`},
		{"codex auto", Spec{Harness: "codex", Model: "gpt-6-astra", Permissions: PermAuto}, helps,
			"-m gpt-6-astra --approve-for-me -c features.apps=false"},
		{"codex auto, older codex", Spec{Harness: "codex", Permissions: PermAuto}, map[string]string{"codex": "--full-auto"},
			"--full-auto -c features.apps=false"},
		{"codex auto, no auto flag", Spec{Harness: "codex", Permissions: PermAuto}, nil,
			"--sandbox workspace-write --ask-for-approval on-request -c features.apps=false"},
		{"codex bypass, resumed", Spec{Harness: "codex", Effort: "low", Permissions: PermBypass, Session: "019a"}, helps,
			`resume 019a -c model_reasoning_effort="low" --dangerously-bypass-approvals-and-sandbox -c features.apps=false`},
		{"pi", Spec{Harness: "pi", Model: "deepseek/deepseek-v4-flash", Effort: "low", Permissions: PermBypass}, helps,
			"--model deepseek/deepseek-v4-flash --thinking low"},
		{"pi resumed", Spec{Harness: "pi", Session: pi}, helps, "--session " + pi},
		// A new Pi session gets its id up front where pi takes one, so its
		// transcript is found when herdr reports no session.
		{"pi new, with --session-id", Spec{Harness: "pi"}, map[string]string{"pi": "--session-id <id>  Use exact project session ID"},
			"--session-id 00000000-0000-4000-8000-000000000001"},
		// [harness.X] args from config, after routr's own flags.
		{"config args", Spec{Harness: "claude", Model: "opus", Args: []string{"--add-dir", "/shared"}}, helps,
			"--model opus --no-chrome --strict-mcp-config --add-dir /shared --session-id 00000000-0000-4000-8000-000000000001"},
		// Claude in Chrome stays off unless config gives it back: the
		// later flag wins.
		{"claude with chrome from config", Spec{Harness: "claude", Permissions: PermBypass, Args: []string{"--chrome"}}, helps,
			"--dangerously-skip-permissions --no-chrome --strict-mcp-config --chrome --session-id 00000000-0000-4000-8000-000000000001"},
		{"config args, resumed", Spec{Harness: "codex", Session: "019a", Args: []string{"--search"}}, helps,
			"resume 019a -c features.apps=false --search"},
		// The user's MCP servers and connectors stay out (start_mcp_servers);
		// with it, the harness starts with them as the user's sessions do.
		{"claude with MCP servers", Spec{Harness: "claude", MCP: true}, helps,
			"--no-chrome --session-id 00000000-0000-4000-8000-000000000001"},
		{"pi without MCP servers", Spec{Harness: "pi"}, map[string]string{"pi": "--no-mcp  Disable built-in MCP support"},
			"--no-mcp"},
		{"pi with MCP servers", Spec{Harness: "pi", MCP: true}, map[string]string{"pi": "--no-mcp  Disable built-in MCP support"},
			""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, session, err := newDriver(newFake(), tt.help).argv(context.Background(), tt.spec)
			if err != nil || strings.Join(args, " ") != tt.want {
				t.Errorf("argv = %q, %v; want %q", strings.Join(args, " "), err, tt.want)
			}
			if tt.spec.Session != "" && session != tt.spec.Session {
				t.Errorf("session = %q, want the resumed one", session)
			}
		})
	}
	d := newDriver(newFake(), helps)
	for _, s := range []Spec{
		{Harness: "claude", Permissions: "yolo"},
		{Harness: "codex", Session: "--last"},
		{Harness: "gemini"},
	} {
		if _, _, err := d.argv(context.Background(), s); err == nil {
			t.Errorf("%+v: want an error", s)
		}
	}
}

// Codex has no switch for all its MCP servers: each one it would start is
// turned off by name, and a list that fails fails the start.
func TestArgvTurnsOffCodexMCPServers(t *testing.T) {
	d := newDriver(newFake(), helps)
	var asked int
	d.mcpServers = func(context.Context, Spec) ([]string, error) {
		asked++
		return []string{"playwright", "dev_docs-2"}, nil
	}
	ctx := context.Background()
	args, _, err := d.argv(ctx, Spec{Harness: "codex", Session: "019a"})
	if want := "resume 019a -c features.apps=false -c mcp_servers.playwright.enabled=false -c mcp_servers.dev_docs-2.enabled=false"; err != nil || strings.Join(args, " ") != want {
		t.Errorf("argv = %q, %v; want %q", strings.Join(args, " "), err, want)
	}
	asked = 0
	if args, _, err := d.argv(ctx, Spec{Harness: "codex", MCP: true}); err != nil || len(args) != 0 || asked != 0 {
		t.Errorf("with start_mcp_servers: argv = %q, %v, listed %d times; want none, unlisted", args, err, asked)
	}
	d.mcpServers = func(context.Context, Spec) ([]string, error) { return nil, errors.New("codex mcp list: exit status 1") }
	if _, _, err := d.argv(ctx, Spec{Harness: "codex"}); err == nil || !strings.Contains(err.Error(), "start_mcp_servers = true") {
		t.Errorf("a failed list: err = %v; want the start to fail, naming the setting", err)
	}
	d.mcpServers = func(context.Context, Spec) ([]string, error) { return []string{"a.b"}, nil }
	if _, _, err := d.argv(ctx, Spec{Harness: "codex"}); err == nil {
		t.Error("a server name that is not a bare key: want an error, not a -c that turns off something else")
	}
}

// codexMCPServers asks Codex as the tab will start it: in its cwd, with its
// env, and only for the servers that are enabled.
func TestCodexMCPServers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for codex")
	}
	bin, cwd := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
[ "$*" = "mcp list --json" ] || { echo "args: $*" >&2; exit 2; }
[ "$PWD" = "` + cwd + `" ] || { echo "cwd: $PWD" >&2; exit 2; }
[ "$CODEX_HOME" = /tab/codex ] || { echo "CODEX_HOME: $CODEX_HOME" >&2; exit 2; }
echo '[{"name":"playwright","enabled":true},{"name":"off","enabled":false},{"name":"docs","enabled":true}]'
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", "/watcher/codex")
	got, err := codexMCPServers(context.Background(), Spec{Cwd: cwd, Env: map[string]string{"CODEX_HOME": "/tab/codex"}})
	if err != nil || !slices.Equal(got, []string{"playwright", "docs"}) {
		t.Errorf("codexMCPServers = %q, %v; want [playwright docs]", got, err)
	}
	if _, err := codexMCPServers(context.Background(), Spec{Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "cwd:") {
		t.Errorf("a failing codex: err = %v; want its stderr", err)
	}
}

func spec(t *testing.T, harness string) Spec {
	return Spec{ID: 3, Label: "3 review · " + harness, Harness: harness, Cwd: t.TempDir()}
}

func TestStartOpensATabInTheRoutrWorkspace(t *testing.T) {
	f := newFake()
	d := newDriver(f, helps)
	s := spec(t, "claude")
	s.Env = map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-alt", "ROUTR_ID": "3"}
	h, err := d.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	wantFile := filepath.Join(home, ".claude-alt", "projects", strings.ReplaceAll(strings.ReplaceAll(s.Cwd, "/", "-"), "_", "-"),
		"00000000-0000-4000-8000-000000000001.jsonl")
	if h.Pane != "w1:p1" || h.Tab != "w1:t1" || h.Harness != "claude" || h.Socket != "/tmp/fake.sock" ||
		h.Session != "00000000-0000-4000-8000-000000000001" || h.Transcript != wantFile || !strings.HasPrefix(h.Agent, "routr-3-") {
		t.Errorf("handle = %+v\nwant transcript %s", h, wantFile)
	}
	if got := f.called(); !slices.Equal(got, []string{"workspace.create routr focus=false", "tab.rename w1:t1 3 review · claude"}) {
		t.Errorf("calls = %q", got)
	}
	if f.tabEnv["CLAUDE_CONFIG_DIR"] != home+"/.claude-alt" || f.tabEnv["ROUTR_ID"] != "3" {
		t.Errorf("env = %v", f.tabEnv)
	}
	if f.panes[h.Pane].Label != s.Label || f.startKind != "claude" {
		t.Errorf("pane label %q, kind %q", f.panes[h.Pane].Label, f.startKind)
	}

	// The next worker gets a tab in the same workspace.
	h2, err := d.Start(context.Background(), spec(t, "codex"))
	if err != nil || h2.Pane != "w1:p2" || h2.Session != "" {
		t.Errorf("second handle = %+v, %v", h2, err)
	}
	if got := f.called(); got[len(got)-1] != "tab.create w1 3 review · codex focus=false" {
		t.Errorf("calls = %q", got)
	}
}

func TestStartInTheCallersWorkspace(t *testing.T) {
	f := newFake()
	f.workspaces = []herdr.Workspace{{WorkspaceID: "w1", Label: "main"}}
	s := spec(t, "codex")
	s.Workspace = "w1"
	h, err := newDriver(f, helps).Start(context.Background(), s)
	if err != nil || h.Pane != "w1:p1" {
		t.Fatalf("handle = %+v, %v", h, err)
	}
	// A workspace that is gone falls back to the routr one.
	s.Workspace = "w9"
	h, err = newDriver(f, helps).Start(context.Background(), s)
	if err != nil || h.Pane != "w2:p2" || f.workspaces[1].Label != Workspace {
		t.Fatalf("handle = %+v, %v; workspaces %v", h, err, f.workspaces)
	}
}

// Claude's folder-trust dialog highlights "No, exit" (menu package doc).
const trustScreen = `
 Do you trust the files in this folder?

 /tmp/w

 ❯ 1. No, exit
   2. Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

const codexApproval = `
  Would you like to run the following command?

  $ go test ./...

› 1. Yes, proceed (y)
  2. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel
`

// Claude Code 2.1.283 asking before a Write (live, TestLiveAskAndExit).
const claudeWrite = `
 ● Write(ask.txt)
 ────────────────────────────
  Create file
  ask.txt
  1 ok
  Do you want to create ask.txt?
  ❯ 1. Yes
    2. Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)
    3. No

  Esc to cancel · Tab to amend
`

// Claude Code 2.1.287's Chrome permission menu, laid out like claudeWrite
// with the text its bundle draws (not captured from a live screen).
// `herdr agent explain --file` calls it idle: no rule matches.
const claudeChrome = `
 ● claude-in-chrome - tabs_context_mcp (MCP)(createIfEmpty: true)
 ────────────────────────────
  Claude in Chrome wants to create a browser window and read your tabs

  ❯ 1. Allow
    2. Deny (esc)
`

// Idle screens, read live from herdr (Claude Code 2.1.287, Codex 0.160).
const claudeIdle = `
 ▐▛███▛█   Claude Code v2.1.287
▝▜██████▀  Haiku 4.5 · API Usage Billing
 ▝▝   ▝▝   /tmp/plan-test

────────────────────────────────────────
❯ Try "fix typecheck errors"
────────────────────────────────────────
  ⏸ manual mode on · ← 1 agent
`

const codexIdle = `
› Ask Codex to do anything

  GPT-6.1-Sol low · /tmp/routr-exp-work · main · Context 100% left
  ? for shortcuts                                        ⚠ 1 warning · f2 to view
`

const codexUpdate = `
  ✨ Update available! 0.157.1 -> 0.158.0

› 1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
  2. Skip
  3. Skip until next version

  Press enter to continue
`

// Codex 0.159 asks about hooks it hasn't run before.
const codexHooks = `
  Hooks need review
  4 hooks are new or changed.
  Hooks can run outside the sandbox after you trust them.


› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)

  enter confirm · esc skip
`

// Behind "Review hooks".
const codexHooksList = `
  Hooks
  Lifecycle hooks from config and enabled plugins.

  ⚠ 4 hooks need review before they can run.

  Event                 Installed   Active      Review      Description

  PreToolUse            1           1           0           Before a tool executes
› PreCompact            1           0           1           Before context compaction
  SessionStart          3           2           1           When a new session starts

  t trust all · enter review · esc close
`

const piNotice = `
 │ Update Available │ Welcome back! │
 ╰──── Press any key to continue (26s) ────╯
 >
`

func TestStartHandlesStartupScreens(t *testing.T) {
	tests := []struct {
		name   string
		status herdr.AgentStatus
		screen string
		want   []string // keys that clear it
	}{
		{"claude trust, blocked", herdr.Blocked, trustScreen, []string{"down", "enter"}},
		{"codex trust reported idle", herdr.Idle, strings.Replace(trustScreen, "❯", "›", 1), []string{"down", "enter"}},
		{"codex update is skipped", herdr.Idle, codexUpdate, []string{"down", "enter"}},
		{"pi notice", herdr.Idle, piNotice, []string{"esc"}},
		{"codex hooks are left untrusted", herdr.Idle, codexHooks, []string{"down", "down", "enter"}},
		{"codex hooks list is closed", herdr.Idle, codexHooksList, []string{"esc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.onStart = func(f *fakeHerdr, pane string) { f.setAgent(pane, tt.status, tt.screen) }
			f.onKey = func(f *fakeHerdr, pane, _ string) {
				if slices.Equal(f.pressed(), tt.want) {
					f.setAgent(pane, herdr.Idle, "❯ ")
				}
			}
			if _, err := newDriver(f, helps).Start(context.Background(), spec(t, "claude")); err != nil {
				t.Fatal(err)
			}
			if got := f.pressed(); !slices.Equal(got, tt.want) {
				t.Errorf("keys = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartReportsWhatItCannotAnswer(t *testing.T) {
	f := newFake()
	f.onStart = func(f *fakeHerdr, pane string) {
		f.setAgent(pane, herdr.Blocked, "Try the new model?\n❯ 1. Try gpt-7\n  2. Keep gpt-6-astra")
	}
	h, err := newDriver(f, helps).Start(context.Background(), spec(t, "codex"))
	if !errors.Is(err, ErrStartupDialog) || !strings.Contains(err.Error(), "Try the new model?\n1. Try gpt-7\n2. Keep gpt-6-astra") {
		t.Errorf("err = %v", err)
	}
	if h.Pane == "" || len(f.pressed()) != 0 {
		t.Errorf("handle %+v, keys %v: want the pane back and nothing pressed", h, f.pressed())
	}
}

func TestStartReportsAHarnessThatExits(t *testing.T) {
	f := newFake()
	f.onStart = func(f *fakeHerdr, pane string) { f.screens[pane] = "error: unexpected argument '--full-auto'\n$ " }
	_, err := newDriver(f, helps).Start(context.Background(), spec(t, "codex"))
	if err == nil || !strings.Contains(err.Error(), "exited during startup") || !strings.Contains(err.Error(), "--full-auto") {
		t.Errorf("err = %v", err)
	}
}

// A prompt on screen is not a free shell: its startup files may still run
// a child in the foreground, and herdr then refuses agent.start.
func TestStartWaitsForTheShellInTheForeground(t *testing.T) {
	f := newFake()
	f.busyFor = 5
	if _, err := newDriver(f, helps).Start(context.Background(), spec(t, "claude")); err != nil {
		t.Fatal(err)
	}
	if f.startBusy || f.starts != 1 {
		t.Errorf("agent.start with a child in the foreground: %v, %d calls", f.startBusy, f.starts)
	}
}

func busyErr() error {
	return &herdr.APIError{Method: "agent.start", Code: "agent_pane_busy", Message: "pane w1:p1 is not an available shell"}
}

func TestStartRetriesAPaneBusyStart(t *testing.T) {
	f := newFake()
	f.startErrs = []error{busyErr(), busyErr()}
	h, err := newDriver(f, helps).Start(context.Background(), spec(t, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Pane != "w1:p1" || f.starts != 3 || len(f.panes) != 1 {
		t.Errorf("handle %+v, %d starts, %d panes: want one pane started in three tries", h, f.starts, len(f.panes))
	}
	if slices.ContainsFunc(f.called(), func(c string) bool { return strings.HasPrefix(c, "close") }) {
		t.Errorf("calls = %q: the pane was closed", f.called())
	}
}

func TestStartGivesUpOnPaneBusyWithoutTheShellPrompt(t *testing.T) {
	f := newFake()
	f.startErrs = []error{busyErr(), busyErr(), busyErr(), busyErr()}
	f.onStart = func(f *fakeHerdr, pane string) { t.Error("started after giving up") }
	h, err := newDriver(f, helps).Start(context.Background(), spec(t, "claude"))
	if !herdr.IsCode(err, "agent_pane_busy") || f.starts != busyTries {
		t.Fatalf("err = %v after %d starts", err, f.starts)
	}
	if strings.Contains(err.Error(), "$") || strings.Contains(err.Error(), "\n") {
		t.Errorf("err = %q: the pane's screen is no part of it", err)
	}
	if h.Pane == "" {
		t.Error("the pane is not handed back")
	}
}

func TestStartValidates(t *testing.T) {
	d := newDriver(newFake(), helps)
	for _, s := range []Spec{
		{Harness: "gemini", Cwd: t.TempDir()},
		{Harness: "claude", Cwd: "relative"},
		{Harness: "claude", Cwd: filepath.Join(t.TempDir(), "missing")},
	} {
		if _, err := d.Start(context.Background(), s); err == nil {
			t.Errorf("%+v: want an error", s)
		}
	}
}

func TestPoll(t *testing.T) {
	piFile := "/home/u/.pi/agent/sessions/--w--/2026-09-26T06-41-43-848Z_01a0dc72.jsonl"
	codexFile := "/home/u/.codex/sessions/2026/09/26/rollout-2026-09-26T06-41-43-01a0dc72-c3a8-71e9-8b26-634aa69d2d7f.jsonl"
	tests := []struct {
		name    string
		harness string
		setup   func(f *fakeHerdr, pane string)
		want    Status
	}{
		{"gone", "claude", func(f *fakeHerdr, pane string) { delete(f.panes, pane) }, Status{Phase: Gone}},
		{"exited to the shell", "claude", func(f *fakeHerdr, pane string) {}, Status{Phase: Exited}},
		{"no agent detected but something runs", "claude", func(f *fakeHerdr, pane string) { f.atShell[pane] = false },
			Status{Phase: Working}},
		{"working", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Working, "…") },
			Status{Phase: Working, Seq: 1}},
		{"idle", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Idle, "❯ ") },
			Status{Phase: Idle, Seq: 1}},
		{"done counts as idle", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Done, "❯ ") },
			Status{Phase: Idle, Seq: 1}},
		{"blocked on a menu", "codex", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Blocked, codexApproval) },
			Status{Phase: Asking, Seq: 1, Question: "Would you like to run the following command?\n$ go test ./...\n" +
				"1. Yes, proceed (y)\n2. No, and tell Codex what to do differently (esc)",
				Options: []string{"Yes, proceed (y)", "No, and tell Codex what to do differently (esc)"}}},
		{"claude asks before a write", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Blocked, claudeWrite) },
			Status{Phase: Asking, Seq: 1, Question: "Do you want to create ask.txt?\n1. Yes\n2. Yes, and switch to accept edits " +
				"(auto-approve file edits and common file commands) for this session (shift+tab)\n3. No",
				Options: []string{"Yes", "Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)", "No"}}},
		{"blocked on free text", "claude", func(f *fakeHerdr, pane string) {
			f.setAgent(pane, herdr.Blocked, "What should the file be called?\n> ")
		},
			Status{Phase: Asking, Seq: 1, Question: "What should the file be called?"}},
		{"idle on codex's trust dialog", "codex", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Idle, trustScreen) },
			Status{Phase: Asking, Seq: 1, Question: "Do you trust the files in this folder?\n/tmp/w\n1. No, exit\n2. Yes, I trust this folder",
				Options: []string{"No, exit", "Yes, I trust this folder"}}},
		// herdr has no rule for Claude's Chrome permission menu.
		{"idle on a menu herdr doesn't know", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Idle, claudeChrome) },
			Status{Phase: Asking, Seq: 1, Question: "● claude-in-chrome - tabs_context_mcp (MCP)(createIfEmpty: true) " +
				"Claude in Chrome wants to create a browser window and read your tabs\n1. Allow\n2. Deny (esc)",
				Options: []string{"Allow", "Deny (esc)"}}},
		{"done on a menu herdr doesn't know", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Done, claudeChrome) },
			Status{Phase: Asking, Seq: 1, Question: "● claude-in-chrome - tabs_context_mcp (MCP)(createIfEmpty: true) " +
				"Claude in Chrome wants to create a browser window and read your tabs\n1. Allow\n2. Deny (esc)",
				Options: []string{"Allow", "Deny (esc)"}}},
		{"claude idle", "claude", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Idle, claudeIdle) },
			Status{Phase: Idle, Seq: 1}},
		{"codex idle", "codex", func(f *fakeHerdr, pane string) { f.setAgent(pane, herdr.Idle, codexIdle) },
			Status{Phase: Idle, Seq: 1}},
		// Two lines typed in the prompt box parse as an unnumbered menu.
		{"claude idle with a draft", "claude", func(f *fakeHerdr, pane string) {
			f.setAgent(pane, herdr.Idle, strings.Replace(claudeIdle, `❯ Try "fix typecheck errors"`, "❯ check the tests\n  then fix them", 1))
		}, Status{Phase: Idle, Seq: 1}},
		{"launch pending is not idle", "claude", func(f *fakeHerdr, pane string) {
			f.setAgent(pane, herdr.Idle, "❯ ")
			f.agents[pane].LaunchPending = true
		}, Status{Phase: Working, Seq: 1}},
		{"pi session path", "pi", func(f *fakeHerdr, pane string) {
			f.setAgent(pane, herdr.Idle, "> ")
			f.agents[pane].Session = &herdr.AgentSession{Kind: "path", Value: piFile}
		}, Status{Phase: Idle, Seq: 1, Session: piFile, Transcript: piFile}},
		{"codex session path", "codex", func(f *fakeHerdr, pane string) {
			f.setAgent(pane, herdr.Idle, "› ")
			f.agents[pane].Session = &herdr.AgentSession{Kind: "path", Value: codexFile}
		}, Status{Phase: Idle, Seq: 1, Session: "01a0dc72-c3a8-71e9-8b26-634aa69d2d7f", Transcript: codexFile}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			p := f.addPane("w1", "")
			tt.setup(f, p.PaneID)
			st, err := newDriver(f, helps).Poll(context.Background(), Handle{Pane: p.PaneID, Harness: tt.harness})
			if err != nil {
				t.Fatal(err)
			}
			if st.Phase != tt.want.Phase || st.Seq != tt.want.Seq || st.Question != tt.want.Question ||
				!slices.Equal(st.Options, tt.want.Options) || st.Session != tt.want.Session || st.Transcript != tt.want.Transcript {
				t.Errorf("status = %+v\nwant     %+v", st, tt.want)
			}
		})
	}
}

func TestPollKeepsTheKnownTranscriptForAnIDSession(t *testing.T) {
	f := newFake()
	p := f.addPane("w1", "")
	f.setAgent(p.PaneID, herdr.Idle, "❯ ")
	f.agents[p.PaneID].Session = &herdr.AgentSession{Kind: "id", Value: "abc"}
	h := Handle{Pane: p.PaneID, Harness: "claude", Session: "abc", Transcript: "/cfg/projects/-w/abc.jsonl"}
	st, err := newDriver(f, helps).Poll(context.Background(), h)
	if err != nil || st.Session != "abc" || st.Transcript != h.Transcript {
		t.Errorf("status = %+v, %v", st, err)
	}
}

// The transcript is looked for with the tab's environment (config [env]),
// not the watcher's: with CODEX_HOME set only there, Codex's was never
// found, the sessions tree was walked on every poll, and final.md was empty.
func TestPollFindsTheTranscriptWithTheTabsEnv(t *testing.T) {
	const id = "01a0dc72-c3a8-71e9-8b26-634aa69d2d7f"
	codexHome, piDir := t.TempDir(), t.TempDir()
	codexFile := filepath.Join(codexHome, "sessions", "2026", "09", "26", "rollout-2026-09-26T06-41-43-"+id+".jsonl")
	piFile := filepath.Join(piDir, "sessions", "--w--", "2026-09-26T08-09-10-976Z_"+id+".jsonl")
	for _, f := range []string{codexFile, piFile} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("{}\n"), 0o644)
	}
	t.Setenv("CODEX_HOME", t.TempDir()) // the watcher's own: not where the tab's codex writes
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	for _, tt := range []struct {
		harness string
		session *herdr.AgentSession // what herdr reports
		env     map[string]string
		want    string
	}{
		{"codex", &herdr.AgentSession{Kind: "id", Value: id}, map[string]string{"CODEX_HOME": codexHome}, codexFile},
		// herdr reported no Pi session; routr gave it the id (--session-id).
		{"pi", nil, map[string]string{"PI_CODING_AGENT_DIR": piDir}, piFile},
	} {
		f := newFake()
		p := f.addPane("w1", "")
		f.setAgent(p.PaneID, herdr.Idle, "› ")
		f.agents[p.PaneID].Session = tt.session
		h := Handle{Pane: p.PaneID, Harness: tt.harness, Cwd: "/w", Env: tt.env}
		if tt.session == nil {
			h.Session = id
		}
		st, err := newDriver(f, helps).Poll(context.Background(), h)
		if err != nil || st.Session != id || st.Transcript != tt.want {
			t.Errorf("%s %v: status = %+v, %v; want transcript %s", tt.harness, tt.env, st, err, tt.want)
		}
	}
}

// A variable the tab inherited from herdr, rather than got from routr, is
// read from the pane's shell, not from the watcher: a herdr server started
// with another CLAUDE_CONFIG_DIR than the watcher's had its workers'
// transcripts looked for in the watcher's directory.
func TestPollFindsTheTranscriptWithTheEnvTheShellInherited(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a process's environment is read from /proc")
	}
	tabDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // the watcher's own
	// A shell that has drawn its prompt: one just exec'd may not show its
	// environment in /proc yet.
	shell := exec.Command("sh", "-c", "echo ready; read x")
	shell.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+tabDir)
	stdin, _ := shell.StdinPipe()
	stdout, _ := shell.StdoutPipe()
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); shell.Wait() })
	if _, err := stdout.Read(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	f.shellPID = shell.Process.Pid
	p := f.addPane("w1", "")
	f.setAgent(p.PaneID, herdr.Idle, "❯ ")
	f.agents[p.PaneID].Session = &herdr.AgentSession{Kind: "id", Value: "abc"}
	h := Handle{Pane: p.PaneID, Harness: "claude", Cwd: "/w"}
	st, err := newDriver(f, helps).Poll(context.Background(), h)
	if want := filepath.Join(tabDir, "projects", "-w", "abc.jsonl"); err != nil || st.Transcript != want {
		t.Errorf("status = %+v, %v; want transcript %s", st, err, want)
	}
}

// macOS shows no other process's environment (no /proc, and ps and
// kern.procargs2 hide it), so a tab's inherited CLAUDE_CONFIG_DIR cannot be
// read there: every Claude worker herdr reported only a session id for had
// "no transcript". The session id is unique, so its file is looked for
// where it can be, and only one that exists is taken.
func TestPollFindsTheTranscriptWhenTheTabsEnvCannotBeRead(t *testing.T) {
	home, watcherDir, tabDir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	file := func(dir, id string) string { return filepath.Join(dir, "projects", "-w", id+".jsonl") }
	for _, f := range []string{file(filepath.Join(home, ".claude"), "dflt"), file(watcherDir, "wtch"), file(tabDir, "tab"), file(watcherDir, "tab")} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("{}\n"), 0o644)
	}
	for _, tt := range []struct {
		name, id, watcher string
		env               map[string]string
		want              string
	}{
		{"the harness default", "dflt", watcherDir, nil, file(filepath.Join(home, ".claude"), "dflt")},
		{"the watcher's own", "wtch", watcherDir, nil, file(watcherDir, "wtch")},
		{"routr's env for the tab first", "tab", watcherDir, map[string]string{"CLAUDE_CONFIG_DIR": tabDir}, file(tabDir, "tab")},
		{"nowhere: none rather than a guess", "gone", watcherDir, nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tt.watcher)
			f := newFake()
			f.shellPID = 1 << 30 // no such process: its environment cannot be read
			p := f.addPane("w1", "")
			f.setAgent(p.PaneID, herdr.Idle, "❯ ")
			f.agents[p.PaneID].Session = &herdr.AgentSession{Kind: "id", Value: tt.id}
			h := Handle{Pane: p.PaneID, Harness: "claude", Cwd: "/w", Env: tt.env}
			st, err := newDriver(f, helps).Poll(context.Background(), h)
			if err != nil || st.Session != tt.id || st.Transcript != tt.want {
				t.Errorf("status = %+v, %v; want transcript %q", st, err, tt.want)
			}
		})
	}
}

func TestPrompt(t *testing.T) {
	tests := []struct {
		name     string
		onPrompt func(f *fakeHerdr, pane, text string) error
		wantErr  bool
	}{
		{"picked up", func(f *fakeHerdr, pane, _ string) error { f.setAgent(pane, herdr.Working, ""); return nil }, false},
		{"finished between samples", func(f *fakeHerdr, pane, _ string) error {
			f.setAgent(pane, herdr.Working, "")
			f.setAgent(pane, herdr.Idle, "pong")
			return &herdr.APIError{Code: "agent_prompt_stalled"}
		}, false},
		{"swallowed", func(*fakeHerdr, string, string) error { return &herdr.APIError{Code: "agent_prompt_stalled"} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			p := f.addPane("w1", "")
			f.setAgent(p.PaneID, herdr.Idle, "❯ ")
			var got string
			f.onPrompt = func(f *fakeHerdr, pane, text string) error { got = text; return tt.onPrompt(f, pane, text) }
			err := newDriver(f, helps).Prompt(context.Background(), Handle{Pane: p.PaneID}, "reply pong")
			if (err != nil) != tt.wantErr || got != "reply pong" {
				t.Errorf("err = %v, sent %q", err, got)
			}
		})
	}
}

// Answer presses keys only on the menu expected, confirms only the option
// highlighted, and waits for the menu to go.
func TestAnswer(t *testing.T) {
	_, _, trustFP := question(trustScreen)
	_, _, otherFP := question(claudeWrite)
	trustOn2 := strings.NewReplacer(" ❯ 1. No", "   1. No", "   2. Yes", " ❯ 2. Yes").Replace(trustScreen)
	const working = "● Writing ask.txt…"
	tests := []struct {
		name    string
		screen  string
		option  int
		expect  string
		screens map[string]string // the screen a key leaves; a key not here changes nothing
		keys    []string
		text    string
		changed string // a *ChangedError with this fingerprint; "-" = with none
		err     error  // or this error
	}{
		{"the menu on screen", trustScreen, 2, "", map[string]string{"down": trustOn2, "enter": working},
			[]string{"down", "enter"}, "Yes, I trust this folder", "", nil},
		{"the menu expected", trustScreen, 1, trustFP, map[string]string{"enter": working}, []string{"enter"}, "No, exit", "", nil},
		{"another question comes next", trustScreen, 2, trustFP, map[string]string{"down": trustOn2, "enter": claudeWrite},
			[]string{"down", "enter"}, "Yes, I trust this folder", "", nil},
		{"another menu than expected", claudeWrite, 1, trustFP, nil, nil, "", otherFP, nil},
		{"no menu any more", "What should the file be called?\n> ", 1, trustFP, nil, nil, "", "-", nil},
		{"no menu", "What should the file be called?\n> ", 1, "", nil, nil, "", "", errors.New("no menu")},
		{"no option 0", trustScreen, 0, trustFP, nil, nil, "", "", errors.New("no option 0")},
		{"no option 3", trustScreen, 3, "", nil, nil, "", "", errors.New("no option 3; pick 1-2")},
		// The arrows only move the highlight: enter is what chooses.
		{"an arrow key gets lost", trustScreen, 2, "", nil, []string{"down"}, "", "", errors.New("the highlight did not reach option 2")},
		{"another question while the highlight moves", trustScreen, 2, trustFP, map[string]string{"down": claudeWrite},
			[]string{"down"}, "", otherFP, nil},
		{"the question stays", trustScreen, 1, "", nil, []string{"enter"}, "No, exit", "", ErrStillAsking},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			p := f.addPane("w1", "")
			f.setAgent(p.PaneID, herdr.Blocked, tt.screen)
			f.onKey = func(f *fakeHerdr, pane, key string) {
				if screen, ok := tt.screens[key]; ok && key == "enter" {
					f.setAgent(pane, herdr.Working, screen)
				} else if ok {
					f.setAgent(pane, herdr.Blocked, screen)
				}
			}
			text, err := newDriver(f, helps).Answer(context.Background(), Handle{Pane: p.PaneID, Harness: "claude"}, tt.option, tt.expect, false)
			if got := f.pressed(); !slices.Equal(got, tt.keys) || text != tt.text {
				t.Errorf("keys = %v, text %q; want %v, %q", got, text, tt.keys, tt.text)
			}
			var changed *ChangedError
			switch {
			case tt.changed != "":
				if !errors.As(err, &changed) || changed.Fingerprint != strings.Trim(tt.changed, "-") {
					t.Fatalf("err = %v, want another menu %q", err, tt.changed)
				}
				if tt.changed != "-" && (changed.Question == "" || len(changed.Options) == 0) {
					t.Errorf("what is on screen now: %+v", changed)
				}
			case tt.err == ErrStillAsking:
				if !errors.Is(err, ErrStillAsking) {
					t.Errorf("err = %v, want ErrStillAsking", err)
				}
			case tt.err != nil:
				if err == nil || !strings.Contains(err.Error(), tt.err.Error()) || errors.As(err, &changed) {
					t.Errorf("err = %v, want %q", err, tt.err)
				}
			case err != nil:
				t.Errorf("err = %v", err)
			}
		})
	}
}

// With once, an answer nobody saw on screen, Answer presses nothing on an
// option that saves a rule or lasts past this call; the one-off options
// still go, and without once any option does.
func TestAnswerOnce(t *testing.T) {
	bash := claudeBash(120)
	_, _, fp := question(bash)
	on2 := strings.NewReplacer(" ❯ 1. Yes", "   1. Yes", "   2. Yes, and", " ❯ 2. Yes, and").Replace(bash)
	const working = "● Running echo one > one.txt…"
	for _, tt := range []struct {
		name    string
		screen  string
		option  int
		once    bool
		keys    []string
		lasting bool // a *LastingError
	}{
		{"a rule from this project, by agent or jev", bash, 2, true, nil, true},
		{"a mode for the session", claudeWrite, 2, true, nil, true},
		{"the one-off yes", bash, 1, true, []string{"enter"}, false},
		{"a rule, by the user", bash, 2, false, []string{"down", "enter"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			p := f.addPane("w1", "")
			f.setAgent(p.PaneID, herdr.Blocked, tt.screen)
			f.onKey = func(f *fakeHerdr, pane, key string) {
				if key == "enter" {
					f.setAgent(pane, herdr.Working, working)
				} else {
					f.setAgent(pane, herdr.Blocked, on2)
				}
			}
			expect := ""
			if tt.screen == bash {
				expect = fp
			}
			_, err := newDriver(f, helps).Answer(context.Background(), Handle{Pane: p.PaneID, Harness: "claude"}, tt.option, expect, tt.once)
			if got := f.pressed(); !slices.Equal(got, tt.keys) {
				t.Errorf("keys = %v, want %v", got, tt.keys)
			}
			var lasting *LastingError
			switch {
			case tt.lasting && (!errors.As(err, &lasting) || lasting.Option != tt.option || len(lasting.Options) != 3):
				t.Errorf("err = %v, want a *LastingError for option %d", err, tt.option)
			case !tt.lasting && err != nil:
				t.Errorf("err = %v", err)
			}
		})
	}
}

// claudeBash is Claude Code 2.1.287 asking before a Bash command (as herdr
// reads it), its rules as wide as the pane. The ● of the
// tool call above the dialog blinks while it waits.
func claudeBash(width int) string {
	rule, dashes := strings.Repeat("─", width), strings.Repeat("╌", width)
	return `
❯ Run echo one > one.txt with the Bash tool.

  Creating one.txt with content "one"
  ⎿  $ echo one > one.txt

` + rule + `
 Bash command
 Create one.txt with content "one"
` + dashes + `
 echo one > one.txt
` + dashes + `
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to /tmp/TestProbe/001 from this project
   3. No

 Esc to cancel · Tab to amend
`
}

// A fingerprint names one dialog however it is drawn, and tells it from
// any other.
func TestFingerprint(t *testing.T) {
	edit := func(s string, pairs ...string) string { return strings.NewReplacer(pairs...).Replace(s) }
	bash := claudeBash(120)
	codexHeader := "╭────────────────────╮\n│ >_ OpenAI Codex    │\n╰────────────────────╯\n\n• Running go test ./...\n"
	tests := []struct {
		name       string
		base, same string // same: drawn differently, the same dialog
		other      string // another dialog: "" = none to compare
	}{
		{"the highlight moves and the footer with it", bash,
			edit(bash, " ❯ 1. Yes\n   2.", "   1. Yes\n ❯ 2.", "Esc to cancel · Tab to amend", "Esc to cancel"), ""},
		{"the tool call's ● blinks above it", bash, edit(bash, `  Creating one.txt`, `● Creating one.txt`), ""},
		{"a narrow pane wraps it anew", bash,
			edit(claudeBash(48), ` Create one.txt with content "one"`, " Create one.txt with\n content \"one\"",
				"2. Yes, and always allow access to /tmp/TestProbe/001 from this project",
				"2. Yes, and always allow access to\n      /tmp/TestProbe/001 from this project"), ""},
		{"another command, the same question and options", bash, "", edit(bash, "echo one > one.txt", "echo two > two.txt")},
		{"another question", bash, "", edit(bash, "Do you want to proceed?", "Do you want to run it?")},
		{"another option", bash, "", edit(bash, "3. No\n", "3. No, and tell Claude what to do differently (esc)\n")},
		{"one more option", bash, "", edit(bash, "3. No\n", "3. No\n   4. Ask me later\n")},
		{"codex: the highlight moves", codexApproval,
			edit(codexApproval, "› 1. Yes, proceed (y)\n  2.", "  1. Yes, proceed (y)\n› 2."), ""},
		// The rule of Codex's header box is not the dialog's: the running
		// cell under it, whose bullet animates, is left out.
		{"codex: its history animates above the question", codexHeader + codexApproval,
			edit(codexHeader, "• Running", "◦ Running") + codexApproval, ""},
		{"codex: another command", codexApproval, "", edit(codexApproval, "go test ./...", "go vet ./...")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, fp := question(tt.base)
			if len(fp) != 16 {
				t.Fatalf("fingerprint = %q", fp)
			}
			if tt.same != "" {
				if _, opts, got := question(tt.same); got != fp || len(opts) == 0 {
					t.Errorf("redrawn: %q (options %q), want %q", got, opts, fp)
				}
			}
			if tt.other != "" {
				if _, _, got := question(tt.other); got == fp || got == "" {
					t.Errorf("another dialog: %q, the same as or no fingerprint", got)
				}
			}
		})
	}
	if _, _, fp := question("What should the file be called?\n> "); fp != "" {
		t.Errorf("a question without a menu has fingerprint %q", fp)
	}
}

func TestShowHideClose(t *testing.T) {
	f := newFake()
	f.workspaces = []herdr.Workspace{{WorkspaceID: "w1"}, {WorkspaceID: "w2"}}
	caller := f.addPane("w1", "")
	worker := f.addPane("w1", "3 review · codex")
	other := f.addPane("w2", "")
	d := newDriver(f, helps)
	ctx := context.Background()
	h := Handle{Pane: worker.PaneID, Harness: "codex"}

	if err := d.Show(ctx, h, caller.PaneID, false); err != nil {
		t.Fatal(err)
	}
	if err := d.Show(ctx, h, caller.PaneID, true); err != nil { // already there: focus only
		t.Fatal(err)
	}
	if err := d.Hide(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := d.Hide(ctx, h); err != nil { // alone in its tab: nothing to do
		t.Fatal(err)
	}
	want := []string{
		"move w1:p2 beside w1:p1 in w1:t1",
		"focus w1:p2",
		"focus w1:p1", // the worker was focused: the caller keeps the client
		`move w1:p2 to new tab "3 review · codex"`,
	}
	if got := f.called(); !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant %q", got, want)
	}

	if err := d.Show(ctx, h, other.PaneID, false); !errors.Is(err, ErrOtherWorkspace) {
		t.Errorf("show across workspaces: err = %v", err)
	}
	if err := d.Close(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(ctx, h); err != nil {
		t.Errorf("closing a closed pane: %v", err)
	}
	if err := d.Hide(ctx, h); err != nil {
		t.Errorf("hiding a closed pane: %v", err)
	}
	if st, _ := d.Poll(ctx, h); st.Phase != Gone {
		t.Errorf("phase after close = %s", st.Phase)
	}
}

func TestLastReply(t *testing.T) {
	file := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(file, []byte(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"reply pong"}]}}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"pong"}],"stopReason":"stop"}}
`), 0o600)
	f := newFake()
	p := f.addPane("w1", "")
	f.setAgent(p.PaneID, herdr.Idle, "> ")
	d := newDriver(f, helps)
	if _, err := d.LastReply(context.Background(), Handle{Pane: p.PaneID, Harness: "pi"}); err == nil {
		t.Error("want an error before the session is known")
	}
	f.agents[p.PaneID].Session = &herdr.AgentSession{Kind: "path", Value: file}
	got, err := d.LastReply(context.Background(), Handle{Pane: p.PaneID, Harness: "pi"})
	if err != nil || got != "pong" {
		t.Errorf("reply = %q, %v", got, err)
	}
}

func TestDialogText(t *testing.T) {
	tests := []struct{ name, screen, first, q, detail string }{
		{"boxed", "╭────╮\n│ Allow edit to calc.py? │\n│ ❯ 1. Yes │\n│   2. No │\n╰────╯", "Yes", "Allow edit to calc.py?", ""},
		{"codex command", codexApproval, "Yes, proceed (y)", "Would you like to run the following command?", "$ go test ./..."},
		{"claude trust, narrow pane", "Accessing workspace:\n/tmp/w\nQuick safety check: Is this a project you created or one\nyou trust? (Like your own code, or work from your\nteam). If not, review it first.\n❯ 1. Yes, I trust this folder\n  2. No, exit",
			"Yes, I trust this folder", "Quick safety check: Is this a project you created or one you trust?", "(Like your own code, or work from your team). If not, review it first."},
		{"a later option starts like the first", claudeWrite, "Yes", "Do you want to create ask.txt?", ""},
		{"no question mark", "Working tree\npick one\n❯ 1. a\n  2. b", "a", "Working tree pick one", ""},
		{"empty", "\n\n", "", "(no question text on screen)", ""},
	}
	for _, tt := range tests {
		if q, d := dialogText(tt.screen, tt.first); q != tt.q || d != tt.detail {
			t.Errorf("%s: got %q / %q, want %q / %q", tt.name, q, d, tt.q, tt.detail)
		}
	}
}

func TestStartupKeysLeavesOrdinaryScreensAlone(t *testing.T) {
	for _, tt := range []struct {
		status herdr.AgentStatus
		screen string
	}{
		{herdr.Idle, "❯ \n  ? for shortcuts"},
		{herdr.Idle, "Pick one\n❯ 1. a\n  2. b"}, // an idle menu that is not a startup dialog
		{herdr.Blocked, "What should the file be called?\n> "},
	} {
		if keys, dialog := startupKeys(tt.status, tt.screen); keys != nil || dialog {
			t.Errorf("%s %q: keys %v dialog %v", tt.status, tt.screen, keys, dialog)
		}
	}
}
