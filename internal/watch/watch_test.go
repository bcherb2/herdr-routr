package watch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/watch/watchtest"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// rig is a watcher over a temp repo, a fake driver and a fake clock.
type rig struct {
	t    *testing.T
	w    *Watcher
	fake *watchtest.Fake
	now  time.Time
	p    registry.Paths
	s    *registry.Store
	cwd  string
}

type testLog struct{ t *testing.T }

func (l testLog) Write(b []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, fake: &watchtest.Fake{}, now: time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC), cwd: dir}
	r.p = registry.Paths{Dir: filepath.Join(dir, ".routr")}
	if err := r.p.Init(); err != nil {
		t.Fatal(err)
	}
	r.w = r.watcher()
	r.s = &registry.Store{Path: r.p.Registry(), Now: func() time.Time { return r.now }}
	return r
}

func (r *rig) watcher() *Watcher {
	return &Watcher{
		Paths: r.p,
		Dial:  func(string) worker.Driver { return r.fake },
		Env:   func() (map[string]string, error) { return map[string]string{"CLAUDE_CONFIG_DIR": "/cfg"}, nil },
		Bin:   "/usr/local/bin/routr",
		Now:   func() time.Time { return r.now },
		Log:   log.New(testLog{r.t}, "watch: ", 0),
	}
}

// add queues a worker with test defaults.
func (r *rig) add(w registry.Worker) int {
	r.t.Helper()
	if w.Role == "" {
		w.Role = "task"
	}
	if w.Harness == "" {
		w.Harness = "claude"
	}
	if w.Cwd == "" {
		w.Cwd = r.cwd
	}
	w.Socket, w.State = "test.sock", registry.Pending
	got, err := r.s.Create(w)
	if err != nil {
		r.t.Fatal(err)
	}
	return got.ID
}

// tick advances the clock and runs n ticks.
func (r *rig) tick(n int) {
	r.t.Helper()
	for range n {
		r.now = r.now.Add(600 * time.Millisecond)
		if _, err := r.w.Tick(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
}

// settle runs enough ticks for any phase to settle and act.
func (r *rig) settle() { r.tick(4) }

func (r *rig) get(id int) registry.Worker {
	r.t.Helper()
	w, err := r.s.Get(id)
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) state(id int, want registry.State) registry.Worker {
	r.t.Helper()
	w := r.get(id)
	if w.State != want {
		r.t.Fatalf("worker %d is %s, want %s (note %q, error %q)", id, w.State, want, w.Note, w.Error)
	}
	return w
}

// done plays the worker running `routr done`, then ending its turn.
func (r *rig) done(id int, output string, v registry.Verdict, msg, reply string) {
	r.t.Helper()
	if output != "" {
		output = filepath.Join(r.cwd, output)
		os.WriteFile(output, []byte("result of "+msg), 0o644)
	}
	if err := registry.WriteDone(r.p.RunDir(id), registry.DoneRecord{Output: output, Verdict: v, Message: msg, At: r.now}); err != nil {
		r.t.Fatal(err)
	}
	r.fake.Finish(id, reply)
}

// final is the path of worker id's final message in its current run.
func (r *rig) final(id int) string { return r.p.FinalPath(r.get(id)) }

func (r *rig) read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// abs is path in the rig's repo, as a brief gives it.
func (r *rig) abs(path string) string { return filepath.Join(r.cwd, path) }

func TestSingleWorker(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Role: "task", Preamble: "You are a worker.", Brief: "Fix {{task}}.", Params: map[string]string{"task": "calc.py"},
		Output: "NOTES.md", Model: "sonnet", Effort: "low", Permissions: "auto", Workspace: "w9", Args: []string{"--add-dir", "/shared"}})
	r.tick(1)
	w := r.state(id, registry.Running)
	if w.Live == nil || w.Session != "sess-1" || w.Started.IsZero() || w.Transcript == "" {
		t.Fatalf("after start: %+v", w)
	}
	panes := r.fake.Panes(id)
	spec := panes[0].Spec
	if spec.Label != "1 task · claude" || spec.Model != "sonnet" || spec.Effort != "low" || spec.Permissions != "auto" || spec.Workspace != "w9" || spec.Cwd != r.cwd ||
		strings.Join(spec.Args, " ") != "--add-dir /shared" {
		t.Errorf("spec = %+v", spec)
	}
	for k, v := range map[string]string{"ROUTR_ID": "1", "ROUTR_STATE_DIR": r.p.Dir, "ROUTR_RUN_DIR": r.p.RunDir(1), "ROUTR_BIN": "/usr/local/bin/routr", "CLAUDE_CONFIG_DIR": "/cfg"} {
		if spec.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, spec.Env[k], v)
		}
	}
	prompt := panes[0].Prompts[0]
	for _, want := range []string{"You are a worker.", "Fix calc.py.", "/usr/local/bin/routr done --output " + r.abs("NOTES.md") + " 'SUMMARY'", "worker 1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if r.read(filepath.Join(r.p.RunDir(id), "brief.md")) != prompt {
		t.Error("brief.md is not the prompt as sent")
	}

	r.settle() // working: nothing changes
	r.state(id, registry.Running)
	// The driver looks for the transcript with the tab's environment.
	if env := r.fake.Panes(id)[0].Env; env["CLAUDE_CONFIG_DIR"] != "/cfg" {
		t.Errorf("Poll got env %v, want config [env]", env)
	}
	r.done(id, "NOTES.md", "", "fixed the sign bug", "I changed a - b to a + b.")
	r.tick(1) // not settled yet
	r.state(id, registry.Running)
	r.settle()
	w = r.state(id, registry.Done)
	if w.Summary != "fixed the sign bug" || w.Output != filepath.Join(r.cwd, "NOTES.md") || w.Finished.IsZero() || w.Live != nil {
		t.Errorf("done: %+v", w)
	}
	if got := r.read(r.final(id)); got != "fixed the sign bug\n\nI changed a - b to a + b.\n" {
		t.Errorf("final.md = %q", got)
	}
	if r.fake.Open(id) != 0 {
		t.Error("the tab was not closed on done")
	}
	if registry.HasDone(r.p.RunDir(id)) || r.read(filepath.Join(r.p.RunDir(id), "done.1.json")) == "" {
		t.Error("done.json was not taken aside")
	}
	if busy, _ := r.w.Tick(context.Background()); busy {
		t.Error("busy with nothing to do")
	}
}

