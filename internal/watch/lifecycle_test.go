package watch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// dieWhileStarting leaves worker id as a watcher that died mid-start does:
// admitted (starting, holding the lock), its brief written and, with tab, the
// tab opened and recorded.
func (r *rig) dieWhileStarting(id int, tab bool) worker.Handle {
	r.t.Helper()
	if _, h, err := r.s.Claim(id, func(x *registry.Worker) error { x.State = registry.Starting; return nil }); err != nil || h != id {
		r.t.Fatalf("claim: %d %v", h, err)
	}
	wk := r.get(id)
	os.MkdirAll(r.p.RunDir(id), 0o755)
	os.WriteFile(filepath.Join(r.p.RunDir(id), registry.BriefFile), []byte("the brief as composed"), 0o644)
	if !tab {
		return worker.Handle{}
	}
	h, err := r.fake.Start(context.Background(), worker.Spec{ID: id, Harness: wk.Harness, Cwd: wk.Cwd})
	if err != nil {
		r.t.Fatal(err)
	}
	r.s.Update(id, func(x *registry.Worker) { record(x, h) })
	return h
}

// A watcher that died between taking a worker to `starting` and its final
// write left it starting for good: it held the directory lock, kept every
// later watcher busy, and `routr send` refused it.
func TestAWorkerLeftStartingIsPickedUp(t *testing.T) {
	t.Run("no tab yet: it starts again", func(t *testing.T) {
		r := newRig(t)
		id := r.add(registry.Worker{Write: true, Brief: "go"})
		other := r.add(registry.Worker{Write: true, Brief: "next"})
		r.dieWhileStarting(id, false)
		r.tick(1)
		r.state(id, registry.Running)
		if n := len(r.fake.Panes(id)); n != 1 {
			t.Errorf("%d tabs", n)
		}
		r.state(other, registry.Waiting)
		if m, _ := r.s.Holders(); m[r.cwd] != id {
			t.Errorf("holders = %v", m)
		}
	})
	t.Run("tab open, brief not taken: it is sent", func(t *testing.T) {
		r := newRig(t)
		id := r.add(registry.Worker{Write: true, Brief: "go"})
		r.dieWhileStarting(id, true)
		r.tick(1)
		r.state(id, registry.Running)
		if p := r.fake.Prompts(id); len(p) != 1 || p[0] != "the brief as composed" || len(r.fake.Panes(id)) != 1 {
			t.Errorf("prompts %q, %d tabs", p, len(r.fake.Panes(id)))
		}
		r.done(id, "", "", "did it", "")
		r.settle()
		r.state(id, registry.Done)
	})
	t.Run("tab open, brief taken: it runs", func(t *testing.T) {
		r := newRig(t)
		id := r.add(registry.Worker{Brief: "go"})
		h := r.dieWhileStarting(id, true)
		r.fake.Prompt(context.Background(), h, "the brief as composed")
		r.tick(1)
		if w := r.state(id, registry.Running); w.Started.IsZero() || w.Prompted.IsZero() {
			t.Errorf("running: %+v", w)
		}
		if p := r.fake.Prompts(id); len(p) != 1 {
			t.Errorf("the brief was sent again: %q", p)
		}
	})
	t.Run("tab gone: it starts again", func(t *testing.T) {
		r := newRig(t)
		id := r.add(registry.Worker{Brief: "go"})
		r.dieWhileStarting(id, true)
		r.fake.Kill(id)
		r.tick(1)
		r.state(id, registry.Running)
		if n := r.fake.Open(id); n != 1 || len(r.fake.Panes(id)) != 2 {
			t.Errorf("open %d of %d tabs", n, len(r.fake.Panes(id)))
		}
	})
	t.Run("the next watcher is not kept busy", func(t *testing.T) {
		r := newRig(t)
		id := r.add(registry.Worker{Brief: "go"})
		r.dieWhileStarting(id, true)
		r.tick(1)
		r.done(id, "", "", "did it", "")
		r.settle()
		r.state(id, registry.Done)
		if busy, _ := r.w.Tick(context.Background()); busy {
			t.Error("busy with nothing to do")
		}
	})
}

