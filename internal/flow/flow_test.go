package flow

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

const builtInNames = "adversarial-review,best-of-n,compare-models,debug-hypotheses,deep-research,fix-until-green,plan-redteam,plan-to-impl,repro-fix,research,security-review"

func TestBuiltIns(t *testing.T) {
	set, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(set.Names(), ","); got != builtInNames {
		t.Fatalf("names = %s", got)
	}
	for _, n := range set.Names() {
		if f := set[n]; f.Source != "built-in" || f.Description == "" {
			t.Errorf("%s: source %q description %q", n, f.Source, f.Description)
		}
	}
	rt := set["plan-redteam"].Steps[1]
	if rt.Harness != "codex" || rt.Effort != "high" || rt.Role != "review" {
		t.Errorf("redteam = %+v", rt)
	}
	if got := set["plan-to-impl"].Describe(); got != "plan-to-impl (task, in): plan → review → implement → code-review — plan with you, red-team it, gate, implement, review the code" {
		t.Errorf("describe = %q", got)
	}
	d := set["plan-to-impl"].Detail()
	for _, want := range []string{
		"from: built-in",
		"params: task, in",
		"step review: role review · after plan · output REDTEAM.md · on fail resume plan, max 3",
		"step implement: role implement · after review · gate you",
		"  on fail: Fix {{code-review.final}}.",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
}

// docs/default-flows.md charts every built-in flow, a "## NAME" section each.
func TestDefaultFlowsDocChartsEveryBuiltIn(t *testing.T) {
	doc, err := os.ReadFile("../../docs/default-flows.md")
	if err != nil {
		t.Fatal(err)
	}
	set, err := load()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range set.Names() {
		if !strings.Contains(string(doc), "\n## "+n+"\n") {
			t.Errorf("docs/default-flows.md has no \"## %s\" section", n)
		}
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLayers(t *testing.T) {
	cfg, root := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	user := filepath.Join(cfg, "routr", "flows", "mine.toml")
	write(t, user, `
[flow.plan-redteam]
[[flow.plan-redteam.step]]
id = "only"
brief = "just do it"

[flow.u]
description = "user"
[[flow.u.step]]
id = "s"
brief = "user version"
`)
	write(t, filepath.Join(cfg, "routr", "flows", "notes.txt"), "not toml")
	repo := filepath.Join(root, ".routr", "flows", "repo.toml")
	write(t, repo, `
[flow.u]
description = "repo"
[[flow.u.step]]
id = "s"
brief = "repo version"
`)
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if f := set["plan-redteam"]; len(f.Steps) != 1 || f.Source != user {
		t.Errorf("plan-redteam = %+v", f)
	}
	if f := set["u"]; f.Description != "repo" || f.Source != repo {
		t.Errorf("u = %+v", f)
	}
	if got := strings.Join(set.Names(), ","); got != builtInNames+",u" {
		t.Errorf("names = %s", got)
	}

	// No repo root and no user flows: built-ins only.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if set, err := Load(""); err != nil || strings.Join(set.Names(), ",") != builtInNames {
		t.Errorf("built-ins only: %v %v", set.Names(), err)
	}
}

// A flow's Text is its own table and steps as written: parsed alone, it is
// the same flow, and it carries no other flow's lines or the comments
// before the next flow.
func TestTextIsTheFlowAsWritten(t *testing.T) {
	set, err := load()
	if err != nil {
		t.Fatal(err)
	}
	src := `# a file comment
[flow.one]
description = "first"
[[flow.one.step]]
id = "a"
brief = "do a"
# about a's sibling
[[flow.one.step]]
id = "b"
after = ["a"]
brief = """
multi
[flow.not-a-header]
"""

# --- the second flow
[flow.two]
[[flow.two.step]]
id = "c"
brief = "do c"
`
	mine, err := Parse([]byte(src), "mine.toml")
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range mine {
		set[name] = f
	}
	for _, n := range set.Names() {
		f := set[n]
		again, err := Parse([]byte(f.Text), "text")
		if err != nil {
			t.Errorf("%s: its text does not parse: %v\n%s", n, err, f.Text)
			continue
		}
		g, ok := again[n]
		if len(again) != 1 || !ok || g.Description != f.Description || !reflect.DeepEqual(g.Params, f.Params) || !reflect.DeepEqual(g.Steps, f.Steps) {
			t.Errorf("%s: its text is not the flow: %v\n%s", n, again.Names(), f.Text)
		}
	}
	if one := mine["one"].Text; strings.Contains(one, "second flow") || !strings.Contains(one, "# about a's sibling") || !strings.HasSuffix(one, "\"\"\"\n") {
		t.Errorf("one:\n%s", one)
	}
}

func TestLoadErrorsNameTheFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := filepath.Join(cfg, "routr", "flows")
	flow := "[flow.x]\n[[flow.x.step]]\nid = \"s\"\nbrief = \"b\"\n"
	write(t, filepath.Join(dir, "a.toml"), flow)
	write(t, filepath.Join(dir, "b.toml"), flow)
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "b.toml: flow x is also defined in "+filepath.Join(dir, "a.toml")) {
		t.Errorf("duplicate: %v", err)
	}

	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bad := filepath.Join(root, ".routr", "flows", "bad.toml")
	write(t, bad, "[flow.x]\n[[flow.x.step]]\nid = \"s\"\nbrief = \"{{nope}}\"\n")
	_, err = Load(root)
	if err == nil || !strings.Contains(err.Error(), "flows "+bad+": flow x step s: brief: {{nope}}") {
		t.Errorf("bad file: %v", err)
	}
}