func TestChainWithAfter(t *testing.T) {
	r := newRig(t)
	plan := r.add(registry.Worker{Role: "plan", Chain: "c1", Step: "plan", Brief: "Plan it.", Output: "PLAN.md"})
	review := r.add(registry.Worker{Role: "review", Chain: "c1", Step: "review", After: []int{plan}, WantVerdict: true,
		Brief: "Red-team {{prev.output}} (same as {{plan.output}}); plan said {{plan.final}}."})
	r.tick(1)
	r.state(plan, registry.Running)
	w := r.state(review, registry.Waiting)
	if !strings.Contains(w.Note, "waiting on 1 plan") {
		t.Errorf("note = %q", w.Note)
	}
	r.done(plan, "PLAN.md", "", "plan written", "Done.")
	r.settle()
	r.state(plan, registry.Done)
	r.tick(1)
	r.state(review, registry.Running)
	prompt := r.fake.Prompts(review)[0]
	want := fmt.Sprintf("Red-team %s (same as %[1]s); plan said %s.", r.abs("PLAN.md"), r.final(plan))
	if !strings.Contains(prompt, want) || !strings.Contains(prompt, "--verdict pass|fail") {
		t.Errorf("review prompt:\n%s", prompt)
	}
}

// A flow's steps with no dependency between them run side by side, and a
// step after both waits for the slower one.
func TestParallelStepsThenJoin(t *testing.T) {
	r := newRig(t)
	set, err := flow.Parse([]byte(`
[flow.diamond]
params = ["task"]
[[flow.diamond.step]]
id = "plan"
output = "PLAN.md"
brief = "Plan {{task}}."
[[flow.diamond.step]]
id = "a"
role = "review"
after = ["plan"]
output = "A.md"
brief = "Review {{plan.output}} for bugs."
[[flow.diamond.step]]
id = "b"
role = "review"
after = ["plan"]
output = "B.md"
brief = "Review {{plan.output}} for risks."
[[flow.diamond.step]]
id = "join"
role = "summarize"
after = ["a", "b"]
brief = "Merge {{a.output}} and {{b.output}}."
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["diamond"].Start(r.s, "d1", map[string]string{"task": "x"}, r.cwd, r.p.WorkDir("d1"))
	if err != nil {
		t.Fatal(err)
	}
	plan, a, b, join := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID
	r.tick(1)
	r.state(plan, registry.Running)
	r.state(a, registry.Waiting)
	r.done(plan, "PLAN.md", "", "planned", "")
	r.settle()
	r.tick(1)
	r.state(a, registry.Running)
	r.state(b, registry.Running)
	r.state(join, registry.Waiting)
	r.done(b, "B.md", "", "b", "")
	r.settle()
	r.state(b, registry.Done)
	if w := r.state(join, registry.Waiting); !strings.Contains(w.Note, "a") {
		t.Errorf("join note = %q", w.Note)
	}
	r.done(a, "A.md", "", "a", "")
	r.settle()
	r.tick(1)
	r.state(join, registry.Running)
	if p := r.fake.Prompts(join)[0]; !strings.Contains(p, "Merge "+r.abs("A.md")+" and "+r.abs("B.md")+".") {
		t.Errorf("join prompt:\n%s", p)
	}
}

func TestBadRefFailsAtStart(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "Review {{prev.output}}"})
	r.tick(1)
	if w := r.state(id, registry.Failed); !strings.Contains(w.Error, "{{prev.output}}") {
		t.Errorf("error = %q", w.Error)
	}
}

func TestDirectoryLock(t *testing.T) {
	r := newRig(t)
	a := r.add(registry.Worker{Write: true, Brief: "a"})
	b := r.add(registry.Worker{Write: true, Brief: "b"})
	ro := r.add(registry.Worker{Role: "review", Brief: "read only"})
	other := r.add(registry.Worker{Write: true, Brief: "other tree", Tree: "/elsewhere"})
	r.tick(1)
	r.state(a, registry.Running)
	r.state(ro, registry.Running)
	r.state(other, registry.Running)
	if w := r.state(b, registry.Waiting); !strings.Contains(w.Note, "directory lock") || !strings.Contains(w.Note, "worker 1") {
		t.Errorf("note = %q", w.Note)
	}
	r.done(a, "", "", "a done", "")
	r.settle()
	r.state(a, registry.Done)
	r.tick(1)
	r.state(b, registry.Running)
	if m, _ := r.s.Holders(); m[r.cwd] != b || m["/elsewhere"] != other {
		t.Errorf("holders = %v", m)
	}
	// A CLI cancel frees the lock through the watcher's repair.
	r.s.Update(b, func(w *registry.Worker) { w.State, w.Live = registry.Cancelled, nil })
	r.tick(1)
	if m, _ := r.s.Holders(); m[r.cwd] != 0 {
		t.Errorf("cancelled holder kept the lock: %v", m)
	}
}

func TestGateAndApprove(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Role: "implement", Write: true, Gate: "you", Brief: "Implement."})
	r.settle()
	if w := r.state(id, registry.Gated); !strings.Contains(w.Note, "routr approve 1") {
		t.Errorf("note = %q", w.Note)
	}
	if busy, _ := r.w.Tick(context.Background()); busy {
		t.Error("a gate needs you; the watcher is not busy")
	}
	r.s.Update(id, func(w *registry.Worker) { w.GateOpen = true }) // routr approve
	r.tick(1)
	r.state(id, registry.Running)
}

// reviewLoop is plan → review with on_fail back to plan, max rounds.
func reviewLoop(r *rig, max int) (plan, review int) {
	plan = r.add(registry.Worker{Role: "plan", Chain: "c", Step: "plan", Write: true, Brief: "Plan.", Output: "PLAN.md"})
	review = r.add(registry.Worker{Role: "review", Chain: "c", Step: "review", After: []int{plan}, WantVerdict: true, Output: "REDTEAM.md",
		Brief: "Red-team {{plan.output}}.", OnFail: &registry.OnFail{Resume: plan, Brief: "Address {{review.output}} ({{review.verdict}}).", Max: max}})
	r.tick(1)
	r.done(plan, "PLAN.md", "", "plan v1", "")
	r.settle()
	r.tick(1)
	r.state(review, registry.Running)
	return plan, review
}

func TestReviewLoopFailThenPass(t *testing.T) {
	r := newRig(t)
	plan, review := reviewLoop(r, 3)
	r.done(review, "REDTEAM.md", registry.Fail, "2 blockers", "See REDTEAM.md.")
	r.settle()
	w := r.state(review, registry.Waiting)
	if w.Round != 2 || len(w.After) != 1 || w.After[0] != plan || r.fake.Open(review) != 0 {
		t.Errorf("review after fail: %+v", w)
	}
	p := r.get(plan)
	if p.Turn != 1 || len(p.Inbox) != 0 || p.State != registry.Running || p.Finished.IsZero() == false || !slices.Equal(p.Revisions, []int{1}) {
		t.Errorf("plan after the loop sent it back: %+v", p)
	}
	panes := r.fake.Panes(plan)
	if len(panes) != 2 || panes[1].Spec.Session != "sess-1" {
		t.Fatalf("plan was not resumed in a new tab: %+v", panes)
	}
	// It must be told to run routr done again, or a revision that leaves
	// the output as it was ends maybe_done and the chain stalls.
	if got := panes[1].Prompts[0]; !strings.HasPrefix(got, "Address "+r.abs("REDTEAM.md")+" (fail).\n\n---\nYou are routr worker 1") ||
		!strings.Contains(got, "/usr/local/bin/routr done --output "+r.abs("PLAN.md")) {
		t.Errorf("on_fail brief = %q", got)
	}
	if m, _ := r.s.Holders(); m[r.cwd] != plan {
		t.Errorf("the revising plan worker must hold the lock: %v", m)
	}

	r.done(plan, "PLAN.md", "", "plan v2", "Revised.")
	r.settle()
	r.state(plan, registry.Done)
	if r.read(r.p.TurnFile(plan, 1)) != "Revised." {
		t.Errorf("turn 1 reply = %q", r.read(r.p.TurnFile(plan, 1)))
	}
	r.tick(1)
	r.state(review, registry.Running)
	rp := r.fake.Panes(review)
	if len(rp) != 2 || rp[1].Spec.Session != "sess-2" || !strings.HasPrefix(rp[1].Prompts[0], "The work was revised; review it again (round 2)") {
		t.Fatalf("review rerun: %+v", rp)
	}
	r.done(review, "REDTEAM.md", registry.Pass, "looks good", "")
	r.settle()
	if w := r.state(review, registry.Done); w.Verdict != registry.Pass {
		t.Errorf("verdict = %s", w.Verdict)
	}
}

func TestReviewLoopHitsItsCap(t *testing.T) {
	r := newRig(t)
	plan, review := reviewLoop(r, 2)
	r.done(review, "REDTEAM.md", registry.Fail, "round 1 fail", "")
	r.settle()
	r.done(plan, "PLAN.md", "", "plan v2", "")
	r.settle()
	r.tick(1)
	r.state(review, registry.Running)
	r.done(review, "REDTEAM.md", registry.Fail, "round 2 fail", "")
	r.settle()
	w := r.state(review, registry.NeedsYou)
	if w.Round != 2 || !strings.Contains(w.Note, "cap of 2 rounds") {
		t.Errorf("at the cap: %+v", w)
	}
	if p := r.get(plan); p.State != registry.Done || len(p.Inbox) != 0 {
		t.Errorf("plan must not be sent back past the cap: %+v", p)
	}
}

func TestBlockedThenAnswered(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	// Its transcript has the call the question is about, with no result yet.
	session := r.abs("session.jsonl")
	os.WriteFile(session, []byte(`{"type":"user","message":{"content":"go"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"rm -rf build"}}]}}
