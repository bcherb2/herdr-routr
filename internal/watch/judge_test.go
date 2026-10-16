package watch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/bcherb2/herdr-routr/internal/jev"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// fakeJev answers judges as told, recording each call.
type fakeJev struct {
	mu     sync.Mutex
	calls  []jevCall
	answer func(q map[string]jev.Choice) (jev.Decision, error)
	block  chan struct{} // set: Decide waits for it, or for its context
}

type jevCall struct {
	state    JudgeState
	question QuestionState // a worker's question's state (answer = "jev")
	q        map[string]jev.Choice
}

func (f *fakeJev) Decide(ctx context.Context, state any, q map[string]jev.Choice) (jev.Decision, error) {
	b, _ := json.Marshal(state)
	var st JudgeState
	var qs QuestionState
	json.Unmarshal(b, &st)
	json.Unmarshal(b, &qs)
	f.mu.Lock()
	f.calls = append(f.calls, jevCall{st, qs, q})
	answer, block := f.answer, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return jev.Decision{}, ctx.Err()
		}
	}
	return answer(q)
}

func (f *fakeJev) Calls() []jevCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]jevCall(nil), f.calls...)
}

// choose answers every question with choice at probability p (the rest
// shared by the other options) and confidence p.
func choose(choice string, p float64) func(map[string]jev.Choice) (jev.Decision, error) {
	return func(q map[string]jev.Choice) (jev.Decision, error) {
		d := jev.Decision{ID: "gen-dec-1", Model: "typesafe/jev-1.13-test", Answers: map[string]jev.Answer{}}
		d.Usage.InputTokens, d.Usage.Cost = 530, 0.0000223
		for id, c := range q {
			probs := map[string]float64{}
			for k := range c.Options {
				probs[k] = (1 - p) / float64(len(c.Options)-1)
			}
			probs[choice] = p
			d.Answers[id] = jev.Answer{Choice: choice, Probabilities: probs, Confidence: p}
		}
		return d, nil
	}
}

func (r *rig) jev(answer func(map[string]jev.Choice) (jev.Decision, error)) *fakeJev {
	f := &fakeJev{answer: answer}
	r.w.Jev = f
	r.t.Cleanup(func() { r.reap(r.w) })
	return f
}

// approve plays routr approve on a judge that needs you.
func (r *rig) approve(id int) {
	r.t.Helper()
	if _, err := r.s.Update(id, func(x *registry.Worker) { x.State, x.Judge.Accept = registry.Pending, true }); err != nil {
		r.t.Fatal(err)
	}
}

const panel = `
[flow.panel]
params = ["task"]
[[flow.panel.step]]
id = "implement"
brief = "Do {{task}}."
[[flow.panel.step]]
id = "sec"
role = "review"
after = ["implement"]
output = "SEC.md"
brief = "Review it for security."
[[flow.panel.step]]
id = "perf"
role = "review"
after = ["implement"]
output = "PERF.md"
brief = "Review it for speed."
[[flow.panel.step]]
id = "ship"
after = ["sec", "perf"]
judge = "jev"
brief = "Can {{task}} ship, given {{sec.output}} and {{perf.output}}?"
pass = "ship as is"
fail = "at least one confirmed finding must be fixed first"
read = ["NOTES.md"]
on_fail = { resume = "implement", brief = "Fix what {{sec.output}} and {{perf.output}} found.", max = 2 }
[[flow.panel.step]]
id = "after"
after = ["ship"]
brief = "Release; Jev said {{ship.verdict}}."
`

// panelDone runs the panel flow up to its judge: implement, then both
// reviews done. sec's fail has no on_fail, so it halts the judge until
// you approve past it (routr approve 2); the judge then sees it.
func panelDone(r *rig) (impl, sec, perf, ship, after int) {
	r.t.Helper()
	os.WriteFile(filepath.Join(r.cwd, "NOTES.md"), []byte("ship on Fridays"), 0o644)
	ws := r.startFlow(panel, "panel", map[string]string{"task": "login"})
	impl, sec, perf, ship, after = ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID, ws[4].ID
	r.tick(1)
	r.done(impl, "", "", "implemented", "Done.")
	r.settle()
	r.tick(1)
	r.done(sec, "SEC.md", registry.Fail, "one injection", "See SEC.md.")
	r.done(perf, "PERF.md", registry.Pass, "fast enough", "")
	r.settle()
	r.tick(2)
	if w := r.state(ship, registry.Waiting); !strings.Contains(w.Note, "routr approve 2 lets the chain go on past it") {
		r.t.Fatalf("ship before the approve: %q", w.Note)
	}
	r.s.Update(sec, func(x *registry.Worker) { x.GoOn = true })
	return
}

