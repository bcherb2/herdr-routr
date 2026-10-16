package flow

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// store is a fake Creator. Ids start at 10 so that step order and ids differ.
type store struct {
	ws      []registry.Worker
	failAt  int // Create fails on this call (1-based); 0 = never
	failUpd bool
	calls   int
}

func (s *store) Create(w registry.Worker) (registry.Worker, error) {
	s.calls++
	if s.calls == s.failAt {
		return registry.Worker{}, errors.New("disk full")
	}
	w.ID = 10 + len(s.ws)
	s.ws = append(s.ws, w)
	return w, nil
}

func (s *store) Update(id int, fn func(*registry.Worker)) (registry.Worker, error) {
	if s.failUpd {
		return registry.Worker{}, errors.New("locked")
	}
	for i := range s.ws {
		if s.ws[i].ID == id {
			fn(&s.ws[i])
			return s.ws[i], nil
		}
	}
	return registry.Worker{}, fmt.Errorf("no worker %d", id)
}

func builtIn(t *testing.T, name string) Flow {
	t.Helper()
	set, err := load()
	if err != nil {
		t.Fatal(err)
	}
	return set[name]
}

func TestStartPlanToImpl(t *testing.T) {
	s := &store{}
	params := map[string]string{"task": "add OAuth", "in": "DECISIONS.md"}
	ws, err := builtIn(t, "plan-to-impl").Start(s, "c1", params, "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ws, s.ws) || len(ws) != 4 {
		t.Fatalf("returned %+v\nstored %+v", ws, s.ws)
	}
	plan, review, impl, cr := ws[0], ws[1], ws[2], ws[3]
	for i, w := range ws {
		if w.ID != 10+i || w.Chain != "c1" || w.Cwd != "/repo" || w.State != registry.Pending || !reflect.DeepEqual(w.Params, params) {
			t.Errorf("worker %d = %+v", i, w)
		}
	}
	if plan.Step != "plan" || plan.Role != "plan" || !plan.Interactive || plan.Output != "PLAN.md" || plan.After != nil || plan.OnFail != nil {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Brief != "Write PLAN.md for: {{task}}. Decisions so far are in {{in}}." {
		t.Errorf("plan brief is not the raw template: %q", plan.Brief)
	}
	if !reflect.DeepEqual(review.After, []int{10}) || *review.OnFail != (registry.OnFail{Resume: 10, Brief: "Address {{review.output}}.", Max: 3}) {
		t.Errorf("review = %+v, on_fail %+v", review, review.OnFail)
	}
	if !strings.HasSuffix(review.Brief, askVerdict) || strings.HasSuffix(plan.Brief, askVerdict) {
		t.Errorf("verdict ask: review %q plan %q", review.Brief, plan.Brief)
	}
	if impl.Gate != "you" || impl.Role != "implement" || !reflect.DeepEqual(impl.After, []int{11}) {
		t.Errorf("implement = %+v", impl)
	}
	if !reflect.DeepEqual(cr.After, []int{12}) || cr.OnFail.Resume != 12 || cr.OnFail.Max != 2 {
		t.Errorf("code-review = %+v", cr)
	}
	// Workers must not share one params map.
	plan.Params["task"] = "changed"
	if review.Params["task"] != "add OAuth" {
		t.Error("params are shared between workers")
	}
}

func TestStartPinsAndDefaults(t *testing.T) {
	s := &store{}
	ws, err := builtIn(t, "plan-redteam").Start(s, "c", map[string]string{"task": "x"}, "/r", "")
	if err != nil {
		t.Fatal(err)
	}
	if ws[0].Role != "task" || ws[0].Harness != "" || ws[1].Harness != "codex" || ws[1].Effort != "high" || ws[1].Model != "" {
		t.Errorf("workers = %+v", ws)
	}
	set, err := Parse([]byte(flowSrc("", `id = "s"`+"\n"+`brief = "b"`)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err = set["a"].Start(&store{}, "c", nil, "/r", "")
	if err != nil || ws[0].Role != "task" || ws[0].Params != nil || ws[0].Answerer != "" {
		t.Errorf("no params: %+v %v", ws, err)
	}
}

// A step's env and args go on its workers as written (the Creator lays them
// over config's), a foreach step's copies and a run step's env included;
// a judge has neither.
func TestStartEnvAndArgs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "1.md"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "2.md"), nil, 0o644)
	set, err := Parse([]byte(flowSrc(`"in"`,
		`id = "dig"`+"\n"+`brief = "b"`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`env = { A = "1" }`+"\n"+`args = ["--chrome"]`,
		`id = "check"`+"\n"+`after = ["dig"]`+"\n"+`run = "true"`+"\n"+`env = { R = "r" }`,
		`id = "more"`+"\n"+`after = ["dig"]`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`brief = "b"`+"\n"+`env = { M = "m" }`+"\n"+`args = ["--x"]`,
		`id = "j"`+"\n"+`after = ["dig"]`+"\n"+`judge = "jev"`+"\n"+`brief = "ok?"`,
	)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["a"].Start(&store{}, "c", map[string]string{"in": dir}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws[:2] {
		if w.Env["A"] != "1" || !slices.Equal(w.Args, []string{"--chrome"}) {
			t.Errorf("dig copy = %+v", w)
		}
	}
	if ws[0].Env["X"] = "y"; ws[1].Env["X"] != "" {
		t.Error("the copies share one env map")
	}
	if check := ws[2]; check.Env["R"] != "r" || check.Args != nil {
		t.Errorf("check = %+v", check)
	}
	more := ws[3]
	if more.Fanout == nil || more.Env["M"] != "m" || !slices.Equal(more.Args, []string{"--x"}) {
		t.Errorf("runtime foreach = %+v", more)
	}
	if j := ws[4]; j.Env != nil || j.Args != nil {
		t.Errorf("judge = %+v", j)
	}
	copies, err := Expand(more, "")
	if err != nil || len(copies) != 2 {
		t.Fatalf("expand: %v %v", copies, err)
	}
	if copies[0].Env["M"] = "changed"; copies[1].Env["M"] != "m" || more.Env["M"] != "m" {
		t.Error("expanded copies share their fanout's env map")
	}
}

// A step's answer goes on its workers, a foreach step's copies and a
// runtime foreach's worker included, as a pin the Creator resolves; a run
// or judge step has none.
func TestStartAnswer(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.md", "b.md"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	set, err := Parse([]byte(flowSrc(`"in"`,
		`id = "dig"`+"\n"+`brief = "b"`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`answer = "jev"`,
		`id = "more"`+"\n"+`after = ["dig"]`+"\n"+`brief = "b"`+"\n"+`foreach = "{{in}}/*.md"`+"\n"+`answer = "agent"`,
		`id = "fix"`+"\n"+`after = ["more"]`+"\n"+`brief = "b"`,
		`id = "check"`+"\n"+`after = ["fix"]`+"\n"+`run = "true"`,
		`id = "ok"`+"\n"+`after = ["fix"]`+"\n"+`judge = "jev"`+"\n"+`brief = "Done?"`,
	)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["a"].Start(&store{}, "c", map[string]string{"in": "."}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		got = append(got, w.Step+"="+w.Answerer)
	}
	if want := "dig=jev dig=jev more=agent fix= check= ok="; strings.Join(got, " ") != want {
		t.Errorf("answerers = %s, want %s", strings.Join(got, " "), want)
	}
	copies, err := Expand(ws[2], "")
	if err != nil || len(copies) != 2 || copies[0].Answerer != "agent" {
		t.Errorf("expanded copies = %+v, %v", copies, err)
	}
}

func TestStartForeachFansOutAndJoins(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "briefs", "b.md"), "b")
	write(t, filepath.Join(cwd, "briefs", "a.md"), "a")
	write(t, filepath.Join(cwd, "briefs", "notes.txt"), "x")
	if err := os.Mkdir(filepath.Join(cwd, "briefs", "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &store{}
	ws, err := builtIn(t, "research").Start(s, "r", map[string]string{"in": "briefs"}, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("workers = %+v", ws)
	}
	for i, name := range []string{"a.md", "b.md"} {
		w := ws[i]
		want := map[string]string{"in": "briefs", itemPath: filepath.Join("briefs", name), itemName: name}
		if w.Step != "dig" || w.Output != "research/"+name || !reflect.DeepEqual(w.Params, want) || w.After != nil {
			t.Errorf("dig %d = %+v", i, w)
		}
	}
	if syn := ws[2]; syn.Step != "synthesize" || !reflect.DeepEqual(syn.After, []int{10, 11}) || syn.Params[itemPath] != "" {
		t.Errorf("synthesize = %+v", syn)
	}

	// An absolute pattern keeps absolute item paths.
	ws, err = builtIn(t, "research").Start(&store{}, "r", map[string]string{"in": filepath.Join(cwd, "briefs")}, "/elsewhere", "")
	if err != nil || ws[0].Params[itemPath] != filepath.Join(cwd, "briefs", "a.md") {
		t.Errorf("absolute: %+v %v", ws, err)
	}
}

// models is a models fan-out with a join, the shape of compare-models.
const models = `
[flow.bake]
params = ["task"]
[[flow.bake.step]]
id = "field"
role = "research"
harness = "pi"
parallel = 2
models = ["openrouter/one/a", "openrouter/two/b:free"]
output = "{{work}}/answers/{{item.name}}.md"
brief = "You are {{item.model}}; answer {{task}} into {{item.name}}."
[[flow.bake.step]]
id = "compare"
after = ["field"]
harness = "claude"
brief = "Rank {{field.outputs}}."
`

func TestStartModelsFanOutAndJoins(t *testing.T) {
	set, err := Parse([]byte(models), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["bake"].Start(&store{}, "c1", map[string]string{"task": "x"}, "/repo", "/repo/.routr/work/c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("workers = %+v", ws)
	}
	for i, m := range []string{"openrouter/one/a", "openrouter/two/b:free"} {
		w := ws[i]
		want := map[string]string{"task": "x", itemModel: m, itemName: slug(m)}
		if w.Step != "field" || w.Model != m || w.Harness != "pi" || w.Parallel != 2 ||
			!reflect.DeepEqual(w.Params, want) || w.After != nil || w.Fanout != nil {
			t.Errorf("copy %d = %+v", i, w)
		}
		if want := ".routr/work/c1/answers/" + slug(m) + ".md"; w.Output != want {
			t.Errorf("copy %d output = %q, want %q", i, w.Output, want)
		}
	}
	if cmp := ws[2]; cmp.Step != "compare" || !reflect.DeepEqual(cmp.After, []int{10, 11}) {
		t.Errorf("compare = %+v", cmp)
	}

	// A copy's brief renders its model; the join's refs collect every copy's
	// output, in id order, as the step's and prev's outputs.
	all := []registry.Worker{ws[2], ws[0], ws[1]}
	r := RefsFor(ws[1], all, finalPath)
	if *r.Item != (Item{Model: "openrouter/two/b:free", Name: "openrouter-two-b-free"}) {
		t.Errorf("copy refs item = %+v", r.Item)
	}
	if brief, err := Render(ws[1].Brief, r); err != nil || brief != "You are openrouter/two/b:free; answer x into openrouter-two-b-free." {
		t.Errorf("copy brief = %q, %v", brief, err)
	}
	r = RefsFor(ws[2], all, finalPath)
	outs := StepRef{Outputs: []string{".routr/work/c1/answers/openrouter-one-a.md", ".routr/work/c1/answers/openrouter-two-b-free.md"}}
	if r.Item != nil || !reflect.DeepEqual(r.Steps["field"], outs) || !reflect.DeepEqual(r.Steps["prev"], outs) {
		t.Errorf("compare refs = %+v", r)
	}
	if brief, err := Render(ws[2].Brief, r); err != nil || !strings.HasPrefix(brief, "Rank .routr/work/c1/answers/openrouter-one-a.md") {
		t.Errorf("compare brief = %q, %v", brief, err)
	}
}

// runtime is a planner, a foreach step after it and a join.
const runtime = `
[flow.orch]
params = ["dir"]
[[flow.orch.step]]
id = "plan"
brief = "Write {{dir}}/*.md."
[[flow.orch.step]]
id = "dig"
role = "research"
after = ["plan"]
foreach = "{{dir}}/*.md"
output = "out/{{item.name}}"
max_items = 2
parallel = 1
effort = "low"
brief = "Do {{item.path}}."
[[flow.orch.step]]
id = "join"
after = ["dig"]
brief = "Merge {{dig.outputs}}."
`

func TestStartRuntimeForeachQueuesOneWorker(t *testing.T) {
	set, err := Parse([]byte(runtime), "t")
	if err != nil {
		t.Fatal(err)
	}
	// The files need not exist yet.
	ws, err := set["orch"].Start(&store{}, "c", map[string]string{"dir": "tasks"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("workers = %+v", ws)
	}
	dig := ws[1]
	want := registry.Fanout{Glob: "{{dir}}/*.md", Output: "out/{{item.name}}", MaxItems: 2}
	if dig.Fanout == nil || !reflect.DeepEqual(*dig.Fanout, want) || dig.Parallel != 1 || dig.Output != "" ||
		dig.Params[itemPath] != "" || !reflect.DeepEqual(dig.After, []int{10}) || dig.Effort != "low" {
		t.Errorf("dig = %+v, fanout %+v", dig, dig.Fanout)
	}
	if !reflect.DeepEqual(ws[2].After, []int{11}) || ws[2].Fanout != nil {
		t.Errorf("join = %+v", ws[2])
	}
	// Static foreach steps get the default cap; parallel only when set.
	if f := builtIn(t, "research").Steps[0]; f.maxItems() != DefaultMaxItems || f.parallel() != 0 || f.Runtime() {
		t.Errorf("research dig: max %d parallel %d", f.maxItems(), f.parallel())
	}

	// An output a param takes outside the directory fails at start.
	bad, err := Parse([]byte(strings.Replace(runtime, `output = "out/{{item.name}}"`, `output = "{{dir}}/{{item.name}}"`, 1)), "t")
	if err != nil {
		t.Fatal(err)
	}
	s := &store{}
	if _, err := bad["orch"].Start(s, "c", map[string]string{"dir": "../up"}, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "step dig: output") || len(s.ws) != 0 {
		t.Errorf("escaping output: %v, created %d", err, len(s.ws))
	}
}

func TestExpand(t *testing.T) {
	cwd := t.TempDir()
	set, err := Parse([]byte(runtime), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["orch"].Start(&store{}, "c", map[string]string{"dir": "tasks"}, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	fan := ws[1]
	fan.Note, fan.State = "waiting on 10 task (running)", registry.Waiting

	if _, err := Expand(fan, ""); err == nil || !strings.Contains(err.Error(), `foreach "tasks/*.md" matched no files in `+cwd) {
		t.Errorf("no files: %v", err)
	}
	write(t, filepath.Join(cwd, "tasks", "b.md"), "b")
	write(t, filepath.Join(cwd, "tasks", "a.md"), "a")
	copies, err := Expand(fan, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %+v", copies)
	}
	for i, name := range []string{"a.md", "b.md"} {
		w := copies[i]
		wantParams := map[string]string{"dir": "tasks", itemPath: filepath.Join("tasks", name), itemName: name}
		if w.ID != 0 || w.Fanout != nil || w.State != registry.Pending || w.Note != "" || w.Output != "out/"+name ||
			!reflect.DeepEqual(w.Params, wantParams) || !reflect.DeepEqual(w.After, []int{10}) ||
			w.Step != "dig" || w.Chain != "c" || w.Role != "research" || w.Effort != "low" || w.Parallel != 1 || w.Brief != "Do {{item.path}}." {
			t.Errorf("copy %d = %+v", i, w)
		}
	}
	copies[0].After[0], copies[0].Params["dir"] = 99, "x"
	if fan.After[0] != 10 || fan.Params["dir"] != "tasks" || copies[1].Params["dir"] != "tasks" {
		t.Error("copies share After or Params")
	}

	write(t, filepath.Join(cwd, "tasks", "c.md"), "c")
	if _, err := Expand(fan, ""); err == nil || !strings.Contains(err.Error(), "matched 3 files in "+cwd+", more than max_items (2)") {
		t.Errorf("over the cap: %v", err)
	}
	if _, err := Expand(ws[0], ""); err == nil {
		t.Error("expanded a plain worker")
	}
}

// A foreach step with allow_empty that matches nothing is done with no
// copies, when the flow starts or once its after are done; the steps after
// it read its outputs as none.
func TestForeachAllowEmpty(t *testing.T) {
	cwd := t.TempDir()
	src := `
[flow.f]
params = ["dir"]
[[flow.f.step]]
id = "now"
foreach = "{{dir}}/*.md"
allow_empty = true
output = "out/{{item.name}}"
brief = "Do {{item.path}}."
[[flow.f.step]]
id = "plan"
brief = "Plan."
[[flow.f.step]]
id = "later"
after = ["plan"]
foreach = "{{dir}}/*.txt"
allow_empty = true
brief = "Do {{item.path}}."
[[flow.f.step]]
id = "join"
after = ["now", "later"]
brief = "Their verdicts: {{now.outputs}}; {{later.outputs}}; {{prev.outputs}}."
[[flow.f.step]]
id = "check"
after = ["now"]
run = "cat {{now.outputs}} /dev/null"
`
	set, err := Parse([]byte(src), "t")
	if err != nil {
		t.Fatal(err)
	}
	s := &store{}
	ws, err := set["f"].Start(s, "c", map[string]string{"dir": "tasks"}, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 5 {
		t.Fatalf("workers = %+v", ws)
	}
	now := ws[0]
	want := registry.Fanout{Glob: "{{dir}}/*.md", Output: "out/{{item.name}}", MaxItems: DefaultMaxItems, AllowEmpty: true}
	if now.State != registry.Done || now.Fanout == nil || !reflect.DeepEqual(*now.Fanout, want) || !now.Empty() ||
		now.Summary != "foreach {{dir}}/*.md matched no files; allow_empty, so nothing ran" || now.Note != now.Summary {
		t.Errorf("now = %+v", now)
	}
	if !reflect.DeepEqual(ws[3].After, []int{10, 12}) {
		t.Errorf("join after = %v", ws[3].After)
	}
	later := ws[2]
	if later.Fanout == nil || !later.Fanout.AllowEmpty || later.State != registry.Pending {
		t.Errorf("later = %+v", later)
	}
	copies, err := Expand(later, "")
	if err != nil || len(copies) != 0 {
		t.Errorf("expand with no match: %+v, %v", copies, err)
	}
	// The watcher marks it done with no copies.
	later.State = registry.Done
	all := append(slices.Clone(ws[:2]), later, ws[3], ws[4])
	r := RefsFor(ws[3], all, finalPath)
	if !r.Steps["now"].Empty || !r.Steps["later"].Empty || !r.Steps["prev"].Empty {
		t.Errorf("refs = %+v", r.Steps)
	}
	if brief, err := Render(ws[3].Brief, r); err != nil || brief != "Their verdicts: (none); (none); (none)." {
		t.Errorf("join brief = %q, %v", brief, err)
	}
	if cmd, err := RenderShell(ws[4].Run, RefsFor(ws[4], all, finalPath)); err != nil || cmd != "cat  /dev/null" {
		t.Errorf("check = %q, %v", cmd, err)
	}
	// Without allow_empty the same start fails, and creates nothing.
	strict, err := Parse([]byte(strings.Replace(src, "allow_empty = true\noutput", "output", 1)), "t")
	if err != nil {
		t.Fatal(err)
	}
	s = &store{}
	if _, err := strict["f"].Start(s, "c", map[string]string{"dir": "tasks"}, cwd, ""); err == nil || !strings.Contains(err.Error(), "matched no files") || len(s.ws) != 0 {
		t.Errorf("strict: %v, created %d", err, len(s.ws))
	}
}

func TestStartForeachCap(t *testing.T) {
	cwd := t.TempDir()
	for i := range DefaultMaxItems + 1 {
		write(t, filepath.Join(cwd, "b", fmt.Sprintf("%02d.md", i)), "x")
	}
	s := &store{}
	_, err := builtIn(t, "research").Start(s, "r", map[string]string{"in": "b"}, cwd, "")
	if err == nil || !strings.Contains(err.Error(), "matched 13 files in "+cwd+", more than max_items (12)") || len(s.ws) != 0 {
		t.Errorf("err %v, created %d", err, len(s.ws))
	}
}

func TestRefsForRuntimeForeach(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "tasks", "a.md"), "a")
	write(t, filepath.Join(cwd, "tasks", "b.md"), "b")
	set, err := Parse([]byte(runtime), "t")
	if err != nil {
		t.Fatal(err)
	}
	s := &store{}
	ws, err := set["orch"].Start(s, "c", map[string]string{"dir": "tasks"}, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	// The copies come after the join in id order, as when the watcher
	// expands the foreach worker.
	copies, err := Expand(ws[1], "")
	if err != nil {
		t.Fatal(err)
	}
	all := ws
	for _, c := range copies {
		got, _ := s.Create(c)
		all = append(all, got)
	}
	join := ws[2]
	r := RefsFor(join, all, finalPath)
	outs := StepRef{Outputs: []string{"out/a.md", "out/b.md"}}
	if !reflect.DeepEqual(r.Steps["dig"], outs) || !reflect.DeepEqual(r.Steps["prev"], outs) {
		t.Errorf("join refs = %+v", r.Steps)
	}
	if brief, err := Render(join.Brief, r); err != nil || brief != "Merge out/a.md out/b.md." {
		t.Errorf("join brief = %q, %v", brief, err)
	}
	if brief, err := Render(all[4].Brief, RefsFor(all[4], all, finalPath)); err != nil || brief != "Do tasks/b.md." {
		t.Errorf("copy brief = %q, %v", brief, err)
	}
}

func TestStartRejectsBeforeCreating(t *testing.T) {
	escape, err := Parse([]byte(flowSrc(`"out"`, `id = "s"`+"\n"+`brief = "b"`+"\n"+`output = "{{out}}"`)), "t")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		f      Flow
		chain  string
		params map[string]string
		want   string
	}{
		{"no chain", builtIn(t, "plan-redteam"), "", map[string]string{"task": "x"}, "flow plan-redteam: no chain id"},
		{"missing param", builtIn(t, "plan-to-impl"), "c", map[string]string{"task": "x", "in": " "}, `flow plan-to-impl: missing param "in"`},
		{"unknown param", builtIn(t, "plan-redteam"), "c", map[string]string{"task": "x", "tsak": "y"}, `unknown param "tsak" (params: task)`},
		{"no matches", builtIn(t, "research"), "c", map[string]string{"in": "nothing-here"}, `flow research step dig: foreach "nothing-here/*.md" matched no files in`},
		{"escaping output", escape["a"], "c", map[string]string{"out": "../x.md"}, `flow a step s: output "../x.md" must be a relative path`},
		{"invalid flow", Flow{Name: "hand-made"}, "c", nil, "flow hand-made has no steps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &store{}
			ws, err := tt.f.Start(s, tt.chain, tt.params, t.TempDir(), "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if ws != nil || len(s.ws) != 0 {
				t.Errorf("created %+v", s.ws)
			}
		})
	}
}

func TestStartCancelsOnCreateFailure(t *testing.T) {
	s := &store{failAt: 3}
	ws, err := builtIn(t, "plan-to-impl").Start(s, "c", map[string]string{"task": "x", "in": "y"}, "/r", "")
	if ws != nil || err == nil || !strings.Contains(err.Error(), "step implement: disk full") || !strings.Contains(err.Error(), "cancelled workers 10, 11") {
		t.Fatalf("ws %+v err %v", ws, err)
	}
	for _, w := range s.ws {
		if w.State != registry.Cancelled || !strings.Contains(w.Error, "flow start failed") {
			t.Errorf("worker %+v", w)
		}
	}

	s = &store{failAt: 2, failUpd: true}
	_, err = builtIn(t, "plan-to-impl").Start(s, "c", map[string]string{"task": "x", "in": "y"}, "/r", "")
	if err == nil || !strings.Contains(err.Error(), "could not cancel worker 10: locked") {
		t.Errorf("err %v", err)
	}
}

// {{chain}} and {{work}} work everywhere refs do; the work directory is
// shown relative to the cwd when it lies inside it.
func TestStartChainAndWork(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, ".routr", "work", "c")
	write(t, filepath.Join(work, "in", "a.md"), "a")
	set, err := Parse([]byte(flowSrc("",
		`id = "now"`+"\n"+`foreach = "{{work}}/in/*.md"`+"\n"+`output = "{{work}}/out/{{item.name}}"`+"\n"+`brief = "{{item.path}} in {{chain}}"`,
		`id = "plan"`+"\n"+`output = "{{work}}/PLAN-{{chain}}.md"`+"\n"+`brief = "Write {{work}}/findings/*.md."`,
		`id = "dig"`+"\n"+`after = ["plan"]`+"\n"+`foreach = "{{work}}/findings/*.md"`+"\n"+`output = "{{work}}/r/{{item.name}}"`+"\n"+`brief = "{{item.path}}"`,
		`id = "review"`+"\n"+`after = ["dig"]`+"\n"+`brief = "{{dig.outputs}} in {{work}}"`+"\n"+`on_fail = { resume = "plan", brief = "See {{work}} of {{chain}}.", max = 1 }`,
	)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["a"].Start(&store{}, "c", nil, root, work)
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(".routr", "work", "c")
	if now := ws[0]; now.Params["item.path"] != filepath.Join(rel, "in", "a.md") || now.Output != filepath.Join(rel, "out", "a.md") {
		t.Errorf("foreach over {{work}}: %+v", now)
	}
	if ws[1].Output != filepath.Join(rel, "PLAN-c.md") {
		t.Errorf("plan output = %q", ws[1].Output)
	}
	write(t, filepath.Join(work, "findings", "x.md"), "x")
	copies, err := Expand(ws[2], work)
	if err != nil || len(copies) != 1 || copies[0].Params["item.path"] != filepath.Join(rel, "findings", "x.md") || copies[0].Output != filepath.Join(rel, "r", "x.md") {
		t.Errorf("expand = %+v, %v", copies, err)
	}

	// From a subdirectory the work directory lies outside: its paths are
	// absolute, and an output in it is still fine.
	sub := filepath.Join(root, "sub")
	os.Mkdir(sub, 0o755)
	if ws, err = set["a"].Start(&store{}, "c", nil, sub, work); err != nil || ws[1].Output != filepath.Join(work, "PLAN-c.md") ||
		ws[0].Params["item.path"] != filepath.Join(work, "in", "a.md") {
		t.Errorf("from sub: %+v, %v", ws, err)
	}

	esc, err := Parse([]byte(flowSrc("", `id = "s"`+"\n"+`brief = "b"`+"\n"+`output = "{{work}}/../x.md"`)), "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := esc["a"].Start(&store{}, "c", nil, sub, work); err == nil || !strings.Contains(err.Error(), "must be a relative path inside the directory") {
		t.Errorf("an output out of the work directory: %v", err)
	}
	if _, err := set["a"].Start(&store{}, "c", nil, root, ""); err == nil || !strings.Contains(err.Error(), "{{work}}: no work directory is known here") {
		t.Errorf("no work directory: %v", err)
	}
}

func TestStartRunStep(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"check"`,
		`id = "implement"`+"\n"+`brief = "Fix it."`,
		`id = "check"`+"\n"+`after = ["implement"]`+"\n"+`run = "{{check}}"`+"\n"+`expect = "fail"`+"\n"+
			`on_fail = { resume = "implement", brief = "See {{check.output}}.", max = 5 }`,
		`id = "again"`+"\n"+`after = ["check"]`+"\n"+`run = "true"`+"\n"+`timeout = "90s"`,
	)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["a"].Start(&store{}, "c", map[string]string{"check": "go test ./..."}, "/r", "")
	if err != nil {
		t.Fatal(err)
	}
	check, again := ws[1], ws[2]
	if check.Role != "run" || check.Run != "{{check}}" || check.Brief != "" || !check.Write || check.Expect != registry.Fail ||
		check.Timeout != registry.DefaultRunTimeout || check.OnFail == nil || check.OnFail.Resume != 10 || check.Params["check"] != "go test ./..." ||
		!reflect.DeepEqual(check.After, []int{10}) || check.WantVerdict || check.Output != "" {
		t.Errorf("check = %+v", check)
	}
	if again.Timeout != 90*time.Second || again.Expect != "" {
		t.Errorf("again = %+v", again)
	}
}

func TestStartJudgeStep(t *testing.T) {
	set, err := Parse([]byte(flowSrc(`"task"`,
		`id = "a"`+"\n"+`brief = "A {{task}}."`,
		`id = "b"`+"\n"+`brief = "B."`,
		`id = "best"`+"\n"+`after = ["a", "b"]`+"\n"+`judge = "jev"`+"\n"+`pick = true`+"\n"+`brief = "Which is best for {{task}}?"`+"\n"+`read = ["NOTES.md"]`,
		`id = "ok"`+"\n"+`after = ["best"]`+"\n"+`judge = "jev"`+"\n"+`brief = "Good enough?"`+"\n"+`min_confidence = 0.9`+"\n"+`fail = "no"`+"\n"+
			`on_fail = { resume = "a", brief = "Again.", max = 2 }`,
	)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["a"].Start(&store{}, "c", map[string]string{"task": "x"}, "/r", "")
	if err != nil {
		t.Fatal(err)
	}
	best, ok := ws[2], ws[3]
	if best.Role != "judge" || best.Brief != "Which is best for {{task}}?" || best.Write || best.Harness != "" || best.WantVerdict ||
		best.Judge == nil || !best.Judge.Pick || best.Judge.MinConfidence != registry.DefaultMinConfidence ||
		!reflect.DeepEqual(best.Judge.From, []int{10, 11}) || !reflect.DeepEqual(best.Judge.Read, []string{"NOTES.md"}) {
		t.Errorf("best = %+v %+v", best, best.Judge)
	}
	if j := ok.Judge; j == nil || j.Pick || j.Fail != "no" || j.MinConfidence != 0.9 || !reflect.DeepEqual(j.From, []int{12}) ||
		ok.OnFail == nil || ok.OnFail.Resume != 10 || ok.Brief != "Good enough?" || ok.Label() != "13 ok · jev" || ok.Tag() != "jev" {
		t.Errorf("ok = %+v %+v", ok, j)
	}
}

// finalPath is a worker's final message in a repo at /repo.
var finalPath = registry.Paths{Dir: "/repo/.routr"}.FinalPath

func TestRefsForChain(t *testing.T) {
	s := &store{}
	params := map[string]string{"task": "add OAuth", "in": "DECISIONS.md"}
	ws, err := builtIn(t, "plan-to-impl").Start(s, "c1", params, "/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	ws[0].State = registry.Done
	ws[1].State, ws[1].Verdict = registry.Done, registry.Fail
	ws[2].Output = "DIFF.md"
	other := registry.Worker{ID: 5, Chain: "c0", Step: "plan", Output: "OTHER.md"}
	all := append([]registry.Worker{other}, ws...)

	r := RefsFor(ws[3], all, finalPath)
	want := Refs{
		Params: params,
		Chain:  "c1",
		Steps: map[string]StepRef{
			"plan":        {Output: "PLAN.md", Final: "/repo/.routr/runs/10/final.md", Outputs: []string{"PLAN.md"}},
			"review":      {Output: "REDTEAM.md", Final: "/repo/.routr/runs/11/final.md", Verdict: "fail", Outputs: []string{"REDTEAM.md"}},
			"implement":   {Output: "DIFF.md", Final: "/repo/.routr/runs/12/final.md", Outputs: []string{"DIFF.md"}},
			"code-review": {Final: "/repo/.routr/runs/13/final.md"},
			"prev":        {Output: "DIFF.md", Final: "/repo/.routr/runs/12/final.md", Outputs: []string{"DIFF.md"}},
		},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("refs\n got %+v\nwant %+v", r, want)
	}

	// Later steps are not visible; the failing step's own refs are, for its on_fail brief.
	r = RefsFor(ws[1], all, finalPath)
	if _, ok := r.Steps["implement"]; ok {
		t.Error("a later step is visible")
	}
	got, err := Render(ws[1].OnFail.Brief, r)
	if err != nil || got != "Address REDTEAM.md." {
		t.Errorf("on_fail brief = %q, %v", got, err)
	}

	// Every brief renders from the records.
	for _, w := range ws {
		if _, err := Render(w.Brief, RefsFor(w, all, finalPath)); err != nil {
			t.Errorf("%s: %v", w.Step, err)
		}
	}
}

func TestRefsForForeach(t *testing.T) {
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "q", "one.md"), "1")
	write(t, filepath.Join(cwd, "q", "two.md"), "2")
	ws, err := builtIn(t, "research").Start(&store{}, "r", map[string]string{"in": "q"}, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	// Records in any order.
	all := []registry.Worker{ws[2], ws[1], ws[0]}

	r := RefsFor(ws[1], all, finalPath)
	if !reflect.DeepEqual(r.Params, map[string]string{"in": "q"}) || *r.Item != (Item{Path: filepath.Join("q", "two.md"), Name: "two.md"}) {
		t.Errorf("copy refs = %+v item %+v", r, r.Item)
	}
	brief, err := Render(ws[1].Brief, r)
	if err != nil || brief != "Research the question in q/two.md. Write your findings to research/two.md." {
		t.Errorf("dig brief = %q, %v", brief, err)
	}

	r = RefsFor(ws[2], all, finalPath)
	outs := StepRef{Outputs: []string{"research/one.md", "research/two.md"}}
	if r.Item != nil || !reflect.DeepEqual(r.Steps["dig"], outs) || !reflect.DeepEqual(r.Steps["prev"], outs) {
		t.Errorf("synthesize refs = %+v", r)
	}
	brief, err = Render(ws[2].Brief, r)
	if err != nil || brief != "Synthesize research/one.md research/two.md into research/SUMMARY.md." {
		t.Errorf("synthesize brief = %q, %v", brief, err)
	}
}

func TestRefsForPipe(t *testing.T) {
	up := registry.Worker{ID: 3, Output: "PLAN.md", Verdict: registry.Pass}
	w := registry.Worker{ID: 4, After: []int{3, 9}, Params: map[string]string{"task": "t"}}
	r := RefsFor(w, []registry.Worker{w, up}, finalPath)
	want := Refs{
		Params: map[string]string{"task": "t"},
		Chain:  "c4", // outside a chain: the name a pipe from it gives one
		Steps:  map[string]StepRef{"prev": {Output: "PLAN.md", Final: "/repo/.routr/runs/3/final.md", Verdict: "pass", Outputs: []string{"PLAN.md"}}},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("refs = %+v", r)
	}
	if r := RefsFor(registry.Worker{ID: 1, After: []int{99}}, nil, nil); len(r.Steps) != 0 || len(r.Params) != 0 {
		t.Errorf("missing upstream: %+v", r)
	}
}

// gitRepo makes a git repo in a temp directory with one commit, and
// returns its directory and that commit.
func gitRepo(t *testing.T) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	write(t, filepath.Join(dir, "sub", "main.go"), "package main\n")
	run("add", ".")
	run("commit", "-q", "-m", "first")
	return dir, run("rev-parse", "HEAD")
}

const trees = `
[flow.best]
params = ["task", "dir"]
[[flow.best.step]]
id = "a"
worktree = true
output = "{{work}}/a.md"
brief = "Try {{task}}."
[[flow.best.step]]
id = "b"
worktree = true
foreach = "{{dir}}/*.md"
output = "{{work}}/{{item.name}}"
brief = "Try {{item.path}}."
[[flow.best.step]]
id = "plan"
brief = "Plan."
[[flow.best.step]]
id = "c"
after = ["plan"]
worktree = true
foreach = "{{work}}/parts/*.md"
brief = "Do {{item.path}}."
[[flow.best.step]]
id = "check"
after = ["a"]
run = "cd {{a.worktree}} && git log -1 {{a.branch}}"
`

// A worktree step's workers start from HEAD where the flow started; their
// outputs and work-directory items are absolute, since their cwd will be
// their worktree.
func TestStartWorktreeSteps(t *testing.T) {
	repo, head := gitRepo(t)
	cwd := filepath.Join(repo, "sub")
	work := filepath.Join(repo, ".routr", "work", "c")
	write(t, filepath.Join(cwd, "in", "x.md"), "x")
	write(t, filepath.Join(cwd, "in", "y.md"), "y")
	set, err := Parse([]byte(trees), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["best"].Start(&store{}, "c", map[string]string{"task": "t", "dir": "in"}, cwd, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 6 {
		t.Fatalf("workers = %+v", ws)
	}
	want := registry.Checkout{Base: head, Repo: repo, Sub: "sub"}
	for _, i := range []int{0, 1, 2, 4} {
		if c := ws[i].Checkout; c == nil || *c != want || ws[i].Cwd != cwd {
			t.Errorf("worker %d: cwd %s checkout %+v", i, ws[i].Cwd, c)
		}
	}
	if ws[1].Checkout == ws[2].Checkout {
		t.Error("the copies share a Checkout")
	}
	if ws[3].Checkout != nil || ws[5].Checkout != nil {
		t.Errorf("plan or check has a worktree: %+v, %+v", ws[3], ws[5])
	}
	// The check after a runs in a's worktree, once the watcher has named it.
	if ws[5].TreeOf != ws[0].ID || ws[5].Cwd != cwd {
		t.Errorf("check: tree of %d, cwd %s", ws[5].TreeOf, ws[5].Cwd)
	}
	if ws[0].Output != filepath.Join(work, "a.md") || ws[1].Output != filepath.Join(work, "x.md") || ws[1].Params[itemPath] != "in/x.md" {
		t.Errorf("a output %q, b output %q item %q", ws[0].Output, ws[1].Output, ws[1].Params[itemPath])
	}

	// The runtime copies: each its own Checkout, items absolute.
	write(t, filepath.Join(work, "parts", "p.md"), "p")
	copies, err := Expand(ws[4], work)
	if err != nil || len(copies) != 1 {
		t.Fatalf("expand: %+v, %v", copies, err)
	}
	if c := copies[0]; c.Checkout == nil || c.Checkout == ws[4].Checkout || *c.Checkout != want || c.Params[itemPath] != filepath.Join(work, "parts", "p.md") {
		t.Errorf("copy = %+v, checkout %+v", c, c.Checkout)
	}

	// Outside a git repo it does not start.
	s := &store{}
	if _, err := set["best"].Start(s, "c", map[string]string{"task": "t", "dir": "in"}, t.TempDir(), work); err == nil ||
		!strings.Contains(err.Error(), "flow best: worktree = true needs a git repo: ") || !strings.Contains(err.Error(), "is not in a git repo") || len(s.ws) != 0 {
		t.Errorf("no repo: %v, created %d", err, len(s.ws))
	}
}

func TestRefsForWorktrees(t *testing.T) {
	a := registry.Worker{ID: 1, Chain: "c", Step: "a", Checkout: &registry.Checkout{Path: "/r/.routr/trees/c/a-1", Branch: "routr/c/a-1"}, State: registry.Done}
	b := registry.Worker{ID: 2, Chain: "c", Step: "b", Checkout: &registry.Checkout{Path: "/r/.routr/trees/c/b 2", Branch: "routr/c/b-2"}, State: registry.Done}
	best := registry.Worker{ID: 3, Chain: "c", Step: "best", After: []int{1, 2}, Judge: &registry.Judge{Pick: true, Winner: 2}, State: registry.Done}
	run := registry.Worker{ID: 4, Chain: "c", Step: "check", After: []int{3}, Run: "cd {{b.worktree}} && git merge {{best.branch}}"}
	all := []registry.Worker{a, b, best, run}
	r := RefsFor(run, all, finalPath)
	if r.Steps["a"].Worktree != a.Checkout.Path || r.Steps["best"].Branch != "routr/c/b-2" || r.Steps["best"].Worktree != b.Checkout.Path {
		t.Errorf("refs = %+v", r.Steps)
	}
	if cmd, err := RenderShell(run.Run, r); err != nil || cmd != "cd '/r/.routr/trees/c/b 2' && git merge routr/c/b-2" {
		t.Errorf("command = %q, %v", cmd, err)
	}
	if _, err := Render("{{check.worktree}}", RefsFor(registry.Worker{ID: 5, Chain: "c", After: []int{4}}, append(all, registry.Worker{ID: 5, Chain: "c"}), finalPath)); err == nil ||
		!strings.Contains(err.Error(), `step "check" has no worktree`) {
		t.Errorf("no worktree: %v", err)
	}
}

const checks = `
[flow.f]
params = ["task"]
[[flow.f.step]]
id = "plan"
output = "{{work}}/PLAN.md"
write = false
brief = "Plan {{task}}."
[[flow.f.step]]
id = "a"
after = ["plan"]
worktree = true
brief = "Try it."
[[flow.f.step]]
id = "b"
after = ["plan"]
worktree = true
brief = "Try it too."
[[flow.f.step]]
id = "many"
after = ["plan"]
worktree = true
foreach = "{{work}}/*.md"
brief = "Do {{item.path}}."
[[flow.f.step]]
id = "check-a"
after = ["a", "plan"]
run = "go test ./..."
[[flow.f.step]]
id = "both"
after = ["a", "b"]
run = "true"
[[flow.f.step]]
id = "copies"
after = ["many"]
run = "true"
[[flow.f.step]]
id = "best"
after = ["a", "b", "check-a"]
judge = "jev"
pick = true
brief = "Pick one."
[[flow.f.step]]
id = "check-best"
after = ["best"]
run = "true"
[[flow.f.step]]
id = "hunt"
role = "review"
after = ["check-best"]
verdict = true
brief = "Hunt."
`

// A run step after one worktree step (or a pick over worktree steps) runs
// in its worktree; after none, or after several or a foreach's copies, it
// stays in the flow's cwd. write and verdict reach the workers.
func TestStartRunStepsInheritTheWorktree(t *testing.T) {
	repo, _ := gitRepo(t)
	set, err := Parse([]byte(checks), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["f"].Start(&store{}, "c", map[string]string{"task": "t"}, repo, filepath.Join(repo, ".routr", "work", "c"))
	if err != nil {
		t.Fatal(err)
	}
	id := map[string]registry.Worker{}
	for _, w := range ws {
		id[w.Step] = w
	}
	for step, want := range map[string]int{"check-a": id["a"].ID, "both": 0, "copies": 0, "check-best": id["best"].ID} {
		if got := id[step].TreeOf; got != want {
			t.Errorf("%s: tree of %d, want %d", step, got, want)
		}
	}
	if p := id["plan"]; p.WritePin == nil || *p.WritePin {
		t.Errorf("plan = %+v", p)
	}
	if h := id["hunt"]; h.WritePin != nil || !strings.HasSuffix(h.Brief, askVerdict) {
		t.Errorf("hunt = %+v", h)
	}
}
