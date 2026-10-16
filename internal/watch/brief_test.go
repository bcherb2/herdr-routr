package watch

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

func TestRefsFromTheRegistry(t *testing.T) {
	dir := t.TempDir()
	p := registry.Paths{Dir: filepath.Join(dir, ".routr")}
	all := []registry.Worker{
		{ID: 1, Runs: 2, Chain: "c", Step: "plan", Cwd: dir, Output: "PLAN.md", Verdict: registry.Pass},
		{ID: 2, Chain: "c", Step: "dig", Cwd: dir, Output: filepath.Join(dir, "research", "a.md"), Params: map[string]string{"item.path": "b/a.md", "item.name": "a.md"}},
		{ID: 3, Chain: "c", Step: "dig", Cwd: dir, Output: "research/b.md", Params: map[string]string{"item.path": "b/b.md", "item.name": "b.md"}},
		{ID: 4, Chain: "other", Step: "plan", Cwd: dir, Output: "NOPE.md"},
		{ID: 5, Chain: "c", Step: "synth", Cwd: filepath.Join(dir, "sub"), After: []int{1}, Params: map[string]string{"task": "t"},
			Brief: "{{task}}: {{plan.output}} {{plan.final}} {{plan.verdict}} | {{dig.outputs}} | {{prev.output}}"},
	}
	got, err := Render(all[4], all, p)
	if err != nil {
		t.Fatal(err)
	}
	// A brief's paths are absolute, from any cwd.
	// {{plan.final}} is its current run's.
	want := "t: " + filepath.Join(dir, "PLAN.md") + " " + filepath.Join(p.RunDir(1), "final-2.md") + " pass | " +
		filepath.Join(dir, "research/a.md") + " " + filepath.Join(dir, "research/b.md") + " | " + filepath.Join(dir, "PLAN.md")
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	all[4].Cwd = dir
	if got, _ = Render(all[4], all, p); got != want {
		t.Errorf("from the repo root:\ngot  %s\nwant %s", got, want)
	}
	// Relative to dir (a run step's cwd) where they lie inside it.
	got, _ = flow.Render(all[4].Brief, Refs(all[4], all, p, dir))
	if want := "t: PLAN.md .routr/runs/1/final-2.md pass | research/a.md research/b.md | PLAN.md"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// From sub/, files in the repo root are outside it, so they stay absolute.
	if got, _ = flow.Render(all[4].Brief, Refs(all[4], all, p, filepath.Join(dir, "sub"))); got != want {
		t.Errorf("from sub/:\ngot  %s\nwant %s", got, want)
	}
	// A foreach item is matched relative to the copy's cwd.
	dig := all[1]
	dig.Brief = "{{item.path}} {{item.name}}"
	if got, _ = Render(dig, all, p); got != filepath.Join(dir, "b/a.md")+" a.md" {
		t.Errorf("item: %s", got)
	}
}