func TestJudgeVerdictPasses(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("pass", 0.9))
	_, sec, perf, ship, after := panelDone(r)
	w := r.await(ship, registry.Done)
	if w.Verdict != registry.Pass || w.Summary != "Jev: pass (p 0.90, confidence 0.90)" || w.Live != nil || w.Write || w.Label() != "4 ship · jev" {
		t.Errorf("ship = %+v", w)
	}
	if len(r.fake.Panes(ship)) != 0 {
		t.Error("a judge opened a tab")
	}
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	q := calls[0].q["verdict"]
	if q.Instructions != "Can login ship, given SEC.md and PERF.md?" || q.Options["pass"] != "ship as is" ||
		q.Options["fail"] != "at least one confirmed finding must be fixed first" || len(calls[0].q) != 1 {
		t.Errorf("question = %+v", calls[0].q)
	}
	st := calls[0].state
	if st.Judge != (JudgeInfo{Chain: "c1", Step: "ship", Mode: "verdict", Round: 1}) || len(st.Upstream) != 2 {
		t.Fatalf("state = %+v", st)
	}
	s, p := st.Upstream[0], st.Upstream[1]
	if s.ID != "sec-2" || s.Worker != sec || s.Role != "review" || s.Harness != "" || s.Verdict != "fail" || s.Summary != "one injection" ||
		s.OutputPath != "SEC.md" || s.Output != "result of one injection" || s.Final != "one injection\n\nSee SEC.md.\n" {
		t.Errorf("sec = %+v", s)
	}
	if p.Worker != perf || p.Verdict != "pass" || p.Output != "result of fast enough" {
		t.Errorf("perf = %+v", p)
	}
	if len(st.Files) != 1 || st.Files[0] != (File{Path: "NOTES.md", Content: "ship on Fridays"}) {
		t.Errorf("files = %+v", st.Files)
	}

	dir := r.p.RunDir(ship)
	final := r.read(r.final(ship))
	for _, want := range []string{"Jev: pass (p 0.90, confidence 0.90)\n", "  pass         0.90  ship as is\n", "  fail         0.10  at least one",
		"asked: Can login ship", "judged: 2 review (SEC.md); 3 review (PERF.md)",
		"decision gen-dec-1 (typesafe/jev-1.13-test), $0.00002230, 530 input tokens", "the state Jev saw: .routr/runs/4/state.json"} {
		if !strings.Contains(final, want) {
			t.Errorf("final.md lacks %q:\n%s", want, final)
		}
	}
	var rec jevRecord
	if err := json.Unmarshal([]byte(r.read(filepath.Join(dir, "jev.json"))), &rec); err != nil || rec.Decision == nil || rec.Decision.ID != "gen-dec-1" ||
		rec.MinConfidence != 0.6 || rec.Question != "verdict" {
		t.Errorf("jev.json = %+v (%v)", rec, err)
	}
	if !strings.Contains(r.read(filepath.Join(dir, "state.json")), `"output_path": "SEC.md"`) || r.read(filepath.Join(dir, "brief.md")) != "Can login ship, given SEC.md and PERF.md?\n" {
		t.Error("state.json or brief.md")
	}
	r.tick(1)
	if p := r.fake.Prompts(after); len(p) != 1 || !strings.Contains(p[0], "Release; Jev said pass.") {
		t.Errorf("after's prompt: %q", p)
	}
}