`), 0o600)
	r.s.Update(id, func(x *registry.Worker) { x.Transcript = session })
	r.fake.Ask(id, "Allow rm -rf build?", "Yes", "No")
	r.tick(1)
	r.state(id, registry.Running) // not settled yet
	r.settle()
	w := r.state(id, registry.Blocked)
	if w.Question != "Allow rm -rf build?\n1. Yes\n2. No" {
		t.Errorf("question = %q", w.Question)
	}
	// The menu as data, for routr answer --expect.
	pane := r.fake.Panes(id)[0]
	if !slices.Equal(w.Options, []string{"Yes", "No"}) || w.Fingerprint == "" || w.Fingerprint != pane.Fingerprint || w.BlockSeq != pane.Seq {
		t.Errorf("menu = %q %q seq %d; pane %q seq %d", w.Options, w.Fingerprint, w.BlockSeq, pane.Fingerprint, pane.Seq)
	}
	if p := w.Pending; p == nil || *p != (registry.ToolCall{Tool: "Bash", Summary: "Bash: rm -rf build", Input: `{"command":"rm -rf build"}`}) {
		t.Errorf("pending = %+v", p)
	}
	r.fake.Answer(context.Background(), Handle(w), 2, w.Fingerprint, false) // routr answer 1 2 --expect …
	r.settle()
	if w := r.state(id, registry.Running); w.Question != "" || w.Options != nil || w.Fingerprint != "" || w.BlockSeq != 0 || w.Pending != nil {
		t.Errorf("question kept: %+v", w)
	}
}

// After `routr answer`, herdr reports the old block for a moment; the
// watcher must not block the worker again at once, on a screen with no
// question, and have `routr wait` return exit 4. A second prompt right after
// keeps herdr's sequence number and must still be seen.
func TestAnsweredQuestionIsNotAskedAgain(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.Ask(id, "Create PLAN.md?", "Yes", "No")
	r.settle()
	r.state(id, registry.Blocked)
	answer := func() { // what routr answer does to the registry; the fake still shows the question
		r.s.Update(id, func(x *registry.Worker) { x.State, x.Question = registry.Running, "" })
	}
	answer()
	r.tick(1)
	r.state(id, registry.Running)
	r.fake.SetPhase(id, worker.Working)
	r.settle()
	r.state(id, registry.Running)

	r.fake.Ask(id, "Run routr done?", "Yes", "No")
	r.settle()
	r.state(id, registry.Blocked)
	answer()
	r.fake.Reask(id, "Run ls?", "Yes", "No")
	r.tick(1)
	r.state(id, registry.Running)
	r.settle()
	if w := r.state(id, registry.Blocked); !strings.HasPrefix(w.Question, "Run ls?") {
		t.Errorf("question = %q", w.Question)
	}
}

func TestPaneGoneThenSendResumes(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.Kill(id)
	r.settle()
	w := r.state(id, registry.Closed)
	if w.Live != nil || w.Session != "sess-1" {
		t.Fatalf("closed: %+v", w)
	}
	// routr send
	r.s.Update(id, func(w *registry.Worker) { w.Inbox = append(w.Inbox, "why?") })
	r.tick(1)
	w = r.state(id, registry.Running)
	panes := r.fake.Panes(id)
	if len(panes) != 2 || panes[1].Spec.Session != "sess-1" || panes[1].Prompts[0] != "why?" || w.Turn != 1 {
		t.Fatalf("resume: %+v / %+v", w, panes)
	}
	r.fake.Finish(id, "because.")
	r.settle()
	if got := r.read(r.p.TurnFile(id, 1)); got != "because." {
		t.Errorf("turns/1.md = %q", got)
	}
	// It never ran routr done, so after the reply it is maybe_done.
	r.state(id, registry.MaybeDone)
}

// A done worker that answers its follow-ups is done again, and its tab
// closes once no follow-up waits to go into it, so tabs reopened by routr
// send don't stay until routr close and pile up.
func TestFollowUpToADoneWorkerKeepsItDoneAndClosesItsTab(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.done(id, "", registry.Fail, "found 2 bugs", "")
	r.settle()
	r.state(id, registry.Done)
	r.s.Update(id, func(w *registry.Worker) { w.Inbox = []string{"is bug 2 real?", "and bug 1?"} })
	r.tick(1)
	if w := r.state(id, registry.Running); w.Rest != registry.Done || len(w.Inbox) != 1 {
		t.Fatalf("delivering: %+v", w)
	}
	r.fake.Finish(id, "yes, line 4.")
	r.settle()
	if w := r.get(id); w.Live == nil || r.fake.Open(id) != 1 {
		t.Fatalf("the tab closed with a follow-up still to go into it: %+v", w.Live)
	}
	r.tick(1) // the second follow-up goes once the first is answered
	r.fake.Finish(id, "also real.")
	r.settle()
	w := r.state(id, registry.Done)
	if w.Turn != 2 || w.Replies != 2 || w.Live != nil || r.fake.Open(id) != 0 || w.Verdict != registry.Fail {
		t.Errorf("after two follow-ups: %+v", w)
	}
	if r.read(r.p.TurnFile(id, 2)) != "also real." {
		t.Error("turns/2.md")
	}
}

func TestIdleWithoutDone(t *testing.T) {
	r := newRig(t)
	quiet := r.add(registry.Worker{Brief: "go"})
	wrote := r.add(registry.Worker{Brief: "go", Output: "OUT.md"})
	talk := r.add(registry.Worker{Brief: "go", Interactive: true})
	r.tick(1)
	r.fake.Finish(quiet, "I think I'm done")
	r.fake.Finish(wrote, "wrote it")
	r.fake.Finish(talk, "what do you think?")
	out := filepath.Join(r.cwd, "OUT.md")
	os.WriteFile(out, []byte("x"), 0o644)
	os.Chtimes(out, r.now.Add(time.Second), r.now.Add(time.Second))
	r.settle()
	if w := r.state(quiet, registry.MaybeDone); !strings.Contains(w.Note, "routr done") || r.read(r.final(quiet)) != "I think I'm done" {
		t.Errorf("maybe_done: %+v", w)
	}
	if w := r.state(wrote, registry.Done); !strings.Contains(w.Note, "did not run routr done") {
		t.Errorf("output-changed done: %+v", w)
	}
	r.state(talk, registry.Running)
	// Activity takes maybe_done back to running.
	r.fake.SetPhase(quiet, worker.Working)
	r.settle()
	r.state(quiet, registry.Running)
}

func TestHarnessExited(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Write: true, Brief: "go"})
	r.tick(1)
	r.fake.SetPhase(id, worker.Exited)
	r.settle()
	if w := r.state(id, registry.Failed); !strings.Contains(w.Error, "exited") || strings.Contains(w.Error, "routr send") {
		t.Errorf("error = %q", w.Error)
	}
	if m, _ := r.s.Holders(); len(m) != 0 {
		t.Errorf("lock kept: %v", m)
	}
}

func TestStartFailure(t *testing.T) {
	r := newRig(t)
	r.fake.StartErr = errors.New("codex not logged in")
	id := r.add(registry.Worker{Harness: "codex", Brief: "go"})
	r.tick(1)
	if w := r.state(id, registry.Failed); !strings.Contains(w.Error, "could not start codex: codex not logged in") {
		t.Errorf("error = %q", w.Error)
	}
}

func TestRestartMidRunPicksUp(t *testing.T) {
	r := newRig(t)
	a := r.add(registry.Worker{Chain: "c", Step: "a", Brief: "a", Output: "A.md"})
	b := r.add(registry.Worker{Chain: "c", Step: "b", After: []int{a}, Brief: "b after {{a.output}}"})
	r.tick(1)
	r.state(a, registry.Running)
	r.w = r.watcher() // the watcher died; a new one starts from the registry
	r.done(a, "A.md", "", "a done", "")
	r.settle()
	r.state(a, registry.Done)
	r.tick(1)
	r.state(b, registry.Running)
	if !strings.Contains(r.fake.Prompts(b)[0], "b after "+r.abs("A.md")) {
		t.Error(r.fake.Prompts(b)[0])
	}
}

// A tab kept to look at (an API error that needs you) stays while the chain
// runs, and closes once every worker in it has ended.
func TestEndedChainClosesKeptTabs(t *testing.T) {
	r := newRig(t)
	a := r.add(registry.Worker{Chain: "c", Brief: "a"})
	b := r.add(registry.Worker{Chain: "c", After: []int{a}, Brief: "b"})
	r.tick(1)
	r.fake.Fail(a, "Invalid API key · Please run /login")
	r.settle()
	if w := r.state(a, registry.NeedsYou); w.Live == nil {
		t.Fatal("an API error that needs you keeps the tab while the chain runs")
	}
	r.s.Update(b, func(w *registry.Worker) { w.State = registry.Cancelled })
	r.settle()
	if w := r.get(a); w.Live != nil || r.fake.Open(a) != 0 {
		t.Errorf("chain ended but a's tab is open: %+v", w.Live)
	}
}

// A follow-up to a worker of a chain that is still running closes its tab
// once answered, as for any finished worker: the chain does not keep it.
func TestFollowUpInARunningChainClosesItsTab(t *testing.T) {
	r := newRig(t)
	a := r.add(registry.Worker{Chain: "c", Brief: "a"})
	r.add(registry.Worker{Chain: "c", After: []int{a}, Brief: "b"})
	r.tick(1)
	r.done(a, "", "", "a", "")
	r.settle()
	r.s.Update(a, func(w *registry.Worker) { w.Inbox = []string{"more?"} })
	r.tick(2)
	r.fake.Finish(a, "sure")
	r.settle()
	if w := r.state(a, registry.Done); w.Live != nil || r.fake.Open(a) != 0 || r.read(r.p.TurnFile(a, 1)) != "sure" {
		t.Errorf("after the reply: %+v", w.Live)
	}
}

func TestCancelledWhileStartingClosesTheTab(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.w.Dial = func(string) worker.Driver {
		return cancelOnStart{Fake: r.fake, cancel: func() {
			r.s.Update(id, func(w *registry.Worker) { w.State = registry.Cancelled })
		}}
	}
	r.tick(1)
	r.state(id, registry.Cancelled)
	if r.fake.Open(id) != 0 {
		t.Error("the tab of a worker cancelled mid-start stayed open")
	}
}

type cancelOnStart struct {
	*watchtest.Fake
	cancel func()
}

func (c cancelOnStart) Prompt(ctx context.Context, h worker.Handle, text string) error {
	c.cancel()
	return c.Fake.Prompt(ctx, h, text)
}

func TestObserveDistrustsIdleFromBeforeThePrompt(t *testing.T) {
	r := newRig(t)
	w := r.w
	w.init()
	st := worker.Status{Phase: worker.Idle, Seq: 5}
	w.observe(1, st)
	r.now = r.now.Add(2 * time.Second)
	if _, ok := w.observe(1, st); !ok {
		t.Fatal("a held idle settles")
	}
	w.prompted(1)
	r.now = r.now.Add(2 * time.Second)
	if _, ok := w.observe(1, st); ok {
		t.Fatal("the idle from before the prompt was taken for the end of the turn")
	}
	st.Seq = 7 // worked and went idle between two polls
	w.observe(1, st)
	r.now = r.now.Add(1100 * time.Millisecond)
	if ph, ok := w.observe(1, st); !ok || ph != worker.Idle {
		t.Fatal("a new idle settles")
	}
	// Flapping resets the settle time.
	w.observe(1, worker.Status{Phase: worker.Working, Seq: 8})
	r.now = r.now.Add(500 * time.Millisecond)
	w.observe(1, worker.Status{Phase: worker.Idle, Seq: 9})
	r.now = r.now.Add(500 * time.Millisecond)
	if _, ok := w.observe(1, worker.Status{Phase: worker.Idle, Seq: 9}); ok {
		t.Fatal("settled before holding for Settle")
	}
}

// `routr start` run by Codex inside its sandbox spawns the watcher there,
// where it cannot connect to herdr. A worker is
// never failed for that: it waits in the queue, and a watcher that the
// sandbox denies the socket exits so one started outside it can take over.
func TestHerdrOutOfReachRequeues(t *testing.T) {
	dial := func(errno syscall.Errno) error {
		return fmt.Errorf("worker: open tab: herdr tab.create: dial /s: %w",
			&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", errno)})
	}
	r := newRig(t)
	r.fake.StartErr = dial(syscall.ECONNREFUSED) // herdr is down
	id := r.add(registry.Worker{Brief: "go", Write: true})
	r.tick(1)
	if w := r.state(id, registry.Pending); !strings.Contains(w.Note, "waiting for herdr") {
		t.Errorf("note = %q", w.Note)
	}
	if m, _ := r.s.Holders(); len(m) != 0 {
		t.Errorf("a requeued worker must not hold the lock: %v", m)
	}
	if r.w.deniedErr() != nil {
		t.Error("a server that is down is not a sandbox")
	}
	r.tick(1) // herdr is back
	r.state(id, registry.Running)

	r = newRig(t)
	r.fake.StartErr = dial(syscall.EPERM) // Codex's sandbox
	id = r.add(registry.Worker{Brief: "go"})
	r.w.Sleep = func(context.Context, time.Duration) error { return errors.New("did not exit") }
	if err := r.w.Run(context.Background()); !errors.Is(err, ErrDenied) {
		t.Fatalf("Run = %v, want ErrDenied", err)
	}
	r.state(id, registry.Pending)
	r.w = r.watcher() // a watcher started outside the sandbox
	r.tick(1)
	r.state(id, registry.Running)

	// A sandboxed watcher with only running workers to read gives up too,
	// rather than hold the watcher lock while it can do nothing.
	r.w = r.watcher()
	r.w.Sleep = func(context.Context, time.Duration) error { return errors.New("did not exit") }
	r.fake.PollErr = dial(syscall.EACCES)
	if err := r.w.Run(context.Background()); !errors.Is(err, ErrDenied) {
		t.Fatalf("Run = %v, want ErrDenied", err)
	}
	r.state(id, registry.Running)
}

func TestRunExitsWhenIdle(t *testing.T) {
	r := newRig(t)
	r.w.IdleExit = 5 * time.Second
	r.w.Build = "2a7d3e18d7de+dirty"
	l := r.logged()
	ticks := 0
	r.w.Sleep = func(ctx context.Context, d time.Duration) error {
		ticks++
		r.now = r.now.Add(d)
		if ticks > 1000 {
			return errors.New("did not exit")
		}
		return nil
	}
	if err := r.w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ticks != 10 {
		t.Errorf("exited after %d ticks of 500ms, want 10", ticks)
	}
	if want := "watching " + r.p.Dir + " (routr 2a7d3e18d7de+dirty)\n"; !strings.HasPrefix(l.String(), want) {
		t.Errorf("the log starts %q, want %q", l.String(), want)
	}
}

func TestHoldIsASingleton(t *testing.T) {
	p := registry.Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
	if Running(p) {
		t.Fatal("running before anyone held it")
	}
	release, err := Hold(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Hold(p); !errors.Is(err, ErrRunning) {
		t.Errorf("second hold: %v", err)
	}
	if !Running(p) {
		t.Error("not running while held")
	}
	release()
	if Running(p) {
		t.Error("running after release")
	}
}

// A reply the watcher can't read yet (its transcript not written or found)
// holds the worker instead of settling it with an empty reply; after
// unreadWait it settles with a note saying why.
func TestUnreadableReplyHolds(t *testing.T) {
	r := newRig(t)
	quiet := r.add(registry.Worker{Brief: "go"})
	asked := r.add(registry.Worker{Brief: "go"})
	talk := r.add(registry.Worker{Brief: "go", Interactive: true})
	r.tick(1)
	r.done(asked, "", "", "found it", "")
	r.settle()
	r.state(asked, registry.Done)
	r.s.Update(asked, func(w *registry.Worker) { w.Inbox = []string{"why?"} })
	r.tick(1)
	r.fake.ReadErr = errors.New("no transcript yet")
	r.fake.Finish(quiet, "done, I think")
	r.fake.Finish(asked, "because")
	r.fake.Finish(talk, "what now?")
	r.settle()
	r.state(quiet, registry.Running)
	if w := r.state(asked, registry.Running); w.Replies != 0 {
		t.Fatalf("a follow-up was settled without its reply: %+v", w)
	}
	r.state(talk, registry.Running) // needs no reply, so nothing to hold
	r.fake.ReadErr = nil
	r.settle()
	if w := r.state(quiet, registry.MaybeDone); r.read(r.final(quiet)) != "done, I think" {
		t.Errorf("once readable: %+v, final.md %q", w, r.read(r.final(quiet)))
	}
	if w := r.state(asked, registry.Done); w.Replies != 1 || r.read(r.p.TurnFile(asked, 1)) != "because" {
		t.Errorf("once readable: %+v", w)
	}

	// A reply that stays unreadable settles after unreadWait, saying so.
	r.fake.SetPhase(quiet, worker.Working)
	r.settle()
	r.fake.ReadErr = errors.New("open s.jsonl: no such file")
	r.fake.Finish(quiet, "")
	r.settle()
	r.state(quiet, registry.Running)
	r.now = r.now.Add(unreadWait)
	r.settle()
	r.state(quiet, registry.MaybeDone)
	if got := r.read(r.final(quiet)); !strings.Contains(got, "could not read this reply: open s.jsonl: no such file") || !strings.Contains(got, "routr show 1") {
		t.Errorf("final.md = %q", got)
	}
}

// The same hold when the worker ran `routr done` during a follow-up: its
// reply goes to the turn file, so done.json waits until it can be read.
func TestUnreadableReplyHoldsTheFinish(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.done(id, "", "", "first", "")
	r.settle()
	r.state(id, registry.Done)
	r.s.Update(id, func(w *registry.Worker) { w.Inbox = []string{"and then?"} })
	r.tick(1)
	r.fake.ReadErr = errors.New("no transcript yet")
	r.done(id, "", "", "all done", "then this")
	r.settle()
	if w := r.state(id, registry.Running); w.Replies != 0 || !registry.HasDone(r.p.RunDir(id)) {
		t.Fatalf("finished without its reply: %+v", w)
	}
	r.fake.ReadErr = nil
	r.settle()
	if w := r.state(id, registry.Done); w.Replies != 1 || r.read(r.p.TurnFile(id, 1)) != "then this" {
		t.Errorf("once readable: %+v, turn file %q", w, r.read(r.p.TurnFile(id, 1)))
	}
}

// A turn settled without its reply clears the API-error retry state, so a
// later error gets its retries afresh.
func TestGivingUpOnAReplyClearsRetries(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.Fail(id, "Overloaded")
	r.settle()
	r.now = r.now.Add(retryDelay)
	r.settle() // retried
	r.fake.ReadErr = errors.New("gone")
	r.fake.Finish(id, "")
	r.settle()
	r.now = r.now.Add(unreadWait)
	r.settle()
	if w := r.state(id, registry.MaybeDone); w.TurnError != "" || w.Retries != 0 || !w.Retry.IsZero() {
		t.Errorf("retry state kept: %+v", w)
	}
}
