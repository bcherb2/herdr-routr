package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuiltinRoles(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := "hunt,implement,plan,refute,research,review,summarize,task"
	if got := strings.Join(c.RoleNames(), ","); got != want {
		t.Fatalf("roles = %s", got)
	}
	checks := []struct {
		role               string
		write, interactive bool
		gate, output       string
	}{
		{"task", true, false, "", ""},
		{"plan", true, true, "", "PLAN.md"},
		{"review", false, false, "", ""},
		{"hunt", false, false, "", ""},
		{"research", false, false, "", ""},
		{"implement", true, false, "you", ""},
		{"summarize", false, false, "", ""},
	}
	for _, k := range checks {
		r := c.Roles[k.role]
		if r.Writes() != k.write || r.Interactive != k.interactive || r.Gate != k.gate || r.Output != k.output || r.Preamble == "" {
			t.Errorf("%s = %+v", k.role, r)
		}
	}
	// permissions is unset, so a worker can take the caller's mode (TestResolvePermissions).
	if c.Roles["review"].Avoid != "caller" || !c.Roles["review"].Verdict || c.Permissions != "" {
		t.Errorf("review = %+v, permissions %q", c.Roles["review"], c.Permissions)
	}
	// hunt picks a harness and effort as review does, but asks for no verdict.
	rv, h := c.Roles["review"], c.Roles["hunt"]
	if h.Verdict || h.Avoid != rv.Avoid || h.Effort != rv.Effort || !slices.Equal(h.Prefer, rv.Prefer) || !strings.Contains(h.Preamble, "candidate") {
		t.Errorf("hunt = %+v", h)
	}
}

func TestLoadMergesUserUnderRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", "/h")
	user := write(t, dir, "user.toml", `
permissions = "auto"
[env]
CLAUDE_CONFIG_DIR = "~/.claude-alt"
[harness.claude]
model = "sonnet"
[roles.review]
prefer = ["claude"]
effort = "medium"
[roles.review.models]
claude = "opus"
[roles.lint]
preamble = "lint it"
write = false
[registry]
keep_days = 30
max = 500
`)
	repo := write(t, dir, "repo.toml", `
[harness.codex]
model = "gpt-6-astra"
args = ["-c", "x=1"]
[roles.review]
effort = "low"
models = { codex = "gpt-6-astra" }
[registry]
max = 50
`)
	c, err := Load(user, filepath.Join(dir, "missing.toml"), repo)
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions != "auto" || c.Env["CLAUDE_CONFIG_DIR"] != "/h/.claude-alt" {
		t.Errorf("permissions %q env %v", c.Permissions, c.Env)
	}
	r := c.Roles["review"]
	if r.Effort != "low" || strings.Join(r.Prefer, ",") != "claude" || r.Avoid != "caller" || !r.Verdict {
		t.Errorf("review (repo wins per key, built-in kept) = %+v", r)
	}
	if r.Models["claude"] != "opus" || r.Models["codex"] != "gpt-6-astra" {
		t.Errorf("models merge key by key: %v", r.Models)
	}
	if c.Harness["claude"].Model != "sonnet" || c.Harness["codex"].Args[1] != "x=1" {
		t.Errorf("harness = %+v", c.Harness)
	}
	if c.Roles["lint"].Writes() || c.Roles["lint"].Preamble != "lint it" {
		t.Errorf("new role = %+v", c.Roles["lint"])
	}
	if c.Registry != (Registry{KeepDays: 30, Max: 50}) {
		t.Errorf("registry = %+v", c.Registry)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	for text, want := range map[string]string{
		"[roles.review]\nprefr = [\"codex\"]\n":    "roles.review.prefr",
		"permissions = \"yolo\"\n":                 "permissions",
		"[roles.x]\nprefer = [\"gemini\"]\n":       "gemini",
		"[roles.x]\neffort = \"turbo\"\n":          "effort",
		"[roles.x]\nefforts = {codex = \"max\"}\n": "roles.x.efforts.codex: codex doesn't take effort \"max\"",
		"[harness.codex]\neffort = \"off\"\n":      "harness.codex.effort",
		"[roles.x]\ngate = \"ci\"\n":               "gate",
		"[registry]\nkeep_days = -1\n":             "registry.keep_days",
		"[registry]\nmax = -5\n":                   "registry.max",
		"[registry]\nkeep = 3\n":                   "registry.keep",
		"not toml":                                 "bad.toml",
		"[questions]\nanswer = \"me\"\n":           `questions.answer = "me": use one of human, agent, jev`,
		"[questions]\nanswer = \"\"\n":             `questions.answer = "": use one of human, agent, jev`,
		"[questions]\nfallback = \"jev\"\n":        `questions.fallback = "jev": use one of human, agent`,
		"[questions]\nmin_confidence = 0\n":        "questions.min_confidence = 0: use a number above 0 and at most 1",
		"[questions]\nmin_confidence = 1.2\n":      "questions.min_confidence = 1.2",
		"[questions]\nmin_confidence = \"high\"\n": "bad.toml",
		"[questions]\ncontext = [\"/etc/x\"]\n":    `questions.context: "/etc/x": use a path relative to the repo root`,
		"[questions]\ncontext = [\"../x.md\"]\n":   `questions.context: "../x.md"`,
		"[questions]\ncontext = \"SPEC.md\"\n":     "bad.toml",
		"[questions]\nanswerer = \"jev\"\n":        "questions.answerer",
	} {
		p := write(t, dir, "bad.toml", text)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want mention of %s", text, err, want)
		}
	}
}