// flowSrc builds a one-flow file "a" with params p and the given steps.
func flowSrc(params string, steps ...string) string {
	s := "[flow.a]\nparams = [" + params + "]\n"
	for _, st := range steps {
		s += "[[flow.a.step]]\n" + st + "\n"
	}
	return s
}

func TestParseRejects(t *testing.T) {
	const (
		plan  = `id = "plan"` + "\n" + `brief = "plan {{task}}"` + "\n" + `output = "PLAN.md"`
		dig   = `id = "dig"` + "\n" + `brief = "dig {{item.path}}"` + "\n" + `foreach = "{{in}}/*.md"`
		judge = `id = "j"` + "\n" + `after = ["plan"]` + "\n" + `judge = "jev"` + "\n" + `brief = "Is {{plan.output}} done?"`
	)
	tests := []struct{ name, src, want string }{
		{"bad toml", "[flow.a", "flows t.toml: toml:"},
		{"unknown step key", flowSrc(`"task"`, plan+"\nefort = \"low\""), "flow a: unknown key step.efort"},
		{"unknown top key", "title = \"x\"\n" + flowSrc(`"task"`, plan), "unknown key title"},
		{"unknown on_fail key", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "plan", brief = "b", max = 1, maxx = 2 }`), "unknown key step.on_fail.maxx"},
		{"after is a string", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`after = "plan"`+"\n"+`brief = "b"`), "flows t.toml:"},
		{"bad flow name", "[flow.\"a b\"]\n[[flow.\"a b\".step]]\nid = \"s\"\nbrief = \"b\"", `flow name "a b"`},
		{"no steps", "[flow.a]\ndescription = \"x\"", "flow a has no steps"},
		{"bad param", flowSrc(`"a.b"`, plan), `flow a: param "a.b"`},
		{"duplicate param", flowSrc(`"task", "task"`, plan), `flow a: duplicate param "task"`},
		{"reserved param", flowSrc(`"work"`, `id = "s"`+"\n"+`brief = "b"`), `flow a: param "work" is reserved: every template has {{work}}`},
		{"bad id", flowSrc(`"task"`, `id = "a b"`+"\n"+`brief = "b"`), `flow a step 1: id "a b"`},
		{"missing id", flowSrc(`"task"`, `brief = "b"`), `flow a step 1: id ""`},
		{"reserved id", flowSrc(`"task"`, `id = "prev"`+"\n"+`brief = "b"`), `flow a step 1: id "prev" is reserved`},
		{"duplicate id", flowSrc(`"task"`, plan, plan), `flow a step 2: duplicate step id "plan"`},
		{"empty brief", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = " "`), "flow a step s: empty brief"},
		{"gate", flowSrc(`"task"`, plan+"\ngate = \"me\""), `flow a step plan: gate "me": the only gate is "you"`},
		{"effort", flowSrc(`"task"`, plan+"\neffort = \"max\""), `effort "max"`},
		{"answer", flowSrc(`"task"`, plan+"\nanswer = \"me\""), `flow a step plan: answer "me": use human, agent or jev`},
		{"answer a list", flowSrc(`"task"`, plan+"\nanswer = [\"jev\"]"), "flows t.toml:"},
		{"answer on a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`answer = "jev"`), "flow a step c: answer: a run step runs a command, so it has no answer"},
		{"answer on a judge", flowSrc(`"task"`, plan, judge+"\nanswer = \"human\""), "flow a step j: answer: a judge step asks Jev, so it has no answer"},
		{"env on a judge", flowSrc(`"task"`, plan, judge+"\nenv = { A = \"1\" }"), "flow a step j: env: a judge step asks Jev, so it has no env"},
		{"args on a judge", flowSrc(`"task"`, plan, judge+"\nargs = [\"--x\"]"), "flow a step j: args: a judge step asks Jev, so it has no args"},
		{"args on a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`args = ["--x"]`), "flow a step c: args: a run step runs a command, so it has no args"},
		{"routr's env", flowSrc(`"task"`, plan+"\nenv = { ROUTR_ID = \"9\", ROUTR_STATE_DIR = \"/x\" }"), "flow a step plan: env: ROUTR_ID is routr's own; ROUTR_STATE_DIR is routr's own"},
		{"bad env name", flowSrc(`"task"`, plan+"\nenv = { \"A-B\" = \"1\" }"), `flow a step plan: env: "A-B" is no variable name`},
		{"session args", flowSrc(`"task"`, plan+"\nargs = [\"--resume=abc\"]"), "flow a step plan: args: --resume is routr's"},
		{"codex resume", flowSrc(`"task"`, plan+"\nharness = \"codex\"\nargs = [\"resume\"]"), "flow a step plan: args: resume is routr's"},
		{"env a list", flowSrc(`"task"`, plan+"\nenv = [\"A=1\"]"), "env: use a table"},
		{"env a number", flowSrc(`"task"`, plan+"\nenv = { A = 1 }"), "env: A: use a string"},
		{"write on a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`write = false`), "flow a step c: write: a run step runs a command, so it has no write"},
		{"write on a judge", flowSrc(`"task"`, plan, judge+"\nwrite = false"), "flow a step j: write: a judge step asks Jev, so it has no write"},
		// halt is gone: any step's fail without on_fail halts.
		{"halt", flowSrc(`"task"`, plan+"\nhalt = true"), "flow a: unknown key step.halt"},
		{"escaping output", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "b"`+"\n"+`output = "../x.md"`), `output "../x.md" must be a relative path`},
		{"after unknown", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`after = ["plna"]`+"\n"+`brief = "b"`), `flow a step r: after: no step "plna"`},
		{"after later", flowSrc(`"task"`, `id = "r"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`, plan), `after: step "plan" comes later`},
		{"after self", flowSrc(`"task"`, `id = "r"`+"\n"+`after = ["r"]`+"\n"+`brief = "b"`), `after: step "r" is this step`},
		{"unknown param", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "{{taks}}"`), "brief: {{taks}}: not a param of this flow (params: task)"},
		{"forward ref", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "{{plan.output}}"`, plan), `brief: {{plan.output}}: step "plan" is not upstream`},
		{"sibling ref", flowSrc(`"task"`, plan, `id = "s"`+"\n"+`brief = "{{plan.output}}"`), `step "plan" is not upstream of this step; add it to after`},
		{"no such step", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "{{ghost.final}}"`), `{{ghost.final}}: no step "ghost"`},
		{"bad field", flowSrc(`"task"`, plan, `id = "s"`+"\n"+`after = ["plan"]`+"\n"+`brief = "{{plan.size}}"`), "a step has output, final, verdict, outputs, worktree and branch"},
		{"item outside foreach", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "{{item.path}}"`), "{{item.path}}: only valid in a fan-out step (foreach or models)"},
		{"item field", flowSrc(`"in"`, `id = "dig"`+"\n"+`brief = "{{item.size}}"`+"\n"+`foreach = "{{in}}/*"`), "item has path and name"},
		{"prev without after", flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "{{prev.output}}"`), "{{prev.output}}: this step has no after"},
		{"output of foreach", flowSrc(`"in"`, dig, `id = "s"`+"\n"+`after = ["dig"]`+"\n"+`brief = "{{dig.output}}"`), `"dig" is a fan-out step; use {{dig.outputs}}`},
		{"prev output of foreach", flowSrc(`"in"`, dig, `id = "s"`+"\n"+`after = ["dig"]`+"\n"+`brief = "{{prev.final}}"`), `"dig" is a fan-out step; use {{prev.outputs}}`},
		{"outputs of plain step", flowSrc(`"task"`, plan, `id = "s"`+"\n"+`after = ["plan"]`+"\n"+`brief = "{{plan.outputs}}"`), `"plan" is not a fan-out step; use {{plan.output}}`},
		{"step ref in foreach", flowSrc(`"task", "in"`, plan, `id = "dig"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`foreach = "{{plan.output}}/*"`), "foreach: {{plan.output}}: a foreach glob may use params, chain and work only"},
		{"item in foreach", flowSrc(`"in"`, `id = "dig"`+"\n"+`brief = "b"`+"\n"+`foreach = "{{item.path}}/*"`), "foreach: {{item.path}}: only valid in a fan-out step (foreach or models)"},
		{"bad glob", flowSrc(`"in"`, `id = "dig"`+"\n"+`brief = "b"`+"\n"+`foreach = "{{in}}/[a"`), "foreach \"{{in}}/[a\": syntax error in pattern"},
		{"step ref in output", flowSrc(`"task"`, plan, `id = "s"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`output = "{{plan.output}}.x"`), "output: {{plan.output}}: only params, chain and work are known when the flow starts"},
		{"on_fail without max", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "plan", brief = "again" }`), "flow a step r: on_fail: max must be > 0"},
		{"on_fail empty brief", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "plan", max = 2 }`), "on_fail: empty brief"},
		{"on_fail resume later", flowSrc(`"task"`, `id = "r"`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "plan", brief = "x", max = 2 }`, plan), `on_fail resume: step "plan" comes later`},
		{"on_fail resume unknown", flowSrc(`"task"`, `id = "r"`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "nope", brief = "x", max = 2 }`), `on_fail resume: no step "nope"`},
		{"on_fail resume not upstream", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "plan", brief = "x", max = 2 }`), `on_fail resume: step "plan" is not upstream`},
		{"on_fail on foreach", flowSrc(`"task", "in"`, plan, dig+"\n"+`after = ["plan"]`+"\n"+`on_fail = { resume = "plan", brief = "x", max = 2 }`), "a foreach step cannot loop"},
		{"on_fail resumes foreach", flowSrc(`"in"`, dig, `id = "r"`+"\n"+`after = ["dig"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "dig", brief = "x", max = 2 }`), `on_fail resume: "dig" is a foreach step`},
		{"max_items on a plain step", flowSrc(`"task"`, plan+"\nmax_items = 3"), "flow a step plan: max_items: only a foreach step has it"},
		{"parallel on a plain step", flowSrc(`"task"`, plan+"\nparallel = 2"), "parallel: only a fan-out step (foreach or models) has it"},
		{"max_items 0", flowSrc(`"in"`, dig+"\nmax_items = 0"), "flow a step dig: max_items: must be > 0"},
		{"parallel negative", flowSrc(`"in"`, dig+"\nparallel = -1"), "parallel: must be > 0"},
		{"parallel a string", flowSrc(`"in"`, dig+"\nparallel = \"2\""), "flows t.toml:"},
		// a models step: one worker per listed model
		{"models with model", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`model = "x"`+"\n"+`models = ["a", "b"]`+"\n"+`brief = "b"`), "model: a models step takes each copy's model from models, not model"},
		{"models with foreach", flowSrc(`"task", "in"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a"]`+"\n"+`foreach = "{{in}}/*"`+"\n"+`brief = "b"`), "foreach: a step fans out over files or models, not both"},
		{"models without harness", flowSrc(`"task"`, `id = "field"`+"\n"+`models = ["a"]`+"\n"+`brief = "b"`), "models: pin the harness too"},
		{"models empty", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = []`+"\n"+`brief = "b"`), "models: list at least one model"},
		{"models empty id", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = [""]`+"\n"+`brief = "b"`), "models: an empty model id"},
		{"models duplicate", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a", "a"]`+"\n"+`brief = "b"`), `models: "a" is listed twice`},
		{"models gated", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a"]`+"\n"+`gate = "you"`+"\n"+`brief = "b"`), "gate: a models step cannot gate its copies; gate a run step before it"},
		{"models on fail", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "field", brief = "x", max = 2 }`), "a models step cannot loop"},
		{"on_fail resumes models", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a"]`+"\n"+`brief = "b"`, `id = "r"`+"\n"+`after = ["field"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "field", brief = "x", max = 2 }`), `on_fail resume: "field" is a models step`},
		{"models max_items", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a"]`+"\n"+`max_items = 3`+"\n"+`brief = "b"`), "max_items: only a foreach step has it"},
		{"models path ref", flowSrc(`"task"`, `id = "field"`+"\n"+`harness = "pi"`+"\n"+`models = ["a"]`+"\n"+`brief = "{{item.path}}"`), "item has model and name"},
		{"foreach model ref", flowSrc(`"in"`, `id = "dig"`+"\n"+`brief = "{{item.model}}"`+"\n"+`foreach = "{{in}}/*"`), "item has path and name"},
		{"models on a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`models = ["a"]`), "models: a run step runs a command, so it has no models"},
		{"models on a judge", flowSrc(`"task"`, plan, judge+"\nmodels = [\"a\"]"), "models: a judge step asks Jev, so it has no models"},
		{"allow_empty on a plain step", flowSrc(`"task"`, plan+"\nallow_empty = true"), "flow a step plan: allow_empty: only a foreach step has it"},
		{"allow_empty on a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`allow_empty = true`), "allow_empty: a run step"},
		{"worktree on a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`worktree = true`), "worktree: a run step"},
		{"worktree on a judge", flowSrc(`"task"`, plan, judge+"\nworktree = true"), "worktree: a judge step"},
		{"worktree a string", flowSrc(`"task"`, plan+"\nworktree = \"yes\""), "flows t.toml:"},
		{"worktree ref of a plain step", flowSrc(`"task"`, plan, `id = "c"`+"\n"+`after = ["plan"]`+"\n"+`run = "cd {{plan.worktree}}"`), `run: {{plan.worktree}}: "plan" has no worktree: only a step with worktree = true`},
		{"branch ref not upstream", flowSrc(`"task"`, plan+"\nworktree = true", `id = "c"`+"\n"+`run = "git log {{plan.branch}}"`), `{{plan.branch}}: step "plan" is not upstream`},
		{"worktree ref of a foreach", flowSrc(`"in"`, dig+"\nworktree = true", `id = "c"`+"\n"+`after = ["dig"]`+"\n"+`run = "cd {{dig.worktree}}"`), `"dig" is a foreach step: each copy has its own worktree`},
		{"branch ref of a pick without worktrees", flowSrc(`"task"`, plan, `id = "p2"`+"\n"+`brief = "b"`, `id = "j"`+"\n"+`after = ["plan", "p2"]`+"\n"+`judge = "jev"`+"\n"+`brief = "b"`+"\n"+`pick = true`, `id = "s"`+"\n"+`after = ["j"]`+"\n"+`brief = "merge {{j.branch}}"`), `"j" may pick "plan", which has no worktree = true, so the winner may have no branch`},
		{"allow_empty on a judge", flowSrc(`"task"`, plan, judge+"\nallow_empty = true"), "allow_empty: a judge step"},
		{"step ref in runtime foreach", flowSrc(`"task"`, plan, `id = "dig"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`foreach = "{{plan.output}}/*"`), "foreach: {{plan.output}}: a foreach glob may use params, chain and work only"},
		{"run with brief", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`brief = "b"`), "flow a step c: brief: a run step runs a command, so it has no brief"},
		{"run with role", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`role = "review"`), "role: a run step runs a command"},
		{"run with output", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`output = "x.md"`), "output: a run step runs a command, so it has no output (its output is its log"},
		{"run with foreach", flowSrc(`"in"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`foreach = "{{in}}/*"`), "foreach: a run step"},
		{"run with verdict", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`verdict = true`), "verdict: a run step"},
		{"run with interactive", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`interactive = true`), "interactive: a run step"},
		{"run with model", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`model = "x"`), "model: a run step"},
		{"empty run", flowSrc(`"task"`, `id = "c"`+"\n"+`run = " "`), "flow a step c: empty run"},
		{"bad expect", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`expect = "maybe"`), `expect "maybe": use pass or fail`},
		{"bad timeout", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`timeout = "10"`), `timeout "10": use a duration above 0`},
		{"zero timeout", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`+"\n"+`timeout = "0s"`), `timeout "0s"`},
		{"expect on a brief step", flowSrc(`"task"`, plan+"\nexpect = \"fail\""), "flow a step plan: expect and timeout: only a run step has them"},
		{"run ref", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "cat {{plan.output}}"`, plan), `run: {{plan.output}}: step "plan" is not upstream`},
		{"run item ref", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "cat {{item.path}}"`), "run: {{item.path}}: only valid in a fan-out step (foreach or models)"},
		{"on_fail resumes a run step", flowSrc(`"task"`, `id = "c"`+"\n"+`run = "true"`, `id = "r"`+"\n"+`after = ["c"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "c", brief = "x", max = 2 }`), `on_fail resume: "c" is a run step; it has no session to resume`},
		{"judge not jev", flowSrc(`"task"`, plan, `id = "j"`+"\n"+`after = ["plan"]`+"\n"+`judge = "gpt"`+"\n"+`brief = "b"`), `flow a step j: judge "gpt": the only judge is "jev"`},
		{"judge without after", flowSrc(`"task"`, `id = "j"`+"\n"+`judge = "jev"`+"\n"+`brief = "b"`), "judge: a judge step needs after"},
		{"judge without brief", flowSrc(`"task"`, plan, `id = "j"`+"\n"+`after = ["plan"]`+"\n"+`judge = "jev"`), "flow a step j: empty brief"},
		{"judge with role", flowSrc(`"task"`, plan, judge+"\n"+`role = "review"`), "role: a judge step asks Jev, so it has no role"},
		{"judge with run", flowSrc(`"task"`, plan, judge+"\n"+`run = "true"`), "a judge step asks Jev"},
		{"judge with model", flowSrc(`"task"`, plan, judge+"\n"+`model = "x"`), "model: a judge step"},
		{"judge with output", flowSrc(`"task"`, plan, judge+"\n"+`output = "x.md"`), "output: a judge step"},
		{"judge with interactive", flowSrc(`"task"`, plan, judge+"\n"+`interactive = true`), "interactive: a judge step"},
		{"judge with foreach", flowSrc(`"in"`, `id = "plan"`+"\n"+`brief = "b"`, judge+"\n"+`foreach = "{{in}}/*"`), "foreach: a judge step"},
		{"judge with verdict", flowSrc(`"task"`, plan, judge+"\n"+`verdict = true`), "verdict: a judge step"},
		{"judge with timeout", flowSrc(`"task"`, plan, judge+"\n"+`timeout = "1m"`), "expect and timeout: only a run step"},
		{"pick from one step", flowSrc(`"task"`, plan, judge+"\n"+`pick = true`), "pick: a pick judge needs two agent workers to pick from"},
		{"pick from a step and its check", flowSrc(`"task"`, plan, `id = "c"`+"\n"+`after = ["plan"]`+"\n"+`run = "true"`, `id = "j"`+"\n"+`after = ["plan", "c"]`+"\n"+`judge = "jev"`+"\n"+`brief = "b"`+"\n"+`pick = true`), "needs two agent workers to pick from: two or more after steps that are not run or judge steps"},
		{"pick with pass", flowSrc(`"task"`, plan, `id = "p2"`+"\n"+`brief = "b"`, `id = "j"`+"\n"+`after = ["plan", "p2"]`+"\n"+`judge = "jev"`+"\n"+`brief = "b"`+"\n"+`pick = true`+"\n"+`pass = "x"`), "pass and fail: a pick judge picks"},
		{"pick with on_fail", flowSrc(`"task"`, plan, `id = "p2"`+"\n"+`brief = "b"`, `id = "j"`+"\n"+`after = ["plan", "p2"]`+"\n"+`judge = "jev"`+"\n"+`brief = "b"`+"\n"+`pick = true`+"\n"+`on_fail = { resume = "plan", brief = "x", max = 2 }`), "on_fail: a pick judge always passes"},
		{"min_confidence zero", flowSrc(`"task"`, plan, judge+"\nmin_confidence = 0.0"), "min_confidence 0: use a number above 0 and at most 1"},
		{"min_confidence above 1", flowSrc(`"task"`, plan, judge+"\nmin_confidence = 1.5"), "min_confidence 1.5"},
		{"pass on a brief step", flowSrc(`"task"`, plan+"\npass = \"ok\""), "flow a step plan: pick, pass, fail, min_confidence and read: only a judge step has them"},
		{"read on a brief step", flowSrc(`"task"`, plan+"\nread = [\"x\"]"), "only a judge step has them"},
		{"read escapes", flowSrc(`"task"`, plan, judge+"\n"+`read = ["../secrets"]`), `read "../secrets" must be a relative path inside the directory`},
		{"read outputs", flowSrc(`"in"`, `id = "dig"`+"\n"+`brief = "b"`+"\n"+`foreach = "{{in}}/*"`, `id = "j"`+"\n"+`after = ["dig"]`+"\n"+`judge = "jev"`+"\n"+`brief = "b"`+"\n"+`read = ["{{dig.outputs}}"]`), "one path per entry"},
		{"read bad ref", flowSrc(`"task"`, plan, judge+"\n"+`read = ["{{nope}}"]`), `read "{{nope}}": {{nope}}: not a param`},
		{"verdict judge output ref", flowSrc(`"task"`, plan, judge, `id = "s"`+"\n"+`after = ["j"]`+"\n"+`brief = "{{j.output}}"`), `"j" is a verdict judge, which writes no output; use {{j.verdict}} or {{j.final}}`},
		{"on_fail resumes a judge", flowSrc(`"task"`, plan, judge, `id = "r"`+"\n"+`after = ["j"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "j", brief = "x", max = 2 }`), `on_fail resume: "j" is a judge step; it has no session to resume`},
		{"on_fail brief ref", flowSrc(`"task"`, plan, `id = "r"`+"\n"+`after = ["plan"]`+"\n"+`brief = "b"`+"\n"+`on_fail = { resume = "plan", brief = "{{nope}} {{r.fnial}}", max = 2 }`), "on_fail brief: {{nope}}: not a param"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src), "t.toml")
			if err == nil {
				t.Fatalf("want an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "flows t.toml: ") {
				t.Errorf("error %q\nwant it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseReportsEveryBadRef(t *testing.T) {
	_, err := Parse([]byte(flowSrc(`"task"`, `id = "s"`+"\n"+`brief = "{{a}} {{b}} {{a}} {{item.name}}"`)), "t.toml")
	if err == nil {
		t.Fatal("want an error")
	}
	if n := strings.Count(err.Error(), "{{a}}"); n != 1 {
		t.Errorf("{{a}} reported %d times: %v", n, err)
	}
	for _, want := range []string{"{{b}}", "{{item.name}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %s", err, want)
		}
	}
}

func TestParseAccepts(t *testing.T) {
	src := flowSrc(`"task", "in"`,
		`id = "plan"`+"\n"+`brief = "plan {{task}} from {{ in }}"`+"\n"+`output = "PLAN.md"`+"\n"+`interactive = true`,
		`id = "dig"`+"\n"+`after = ["plan"]`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`output = "r/{{item.name}}"`+"\n"+`brief = "{{item.path}} for {{prev.output}}"`,
		`id = "review"`+"\n"+`role = "review"`+"\n"+`after = ["dig"]`+"\n"+`verdict = true`+"\n"+
			`brief = "{{prev.outputs}} {{dig.outputs}} vs {{plan.output}} {{plan.final}}"`+"\n"+
			`on_fail = { resume = "plan", brief = "Fix {{review.final}} ({{review.verdict}}) in {{plan.output}}", max = 2 }`,
	)
	set, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	f := set["a"]
	if f.Name != "a" || f.Source != "t.toml" || len(f.Steps) != 3 || f.Steps[2].OnFail.Max != 2 || !f.Steps[2].Verdict {
		t.Errorf("flow = %+v", f)
	}
}

// A step's env and args, for its workers; a run step has env too.
func TestParseAcceptsEnvAndArgs(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"task"`,
		`id = "dig"`+"\n"+`brief = "b"`+"\n"+`env = { CLAUDE_CONFIG_DIR = "~/.claude-alt", B = "2" }`+"\n"+`args = ["--chrome", "--add-dir", "/s"]`,
		`id = "check"`+"\n"+`after = ["dig"]`+"\n"+`run = "true"`+"\n"+`env = { GOFLAGS = "-count=1" }`,
		`id = "codex"`+"\n"+`harness = "codex"`+"\n"+`brief = "b"`+"\n"+`args = ["-p", "work"]`, // codex -p is --profile
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	f := set["a"]
	if f.Steps[0].Env["CLAUDE_CONFIG_DIR"] != "~/.claude-alt" || !slices.Equal(f.Steps[0].Args, []string{"--chrome", "--add-dir", "/s"}) || f.Steps[1].Env["GOFLAGS"] != "-count=1" {
		t.Errorf("steps = %+v", f.Steps)
	}
	if d := f.Detail(); !strings.Contains(d, "step dig: role task · env B, CLAUDE_CONFIG_DIR · args --chrome --add-dir /s\n") {
		t.Errorf("detail:\n%s", d)
	}
}

// write = false keeps a step off the directory lock.
func TestParseAcceptsWrite(t *testing.T) {
	src := flowSrc(`"task"`,
		`id = "plan"`+"\n"+`brief = "plan {{task}}"`+"\n"+`output = "{{work}}/PLAN.md"`+"\n"+`write = false`,
		`id = "hunt"`+"\n"+`role = "review"`+"\n"+`after = ["plan"]`+"\n"+`brief = "hunt in {{plan.output}}"`,
	)
	set, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	f := set["a"]
	if w := f.Steps[0].Write; w == nil || *w || f.Steps[1].Write != nil {
		t.Errorf("steps = %+v", f.Steps)
	}
}

// A models step fans out one worker per model; a pick judge may pick the
// best copy.
func TestParseAcceptsAModelsStep(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"task"`,
		`id = "field"`+"\n"+`harness = "pi"`+"\n"+`parallel = 4`+"\n"+`models = ["openrouter/a/one", "openrouter/b/two:free"]`+"\n"+`output = "{{work}}/answers/{{item.name}}.md"`+"\n"+`brief = "You are {{item.model}}; write {{item.name}}."`,
		`id = "pick"`+"\n"+`after = ["field"]`+"\n"+`judge = "jev"`+"\n"+`pick = true`+"\n"+`brief = "Which answer is best?"`,
		`id = "report"`+"\n"+`after = ["pick"]`+"\n"+`brief = "Summarize {{pick.output}} from {{field.outputs}}."`,
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	f := set["a"]
	field := f.Steps[0]
	if !slices.Equal(field.Models, []string{"openrouter/a/one", "openrouter/b/two:free"}) || *field.Parallel != 4 {
		t.Errorf("field = %+v", field)
	}
	if d := f.Detail(); !strings.Contains(d, "step field: role task · models 2 · parallel 4 · output {{work}}/answers/{{item.name}}.md") {
		t.Errorf("detail:\n%s", d)
	}
}

func TestParseAcceptsARunStep(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"check"`,
		`id = "implement"`+"\n"+`brief = "Fix it in {{work}}."`,
		`id = "check"`+"\n"+`after = ["implement"]`+"\n"+`run = "{{check}} > {{work}}/out"`+"\n"+`expect = "fail"`+"\n"+`timeout = "90s"`+"\n"+`gate = "you"`+"\n"+
			`on_fail = { resume = "implement", brief = "Read {{check.output}} ({{check.verdict}}, {{check.final}}).", max = 5 }`,
		`id = "review"`+"\n"+`after = ["check"]`+"\n"+`brief = "The tests say {{check.output}}, {{prev.verdict}}."`,
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	d := set["a"].Detail()
	for _, want := range []string{
		"step check: run · after implement · gate you · expect fail · timeout 90s · on fail resume implement, max 5\n  run: {{check}} > {{work}}/out\n",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
}

func TestParseAcceptsJudgeSteps(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"in"`,
		`id = "review"`+"\n"+`brief = "Review."`+"\n"+`output = "R.md"`,
		`id = "ship"`+"\n"+`after = ["review"]`+"\n"+`judge = "jev"`+"\n"+`brief = "Ship {{review.output}}?"`+"\n"+
			`pass = "ship as is"`+"\n"+`fail = "fix first"`+"\n"+`min_confidence = 0.8`+"\n"+`read = ["{{in}}", "{{work}}/notes.md", "DECISIONS.md"]`+"\n"+
			`on_fail = { resume = "review", brief = "Jev says {{ship.verdict}}; see {{ship.final}}.", max = 2 }`,
		`id = "try"`+"\n"+`brief = "Try {{item.path}}."`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`output = "{{item.name}}.out"`,
		`id = "best"`+"\n"+`after = ["try"]`+"\n"+`judge = "jev"`+"\n"+`pick = true`+"\n"+`gate = "you"`+"\n"+`brief = "Which attempt is best?"`,
		`id = "use"`+"\n"+`after = ["best", "ship"]`+"\n"+`brief = "Build on {{best.output}} ({{best.verdict}}, {{ship.verdict}})."`,
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	d := set["a"].Detail()
	for _, want := range []string{
		"step ship: judge jev · after review · min confidence 0.8 · read {{in}}, {{work}}/notes.md, DECISIONS.md · on fail resume review, max 2\n  brief: Ship {{review.output}}?\n  pass: ship as is\n  fail: fix first\n",
		"step best: judge jev · after try · gate you · pick\n",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
}

func TestDetailShowsForeach(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"in"`,
		`id = "plan"`+"\n"+`brief = "b"`,
		`id = "dig"`+"\n"+`after = ["plan"]`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`max_items = 4`+"\n"+`parallel = 2`+"\n"+`allow_empty = true`+"\n"+`brief = "{{item.path}}"`,
		`id = "now"`+"\n"+`foreach = "{{in}}/*.txt"`+"\n"+`brief = "{{item.path}}"`,
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	d := set["a"].Detail()
	for _, want := range []string{
		"step dig: role task · after plan · foreach {{in}}/*.md, globbed after plan · max items 4 · parallel 2 · allow empty",
		"step now: role task · foreach {{in}}/*.txt\n",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
}

// Best of N: a pick over the attempts, with the checks on them as context.
func TestParseAcceptsAPickWithChecks(t *testing.T) {
	_, err := Parse([]byte(flowSrc(`"task"`,
		`id = "a"`+"\n"+`brief = "Try {{task}}."`,
		`id = "b"`+"\n"+`brief = "Try {{task}} otherwise."`,
		`id = "check-a"`+"\n"+`after = ["a"]`+"\n"+`run = "true"`,
		`id = "check-b"`+"\n"+`after = ["b"]`+"\n"+`run = "true"`,
		`id = "best"`+"\n"+`after = ["a", "b", "check-a", "check-b"]`+"\n"+`judge = "jev"`+"\n"+`pick = true`+"\n"+`brief = "Which passes and is simplest?"`,
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseAcceptsWorktrees(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"task", "check", "in"`,
		`id = "a"`+"\n"+`worktree = true`+"\n"+`interactive = true`+"\n"+`output = "{{work}}/a.md"`+"\n"+`brief = "Try {{task}}."`,
		`id = "b"`+"\n"+`worktree = true`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`brief = "Try {{item.path}}."`,
		`id = "check-a"`+"\n"+`after = ["a"]`+"\n"+`run = "cd {{a.worktree}} && {{check}}"`,
		`id = "best"`+"\n"+`after = ["a", "b", "check-a"]`+"\n"+`judge = "jev"`+"\n"+`pick = true`+"\n"+`read = ["{{a.worktree}}/go.mod"]`+"\n"+`brief = "Which is best? a is on {{a.branch}}."`,
		`id = "merge"`+"\n"+`after = ["best"]`+"\n"+`gate = "you"`+"\n"+`brief = "Merge {{best.branch}} from {{best.worktree}}."`,
	)), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if d := set["a"].Detail(); !strings.Contains(d, "step a: role task · worktree · interactive · output {{work}}/a.md\n") {
		t.Errorf("detail:\n%s", d)
	}
}

// Every built-in starts with its params, and the default set keeps the
// steps that make it work: refuters that may find nothing, jev judges,
// parallel digs, checks that loop back to the step they verify.
func TestBuiltInsStart(t *testing.T) {
	set, err := load()
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := gitRepo(t) // best-of-n takes worktrees
	write(t, filepath.Join(cwd, "briefs", "a.md"), "a")
	for _, n := range set.Names() {
		f := set[n]
		params := map[string]string{}
		for _, p := range f.Params {
			params[p] = "x"
		}
		if _, ok := params["in"]; ok {
			params["in"] = "briefs"
		}
		ws, err := f.Start(&store{}, "c-"+n, params, cwd, filepath.Join(cwd, ".routr", "work", "c-"+n))
		if err != nil || len(ws) == 0 {
			t.Errorf("%s: %d workers, %v", n, len(ws), err)
		}
		// A loop's reviewer waits for the revision only as a worker
		// upstream of it: no loop adds an edge.
		m := map[int]registry.Worker{}
		for _, w := range ws {
			m[w.ID] = w
		}
		for _, w := range ws {
			if w.OnFail != nil && !slices.ContainsFunc(registry.Ancestors(w, m), func(u registry.Worker) bool { return u.ID == w.OnFail.Resume }) {
				t.Errorf("%s step %s: on_fail resumes worker %d, which it does not come after", n, w.Step, w.OnFail.Resume)
			}
		}
	}

	step := func(flow, id string) Step {
		for _, s := range set[flow].Steps {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("%s has no step %s", flow, id)
		return Step{}
	}
	for _, c := range []struct {
		flow, step string
		ok         func(Step) bool
	}{
		{"adversarial-review", "refute", func(s Step) bool { return s.Runtime() && s.AllowEmpty && s.Role == "refute" }},
		{"adversarial-review", "ship", func(s Step) bool { return s.Judge == "jev" && !s.Pick }},
		{"security-review", "verify", func(s Step) bool { return s.Runtime() && s.AllowEmpty && s.Role == "refute" }},
		{"deep-research", "dig", func(s Step) bool { return s.Runtime() && s.parallel() == 4 }},
		{"deep-research", "split", func(s Step) bool {
			return strings.Contains(s.Brief, "the web, unless the question names this repository")
		}},
		// the findings only point at sources; the fact-check opens them
		{"deep-research", "factcheck", func(s Step) bool {
			return s.OnFail != nil && s.OnFail.Resume == "synthesize" && strings.Contains(s.Brief, "A findings file is not a source") &&
				strings.Contains(s.Brief, "at least the five most load-bearing") && strings.Contains(s.Brief, "Fail if a load-bearing claim is contradicted or unverified")
		}},
		{"fix-until-green", "verify", func(s Step) bool { return s.Run == "{{check}}" && s.OnFail.Resume == "implement" }},
		{"repro-fix", "fails", func(s Step) bool { return s.Expect == "fail" && s.OnFail.Resume == "reproduce" }},
		{"repro-fix", "fix", func(s Step) bool { return s.Role == "implement" }},
		{"debug-hypotheses", "pick", func(s Step) bool { return s.Judge == "jev" && s.Pick }},
		{"best-of-n", "go", func(s Step) bool { return s.Gate == "you" && s.Run != "" }},
		{"best-of-n", "b", func(s Step) bool { return s.Worktree && s.Harness == "codex" }},
		{"best-of-n", "pick", func(s Step) bool { return s.Pick && len(s.After) == 6 }},
	} {
		if s := step(c.flow, c.step); !c.ok(s) {
			t.Errorf("%s step %s = %+v", c.flow, c.step, s)
		}
	}
}