func TestComposeTellsTheWorkerHowToFinish(t *testing.T) {
	p := registry.Paths{Dir: "/r/.routr"}
	review := Compose(registry.Worker{ID: 3, Role: "review", Preamble: "Be adversarial.", WantVerdict: true, Cwd: "/r", Output: "REDTEAM.md"}, p, "Red-team PLAN.md.", "/opt/routr/bin/routr")
	// The output is absolute and the summary in single quotes, so that a $
	// or a backtick in it is not shell expansion, which Claude asks about.
	for _, want := range []string{"Be adversarial.\n\nRed-team PLAN.md.", "worker 3 (role review)", "Write your result to /r/REDTEAM.md.",
		" Do not end your turn while a command you started is still running, in the background or not: routr takes an ended turn as finished. Wait for it within the turn.",
		"When the work is finished and its checks have passed, run this command on its own, as your last command (not after a ; or && in another command, and with no edits after it), then end your turn with a short final message:\n\n" +
			"    /opt/routr/bin/routr done --output /r/REDTEAM.md --verdict pass|fail 'SUMMARY'\n",
		"Keep the single quotes", "--verdict pass if",
		"If a check failed, say so in the output and the summary; do not report success.",
		"If the command fails, you have not finished: fix what it says and run it again.",
		"you need no routr skill for it, and start no workers of your own unless your brief says to."} {
		if !strings.Contains(review, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, review)
		}
	}
	if strings.Contains(review, "worktree") {
		t.Errorf("a worker in the main checkout is told of a worktree:\n%s", review)
	}
	plan := Compose(registry.Worker{ID: 1, Role: "plan", Interactive: true}, p, "Plan.", "")
	if !strings.Contains(plan, "The user talks to you in this tab") || !strings.Contains(plan, "--output FILE") ||
		strings.Contains(plan, "--verdict") || !strings.Contains(plan, "Leave out --output") || !strings.Contains(plan, `"$ROUTR_BIN" done`) ||
		strings.Contains(plan, "routr takes an ended turn as finished") { // its turns end to talk to the user
		t.Errorf("plan prompt:\n%s", plan)
	}
	// A worktree has no .routr/: the worker reads it in the main checkout
	// and writes its reports to the chain's work dir.
	tree := Compose(registry.Worker{ID: 25, Role: "task", Chain: "issues-1", Cwd: "/r/.routr/trees/issues-1/fix-25",
		Checkout: &registry.Checkout{Repo: "/r", Path: "/r/.routr/trees/issues-1/fix-25", Made: true}}, p, "Fix it.", "/opt/routr/bin/routr")
	if want := " Your cwd is a git worktree of /r. Files git does not track, such as .routr/…, exist only in the main checkout:" +
		" read them from /r, and write reports to /r/.routr/work/issues-1, not into the worktree."; !strings.Contains(tree, want) {
		t.Errorf("worktree prompt lacks %q:\n%s", want, tree)
	}
}

// The completion protocol is the same in a revision's prompt and in
// on_fail's follow-up, which resume the session without a preamble.
func TestCodexFinishProtocolOnRevision(t *testing.T) {
	p := registry.Paths{Dir: "/r/.routr"}
	wk := registry.Worker{ID: 4, Role: "review", Harness: "codex", Round: 2, WantVerdict: true, Cwd: "/r", Output: "REVIEW.md"}
	for name, got := range map[string]string{"revised": Revised(wk, p, "Review PLAN.md.", "/opt/routr/bin/routr"), "on_fail": Finish(wk, p, "/opt/routr/bin/routr")} {
		for _, want := range []string{"routr takes an ended turn as finished", "run this command on its own, as your last command",
			"/opt/routr/bin/routr done --output /r/REVIEW.md --verdict pass|fail 'SUMMARY'", "If the command fails, you have not finished",
			"you need no routr skill for it"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, got)
			}
		}
		if strings.Count(got, " done --output ") != 1 || strings.Contains(got, "routr start") {
			t.Errorf("%s: one done command and no delegation workflow:\n%s", name, got)
		}
	}
	if r := Revised(wk, p, "Review PLAN.md.", "/x"); !strings.HasPrefix(r, "The work was revised; review it again (round 2)") {
		t.Errorf("revised:\n%s", r)
	}
	wk.WantVerdict, wk.Output = false, ""
	if got := Finish(wk, p, "/x"); strings.Contains(got, "--verdict") || !strings.Contains(got, "Leave out --output") {
		t.Errorf("no verdict asked:\n%s", got)
	}
}

func TestQuestionKeepsTheDriversNumbering(t *testing.T) {
	st := worker.Status{Question: "Trust this folder?\n1. Yes\n2. No", Options: []string{"Yes", "No"}}
	if got := question(st); got != st.Question {
		t.Errorf("got %q", got)
	}
	st = worker.Status{Question: "Allow?", Options: []string{"Yes", "No"}}
	if got := question(st); got != "Allow?\n1. Yes\n2. No" {
		t.Errorf("got %q", got)
	}
}

func TestFollowUpWithoutSessionNeedsYou(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "x"})
	r.s.Update(id, func(w *registry.Worker) { w.State, w.Inbox = registry.Closed, []string{"hello"} })
	r.tick(1)
	if w := r.state(id, registry.NeedsYou); !strings.Contains(w.Error, "no harness session") || strings.Contains(w.Error, "routr retry") || len(w.Inbox) != 0 {
		t.Errorf("%+v", w)
	}
}