// A fail sends the implementer back, as a review's does; the judge then
// asks again with what the implementer did since.
func TestJudgeVerdictFailLoops(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("fail", 0.8))
	impl, sec, perf, ship, _ := panelDone(r)
	w := r.await(ship, registry.Waiting)
	if w.Round != 2 || w.Verdict != "" || !slices.Equal(w.After, []int{sec, perf}) || !w.Finished.IsZero() || !strings.Contains(w.Note, "waiting on 1 task") {
		t.Errorf("ship after its fail: %+v", w)
	}
	revise := r.fake.Panes(impl)[1].Prompts[0]
	if !strings.HasPrefix(revise, "Fix what "+r.abs("SEC.md")+" and "+r.abs("PERF.md")+" found.") {
		t.Errorf("revision prompt:\n%s", revise)
	}

	f.mu.Lock()
	f.answer = choose("pass", 0.7)
	f.mu.Unlock()
	r.done(impl, "", "", "fixed the injection", "")
	r.settle()
	w = r.await(ship, registry.Done)
	if w.Verdict != registry.Pass || w.Round != 2 {
		t.Errorf("ship after the fix: %+v", w)
	}
	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	st := calls[1].state
	if st.Judge.Round != 2 || len(st.Upstream) != 3 || st.Upstream[0].Worker != impl || st.Upstream[0].Summary != "fixed the injection" {
		t.Errorf("round 2 state = %+v", st)
	}
}

// Without on_fail a fail is done, and halts the steps after it, as a run
// step's does.
func TestJudgeFailWithoutOnFailHaltsTheStepsAfterIt(t *testing.T) {
	r := newRig(t)
	r.jev(choose("fail", 0.95))
	a := r.add(registry.Worker{Chain: "c", Step: "a", Brief: "a"})
	id := r.addJudge(registry.Worker{Chain: "c", Step: "ok", After: []int{a}, Brief: "Good?"}, registry.Judge{})
	next := r.add(registry.Worker{Chain: "c", Step: "next", After: []int{id}, Brief: "next"})
	r.tick(1)
	r.done(a, "", "", "did a", "")
	r.settle()
	w := r.await(id, registry.Done)
	if w.Verdict != registry.Fail || w.Finished.IsZero() || !w.Halts() || w.Note != "" || w.Summary != "Jev: fail (p 0.95, confidence 0.95)" {
		t.Errorf("%+v", w)
	}
	r.tick(2)
	if n := r.state(next, registry.Waiting); n.Note != "waits: 2 ok · jev judged fail (p 0.95, confidence 0.95); routr approve 2 lets the chain go on past it. routr result 2 says why" {
		t.Errorf("next's note = %q", n.Note)
	}
	r.s.Update(id, func(x *registry.Worker) { x.GoOn = true }) // routr approve 2
	r.tick(1)
	r.state(next, registry.Running)
}

// addJudge queues a judge over its After.
func (r *rig) addJudge(w registry.Worker, j registry.Judge) int {
	r.t.Helper()
	w.Role, w.Cwd, w.State = "judge", r.cwd, registry.Pending
	if j.From == nil {
		j.From = w.After
	}
	w.Judge = &j
	got, err := r.s.Create(w)
	if err != nil {
		r.t.Fatal(err)
	}
	return got.ID
}

const bestOf = `
[flow.best]
params = ["task"]
[[flow.best.step]]
id = "a"
output = "a.go"
brief = "Try {{task}} one way."
[[flow.best.step]]
id = "b"
brief = "Try {{task}} another way."
[[flow.best.step]]
id = "best"
after = ["a", "b"]
judge = "jev"
pick = true
brief = "Which attempt at {{task}} is best?"
[[flow.best.step]]
id = "use"
after = ["best"]
brief = "Build on {{best.output}} ({{best.verdict}})."
`