// [questions]: the built-in defaults, and each key set by a layer, the
// repo's winning key by key.
func TestQuestions(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if q := c.Questions; q.Answer != "human" || q.Fallback != "human" || q.MinConfidence != 0.8 || q.Context != nil {
		t.Errorf("defaults = %+v", q)
	}
	dir := t.TempDir()
	user := write(t, dir, "user.toml", "[questions]\nanswer = \"jev\"\nfallback = \"agent\"\ncontext = [\"SPEC.md\", \"docs/design.md\"]\n")
	repo := write(t, dir, "repo.toml", "[questions]\nanswer = \"agent\"\nmin_confidence = 1\n")
	if c, err = Load(user, repo); err != nil {
		t.Fatal(err)
	}
	if q := c.Questions; q.Answer != "agent" || q.Fallback != "agent" || q.MinConfidence != 1 || strings.Join(q.Context, ",") != "SPEC.md,docs/design.md" {
		t.Errorf("merged = %+v", q)
	}
	none := write(t, dir, "none.toml", "[questions]\ncontext = []\n")
	if c, err = Load(none); err != nil || c.Questions.Context == nil || len(c.Questions.Context) != 0 {
		t.Errorf("context = [] is not unset: %#v, %v", c.Questions.Context, err)
	}
}

// Workers start without the user's MCP servers unless a layer says so.
func TestStartMCPServers(t *testing.T) {
	c, err := Load()
	if err != nil || c.StartMCPServers {
		t.Errorf("built in: start_mcp_servers = %v, %v; want false", c.StartMCPServers, err)
	}
	dir := t.TempDir()
	user := write(t, dir, "user.toml", "start_mcp_servers = true\n")
	if c, err = Load(user); err != nil || !c.StartMCPServers {
		t.Errorf("user: start_mcp_servers = %v, %v; want true", c.StartMCPServers, err)
	}
	repo := write(t, dir, "repo.toml", "start_mcp_servers = false\n")
	if c, err = Load(user, repo); err != nil || c.StartMCPServers {
		t.Errorf("repo over user: start_mcp_servers = %v, %v; want false", c.StartMCPServers, err)
	}
}

func readyOnly(names ...string) ReadyFunc {
	return func(_ context.Context, h string) error {
		for _, n := range names {
			if n == h {
				return nil
			}
		}
		return errors.New("not logged in")
	}
}

