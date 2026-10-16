package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// orchestrate is a planner that writes task files, one worker per file, and
// a join. extra goes into the foreach step.
func orchestrate(extra string) string {
	return `
[flow.orch]
params = ["task"]
[[flow.orch.step]]
id = "plan"
output = "PLAN.md"
brief = "Split {{task}} into tasks/*.md."
[[flow.orch.step]]
id = "dig"
role = "research"
after = ["plan"]
foreach = "tasks/*.md"
output = "out/{{item.name}}"
brief = "Do {{item.path}} as {{plan.output}} says."
` + extra + `
[[flow.orch.step]]
id = "join"
after = ["dig"]
brief = "Merge {{dig.outputs}} (prev: {{prev.outputs}})."
`
}

// startFlow starts flow name from src in the rig's directory.
func (r *rig) startFlow(src, name string, params map[string]string) []registry.Worker {
	r.t.Helper()
	set, err := flow.Parse([]byte(src), "t")
	if err != nil {
		r.t.Fatal(err)
	}
	ws, err := set[name].Start(r.s, "c1", params, r.cwd, r.p.WorkDir("c1"))
	if err != nil {
		r.t.Fatal(err)
	}
	return ws
}

func (r *rig) write(path string) {
	r.t.Helper()
	path = filepath.Join(r.cwd, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) count() int {
	r.t.Helper()
	all, err := r.s.List()
	if err != nil {
		r.t.Fatal(err)
	}
	return len(all)
}

func TestRuntimeForeachExpandsThenJoins(t *testing.T) {
	r := newRig(t)
	ws := r.startFlow(orchestrate(""), "orch", map[string]string{"task": "x"})
	plan, dig, join := ws[0].ID, ws[1].ID, ws[2].ID
	r.tick(1)
	r.state(plan, registry.Running)
	if w := r.state(dig, registry.Waiting); !strings.Contains(w.Note, "foreach tasks/*.md, once the steps before it are done: waiting on 1 task") {
		t.Errorf("dig note = %q", w.Note)
	}
	for _, f := range []string{"tasks/b.md", "tasks/a.md", "tasks/c.md"} {
		r.write(f)
	}
	os.MkdirAll(filepath.Join(r.cwd, "out"), 0o755)
	r.done(plan, "PLAN.md", "", "split into 3", "")
	r.settle()
	r.tick(1)

	all, _ := r.s.List()
	if len(all) != 6 {
		t.Fatalf("workers = %+v", all)
	}
	copies := []int{4, 5, 6}
	for i, name := range []string{"a.md", "b.md", "c.md"} {
		w := r.state(copies[i], registry.Running) // side by side
		if w.Step != "dig" || w.Role != "research" || w.Chain != "c1" || w.Fanout != nil || w.Output != "out/"+name ||
			w.Params["item.path"] != "tasks/"+name || len(w.After) != 1 || w.After[0] != plan {
			t.Errorf("copy %d = %+v", w.ID, w)
		}
		if p := r.fake.Prompts(w.ID)[0]; !strings.Contains(p, "Do "+r.abs("tasks/"+name)+" as "+r.abs("PLAN.md")+" says.") {
			t.Errorf("copy %d prompt:\n%s", w.ID, p)
		}
	}
	w := r.state(dig, registry.Waiting)
	if !strings.Contains(w.Note, "expanded into 3 workers (4, 5, 6); waiting on 4 research") || w.Output != "" {
		t.Errorf("dig after expanding: note %q output %q", w.Note, w.Output)
	}
	r.state(join, registry.Waiting)

	for i, id := range copies {
		r.done(id, "out/"+[]string{"a.md", "b.md", "c.md"}[i], "", "did it", "")
	}
	r.settle()
	r.tick(2)
	if w := r.state(dig, registry.Done); w.Summary != "expanded into 3 workers (4, 5, 6), all done" || w.Finished.IsZero() {
		t.Errorf("dig done: %+v", w)
	}
	if len(r.fake.Panes(dig)) != 0 {
		t.Error("the foreach worker opened a tab")
	}
	r.state(join, registry.Running)
	outs := r.abs("out/a.md") + " " + r.abs("out/b.md") + " " + r.abs("out/c.md")
	if p := r.fake.Prompts(join)[0]; !strings.Contains(p, "Merge "+outs+" (prev: "+outs+").") {
		t.Errorf("join prompt:\n%s", p)
	}
}

func TestRuntimeForeachWithoutFilesNeedsYou(t *testing.T) {
	r := newRig(t)
	ws := r.startFlow(orchestrate(""), "orch", map[string]string{"task": "x"})
	plan, dig, join := ws[0].ID, ws[1].ID, ws[2].ID
	r.tick(1)
	r.done(plan, "PLAN.md", "", "wrote nothing", "")
	r.settle()
	r.tick(1)
	w := r.state(dig, registry.NeedsYou)
	for _, want := range []string{`foreach "tasks/*.md" matched no files in ` + r.cwd, "nothing was started", "routr approve 2", "routr cancel c1"} {
		if !strings.Contains(w.Note, want) {
			t.Errorf("note %q lacks %q", w.Note, want)
		}
	}
	r.state(join, registry.Waiting)
	if r.count() != 3 {
		t.Error("copies were created")
	}

	// The files appear; routr approve sends it back to look again.
	r.write("tasks/a.md")
	r.s.Update(dig, func(x *registry.Worker) { x.State, x.Note = registry.Pending, "" })
	r.tick(2)
	r.state(4, registry.Running)
	if w := r.get(dig); len(w.Fanout.Items) != 1 || w.Fanout.Items[0] != 4 {
		t.Errorf("dig = %+v", w.Fanout)
	}
}

func TestRuntimeForeachOverItsCapNeedsYou(t *testing.T) {
	r := newRig(t)
	ws := r.startFlow(orchestrate("max_items = 2"), "orch", map[string]string{"task": "x"})
	plan, dig := ws[0].ID, ws[1].ID
	r.tick(1)
	for _, f := range []string{"tasks/a.md", "tasks/b.md", "tasks/c.md"} {
		r.write(f)
	}
	r.done(plan, "PLAN.md", "", "split into 3", "")
	r.settle()
	r.tick(1)
	if w := r.state(dig, registry.NeedsYou); !strings.Contains(w.Note, `foreach "tasks/*.md" matched 3 files in `+r.cwd+", more than max_items (2)") {
		t.Errorf("note = %q", w.Note)
	}
	if r.count() != 3 {
		t.Error("copies were created")
	}
}

func TestParallelCapsCopies(t *testing.T) {
	r := newRig(t)
	for _, f := range []string{"in/a.md", "in/b.md", "in/c.md"} {
		r.write(f)
	}
	ws := r.startFlow(`
[flow.f]
[[flow.f.step]]
id = "dig"
foreach = "in/*.md"
parallel = 1
brief = "Do {{item.path}}."
`, "f", nil)
	a, b, c := ws[0].ID, ws[1].ID, ws[2].ID
	r.tick(1)
	r.state(a, registry.Running)
	for _, id := range []int{b, c} {
		if w := r.state(id, registry.Waiting); w.Note != "waiting for a slot: step dig runs 1 at a time (running: 1)" {
			t.Errorf("%d note = %q", id, w.Note)
		}
	}
	r.done(a, "", "", "a", "")
	r.settle()
	r.tick(1)
	r.state(a, registry.Done)
	r.state(b, registry.Running)
	if w := r.state(c, registry.Waiting); !strings.Contains(w.Note, "(running: 2)") {
		t.Errorf("c note = %q", w.Note)
	}
	// A copy blocked on a question still holds its slot.
	r.fake.Ask(b, "Allow?", "Yes", "No")
	r.settle()
	r.state(b, registry.Blocked)
	r.state(c, registry.Waiting)
}

// A planner writes its findings into the chain's work directory and a
// foreach step globs them there, so a second run of the flow does not pick
// up the first one's files.
func TestRuntimeForeachOverTheWorkDirectory(t *testing.T) {
	const src = `
[flow.find]
[[flow.find.step]]
id = "scan"
brief = "Write one file per finding to {{work}}/findings/ ({{chain}})."
[[flow.find.step]]
id = "fix"
after = ["scan"]
foreach = "{{work}}/findings/*.md"
brief = "Fix {{item.path}}."
`
	r := newRig(t)
	set, err := flow.Parse([]byte(src), "t")
	if err != nil {
		t.Fatal(err)
	}
	start := func(chain string) []registry.Worker {
		ws, err := set["find"].Start(r.s, chain, nil, r.cwd, r.p.WorkDir(chain))
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	ws := start("find-1")
	r.tick(1)
	if p := r.fake.Prompts(ws[0].ID)[0]; !strings.Contains(p, "Write one file per finding to "+r.p.WorkDir("find-1")+"/findings/ (find-1).") {
		t.Errorf("scan prompt:\n%s", p)
	}
	r.write(".routr/work/find-1/findings/a.md")
	r.done(ws[0].ID, "", "", "found 1", "")
	r.settle()
	r.tick(1)
	if w := r.state(3, registry.Running); !strings.Contains(r.fake.Prompts(w.ID)[0], "Fix "+r.p.WorkDir("find-1")+"/findings/a.md.") {
		t.Errorf("copy prompt:\n%s", r.fake.Prompts(w.ID)[0])
	}

	ws = start("find-2")
	r.tick(1)
	r.done(ws[0].ID, "", "", "found nothing", "")
	r.settle()
	r.tick(1)
	if w := r.state(ws[1].ID, registry.NeedsYou); !strings.Contains(w.Note, `".routr/work/find-2/findings/*.md" matched no files`) {
		t.Errorf("second run note = %q", w.Note)
	}
}

// Outside a chain, a worker's {{chain}} is c<ID>, the chain a pipe from it
// starts, and its work directory is made when it starts.
func TestWorkOutsideAChain(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "Notes to {{work}}/notes.md ({{chain}})."})
	r.tick(1)
	r.state(id, registry.Running)
	if p := r.fake.Prompts(id)[0]; !strings.Contains(p, "Notes to "+r.p.WorkDir("c1")+"/notes.md (c1).") {
		t.Errorf("prompt:\n%s", p)
	}
	if fi, err := os.Stat(r.p.WorkDir("c1")); err != nil || !fi.IsDir() {
		t.Errorf("work dir: %v", err)
	}
}