func TestJudgePicksOverTwoSteps(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("b-2", 0.7))
	ws := r.startFlow(bestOf, "best", map[string]string{"task": "parse"})
	a, b, best, use := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID
	r.tick(1)
	r.done(a, "a.go", "", "regexp", "")
	r.done(b, "", "", "hand-written parser", "") // no output file: its final.md stands in
	r.settle()
	w := r.await(best, registry.Done)
	winner := r.final(b)
	if w.Verdict != registry.Pass || w.Output != winner || w.Summary != "Jev: picked b-2 (p 0.70, confidence 0.70)" {
		t.Errorf("best = %+v", w)
	}
	q := f.Calls()[0].q["pick"]
	if len(q.Options) != 2 || !strings.Contains(q.Options["a-1"], "the result of worker 1, step a (task): regexp. It is the upstream entry with id a-1.") {
		t.Errorf("options = %+v", q.Options)
	}
	final := r.read(r.final(best))
	for _, want := range []string{"Jev: picked b-2 (p 0.70, confidence 0.70)\nwinner: b-2, worker 2, output .routr/runs/2/final-1.md\n", "  b-2          0.70  the result of worker 2", "  a-1          0.30  "} {
		if !strings.Contains(final, want) {
			t.Errorf("final.md lacks %q:\n%s", want, final)
		}
	}
	r.tick(1)
	if p := r.fake.Prompts(use); len(p) != 1 || !strings.Contains(p[0], "Build on "+r.final(2)+" (pass).") {
		t.Errorf("use's prompt: %q", p)
	}
}

// A pick over a foreach step's copies: one option per copy, and the step
// after it gets the winning copy's output.
func TestJudgePicksOverForeachCopies(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("dig-7", 0.85))
	ws := r.startFlow(orchestrate(`
[[flow.orch.step]]
id = "best"
after = ["dig"]
judge = "jev"
pick = true
brief = "Which part was done best?"
[[flow.orch.step]]
id = "use"
after = ["best"]
brief = "Use {{best.output}}."
`), "orch", map[string]string{"task": "x"})
	plan, best, use := ws[0].ID, ws[2].ID, ws[3].ID
	r.tick(1)
	for _, p := range []string{"tasks/a.md", "tasks/b.md"} {
		r.write(p)
	}
	os.MkdirAll(filepath.Join(r.cwd, "out"), 0o755)
	r.done(plan, "PLAN.md", "", "split", "")
	r.settle()
	r.tick(1)
	r.done(6, "out/a.md", "", "did a", "") // the copies come after join (5)
	r.done(7, "out/b.md", "", "did b", "")
	r.settle()
	w := r.await(best, registry.Done)
	if w.Output != filepath.Join(r.cwd, "out/b.md") || w.Verdict != registry.Pass {
		t.Errorf("best = %+v", w)
	}
	call := f.Calls()[0]
	if q := call.q["pick"]; len(q.Options) != 2 || !strings.Contains(q.Options["dig-6"], "worker 6, step dig, item a.md (research): did a") {
		t.Errorf("options = %+v", q.Options)
	}
	if ups := call.state.Upstream; len(ups) != 2 || ups[1].Item != "b.md" || ups[1].Output != "result of did b" || call.state.Judge.Mode != "pick" {
		t.Errorf("upstream = %+v", ups)
	}
	if final := r.read(r.final(best)); !strings.Contains(final, "winner: dig-7, worker 7, output out/b.md\n") {
		t.Errorf("final.md:\n%s", final)
	}
	r.tick(1)
	if p := r.fake.Prompts(use); len(p) != 1 || !strings.Contains(p[0], "Use "+r.abs("out/b.md")+".") {
		t.Errorf("use's prompt: %q", p)
	}
}

func TestJudgeLowConfidenceNeedsYouThenApprove(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("pass", 0.55))
	_, _, _, ship, after := panelDone(r)
	w := r.await(ship, registry.NeedsYou)
	if w.Verdict != "" || !w.Finished.IsZero() || w.Summary != "Jev is unsure: pass (p 0.55, confidence 0.55)" ||
		!strings.Contains(w.Note, "it chose pass (pass 0.55, fail 0.45), confidence 0.55, below min_confidence 0.6. routr approve 4 takes its choice") {
		t.Errorf("ship = %+v", w)
	}
	if final := r.read(r.final(ship)); !strings.HasPrefix(final, "Jev is unsure: pass (p 0.55, confidence 0.55), below min_confidence 0.6; routr approve 4 takes its choice\n") {
		t.Errorf("final.md:\n%s", final)
	}
	r.tick(1)
	r.state(after, registry.Waiting)

	r.approve(ship)
	w = r.await(ship, registry.Done)
	if w.Verdict != registry.Pass || w.Judge.Accept {
		t.Errorf("ship after approve = %+v", w)
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("approve asked Jev again: %d calls", n)
	}
	if final := r.read(r.final(ship)); !strings.HasPrefix(final, "Jev: pass (p 0.55, confidence 0.55); below min_confidence 0.6, taken as you approved\n") {
		t.Errorf("final.md:\n%s", final)
	}
}