func TestResolve(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Harness = map[string]Harness{"claude": {Model: "sonnet"}, "codex": {Model: "gpt-6-astra", Effort: "medium"}}
	c.Roles["cheap"] = Role{Prefer: []string{"pi", "claude"}, Effort: "high", Efforts: map[string]string{"pi": "off"}, Models: map[string]string{"pi": "openrouter/deepseek/deepseek-v4-flash"}}
	ctx := context.Background()
	tests := []struct {
		name  string
		req   Request
		ready ReadyFunc
		want  string // harness·model·effort·permissions
		err   string
	}{
		{"task goes to the caller's harness", Request{Caller: "codex"}, readyOnly("claude", "codex"), "codex·gpt-6-astra·medium·default", ""},
		{"task without caller", Request{}, readyOnly("claude", "codex"), "claude·sonnet··default", ""},
		{"review avoids the caller", Request{Role: "review", Caller: "codex"}, readyOnly("claude", "codex"), "claude·sonnet·high·default", ""},
		{"review prefers codex", Request{Role: "review", Caller: "claude"}, readyOnly("claude", "codex"), "codex·gpt-6-astra·high·default", ""},
		{"review skips a harness that is not ready", Request{Role: "review", Caller: "pi"}, readyOnly("claude"), "claude·sonnet·high·bypass", ""},
		{"research is quick on claude", Request{Role: "research"}, readyOnly("claude"), "claude·sonnet·low·default", ""},
		{"flags win", Request{Role: "review", Harness: "codex", Caller: "codex", Model: "m", Effort: "low", Permissions: "bypass"}, readyOnly("codex"), "codex·m·low·bypass", ""},
		{"nothing ready", Request{Role: "review", Caller: "codex"}, readyOnly("codex"), "", "codex: avoided"},
		{"unknown role", Request{Role: "nope"}, readyOnly("claude"), "", "unknown role"},
		{"unknown harness", Request{Harness: "gemini"}, readyOnly("claude"), "", "--harness"},
		{"harness not ready", Request{Harness: "codex"}, readyOnly("claude"), "", "codex cannot run workers: not logged in"},
		{"bad effort", Request{Effort: "turbo"}, readyOnly("claude"), "", `claude doesn't take effort "turbo"`},
		{"efforts differ per harness: claude takes max", Request{Effort: "max"}, readyOnly("claude"), "claude·sonnet·max·default", ""},
		{"efforts differ per harness: codex doesn't", Request{Harness: "codex", Effort: "max"}, readyOnly("codex"), "", "use one of low, medium, high"},
		{"pi takes off", Request{Harness: "pi", Effort: "off"}, readyOnly("pi"), "pi··off·default", ""},
		{"a role's per-harness effort wins over its effort", Request{Role: "cheap"}, readyOnly("pi", "claude"), "pi·openrouter/deepseek/deepseek-v4-flash·off·default", ""},
		{"and its effort covers the other harnesses", Request{Role: "cheap"}, readyOnly("claude"), "claude·sonnet·high·default", ""},
		{"the flag wins over a role's per-harness effort", Request{Role: "cheap", Effort: "xhigh"}, readyOnly("pi"), "pi·openrouter/deepseek/deepseek-v4-flash·xhigh·default", ""},
		{"bad permissions", Request{Permissions: "yolo"}, readyOnly("claude"), "", "--permissions"},
		{"auto needs jev", Request{Harness: "auto"}, readyOnly("claude"), "", "Jev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := c.Resolve(ctx, tt.req, tt.ready)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join([]string{r.Harness, r.Model, r.Effort, r.Permissions}, "·"); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
	r, _ := c.Resolve(ctx, Request{Role: "plan"}, readyOnly("claude"))
	if !r.Write || !r.Interactive || r.Output != "PLAN.md" || r.Preamble == "" {
		t.Errorf("plan = %+v", r)
	}
}

// A worker's permissions: --permissions, then config, then the caller's own
// mode where routr knows it (Pi's: bypass), then default.
func TestResolvePermissions(t *testing.T) {
	tests := []struct {
		name                 string
		flag, config, caller string
		want                 string
	}{
		{"none", "", "", "", "default"},
		{"pi runs without approvals", "", "", "pi", "bypass"},
		{"claude's mode is unknown", "", "", "claude", "default"},
		{"codex's mode is unknown", "", "", "codex", "default"},
		{"config over the caller", "", "auto", "pi", "auto"},
		{"config default over the caller", "", "default", "pi", "default"},
		{"config for other callers", "", "auto", "claude", "auto"},
		{"flag over config and the caller", "default", "auto", "pi", "default"},
		{"flag over the caller", "auto", "", "pi", "auto"},
		{"flag alone", "bypass", "", "codex", "bypass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			c.Permissions = tt.config
			r, err := c.Resolve(context.Background(), Request{Harness: "claude", Permissions: tt.flag, Caller: tt.caller}, readyOnly("claude"))
			if err != nil || r.Permissions != tt.want {
				t.Errorf("permissions = %q, %v; want %q", r.Permissions, err, tt.want)
			}
			if tt.flag == "" && c.PermissionsFor(tt.caller) != tt.want {
				t.Errorf("PermissionsFor(%q) = %q, want %q", tt.caller, c.PermissionsFor(tt.caller), tt.want)
			}
		})
	}
}

// Who answers a worker's questions: --answer (or a flow step's answer, which
// the flow's creator passes when --answer is not given), then config, then
// human.
func TestResolveAnswer(t *testing.T) {
	tests := []struct {
		name, req, config, want, err string
	}{
		{"default", "", "", "human", ""},
		{"config", "", "jev", "jev", ""},
		{"the request over config", "human", "jev", "human", ""},
		{"the request alone", "agent", "", "agent", ""},
		{"a bad request", "me", "jev", "", `--answer "me": use one of human, agent, jev`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			c.Questions.Answer = tt.config
			r, err := c.Resolve(context.Background(), Request{Harness: "claude", Answer: tt.req}, readyOnly("claude"))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || r.Answer != tt.want {
				t.Errorf("answer = %q, %v; want %q", r.Answer, err, tt.want)
			}
		})
	}
}

