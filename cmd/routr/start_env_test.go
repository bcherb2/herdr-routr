package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// withConfigEnv writes config [env] for the repo. routr exports [env] into
// its own process (applyConfig), so the test restores the variables.
func (c *cli) withConfigEnv(kv map[string]string) {
	c.t.Helper()
	var b strings.Builder
	b.WriteString("[env]\n")
	for k, v := range kv {
		c.t.Setenv(k, "")
		b.WriteString(k + " = \"" + v + "\"\n")
	}
	os.MkdirAll(c.p.Dir, 0o755)
	if err := os.WriteFile(c.p.Config(), []byte(b.String()), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func TestStartEnvAndArgs(t *testing.T) {
	c := newCLI(t)
	c.withConfigEnv(map[string]string{"RT_ACCOUNT": "/cfg", "RT_KEY": "secret-from-config"})
	home, _ := os.UserHomeDir()
	c.mustRun("", "start", "--env", "RT_ACCOUNT=~/.claude-alt", "--env", "RT_MORE=a=b", "--arg", "--chrome", "--arg", "--add-dir=/s", "Fix it")
	w := c.worker(1)
	want := map[string]string{"RT_ACCOUNT": filepath.Join(home, ".claude-alt"), "RT_KEY": "secret-from-config", "RT_MORE": "a=b"}
	if !maps.Equal(w.Env, want) || !slices.Equal(w.Args, []string{"--chrome", "--add-dir=/s"}) {
		t.Errorf("env %v args %v, want %v and --chrome --add-dir=/s", w.Env, w.Args, want)
	}
	c.mustRun("", "start", "Plain")
	if w := c.worker(2); !maps.Equal(w.Env, map[string]string{"RT_ACCOUNT": "/cfg", "RT_KEY": "secret-from-config"}) {
		t.Errorf("no --env: env %v, want config's, frozen", w.Env)
	}
	c.mustRun("", "start", "--run", "true", "--env", "RT_MORE=1")
	if w := c.worker(3); w.Env["RT_MORE"] != "1" || w.Env["RT_KEY"] != "secret-from-config" {
		t.Errorf("run step env %v", w.Env)
	}

	// list --json names the variables and never prints their values.
	out := c.mustRun("", "list", "--json")
	if strings.Contains(out, "secret-from-config") || strings.Contains(out, "claude-alt") {
		t.Errorf("list --json prints env values:\n%s", out)
	}
	var vs []struct {
		ID  int      `json:"id"`
		Env []string `json:"env"`
	}
	if err := json.Unmarshal([]byte(out), &vs); err != nil || len(vs) != 3 || !slices.Equal(vs[0].Env, []string{"RT_ACCOUNT", "RT_KEY", "RT_MORE"}) {
		t.Errorf("list --json env: %+v %v", vs, err)
	}
	if fi, err := os.Stat(c.p.Registry()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("registry.json mode: %v %v", fi.Mode(), err)
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"start", "--env", "ROUTR_ID=9", "x"}, "--env: ROUTR_ID is routr's own"},
		{[]string{"start", "--env", "ROUTR_STATE_DIR=/x", "x"}, "ROUTR_STATE_DIR is routr's own"},
		{[]string{"start", "--env", "ROUTR_CWD=/x", "x"}, "ROUTR_CWD is routr's own"},
		{[]string{"start", "--env", "NOVALUE", "x"}, `--env "NOVALUE": use K=V`},
		{[]string{"start", "--env", "1X=a", "x"}, `"1X" is no variable name`},
		{[]string{"start", "--arg", "--resume", "x"}, "--arg: --resume is routr's"},
		{[]string{"start", "--arg", "--session-id=abc", "x"}, "--session-id is routr's"},
		{[]string{"start", "--arg", "-p", "x"}, "-p is routr's"},
		{[]string{"start", "--harness", "codex", "--arg", "resume", "x"}, "resume is routr's"},
		{[]string{"start", "--run", "true", "--arg", "--x"}, "--arg: a --run step runs a command"},
		{[]string{"flow", "start", "plan-redteam", "--task", "t", "--env", "ROUTR_BIN=/x"}, "ROUTR_BIN is routr's own"},
	} {
		if code, _, errOut := c.run("", tt.args...); code != exitUsage || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: exit %d %q, want %d with %q", tt.args, code, errOut, exitUsage, tt.want)
		}
	}
	// Codex's -p is --profile, not print mode.
	c.mustRun("", "start", "--harness", "codex", "--arg", "-p", "--arg", "work", "x")
}