// The tab is recorded as soon as it opens, so a cancel that lands before the
// prompt closes it.
func TestTheTabIsRecordedBeforeThePrompt(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	var live *registry.Live
	r.w.Dial = func(string) worker.Driver {
		return cancelOnStart{Fake: r.fake, cancel: func() { live = r.get(id).Live }}
	}
	r.tick(1)
	if live == nil || live.Pane == "" {
		t.Errorf("no tab recorded when the prompt went: %+v", live)
	}
}

// A Start that fails after opening its tab (a startup dialog routr cannot
// answer) records the tab, so it is not left behind with nothing pointing
// at it.
func TestAFailedStartKeepsItsTabForCancel(t *testing.T) {
	r := newRig(t)
	r.fake.TabErr = errors.New("the harness is waiting on a startup dialog")
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	w := r.state(id, registry.Failed)
	if w.Live == nil || w.Live.Pane != "p1" {
		t.Fatalf("the failed start's tab is not recorded: %+v", w.Live)
	}
	// The handle named the session the harness was to have (Claude's
	// --session-id), but it never ran: no session to resume.
	if w.Session != "" || w.Transcript != "" {
		t.Errorf("a start that failed recorded session %q, transcript %q", w.Session, w.Transcript)
	}
	// What routr cancel then does: it can reach the tab.
	r.fake.Close(context.Background(), Handle(w))
	if r.fake.Open(id) != 0 {
		t.Error("tab still open")
	}

	// herdr out of reach after the tab opened: the tab is closed, the worker
	// queued again without it.
	r = newRig(t)
	r.fake.TabErr = unreachableErr()
	id = r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	if w := r.state(id, registry.Pending); w.Live != nil || r.fake.Open(id) != 0 {
		t.Errorf("requeued with a tab: %+v, %d open", w.Live, r.fake.Open(id))
	}
}

// A reviewer that ran round 1 and fails to start round 2 keeps round 1's
// session: routr retry reruns it with the revised prompt.
func TestARerunThatFailsToStartKeepsItsSession(t *testing.T) {
	r := newRig(t)
	plan, review := reviewLoop(r, 3)
	r.done(review, "REDTEAM.md", registry.Fail, "2 blockers", "See REDTEAM.md.")
	r.settle()
	r.fake.TabErr = errors.New("the harness is waiting on a startup dialog")
	r.done(plan, "PLAN.md", "", "plan v2", "Revised.")
	r.settle()
	if w := r.state(review, registry.Failed); w.Session != "sess-2" {
		t.Errorf("rerun that failed to start: session %q", w.Session)
	}
}

// herdr refuses agent.start while the new tab's shell is busy
// (agent_pane_busy) even after the driver's own tries: the worker fails at
// once, keeping its tab, and routr retry starts it again.
func TestPaneBusyFails(t *testing.T) {
	r := newRig(t)
	r.fake.TabErr = fmt.Errorf("worker: start claude: %w", &herdr.APIError{Method: "agent.start", Code: "agent_pane_busy", Message: "pane p1 is not an available shell"})
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	if w := r.state(id, registry.Failed); !strings.Contains(w.Error, "agent_pane_busy") || w.Live == nil || w.Session != "" {
		t.Errorf("busy start: %+v", w)
	}
}

// A follow-up whose session will not resume is dropped, and says so in
// watch.log, not only on the worker.
func TestFailedResumeIsLogged(t *testing.T) {
	r := newRig(t)
	var logged strings.Builder
	r.w.Log = log.New(&logged, "", 0)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.Kill(id)
	r.settle()
	r.state(id, registry.Closed)
	r.s.Update(id, func(w *registry.Worker) { w.Queue("why?") })
	r.fake.TabErr = errors.New("No conversation found with session ID: sess-1")
	r.tick(1)
	if w := r.state(id, registry.Failed); !strings.Contains(w.Error, "The follow-up was dropped") || w.Session != "sess-1" {
		t.Fatalf("after the failed resume: %+v", w)
	}
	r.fake.SetPhase(id, worker.Exited) // the harness quit in its new tab
	r.settle()
	out := logged.String()
	if want := fmt.Sprintf("%s: could not resume session sess-1: No conversation found with session ID: sess-1; dropped follow-up \"why?\"", r.get(id).Label()); !strings.Contains(out, want) {
		t.Errorf("watch.log lacks %q:\n%s", want, out)
	}
	if n := strings.Count(out, "harness exited"); n != 1 {
		t.Errorf("%d harness exited lines:\n%s", n, out)
	}
}

