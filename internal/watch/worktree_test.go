package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// newGitRig is a rig whose directory is a git repo with one commit.
func newGitRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if real, err := filepath.EvalSymlinks(r.cwd); err == nil && real != r.cwd {
		t.Skipf("temp dir %s is a symlink", r.cwd)
	}
	r.git("init", "-q")
	os.WriteFile(filepath.Join(r.cwd, "main.go"), []byte("package main\n"), 0o644)
	r.git("add", "main.go")
	r.git("commit", "-q", "-m", "first")
	return r
}

// git runs git in dir (the rig's directory if "").
func (r *rig) gitIn(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *rig) git(args ...string) string { return r.gitIn(r.cwd, args...) }

// doneAt plays `routr done --output out` with out absolute.
func (r *rig) doneAt(id int, out, msg string) {
	r.t.Helper()
	os.MkdirAll(filepath.Dir(out), 0o755)
	os.WriteFile(out, []byte("result of "+msg), 0o644)
	if err := registry.WriteDone(r.p.RunDir(id), registry.DoneRecord{Output: out, Message: msg, At: r.now}); err != nil {
		r.t.Fatal(err)
	}
	r.fake.Finish(id, "")
}

const attempts = `
[flow.best]
params = ["task"]
[[flow.best.step]]
id = "a"
worktree = true
output = "{{work}}/a.md"
brief = "Try {{task}}; notes to {{work}}/a.md."
[[flow.best.step]]
id = "b"
worktree = true
output = "b.md"
brief = "Try {{task}} another way."
[[flow.best.step]]
id = "main"
brief = "Edit the main tree."
[[flow.best.step]]
id = "check"
after = ["a", "b"]
run = "cat {{a.output}} {{b.output}} && cd {{a.worktree}} && git rev-parse --abbrev-ref HEAD"
[[flow.best.step]]
id = "merge"
after = ["check"]
brief = "Merge {{a.branch}} from {{a.worktree}}; a wrote {{a.output}}, b {{b.output}}."
`

// Two worktree writers and a writer in the main tree all start in one
// tick: each locks its own tree. The worktrees start from HEAD when the
// flow started, outputs land in the main repo's work directory, and the
// steps after them get their worktree and branch.
func TestWorktreeStepsRunSideBySide(t *testing.T) {
	r := newGitRig(t)
	base := r.git("rev-parse", "HEAD")
	ws := r.startFlow(attempts, "best", map[string]string{"task": "parse"})
	a, b, main, check, merge := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID, ws[4].ID
	for _, id := range []int{a, b, main} {
		r.s.Update(id, func(x *registry.Worker) { x.Write = true }) // as a write role makes them
	}
	// The main tree moves on after the flow started.
	os.WriteFile(filepath.Join(r.cwd, "main.go"), []byte("package main // 2\n"), 0o644)
	r.git("commit", "-q", "-am", "second")

	r.tick(1)
	work := r.p.WorkDir("c1")
	locks, err := r.s.Holders()
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []int{a, b} {
		w := r.state(id, registry.Running)
		step := []string{"a", "b"}[i]
		path := filepath.Join(r.p.Dir, "trees", "c1", step+"-"+string(rune('0'+id)))
		c := w.Checkout
		if c == nil || !c.Made || c.Path != path || c.Branch != "routr/c1/"+step+"-"+string(rune('0'+id)) || c.Base != base || w.Cwd != path || w.Tree != path {
			t.Fatalf("%s = cwd %s tree %s checkout %+v", step, w.Cwd, w.Tree, c)
		}
		if got := r.gitIn(path, "rev-parse", "HEAD"); got != base {
			t.Errorf("%s's worktree is at %s, want the flow's base %s", step, got, base)
		}
		if got := r.gitIn(path, "rev-parse", "--abbrev-ref", "HEAD"); got != c.Branch {
			t.Errorf("%s's worktree is on %s", step, got)
		}
		if b, _ := os.ReadFile(filepath.Join(path, "main.go")); string(b) != "package main\n" {
			t.Errorf("%s's main.go = %q", step, b)
		}
		spec := r.fake.Panes(id)[0].Spec
		if spec.Cwd != path || len(spec.Dirs) != 1 || spec.Dirs[0] != work || spec.Env["ROUTR_STATE_DIR"] != r.p.Dir || spec.Env["ROUTR_CWD"] != path {
			t.Errorf("%s spec: cwd %s dirs %v env %v", step, spec.Cwd, spec.Dirs, spec.Env)
		}
		if locks[path] != id {
			t.Errorf("locks = %v", locks)
		}
	}
	r.state(main, registry.Running)
	if locks[r.cwd] != main || len(r.fake.Panes(main)[0].Spec.Dirs) != 0 {
		t.Errorf("main: locks %v, dirs %v", locks, r.fake.Panes(main)[0].Spec.Dirs)
	}
	if fi, err := os.Stat(work); err != nil || !fi.IsDir() {
		t.Errorf("work dir: %v", err)
	}
	prompt := r.fake.Prompts(a)[0]
	for _, want := range []string{"Try parse; notes to " + work + "/a.md.", "routr done --output " + work + "/a.md"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("a's prompt lacks %q:\n%s", want, prompt)
		}
	}
	if p := r.fake.Prompts(b)[0]; !strings.Contains(p, "routr done --output "+filepath.Join(r.get(b).Cwd, "b.md")) {
		t.Errorf("b's prompt:\n%s", p)
	}
	if st := r.git("status", "--porcelain"); strings.Contains(st, "trees") {
		t.Errorf("the worktrees show in the main tree's status:\n%s", st)
	}

	r.doneAt(a, filepath.Join(work, "a.md"), "tried a")
	bPath := r.get(b).Cwd
	r.doneAt(b, filepath.Join(bPath, "b.md"), "tried b")
	r.done(main, "", "", "edited", "")
	r.settle()
	w := r.await(check, registry.Done)
	log := r.read(w.Output)
	for _, want := range []string{"routr/c1/a-1", "result of tried a", "result of tried b"} {
		if !strings.Contains(log, want) {
			t.Errorf("check's log lacks %q:\n%s", want, log)
		}
	}
	if cmd := r.read(filepath.Join(r.p.RunDir(check), "brief.md")); !strings.Contains(cmd, "&& cd "+r.get(a).Cwd+" && ") {
		t.Errorf("check's command: %s", cmd)
	}
	r.tick(1)
	r.state(merge, registry.Running)
	wantBrief := "Merge routr/c1/a-1 from " + r.get(a).Cwd + "; a wrote " + work + "/a.md, b " + filepath.Join(bPath, "b.md") + "."
	if p := r.fake.Prompts(merge)[0]; !strings.Contains(p, wantBrief) {
		t.Errorf("merge's prompt lacks %q:\n%s", wantBrief, p)
	}
	// The worktrees stay for you to merge.
	if _, err := os.Stat(bPath); err != nil {
		t.Errorf("b's worktree went: %v", err)
	}
}