// config start_mcp_servers is frozen on each worker routr start or flow
// start queues: off unless config turns it on.
func TestStartMCPServersIsFrozen(t *testing.T) {
	c := newCLI(t)
	c.mustRun("", "start", "Plain")
	if c.worker(1).MCP {
		t.Error("built-in config: worker starts with MCP servers")
	}
	os.MkdirAll(c.p.Dir, 0o755)
	if err := os.WriteFile(c.p.Config(), []byte("start_mcp_servers = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.mustRun("", "start", "With")
	if !c.worker(2).MCP {
		t.Error("start_mcp_servers = true: routr start worker has no MCP")
	}
	chain := strings.TrimSpace(c.mustRun("", "flow", "start", "plan-redteam", "--task", "t"))
	for _, w := range c.chain(chain) {
		if !w.Tabless() && !w.MCP {
			t.Errorf("start_mcp_servers = true: flow worker %d has no MCP", w.ID)
		}
	}
}

// A flow step's env and args reach its workers over config's, and flow
// start --env over those, for agent steps; a run step keeps its own env and
// config's, and a judge has none.
func TestFlowStepEnvAndArgs(t *testing.T) {
	c := newCLI(t)
	c.withConfigEnv(map[string]string{"RT_ACCOUNT": "/cfg", "RT_KEY": "k"})
	flow := `[flow.mix]
[[flow.mix.step]]
id = "dig"
role = "research"
brief = "Dig."
env = { RT_ACCOUNT = "/step", RT_STEP = "s" }
args = ["--chrome"]
[[flow.mix.step]]
id = "check"
after = ["dig"]
run = "true"
env = { RT_RUN = "r" }
[[flow.mix.step]]
id = "ship"
after = ["dig"]
judge = "jev"
brief = "Ship it?"
`
	chain := strings.TrimSpace(c.mustRun(flow, "flow", "start", "--file", "-", "--env", "RT_STEP=flag", "--env", "RT_FLAG=f"))
	ws := c.chain(chain)
	if len(ws) != 3 {
		t.Fatalf("chain %s: %+v", chain, ws)
	}
	dig, check, ship := ws[0], ws[1], ws[2]
	if !maps.Equal(dig.Env, map[string]string{"RT_ACCOUNT": "/step", "RT_KEY": "k", "RT_STEP": "flag", "RT_FLAG": "f"}) || !slices.Equal(dig.Args, []string{"--chrome"}) {
		t.Errorf("dig env %v args %v", dig.Env, dig.Args)
	}
	if !maps.Equal(check.Env, map[string]string{"RT_ACCOUNT": "/cfg", "RT_KEY": "k", "RT_RUN": "r"}) {
		t.Errorf("check env %v", check.Env)
	}
	if ship.Env != nil {
		t.Errorf("judge env %v", ship.Env)
	}
	bad := strings.Replace(flow, `args = ["--chrome"]`, `harness = "claude"`+"\n"+`args = ["-c"]`, 1)
	if code, _, errOut := c.run(bad, "flow", "start", "--file", "-"); code == 0 || !strings.Contains(errOut, "-c is routr's") {
		t.Errorf("a step with -c: exit %d %s", code, errOut)
	}
}

func TestFlowStartOverridesHarnessModelEffort(t *testing.T) {
	c := newCLI(t)
	flow := `[flow.mix]
[[flow.mix.step]]
id = "a"
role = "research"
harness = "codex"
model = "gpt-x"
effort = "high"
brief = "A."
[[flow.mix.step]]
id = "b"
harness = "claude"
model = "opus"
brief = "B."
[[flow.mix.step]]
id = "check"
after = ["a", "b"]
run = "true"
[[flow.mix.step]]
id = "pick"
after = ["a", "b"]
judge = "jev"
pick = true
brief = "Which?"
`
	start := func(args ...string) []registry.Worker {
		t.Helper()
		chain := strings.TrimSpace(c.mustRun(flow, append([]string{"flow", "start", "--file", "-"}, args...)...))
		return c.chain(chain)
	}
	ws := start("--harness", "claude", "--model", "haiku", "--effort", "low")
	for _, w := range ws[:2] {
		if w.Harness != "claude" || w.Model != "haiku" || w.Effort != "low" {
			t.Errorf("step %s: %s, want claude · haiku · low", w.Step, w.Tag())
		}
	}
	for _, w := range ws[2:] {
		if w.Harness != "" || w.Model != "" || w.Effort != "" {
			t.Errorf("step %s (run or judge) got %+v", w.Step, w)
		}
	}
	// A harness change drops the step's model, for the role's; the same
	// harness keeps it.
	ws = start("--harness", "claude")
	if a, b := ws[0], ws[1]; a.Harness != "claude" || a.Model != "sonnet" || a.Effort != "high" || b.Model != "opus" {
		t.Errorf("--harness claude: a %s, b %s; want claude · sonnet · high and opus", a.Tag(), b.Tag())
	}
	ws = start("--effort", "medium")
	if a, b := ws[0], ws[1]; a.Tag() != "codex · gpt-x · medium" || b.Tag() != "claude · opus · medium" {
		t.Errorf("--effort medium: a %s, b %s", a.Tag(), b.Tag())
	}
	if code, _, errOut := c.run(flow, "flow", "start", "--file", "-", "--model", "haiku"); code != exitUsage || !strings.Contains(errOut, "--model needs --harness") {
		t.Errorf("--model alone: exit %d %s", code, errOut)
	}
	if code, _, errOut := c.run(flow, "flow", "start", "--file", "-", "--harness", "codex", "--effort", "max"); code == 0 || !strings.Contains(errOut, `codex doesn't take effort "max"`) {
		t.Errorf("--effort max on codex: exit %d %s", code, errOut)
	}
	if code, _, errOut := c.run(flow, "flow", "start", "--file", "-", "--harness", "nope"); code == 0 || !strings.Contains(errOut, `--harness "nope"`) {
		t.Errorf("--harness nope: exit %d %s", code, errOut)
	}
}