// A follow-up to a worker whose harness exited puts it back to work: its
// old error is no longer news, and once it replies it carries on, not
// failed again.
func TestFollowUpToAFailedWorkerClearsItsError(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.SetPhase(id, worker.Exited)
	r.settle()
	r.state(id, registry.Failed)
	r.s.Update(id, func(w *registry.Worker) { w.Queue("carry on") })
	r.tick(1)
	if w := r.state(id, registry.Running); w.Error != "" || w.Rest != "" {
		t.Fatalf("delivering: error %q, rest %q", w.Error, w.Rest)
	}
	r.fake.Finish(id, "on it.")
	r.settle()
	if w := r.get(id); w.State == registry.Failed || w.Error != "" {
		t.Errorf("after the reply: %s, error %q", w.State, w.Error)
	}
}

// unreachableErr is what the driver returns when herdr's socket refuses.
func unreachableErr() error {
	return fmt.Errorf("worker: label pane: herdr pane.rename: dial /s: %w",
		&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
}

// A follow-up in flight when the pane went away (the harness exited, the
// tab closed) is settled with a placeholder reply, so the worker returns to
// rest and the next follow-up can go.
func TestLosingThePaneSettlesAFollowUp(t *testing.T) {
	for _, how := range []string{"exited", "gone", "closed by the watcher"} {
		t.Run(how, func(t *testing.T) {
			r := newRig(t)
			id := r.add(registry.Worker{Brief: "go"})
			r.tick(1)
			r.done(id, "", "", "did it", "")
			r.settle()
			r.state(id, registry.Done)
			r.s.Update(id, func(w *registry.Worker) { w.Queue("why?"); w.Queue("and then?") })
			r.tick(1)
			if w := r.state(id, registry.Running); w.Turn != 1 || w.Rest != registry.Done {
				t.Fatalf("delivering: %+v", w)
			}
			switch how {
			case "exited":
				r.fake.SetPhase(id, worker.Exited)
				r.settle()
			case "gone":
				r.fake.Kill(id)
				r.settle()
			default:
				r.w.closeTab(context.Background(), r.get(id), r.fake)
			}
			w := r.get(id)
			if w.Replies != 1 || !strings.HasPrefix(r.read(r.p.TurnFile(id, 1)), "(") {
				t.Fatalf("turn 1 not settled: %+v, reply %q", w, r.read(r.p.TurnFile(id, 1)))
			}
			// The second follow-up goes: resumed in a new tab, answered, and
			// the worker is done again.
			r.tick(2)
			r.fake.Finish(id, "then this.")
			r.settle()
			w = r.state(id, registry.Done)
			if w.Turn != 2 || w.Replies != 2 || w.TurnsPending() || r.read(r.p.TurnFile(id, 2)) != "then this." {
				t.Errorf("after the second follow-up: %+v", w)
			}
		})
	}
}

// maybe_done keeps the directory lock: the worker may go on working, and
// then two writers would share the directory.
func TestMaybeDoneKeepsTheDirectoryLock(t *testing.T) {
	r := newRig(t)
	a := r.add(registry.Worker{Write: true, Brief: "a"})
	b := r.add(registry.Worker{Write: true, Brief: "b"})
	r.tick(1)
	r.fake.Finish(a, "I think that's it")
	r.settle()
	r.state(a, registry.MaybeDone)
	r.state(b, registry.Waiting)
	r.fake.SetPhase(a, worker.Working)
	r.settle()
	r.state(a, registry.Running)
	r.state(b, registry.Waiting)
	r.done(a, "", "", "a", "")
	r.settle()
	r.tick(1)
	r.state(b, registry.Running)
}

// A write worker that went idle without `routr done` lets the directory
// lock go after registry.MaybeDoneLock, so it can't stall every writer in
// its tree (found live: a haiku worker that never ran `routr done` held up a
// whole chain). If it works again later, it runs on with a note.
func TestMaybeDoneLetsTheLockGoAfterAWhile(t *testing.T) {
	r := newRig(t)
	a := r.add(registry.Worker{Write: true, Brief: "a"})
	b := r.add(registry.Worker{Write: true, Brief: "b"})
	r.tick(1)
	r.fake.Finish(a, "pong")
	r.settle()
	r.state(a, registry.MaybeDone)
	r.state(b, registry.Waiting)
	r.now = r.now.Add(registry.MaybeDoneLock)
	r.tick(1)
	r.state(b, registry.Running)
	if m, _ := r.s.Holders(); m[r.cwd] != b {
		t.Fatalf("holders = %v, want %d", m, b)
	}
	r.fake.SetPhase(a, worker.Working)
	r.settle()
	if w := r.state(a, registry.Running); !strings.Contains(w.Note, "directory lock") || !w.Idle.IsZero() {
		t.Errorf("note = %q, idle = %v", w.Note, w.Idle)
	}
	if m, _ := r.s.Holders(); m[r.cwd] != b {
		t.Errorf("holders = %v, want %d to keep it", m, b)
	}
}

// A review loop's revision must not finish on the output of the round
// before: the plan worker, its tab kept open by a follow-up, sat idle with
// the revision queued behind the directory lock, and its PLAN.md (changed
// since its last prompt) made it done at once.
func TestARevisionWaitsForItsBrief(t *testing.T) {
	r := newRig(t)
	plan := r.add(registry.Worker{Role: "plan", Chain: "c", Step: "plan", Write: true, Brief: "Plan.", Output: "PLAN.md"})
	review := r.add(registry.Worker{Role: "review", Chain: "c", Step: "review", After: []int{plan}, WantVerdict: true, Output: "REDTEAM.md",
		Brief: "Red-team {{plan.output}}.", OnFail: &registry.OnFail{Resume: plan, Brief: "Address {{review.output}}.", Max: 3}})
	other := r.add(registry.Worker{Write: true, Brief: "another writer in the same directory"})
	r.tick(1)
	r.done(plan, "PLAN.md", "", "plan v1", "")
	r.settle()
	r.tick(1)
	r.state(plan, registry.Done)
	r.state(review, registry.Running)
	r.state(other, registry.Running) // holds the lock now
	r.done(review, "REDTEAM.md", registry.Fail, "2 blockers", "")
	r.settle()
	r.settle()
	if w := r.state(plan, registry.Running); len(w.Inbox) != 1 || !strings.Contains(w.Note, "directory lock") {
		t.Fatalf("plan: %+v", w)
	}
	r.state(review, registry.Waiting)
	r.done(other, "", "", "other", "")
	r.settle()
	r.tick(1)
	if p := r.fake.Prompts(plan); !strings.HasPrefix(p[len(p)-1], "Address "+r.abs("REDTEAM.md")+".") {
		t.Fatalf("the revision was not sent: %q", p)
	}
	r.done(plan, "PLAN.md", "", "plan v2", "")
	r.settle()
	r.tick(1)
	r.state(review, registry.Running)
}

// A reviewer sent back to wait by the loop loses its tab. Were follow-ups
// to keep it open, a close of that tab would make it closed and stop the
// loop.
func TestTheLoopClosesTheWaitingReviewersTab(t *testing.T) {
	r := newRig(t)
	plan, review := reviewLoop(r, 3)
	r.done(review, "REDTEAM.md", registry.Fail, "2 blockers", "")
	r.settle()
	if w := r.state(review, registry.Waiting); w.Live != nil || r.fake.Open(review) != 0 {
		t.Fatalf("the waiting reviewer kept its tab: %+v", w.Live)
	}
	r.done(plan, "PLAN.md", "", "plan v2", "")
	r.settle()
	r.tick(1)
	r.state(review, registry.Running)
}