// A worktree that cannot be made fails its worker, which never runs in the
// main tree instead.
func TestWorktreeThatCannotBeMadeFails(t *testing.T) {
	r := newGitRig(t)
	ws := r.startFlow(`
[flow.f]
[[flow.f.step]]
id = "a"
worktree = true
brief = "Try."
`, "f", nil)
	r.git("branch", "routr/c1/a-1") // in the way
	r.tick(1)
	w := r.state(ws[0].ID, registry.Failed)
	for _, want := range []string{"could not make its git worktree", "never runs in the main tree", "git worktree add -b routr/c1/a-1", "already exists"} {
		if !strings.Contains(w.Error, want) {
			t.Errorf("error %q lacks %q", w.Error, want)
		}
	}
	if len(r.fake.Panes(ws[0].ID)) != 0 || w.Checkout.Made {
		t.Errorf("it started anyway: %+v", w)
	}
}

// A runtime foreach with worktree = true: each copy its own worktree, side
// by side, its item from the main repo's work directory.
func TestWorktreeForeachCopies(t *testing.T) {
	r := newGitRig(t)
	ws := r.startFlow(`
[flow.f]
[[flow.f.step]]
id = "plan"
brief = "Write {{work}}/parts/*.md."
[[flow.f.step]]
id = "try"
after = ["plan"]
worktree = true
foreach = "{{work}}/parts/*.md"
brief = "Do {{item.path}}."
`, "f", nil)
	r.tick(1)
	r.write(".routr/work/c1/parts/x.md")
	r.write(".routr/work/c1/parts/y.md")
	r.done(ws[0].ID, "", "", "planned", "")
	r.settle()
	r.tick(1)
	for _, id := range []int{3, 4} {
		w := r.state(id, registry.Running)
		if w.Checkout == nil || w.Cwd != filepath.Join(r.p.Dir, "trees", "c1", "try-"+string(rune('0'+id))) || !w.Checkout.Made {
			t.Errorf("copy %d: cwd %s checkout %+v", id, w.Cwd, w.Checkout)
		}
		item := filepath.Join(r.p.WorkDir("c1"), "parts", []string{"x.md", "y.md"}[id-3])
		if p := r.fake.Prompts(id)[0]; !strings.Contains(p, "Do "+item+".") {
			t.Errorf("copy %d prompt:\n%s", id, p)
		}
	}
	if fan := r.get(ws[1].ID); fan.Checkout == nil || fan.Checkout.Path != "" {
		t.Errorf("the foreach worker got a worktree: %+v", fan.Checkout)
	}
}