func TestJudgeWithoutAKeyNeedsYou(t *testing.T) {
	t.Setenv(JevKeyEnv, "")
	r := newRig(t)
	_, _, _, ship, _ := panelDone(r)
	w := r.await(ship, registry.NeedsYou)
	if w.Verdict != "" || !strings.Contains(w.Note, "Jev could not decide (no Jev key: OPENROUTER_API_KEY is not set where the watcher runs") ||
		!strings.Contains(w.Note, "nothing passed. routr approve 4 asks again; routr cancel c1 stops here") {
		t.Errorf("ship = %+v", w)
	}
	if !strings.Contains(r.read(filepath.Join(r.p.RunDir(ship), "jev.json")), `"error": "no Jev key`) {
		t.Error("jev.json lacks the error")
	}

	// With a key in place, approve asks again.
	f := r.jev(choose("pass", 0.9))
	r.approve(ship)
	if w := r.await(ship, registry.Done); w.Verdict != registry.Pass || len(f.Calls()) != 1 {
		t.Errorf("after approve: %+v, %d calls", w, len(f.Calls()))
	}
}

func TestJudgeErrorsNeedYou(t *testing.T) {
	for _, tt := range []struct {
		name   string
		answer func(map[string]jev.Choice) (jev.Decision, error)
		want   string
	}{
		{"http", func(map[string]jev.Choice) (jev.Decision, error) {
			return jev.Decision{}, &jev.Error{Status: 529, Body: "overloaded"}
		}, "Jev could not decide (jev: HTTP 529: overloaded), so nothing passed"},
		{"no answer", func(map[string]jev.Choice) (jev.Decision, error) { return jev.Decision{ID: "x"}, nil }, `jev: no answer to "verdict"`},
		{"not an option", choose("maybe", 0.9), `jev chose "maybe", which is no option (fail, pass)`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.jev(tt.answer)
			_, _, _, ship, _ := panelDone(r)
			w := r.await(ship, registry.NeedsYou)
			if w.Verdict != "" || !w.Finished.IsZero() || !strings.Contains(w.Note, tt.want) {
				t.Errorf("ship = %+v", w)
			}
			if final := r.read(r.final(ship)); !strings.HasPrefix(final, "Jev could not decide: ") {
				t.Errorf("final.md:\n%s", final)
			}
		})
	}
}

// Every file in the state is cut so that all of it fits the budget: short
// ones whole, long ones to their head and tail, on rune boundaries.
func TestJudgeStateFitsTheBudget(t *testing.T) {
	defer func(n int) { jevBudget = n }(jevBudget)
	jevBudget = 1000
	r := newRig(t)
	f := r.jev(choose("pass", 0.9))
	a := r.add(registry.Worker{Chain: "c", Step: "a", Brief: "a"})
	b := r.add(registry.Worker{Chain: "c", Step: "b", Brief: "b"})
	id := r.addJudge(registry.Worker{Chain: "c", Step: "ok", After: []int{a, b}, Brief: "Good?"}, registry.Judge{Read: []string{"BIG.md"}})
	big := "HEAD" + strings.Repeat("é", 3000) + "TAIL"
	os.WriteFile(filepath.Join(r.cwd, "BIG.md"), []byte(big), 0o644)
	r.tick(1)
	r.done(a, "", "", "short", "")
	registry.WriteDone(r.p.RunDir(b), registry.DoneRecord{Output: filepath.Join(r.cwd, "BIG.md"), Message: "big"})
	r.fake.Finish(b, "")
	r.settle()
	r.await(id, registry.Done)
	st := f.Calls()[0].state
	total := 0
	texts := []string{st.Upstream[0].Final, st.Upstream[1].Output, st.Upstream[1].Final, st.Files[0].Content}
	for _, s := range texts {
		total += len(s) - len(markerOf(s))
		if !utf8.ValidString(s) {
			t.Errorf("not UTF-8: %q", s)
		}
	}
	if total > 1000 {
		t.Errorf("state text is %d bytes, over the budget", total)
	}
	if st.Upstream[0].Final != "short\n" || st.Upstream[1].Final != "big\n" {
		t.Errorf("short files were cut: %q %q", st.Upstream[0].Final, st.Upstream[1].Final)
	}
	for _, s := range []string{st.Upstream[1].Output, st.Files[0].Content} {
		if !strings.HasPrefix(s, "HEAD") || !strings.HasSuffix(s, "TAIL") || !strings.Contains(s, " bytes cut here]") || len(s) < 400 {
			t.Errorf("long file = %q", s)
		}
	}
}

