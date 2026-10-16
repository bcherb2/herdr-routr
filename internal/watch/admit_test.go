package watch

import (
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// A fail you approved past (GoOn) lets every step after it start, not only
// the next: the steps further down see it done.
func TestApprovedFailLetsLaterStepsStart(t *testing.T) {
	r := newRig(t)
	hunt := r.add(registry.Worker{Role: "review", Chain: "c", Step: "hunt", WantVerdict: true, Brief: "Hunt.", Output: "HUNT.md"})
	fix := r.add(registry.Worker{Chain: "c", Step: "fix", After: []int{hunt}, Brief: "Fix it."})
	ship := r.addRun(registry.Worker{Chain: "c", Step: "ship", After: []int{fix}}, "echo ship")
	r.tick(1)
	r.done(hunt, "HUNT.md", registry.Fail, "2 findings", "")
	r.settle()
	r.tick(2)
	r.state(fix, registry.Waiting)
	r.state(ship, registry.Waiting)
	r.s.Update(hunt, func(x *registry.Worker) { x.GoOn = true }) // routr approve 1
	r.await(fix, registry.Running)
	r.done(fix, "", "", "fixed", "")
	r.settle()
	r.await(ship, registry.Done)
}

// A runtime foreach step done once its copies are counts as done for the
// steps two after it, which see its copies upstream too.
func TestFinishedForeachCountsAsDone(t *testing.T) {
	r := newRig(t)
	ws := r.startFlow(orchestrate("")+`
[[flow.orch.step]]
id = "ship"
after = ["join"]
brief = "Ship {{join.final}}."
`, "orch", map[string]string{"task": "x"})
	plan, dig, join, ship := ws[0].ID, ws[1].ID, ws[2].ID, ws[3].ID
	r.write("tasks/a.md")
	r.write("tasks/b.md")
	r.tick(1)
	r.done(plan, "PLAN.md", "", "split into 2", "")
	r.settle()
	r.tick(1)
	copies := r.state(dig, registry.Waiting).Fanout.Items
	if len(copies) != 2 {
		t.Fatalf("copies = %v", copies)
	}
	for _, id := range copies {
		r.state(id, registry.Running)
		r.done(id, "", "", "did it", "")
	}
	r.settle()
	r.tick(2)
	r.state(dig, registry.Done)
	r.state(join, registry.Running)
	if w := r.state(ship, registry.Waiting); !strings.Contains(w.Note, "waiting on 3 task") {
		t.Errorf("ship note = %q, want the join it comes after", w.Note)
	}
	r.done(join, "", "", "merged", "")
	r.settle()
	r.tick(1)
	r.state(ship, registry.Running)
}

// A step not started yet in a side branch waits for a revision upstream of
// it, though the agent step it comes after is done: plan → review loops
// back to plan, and plan → notes → ship, gated. Opened while plan revises,
// ship waits for the revision and runs on it.
func TestSideBranchWaitsForARevision(t *testing.T) {
	r := newRig(t)
	plan := r.add(registry.Worker{Chain: "c", Step: "plan", Brief: "Plan it.", Output: "PLAN.md"})
	review := r.add(registry.Worker{Role: "review", Chain: "c", Step: "review", After: []int{plan}, WantVerdict: true, Brief: "Review it.",
		OnFail: &registry.OnFail{Resume: plan, Brief: "Fix what the review found.", Max: 3}})
	notes := r.add(registry.Worker{Chain: "c", Step: "notes", After: []int{plan}, Brief: "Write notes."})
	ship := r.addRun(registry.Worker{Chain: "c", Step: "ship", After: []int{notes}, Gate: "you"}, "cat PLAN.md >> shipped.txt; echo >> shipped.txt")
	r.tick(1)
	r.done(plan, "PLAN.md", "", "v1", "")
	r.settle()
	r.await(notes, registry.Running)
	r.done(notes, "", "", "noted", "")
	r.settle()
	r.await(ship, registry.Gated)
	r.await(review, registry.Running)
	r.done(review, "", registry.Fail, "1 blocker", "")
	r.settle()
	r.state(plan, registry.Running)
	r.s.Update(ship, func(x *registry.Worker) { x.GateOpen = true }) // routr approve
	r.tick(4)
	if w := r.get(ship); !w.Queued() || r.read(r.abs("shipped.txt")) != "" {
		t.Fatalf("ship ran before the revision: %s, shipped %q", w.State, r.read(r.abs("shipped.txt")))
	} else if !strings.Contains(w.Note, "waiting on 1 task") {
		t.Errorf("ship note = %q, want the revising plan", w.Note)
	}
	r.done(plan, "PLAN.md", "", "v2", "")
	r.settle()
	r.await(ship, registry.Done)
	if got := r.read(r.abs("shipped.txt")); got != "result of v2\n" {
		t.Errorf("ship saw %q, want the revision", got)
	}
}

// A reviewer whose on_fail resumes a worker it does not come after would
// not wait for the revision. No flow loads like that (flow.validate); a
// record that says so needs you, and nothing is sent back.
func TestLoopNeedsItsTargetUpstream(t *testing.T) {
	r := newRig(t)
	m := map[int]*registry.Worker{
		1: {ID: 1, Chain: "c", Role: "task", Harness: "claude", State: registry.Done},
		2: {ID: 2, Chain: "c", Role: "review", Harness: "claude", State: registry.Done, Verdict: registry.Fail,
			OnFail: &registry.OnFail{Resume: 1, Brief: "Fix it.", Max: 3}},
	}
	r.w.loop(m[2], m)
	if x := m[2]; x.State != registry.NeedsYou || x.Round != 0 || !strings.Contains(x.Note, "which it does not come after") {
		t.Errorf("reviewer = %+v", x)
	}
	if p := m[1]; p.State != registry.Done || len(p.Inbox) != 0 {
		t.Errorf("target = %+v", p)
	}
}