func TestDetectCaller(t *testing.T) {
	for env, want := range map[string]string{
		"CLAUDECODE=1":           "claude",
		"CODEX_THREAD_ID=abc":    "codex",
		"CODEX_SANDBOX=seatbelt": "codex",
		"PI_CODING_AGENT=true":   "pi",
		"PI_SESSION_ID=01a0":     "pi",
		// A Pi or Codex started from a shell that Claude Code opened
		// inherits CLAUDECODE; its own variables win.
		"CLAUDECODE=1 PI_SESSION_ID=01a0":   "pi",
		"CLAUDECODE=1 PI_CODING_AGENT=true": "pi",
		"CLAUDECODE=1 CODEX_THREAD_ID=abc":  "codex",
		"":                                  "",
	} {
		vars := map[string]string{}
		for _, kv := range strings.Fields(env) {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
		if got := DetectCaller(func(name string) string { return vars[name] }); got != want {
			t.Errorf("%s: got %q want %q", env, got, want)
		}
	}
}

func TestCallerSession(t *testing.T) {
	env := map[string]string{"PI_SESSION_ID": "p", "CLAUDE_CODE_SESSION_ID": "c", "CODEX_THREAD_ID": "x"}
	get := func(k string) string { return env[k] }
	for h, want := range map[string]string{"pi": "p", "claude": "c", "codex": "x", "": ""} {
		if got := CallerSession(h, get); got != want {
			t.Errorf("%q: %q", h, got)
		}
	}
}

func TestUserPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if UserPath() != "/x/routr/config.toml" {
		t.Error(UserPath())
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	if UserPath() != "/h/.config/routr/config.toml" {
		t.Error(UserPath())
	}
}