// With allow_empty, a runtime foreach that matches nothing is done with no
// copies: the join runs and reads its outputs as none, and a judge after it
// sees no results from it, only the others.
func TestRuntimeForeachAllowEmpty(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("pass", 0.9))
	ws := r.startFlow(orchestrate("allow_empty = true")+`
[[flow.orch.step]]
id = "ship"
after = ["plan", "dig"]
judge = "jev"
brief = "Do the findings in {{dig.outputs}} stop it?"
`, "orch", map[string]string{"task": "x"})
	plan, dig, join, ship := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID
	r.tick(1)
	r.done(plan, "PLAN.md", "", "found nothing to split", "")
	r.settle()
	r.tick(1)
	w := r.state(dig, registry.Done)
	if w.Summary != "foreach tasks/*.md matched no files; allow_empty, so nothing ran" || w.Note != w.Summary || w.Finished.IsZero() || w.Fanout.Items != nil {
		t.Errorf("dig = %+v", w)
	}
	if r.count() != 4 {
		t.Error("copies were created")
	}
	r.tick(1)
	r.state(join, registry.Running)
	if p := r.fake.Prompts(join)[0]; !strings.Contains(p, "Merge (none) (prev: (none)).") {
		t.Errorf("join prompt:\n%s", p)
	}
	r.await(ship, registry.Done)
	call := f.Calls()[0]
	if ups := call.state.Upstream; len(ups) != 1 || ups[0].Step != "plan" || len(call.state.Empty) != 1 || call.state.Empty[0] != "dig" {
		t.Errorf("state = %+v", call.state)
	}
	if b := r.read(filepath.Join(r.p.RunDir(ship), "brief.md")); b != "Do the findings in (none) stop it?\n" {
		t.Errorf("judge brief = %q", b)
	}
}

// A verdict judge after only an empty foreach still asks, with no results;
// a pick has nothing to pick from.
func TestJudgeAfterOnlyAnEmptyForeach(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("pass", 0.9))
	ws := r.startFlow(`
[flow.f]
[[flow.f.step]]
id = "dig"
foreach = "in/*.md"
allow_empty = true
brief = "Do {{item.path}}."
[[flow.f.step]]
id = "ship"
after = ["dig"]
judge = "jev"
brief = "Ship?"
[[flow.f.step]]
id = "best"
after = ["dig"]
judge = "jev"
pick = true
brief = "Best?"
`, "f", nil)
	dig, ship, best := ws[0].ID, ws[1].ID, ws[2].ID
	r.state(dig, registry.Done)
	r.await(ship, registry.Done)
	if c := f.Calls(); len(c) != 1 || len(c[0].state.Upstream) != 0 || len(c[0].state.Empty) != 1 {
		t.Errorf("calls = %+v", c)
	}
	if w := r.state(best, registry.Failed); !strings.Contains(w.Error, "no upstream worker to pick from: dig matched no files") {
		t.Errorf("best error = %q", w.Error)
	}
}