func markerOf(s string) string {
	i := strings.Index(s, "\n[routr: ")
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], "]\n")
	return s[i : i+j+2]
}

func TestFairSplitsTheBudget(t *testing.T) {
	bs := []*blob{{head: strings.Repeat("a", 100)}, {head: strings.Repeat("b", 10)}, {head: strings.Repeat("c", 500)}, {head: strings.Repeat("d", 50)}}
	got := fair(bs, 300)
	if want := []int{100, 10, 140, 50}; !equalInts(got, want) {
		t.Errorf("fair = %v, want %v", got, want)
	}
	if got := fair(bs, 10_000); !equalInts(got, []int{100, 10, 500, 50}) {
		t.Errorf("fair with room = %v", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A watcher that stops mid-call leaves the judge running; the next one
// asks again. Cancelling a judge mid-call drops its call.
func TestJudgeRestartAndCancelMidCall(t *testing.T) {
	r := newRig(t)
	stuck := r.jev(choose("fail", 0.9))
	stuck.block = make(chan struct{})
	_, _, _, ship, _ := panelDone(r)
	r.until(func() bool { r.tick(1); return len(stuck.Calls()) == 1 })
	r.state(ship, registry.Running)

	dead := r.w
	t.Cleanup(func() { r.reap(dead) })
	r.w = r.watcher()
	f := r.jev(choose("pass", 0.9))
	w := r.await(ship, registry.Done)
	if w.Verdict != registry.Pass || len(f.Calls()) != 1 {
		t.Errorf("after the restart: %+v", w)
	}

	r = newRig(t)
	stuck = r.jev(choose("pass", 0.9))
	stuck.block = make(chan struct{})
	_, _, _, ship, _ = panelDone(r)
	r.until(func() bool { r.tick(1); return len(stuck.Calls()) == 1 })
	r.s.Update(ship, func(x *registry.Worker) { x.State = registry.Cancelled }) // routr cancel
	r.tick(1)
	r.until(func() bool {
		r.w.mu.Lock()
		defer r.w.mu.Unlock()
		a := r.w.jobs[ship]
		if a == nil {
			return true
		}
		select {
		case <-a.ended():
			return true
		default:
			return false
		}
	})
	r.tick(2)
	if w := r.state(ship, registry.Cancelled); w.Verdict != "" || fileExists(filepath.Join(r.p.RunDir(ship), "jev.json")) {
		t.Errorf("a cancelled judge got an answer: %+v", w)
	}
}

func TestJudgeWithOneCandidatePicksIt(t *testing.T) {
	r := newRig(t)
	f := r.jev(func(map[string]jev.Choice) (jev.Decision, error) { return jev.Decision{}, errors.New("not called") })
	a := r.add(registry.Worker{Chain: "c", Step: "a", Brief: "a", Output: "a.md"})
	id := r.addJudge(registry.Worker{Chain: "c", Step: "best", After: []int{a}, Brief: "Best?"}, registry.Judge{Pick: true})
	r.tick(1)
	r.done(a, "a.md", "", "did a", "")
	r.settle()
	w := r.await(id, registry.Done)
	if w.Output != filepath.Join(r.cwd, "a.md") || w.Verdict != registry.Pass || len(f.Calls()) != 0 {
		t.Errorf("best = %+v", w)
	}
	if final := r.read(r.final(id)); !strings.HasPrefix(final, "Picked a-1, the only candidate, without asking Jev\nwinner: a-1, worker 1, output a.md\n") {
		t.Errorf("final.md:\n%s", final)
	}
}

// Best of N: the options are the attempts; the checks on them are in the
// state as context, each naming the attempt it checked and its command.
func TestJudgePicksAmongAgentsOnly(t *testing.T) {
	r := newRig(t)
	f := r.jev(choose("b-2", 0.8))
	ws := r.startFlow(`
[flow.best]
params = ["task"]
[[flow.best.step]]
id = "a"
output = "a.go"
brief = "Try {{task}} one way."
[[flow.best.step]]
id = "b"
output = "b.go"
brief = "Try {{task}} another way."
[[flow.best.step]]
id = "check-a"
after = ["a"]
run = "echo checking {{a.output}}"
[[flow.best.step]]
id = "check-b"
after = ["b"]
run = "echo checking {{b.output}}; exit 1"
[[flow.best.step]]
id = "best"
after = ["a", "b", "check-a", "check-b"]
judge = "jev"
pick = true
brief = "Which attempt passes its check and is simplest?"
`, "best", map[string]string{"task": "parse"})
	a, b, ca, cb, best := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID, ws[4].ID
	r.tick(1)
	r.done(a, "a.go", "", "regexp", "")
	r.done(b, "b.go", "", "hand-written", "")
	r.settle()
	r.await(ca, registry.Done)
	// check-b fails: the pick waits for you; routr approve on check-b lets
	// the chain go on.
	r.await(cb, registry.Done)
	r.tick(1)
	r.state(best, registry.Waiting)
	r.s.Update(cb, func(x *registry.Worker) { x.GoOn = true })
	w := r.await(best, registry.Done)
	if w.Output != filepath.Join(r.cwd, "b.go") || w.Note != "" {
		t.Errorf("best = %+v", w)
	}
	call := f.Calls()[0]
	q := call.q["pick"]
	if len(q.Options) != 2 || q.Options["a-1"] == "" || q.Options["b-2"] == "" {
		t.Errorf("options = %+v", q.Options)
	}
	ups := call.state.Upstream
	if len(ups) != 4 {
		t.Fatalf("upstream = %+v", ups)
	}
	chk := ups[2]
	if chk.ID != "check-a-3" || chk.Role != "run" || chk.Harness != "sh" || chk.Command != "echo checking a.go" ||
		len(chk.After) != 1 || chk.After[0] != "a-1" || chk.Verdict != "pass" || !strings.Contains(chk.Output, "checking a.go") {
		t.Errorf("check-a = %+v", chk)
	}
	if cb := ups[3]; cb.Command != "echo checking b.go; exit 1" || len(cb.After) != 1 || cb.After[0] != "b-2" || cb.Verdict != "fail" {
		t.Errorf("check-b = %+v", cb)
	}
	if ups[0].After != nil || ups[0].Command != "" {
		t.Errorf("a = %+v", ups[0])
	}
}

// A pick whose agent candidates are one: it wins without asking, though
// checks are upstream too.
func TestJudgePickOneAgentAmongChecks(t *testing.T) {
	r := newRig(t)
	f := r.jev(func(map[string]jev.Choice) (jev.Decision, error) { return jev.Decision{}, errors.New("not called") })
	a := r.add(registry.Worker{Chain: "c", Step: "a", Brief: "a", Output: "a.md"})
	chk := r.add(registry.Worker{Chain: "c", Step: "check", Role: "run", Run: "true", Write: true, After: []int{a}})
	id := r.addJudge(registry.Worker{Chain: "c", Step: "best", After: []int{a, chk}, Brief: "Best?"}, registry.Judge{Pick: true})
	r.tick(1)
	r.done(a, "a.md", "", "did a", "")
	r.settle()
	w := r.await(id, registry.Done)
	if w.Output != filepath.Join(r.cwd, "a.md") || len(f.Calls()) != 0 {
		t.Errorf("best = %+v", w)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