// A foreach worktree step over files of the repo: each copy's brief names
// the file in its own worktree.
func TestWorktreeForeachOverRepoFiles(t *testing.T) {
	r := newGitRig(t)
	ws := r.startFlow(`
[flow.f]
[[flow.f.step]]
id = "try"
worktree = true
foreach = "*.go"
brief = "Do {{item.path}}."
`, "f", nil)
	r.tick(1)
	w := r.state(ws[0].ID, registry.Running)
	if p := r.fake.Prompts(w.ID)[0]; w.Cwd == r.cwd || !strings.Contains(p, "Do "+filepath.Join(w.Cwd, "main.go")+".") {
		t.Errorf("cwd %s, prompt:\n%s", w.Cwd, p)
	}
}

// A pick over worktree attempts: the step after it gets the winner's
// worktree and branch, and a judge reads files in an attempt's worktree.
func TestPickAmongWorktrees(t *testing.T) {
	r := newGitRig(t)
	f := r.jev(choose("b-2", 0.9))
	ws := r.startFlow(`
[flow.best]
[[flow.best.step]]
id = "a"
worktree = true
brief = "Try."
[[flow.best.step]]
id = "b"
worktree = true
brief = "Try otherwise."
[[flow.best.step]]
id = "best"
after = ["a", "b"]
judge = "jev"
pick = true
read = ["{{a.worktree}}/main.go"]
brief = "Which?"
[[flow.best.step]]
id = "apply"
after = ["best"]
brief = "Merge {{best.branch}} ({{best.worktree}})."
`, "best", nil)
	a, b, best, apply := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID
	r.tick(1)
	r.done(a, "", "", "a", "")
	r.done(b, "", "", "b", "")
	r.settle()
	if w := r.await(best, registry.Done); w.Judge.Winner != b {
		t.Errorf("best = %+v", w.Judge)
	}
	if files := f.Calls()[0].state.Files; len(files) != 1 || files[0].Content != "package main\n" {
		t.Errorf("files = %+v", files)
	}
	r.tick(1)
	r.state(apply, registry.Running)
	if p := r.fake.Prompts(apply)[0]; !strings.Contains(p, "Merge routr/c1/b-2 ("+r.get(b).Cwd+").") {
		t.Errorf("apply's prompt:\n%s", p)
	}
}

const checked = `
[flow.two]
params = ["task"]
[[flow.two.step]]
id = "a"
worktree = true
brief = "Try {{task}}."
[[flow.two.step]]
id = "b"
worktree = true
brief = "Try {{task}} another way."
[[flow.two.step]]
id = "main"
brief = "Edit the main tree."
[[flow.two.step]]
id = "check-a"
after = ["a"]
run = "pwd; sleep 30"
[[flow.two.step]]
id = "check-b"
after = ["b"]
run = "pwd; sleep 30"
`

// A run step after a worktree step runs in that worktree and takes its
// lock: the checks on two attempts run together, while a writer holds the
// main tree.
func TestRunStepsOnDifferentWorktreesRunTogether(t *testing.T) {
	r := newGitRig(t)
	t.Cleanup(func() { r.reap(r.w) })
	ws := r.startFlow(checked, "two", map[string]string{"task": "parse"})
	a, b, main, checkA, checkB := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID, ws[4].ID
	for _, id := range []int{a, b, main} {
		r.s.Update(id, func(x *registry.Worker) { x.Write = true })
	}
	for _, id := range []int{checkA, checkB} {
		r.s.Update(id, func(x *registry.Worker) { x.Write = true }) // as roleCreator leaves a run step
	}
	r.tick(1)
	r.state(main, registry.Running)
	r.doneAt(a, "", "a done")
	r.doneAt(b, "", "b done")
	r.settle()
	r.state(a, registry.Done)
	r.state(b, registry.Done)
	locks, err := r.s.Holders()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]int{{checkA, a}, {checkB, b}} {
		c, tree := r.state(p[0], registry.Running), r.get(p[1])
		if c.Cwd != tree.Cwd || c.Tree != tree.Checkout.Path || locks[c.Tree] != c.ID {
			t.Errorf("check %d: cwd %s tree %s, its step's %s; locks %v", c.ID, c.Cwd, c.Tree, tree.Cwd, locks)
		}
	}
	if r.state(main, registry.Running); locks[r.cwd] != main {
		t.Errorf("main lock: %v", locks)
	}
}
