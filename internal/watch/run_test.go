package watch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// addRun queues a run step that runs cmd.
func (r *rig) addRun(w registry.Worker, cmd string) int {
	r.t.Helper()
	w.Role, w.Run, w.Write = "run", cmd, true
	id := r.add(w)
	r.t.Cleanup(func() { r.reap(r.w) })
	return id
}

// reap kills what w's commands left running, so no test leaves a process.
func (r *rig) reap(w *Watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, j := range w.jobs {
		j.stop()
		<-j.ended()
	}
}

// await ticks, letting real time pass for the commands, until worker id is
// in state want.
func (r *rig) await(id int, want registry.State) registry.Worker {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.tick(1)
		if w := r.get(id); w.State == want {
			return w
		}
	}
	return r.state(id, want)
}

// alive reports whether process group pid has a process left.
func alive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}

func TestRunStepPasses(t *testing.T) {
	r := newRig(t)
	os.Mkdir(filepath.Join(r.cwd, "sub"), 0o755)
	id := r.addRun(registry.Worker{Chain: "c", Step: "check", Cwd: filepath.Join(r.cwd, "sub")},
		`echo "$ROUTR_ID $CLAUDE_CONFIG_DIR $(basename "$PWD")"; echo oops >&2`)
	r.tick(1)
	w := r.state(id, registry.Running)
	if w.Pid == 0 || (w.PidAt == 0 && runtime.GOOS == "linux") || w.Live != nil || len(r.fake.Panes(id)) != 0 || w.Label() != "1 check · run" {
		t.Errorf("running: %+v", w)
	}
	if m, _ := r.s.Holders(); m[w.Worktree()] != id {
		t.Errorf("a run step takes the directory lock: %v", m)
	}
	w = r.await(id, registry.Done)
	log := filepath.Join(r.p.RunDir(id), "run.log")
	if w.Verdict != registry.Pass || w.Output != log || w.Pid != 0 || !strings.HasPrefix(w.Summary, "exit 0 after ") {
		t.Errorf("done: %+v", w)
	}
	if got := r.read(log); got != "1 /cfg sub\noops\n" {
		t.Errorf("run.log = %q", got)
	}
	final := r.read(r.final(id))
	for _, want := range []string{"$ echo \"$ROUTR_ID", "pass: exit 0 after", "its output:\n1 /cfg sub\noops\n"} {
		if !strings.Contains(final, want) {
			t.Errorf("final.md lacks %q:\n%s", want, final)
		}
	}
	if busy, _ := r.w.Tick(context.Background()); busy {
		t.Error("busy with nothing to do")
	}
}

func TestRunStepVerdicts(t *testing.T) {
	for _, tt := range []struct {
		name, cmd string
		expect    registry.Verdict
		state     registry.State
		verdict   registry.Verdict
		summary   string
	}{
		{"fails", "echo broken; exit 3", "", registry.Done, registry.Fail, "exit 3 after"},
		{"fails as expected", "false", registry.Fail, registry.Done, registry.Pass, ", failing as expected"},
		{"passes but should fail", "true", registry.Fail, registry.Done, registry.Fail, ", expected to fail"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			id := r.addRun(registry.Worker{Step: "check", Expect: tt.expect}, tt.cmd)
			w := r.await(id, tt.state)
			if w.Verdict != tt.verdict || !strings.Contains(w.Summary, tt.summary) || w.Finished.IsZero() {
				t.Errorf("%+v", w)
			}
			// A fail is done, its answer, but halts the steps after it.
			if w.Halts() != (tt.verdict == registry.Fail) || w.Note != "" {
				t.Errorf("halts = %v, note = %q", w.Halts(), w.Note)
			}
		})
	}
}

func TestRunStepTimesOut(t *testing.T) {
	r := newRig(t)
	id := r.addRun(registry.Worker{Timeout: 200 * time.Millisecond}, "echo started; sleep 30")
	r.tick(1)
	pid := r.get(id).Pid
	if busy, _ := r.w.Tick(context.Background()); !busy {
		t.Error("a running command does not keep the watcher busy")
	}
	w := r.await(id, registry.Done)
	if w.Verdict != registry.Fail || w.Summary != "timed out after 200ms" || !w.Halts() {
		t.Errorf("%+v", w)
	}
	if alive(pid) {
		t.Error("the timed-out command still runs")
	}
	if final := r.read(r.final(id)); !strings.Contains(final, "fail: timed out after 200ms") || !strings.Contains(final, "started") {
		t.Errorf("final.md:\n%s", final)
	}
}

// A failing check without on_fail is done, and the steps after it wait
// for you, with a note that says why; routr approve on the check lets them
// start, and its verdict stays fail.
func TestFailingCheckHaltsTheStepsAfterIt(t *testing.T) {
	r := newRig(t)
	check := r.addRun(registry.Worker{Chain: "c", Step: "check"}, "echo broken; exit 1")
	next := r.addRun(registry.Worker{Chain: "c", Step: "next", After: []int{check}}, "echo next")
	w := r.await(check, registry.Done)
	if w.Verdict != registry.Fail || !w.Halts() || w.Note != "" {
		t.Errorf("check = %+v", w)
	}
	r.tick(3)
	n := r.state(next, registry.Waiting)
	if !strings.HasPrefix(n.Note, "waits: 1 check · run failed (exit 1 after ") ||
		!strings.HasSuffix(n.Note, "; routr approve 1 lets the chain go on past it. Its log is .routr/runs/1/run.log") {
		t.Errorf("next's note = %q", n.Note)
	}
	r.s.Update(check, func(x *registry.Worker) { x.GoOn = true }) // routr approve 1
	if n := r.await(next, registry.Done); n.Verdict != registry.Pass {
		t.Errorf("next = %+v", n)
	}
	if w := r.get(check); w.Verdict != registry.Fail || w.Halts() {
		t.Errorf("check after approve = %+v", w)
	}
}

// A pipe may queue the next step after the check already failed: it waits
// all the same.
func TestStepQueuedAfterAFailedCheckWaits(t *testing.T) {
	r := newRig(t)
	check := r.addRun(registry.Worker{Step: "check"}, "exit 2")
	r.await(check, registry.Done)
	next := r.addRun(registry.Worker{Chain: "c1", Step: "next", After: []int{check}}, "echo next")
	r.tick(3)
	if n := r.state(next, registry.Waiting); !strings.Contains(n.Note, "routr approve 1 lets the chain go on past it") {
		t.Errorf("next's note = %q", n.Note)
	}
}

// A loop at its cap still needs you; the steps after it wait on it.
func TestRunStepLoopAtItsCapNeedsYou(t *testing.T) {
	r := newRig(t)
	impl := r.add(registry.Worker{Chain: "c", Step: "implement", Brief: "Fix it."})
	check := r.addRun(registry.Worker{Chain: "c", Step: "check", After: []int{impl},
		OnFail: &registry.OnFail{Resume: impl, Brief: "Fix it again.", Max: 1}}, "exit 1")
	next := r.addRun(registry.Worker{Chain: "c", Step: "next", After: []int{check}}, "echo next")
	r.tick(1)
	r.done(impl, "", "", "fixed, I think", "")
	r.settle()
	w := r.await(check, registry.NeedsYou)
	if w.Verdict != registry.Fail || w.Halts() || !strings.Contains(w.Note, "the review loop hit its cap of 1 round") {
		t.Errorf("check = %+v", w)
	}
	if n := r.state(next, registry.Waiting); n.Note != "waiting on 2 check · run, which is needs_you" {
		t.Errorf("next's note = %q", n.Note)
	}
}

// implement ⇄ check until the check passes: the run step's on_fail sends
// its log to the implementer, then runs again once that is done.
func TestRunStepLoopsUntilItPasses(t *testing.T) {
	r := newRig(t)
	set, err := flow.Parse([]byte(`
[flow.fix]
params = ["check"]
[[flow.fix.step]]
id = "implement"
brief = "Fix it."
[[flow.fix.step]]
id = "check"
after = ["implement"]
run = "{{check}}"
on_fail = { resume = "implement", brief = "The check failed; its log is {{check.output}}. Fix the code, not the test.", max = 3 }
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["fix"].Start(r.s, "fix-1", map[string]string{"check": "cat fixed && echo ok"}, r.cwd, r.p.WorkDir("fix-1"))
	if err != nil {
		t.Fatal(err)
	}
	impl, check := ws[0].ID, ws[1].ID
	t.Cleanup(func() { r.reap(r.w) })
	r.tick(1)
	r.done(impl, "", "", "fixed, I think", "")
	r.settle()
	w := r.await(check, registry.Waiting)
	if w.Round != 2 || w.Verdict != "" || len(w.After) != 1 || w.After[0] != impl {
		t.Errorf("check after its fail: %+v", w)
	}
	revise := r.fake.Panes(impl)[1].Prompts[0]
	if !strings.HasPrefix(revise, "The check failed; its log is "+filepath.Join(r.p.RunDir(2), "run.log")+". Fix the code, not the test.") ||
		!strings.Contains(revise, "routr done") {
		t.Errorf("revision prompt:\n%s", revise)
	}

	r.write("fixed")
	r.done(impl, "", "", "fixed now", "")
	r.settle()
	w = r.await(check, registry.Done)
	if w.Verdict != registry.Pass || w.Round != 2 {
		t.Errorf("check after the fix: %+v", w)
	}
	if got := r.read(filepath.Join(r.p.RunDir(check), "run.log")); !strings.Contains(got, "ok") {
		t.Errorf("the second run's log = %q", got)
	}
}

// A check whose tools leave a server running (a compiler cache's, a build
// daemon) detached from its process group: the next round runs as the
// first did, whatever that server still holds. The check waits until the
// server leads its own group (setsid has run), so alive() does not race it.
func TestRunStepLoopsPastADetachedServer(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	r := newRig(t)
	set, err := flow.Parse([]byte(`
[flow.fix]
[[flow.fix.step]]
id = "implement"
brief = "Fix it."
[[flow.fix.step]]
id = "check"
after = ["implement"]
run = "setsid sleep 30 >/dev/null 2>&1 & echo $! >> servers; until [ \"$(ps -o pgid= -p $!)\" -eq $! ]; do sleep 0.01; done; cat fixed"
on_fail = { resume = "implement", brief = "The check failed.", max = 3 }
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := set["fix"].Start(r.s, "fix-1", nil, r.cwd, r.p.WorkDir("fix-1"))
	if err != nil {
		t.Fatal(err)
	}
	impl, check := ws[0].ID, ws[1].ID
	t.Cleanup(func() { r.reap(r.w) })
	t.Cleanup(func() { // the servers the check started, and only those
		for _, s := range strings.Fields(r.read(filepath.Join(r.cwd, "servers"))) {
			if pid, err := strconv.Atoi(s); err == nil {
				killGroup(pid, syscall.SIGKILL)
			}
		}
	})
	r.tick(1)
	r.done(impl, "", "", "fixed, I think", "")
	r.settle()
	r.await(check, registry.Waiting)
	server, err := strconv.Atoi(strings.TrimSpace(r.read(filepath.Join(r.cwd, "servers"))))
	if err != nil || !alive(server) {
		t.Fatalf("the first round's server is not running: %v", err)
	}

	r.write("fixed")
	r.done(impl, "", "", "fixed now", "")
	r.settle()
	if w := r.await(check, registry.Done); w.Verdict != registry.Pass || w.Round != 2 {
		t.Errorf("check after the fix: %+v", w)
	}
	if !alive(server) {
		t.Error("the server was killed: it is not the check's to stop")
	}
}

func TestCancelStopsTheCommand(t *testing.T) {
	r := newRig(t)
	id := r.addRun(registry.Worker{}, "sleep 30")
	r.tick(1)
	w := r.state(id, registry.Running)
	if !alive(w.Pid) {
		t.Fatal("not running")
	}
	// routr cancel marks it cancelled and stops the command (StopRun).
	r.s.Update(id, func(x *registry.Worker) { x.State = registry.Cancelled })
	StopRun(r.p, w)
	r.until(func() bool { return !alive(w.Pid) })

	// Should cancel not reach the process, the watcher stops it.
	id = r.addRun(registry.Worker{}, "sleep 30")
	r.tick(1)
	w = r.state(id, registry.Running)
	r.s.Update(id, func(x *registry.Worker) { x.State = registry.Cancelled })
	r.until(func() bool { r.tick(1); return !alive(w.Pid) })
	r.tick(2)
	if w := r.state(id, registry.Cancelled); w.Verdict != "" {
		t.Errorf("a cancelled command got a verdict: %+v", w)
	}
	StopRun(r.p, w) // nothing runs: a no-op, though its pid is recorded
}

func (r *rig) until(cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	r.t.Fatal("timed out")
}

// A watcher that stops mid-command leaves the step running; the next one
// stops what is left of the old run and runs the command again.
func TestRestartMidCommandRunsItAgain(t *testing.T) {
	needProc(t)
	r := newRig(t)
	id := r.addRun(registry.Worker{},
		`if [ -f once ]; then echo second >> ran; else touch once; echo first >> ran; sleep 30; fi`)
	r.tick(1)
	old := r.state(id, registry.Running)
	r.until(func() bool { return fileExists(filepath.Join(r.cwd, "once")) })

	dead := r.w
	t.Cleanup(func() { r.reap(dead) })
	r.w = r.watcher() // the watcher died; a new one starts from the registry
	w := r.await(id, registry.Done)
	if w.Verdict != registry.Pass || w.Pid != 0 {
		t.Errorf("after the rerun: %+v", w)
	}
	if got := r.read(filepath.Join(r.cwd, "ran")); got != "first\nsecond\n" {
		t.Errorf("ran = %q", got)
	}
	if alive(old.Pid) {
		t.Error("the old run still goes")
	}
}

// orphan sets run step id to running with pid as its process group,
// started at at, as a watcher that stopped mid-command leaves it.
func (r *rig) orphan(id, pid int, at uint64) {
	r.t.Helper()
	r.s.Update(id, func(x *registry.Worker) {
		x.State, x.Pid, x.PidAt, x.Prompted = registry.Running, pid, at, r.now
	})
}

// group starts a process group of its own and returns its pid: one that
// sleeps until killed, or with live false one that is already gone.
func group(t *testing.T, live bool) int {
	t.Helper()
	c := exec.Command("sleep", "30")
	if !live {
		c = exec.Command("true")
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if !live {
		c.Wait()
		return c.Process.Pid
	}
	reaped := make(chan struct{})
	go func() { c.Wait(); close(reaped) }() // a killed one is no zombie left in its group
	t.Cleanup(func() { killGroup(c.Process.Pid, syscall.SIGKILL); <-reaped })
	return c.Process.Pid
}

// leaderless starts a process group whose leader exits and leaves a
// process in it, and returns the group and when its leader started.
func leaderless(t *testing.T) (int, uint64) {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 30 & exit 0")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	t.Cleanup(func() { killGroup(pid, syscall.SIGKILL) })
	st, err := procStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	c.Wait()
	return pid, st.start
}

// startOf is when process pid started.
func startOf(t *testing.T, pid int) uint64 {
	t.Helper()
	st, err := procStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	return st.start
}

// needProc skips a test that needs /proc/<pid>/stat where there is none.
func needProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
}

func TestProcStat(t *testing.T) {
	needProc(t)
	st, err := procStat(os.Getpid())
	if err != nil || st.pgrp != syscall.Getpgrp() || st.start == 0 {
		t.Errorf("procStat(me) = %+v, %v", st, err)
	}
	if _, err := procStat(group(t, false)); err == nil {
		t.Error("procStat of a gone process")
	}
}

// A watcher that took over a run kills its old process group only if the
// group's leader is still the recorded process. A later process that took
// the pid means the run is gone; a group without its leader, or a leader
// with no recorded start, may be another's: the step fails saying how to
// look, instead of a group being killed or the step requeued every tick.
func TestRerunKillsOnlyItsOwnRun(t *testing.T) {
	needProc(t)
	defer func(d time.Duration) { runFree = d }(runFree)
	runFree = 50 * time.Millisecond
	for _, tc := range []struct {
		name  string
		left  func(t *testing.T) (int, uint64) // the old run's pid and start
		alive bool                             // its group has a process the watcher must not kill
		state registry.State
	}{
		{"gone", func(t *testing.T) (int, uint64) { return group(t, false), 1 }, false, registry.Done},
		{"pid reused", func(t *testing.T) (int, uint64) { pid := group(t, true); return pid, startOf(t, pid) + 1 }, true, registry.Done},
		{"start unknown", func(t *testing.T) (int, uint64) { return group(t, true), 0 }, true, registry.Failed},
		{"leader gone", leaderless, true, registry.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			id := r.addRun(registry.Worker{}, "true")
			pid, at := tc.left(t)
			if alive(pid) != tc.alive {
				t.Fatalf("alive(%d) = %v", pid, !tc.alive)
			}
			r.orphan(id, pid, at)
			w := r.await(id, tc.state)
			if tc.state == registry.Failed {
				g := strconv.Itoa(pid)
				if w.Pid != 0 || !strings.Contains(w.Error, "pgrep -ag "+g) || !strings.Contains(w.Error, "kill -- -"+g) {
					t.Errorf("failed: %+v", w)
				}
				if busy, _ := r.w.Tick(context.Background()); busy {
					t.Error("busy with a failed step")
				}
			}
			if alive(pid) != tc.alive {
				t.Errorf("the watcher killed process group %d, which it cannot tell for the run's", pid)
			}
		})
	}
}

// A run step a stopped watcher left keeps the directory lock until its old
// command is gone: no other writer gets the tree while it still runs.
func TestRerunKeepsTheLockUntilTheOldRunIsGone(t *testing.T) {
	needProc(t)
	r := newRig(t)
	writer := r.add(registry.Worker{Write: true, Gate: "you", Brief: "write"})
	id := r.addRun(registry.Worker{}, `if [ -f once ]; then echo second >> ran; else touch once; echo first >> ran; sleep 30; fi`)
	r.tick(1)
	r.state(writer, registry.Gated)
	old := r.state(id, registry.Running)
	r.until(func() bool { return fileExists(filepath.Join(r.cwd, "once")) })

	r.s.Update(writer, func(x *registry.Worker) { x.GateOpen = true }) // routr approve
	dead := r.w
	t.Cleanup(func() { r.reap(dead) })
	r.w = r.watcher()
	r.tick(1)
	if m, _ := r.s.Holders(); m[r.cwd] == writer && oldRun(old.Pid, old.PidAt) == runLives {
		t.Fatal("the writer got the directory lock while the old command still ran")
	}
	r.until(func() bool { return !alive(old.Pid) })
	r.state(writer, registry.Running)
	r.done(writer, "", "", "written", "")
	r.settle()
	if w := r.await(id, registry.Done); w.Verdict != registry.Pass {
		t.Errorf("after the rerun: %+v", w)
	}
	if got := r.read(filepath.Join(r.cwd, "ran")); got != "first\nsecond\n" {
		t.Errorf("ran = %q", got)
	}
}

// Refs to step files are quoted for sh; params go in as written.
func TestRunStepQuotesRefs(t *testing.T) {
	r := newRig(t)
	evil := "it's; touch PWNED.md"
	plan := r.add(registry.Worker{Chain: "c", Step: "plan", Brief: "plan"})
	id := r.addRun(registry.Worker{Chain: "c", Step: "check", After: []int{plan}, Params: map[string]string{"also": "echo one && echo two"}},
		`cat {{plan.output}} > copy.txt; {{also}}; echo {{chain}} {{work}}`)
	r.tick(1)
	r.done(plan, evil, "", "planned", "")
	r.settle()
	r.await(id, registry.Done)
	if fileExists(filepath.Join(r.cwd, "PWNED.md")) {
		t.Fatal("a file name injected a command")
	}
	if got := r.read(filepath.Join(r.cwd, "copy.txt")); got != "result of planned" {
		t.Errorf("copy.txt = %q", got)
	}
	cmd := r.read(filepath.Join(r.p.RunDir(id), "brief.md"))
	if want := `cat 'it'\''s; touch PWNED.md' > copy.txt; echo one && echo two; echo c .routr/work/c` + "\n"; cmd != want {
		t.Errorf("command = %q, want %q", cmd, want)
	}
	if got := r.read(filepath.Join(r.p.RunDir(id), "run.log")); got != "one\ntwo\nc .routr/work/c\n" {
		t.Errorf("run.log = %q", got)
	}
}

func TestRunStepBadRefFails(t *testing.T) {
	r := newRig(t)
	id := r.addRun(registry.Worker{}, "cat {{prev.output}}")
	r.tick(1)
	if w := r.state(id, registry.Failed); !strings.Contains(w.Error, "its command has bad refs: {{prev.output}}") {
		t.Errorf("error = %q", w.Error)
	}
}

func TestRunLogKeepsHeadAndTail(t *testing.T) {
	defer func(n int) { maxRunHalf = n }(maxRunHalf)
	maxRunHalf = 8
	path := filepath.Join(t.TempDir(), "run.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	l := &runLog{f: f}
	for _, s := range []string{"0123", "4567", "89ab", "cdef", "ghij", "klmn", "opqr", "stuv"} {
		l.Write([]byte(s))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if want := "01234567\n[routr: 16 bytes of output cut here]\nopqrstuv"; string(b) != want {
		t.Errorf("run.log = %q, want %q", b, want)
	}
}

// implementCheckReview is implement → check (a run step) → review, the
// review's on_fail resuming implement. The check runs cmd.
func implementCheckReview(r *rig, cmd string) (impl, check, review int) {
	impl = r.add(registry.Worker{Chain: "c", Step: "implement", Brief: "Fix it."})
	check = r.addRun(registry.Worker{Chain: "c", Step: "check", After: []int{impl}}, cmd)
	review = r.add(registry.Worker{Role: "review", Chain: "c", Step: "review", After: []int{check}, WantVerdict: true, Brief: "Review it.",
		OnFail: &registry.OnFail{Resume: impl, Brief: "Fix what the review found.", Max: 3}})
	r.tick(1)
	r.done(impl, "", "", "fixed", "")
	r.settle()
	r.await(check, registry.Done)
	r.await(review, registry.Running)
	r.done(review, "", registry.Fail, "1 blocker", "")
	r.settle()
	return impl, check, review
}

// A review loop between implement and review queues the check in between
// again: it runs on the revision, and round 2 starts only once it passed.
func TestReviewLoopRerunsTheCheckBetween(t *testing.T) {
	r := newRig(t)
	impl, check, review := implementCheckReview(r, "echo run >> runs.txt")
	w := r.state(review, registry.Waiting)
	if w.Round != 2 || !slices.Equal(w.After, []int{check}) {
		t.Errorf("review after its fail: %+v", w)
	}
	c := r.get(check)
	if !c.Queued() || c.Verdict != "" || c.Summary != "" || c.Output != "" || !c.Finished.IsZero() || !slices.Equal(c.After, []int{impl}) {
		t.Errorf("check after the loop: %+v", c)
	}
	r.tick(2)
	if c := r.get(check); !c.Queued() || r.read(r.abs("runs.txt")) != "run\n" {
		t.Errorf("the check ran before the revision: %+v", c)
	}
	r.done(impl, "", "", "fixed again", "")
	r.settle()
	if r.get(review).State != registry.Waiting {
		t.Errorf("review started before the check ran again: %+v", r.get(review))
	}
	if c := r.await(check, registry.Done); c.Verdict != registry.Pass || r.read(r.abs("runs.txt")) != "run\nrun\n" {
		t.Errorf("check round 2: %+v", c)
	}
	if w := r.await(review, registry.Running); w.Round != 2 {
		t.Errorf("review round 2: %+v", w)
	}
}

// The checks a loop queues again wait for the revision even past an agent
// step left done, and when the target holds no lock (write = false, its
// output in {{work}}): plan → lint → check → tidy → recheck → review. Each
// check sees the revised plan, and recheck still runs after check.
func TestReviewLoopChecksWaitPastAnAgent(t *testing.T) {
	r := newRig(t)
	plan := r.add(registry.Worker{Chain: "c", Step: "plan", Brief: "Plan it."})
	lint := r.add(registry.Worker{Chain: "c", Step: "lint", After: []int{plan}, Brief: "Lint it."})
	check := r.addRun(registry.Worker{Chain: "c", Step: "check", After: []int{lint}}, "sleep 0.2; cat PLAN.md >> seen.txt; echo >> seen.txt; echo check >> runs.txt")
	tidy := r.add(registry.Worker{Chain: "c", Step: "tidy", After: []int{check}, Brief: "Tidy it."})
	recheck := r.addRun(registry.Worker{Chain: "c", Step: "recheck", After: []int{tidy}}, "echo recheck >> runs.txt")
	r.s.Update(recheck, func(x *registry.Worker) { x.Write = false }) // no lock orders the two checks
	review := r.add(registry.Worker{Role: "review", Chain: "c", Step: "review", After: []int{recheck}, WantVerdict: true, Brief: "Review it.",
		OnFail: &registry.OnFail{Resume: plan, Brief: "Fix what the review found.", Max: 3}})
	r.tick(1)
	r.done(plan, "PLAN.md", "", "v1", "")
	r.settle()
	r.await(lint, registry.Running)
	r.done(lint, "", "", "clean", "")
	r.settle()
	r.await(tidy, registry.Running)
	r.done(tidy, "", "", "tidy", "")
	r.settle()
	r.await(recheck, registry.Done)
	r.await(review, registry.Running)
	r.done(review, "", registry.Fail, "1 blocker", "")
	r.settle()
	r.state(review, registry.Waiting)
	if p := r.get(plan); p.State != registry.Running || p.Write {
		t.Fatalf("plan revising: %+v", p)
	}
	r.tick(4)
	time.Sleep(400 * time.Millisecond) // time for a check started early to run
	r.tick(2)
	if c, rc := r.get(check), r.get(recheck); !c.Queued() || !rc.Queued() || r.read(r.abs("seen.txt")) != "result of v1\n" {
		t.Fatalf("a check ran before the revision: check=%s recheck=%s seen=%q", c.State, rc.State, r.read(r.abs("seen.txt")))
	}
	r.done(plan, "PLAN.md", "", "v2", "")
	r.settle()
	r.await(check, registry.Done)
	r.await(recheck, registry.Done)
	if seen := r.read(r.abs("seen.txt")); seen != "result of v1\nresult of v2\n" {
		t.Errorf("check saw %q, want the revision", seen)
	}
	if runs := r.read(r.abs("runs.txt")); runs != "check\nrecheck\ncheck\nrecheck\n" {
		t.Errorf("runs = %q, want check before recheck each round", runs)
	}
	for _, id := range []int{lint, tidy} {
		if w := r.get(id); w.State != registry.Done {
			t.Errorf("agent step %d was reset: %+v", id, w)
		}
	}
	if w := r.await(review, registry.Running); w.Round != 2 {
		t.Errorf("review round 2: %+v", w)
	}
}

// A check that fails on the revision halts the review, as any failing
// check does.
func TestReviewLoopCheckFailingAgainHalts(t *testing.T) {
	r := newRig(t)
	impl, check, review := implementCheckReview(r, `echo run >> runs.txt; test "$(wc -l < runs.txt)" -lt 2`)
	r.done(impl, "", "", "fixed again", "")
	r.settle()
	if c := r.await(check, registry.Done); c.Verdict != registry.Fail || !c.Halts() {
		t.Errorf("check round 2: %+v", c)
	}
	r.tick(2)
	if w := r.state(review, registry.Waiting); !strings.HasPrefix(w.Note, "waits: 2 check · run failed (exit 1 after ") {
		t.Errorf("review's note = %q", w.Note)
	}
}

// A worker's fail without on_fail halts the steps after it, as a failing
// check does, until routr approve: whatever kind of step gave it, and
// whether or not its brief asked for a verdict.
func TestWorkerFailHalts(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run("asked for a verdict "+strconv.FormatBool(want), func(t *testing.T) {
			r := newRig(t)
			hunt := r.add(registry.Worker{Role: "review", Chain: "c", Step: "hunt", WantVerdict: want, Brief: "Hunt.", Output: "HUNT.md"})
			next := r.addRun(registry.Worker{Chain: "c", Step: "next", After: []int{hunt}}, "echo next")
			r.tick(1)
			r.done(hunt, "HUNT.md", registry.Fail, "2 findings", "")
			r.settle()
			if h := r.get(hunt); h.Verdict != registry.Fail || !h.Halts() {
				t.Fatalf("hunt = %+v", h)
			}
			r.tick(2)
			n := r.state(next, registry.Waiting)
			if n.Note != "waits: 1 review · claude gave a fail verdict (2 findings); routr approve 1 lets the chain go on past it. Its output says why: HUNT.md" {
				t.Errorf("next's note = %q", n.Note)
			}
			r.s.Update(hunt, func(x *registry.Worker) { x.GoOn = true }) // routr approve 1
			r.await(next, registry.Done)
		})
	}
}

// loop queues again the run and judge steps between the target and the
// reviewer, and only those; its note names the agent steps it leaves.
func TestLoopQueuesTheChecksBetween(t *testing.T) {
	r := newRig(t)
	done := func(id int, after ...int) *registry.Worker {
		return &registry.Worker{ID: id, Runs: 1, Chain: "c", Step: "s" + strconv.Itoa(id), Role: "task", Harness: "claude", After: after,
			State: registry.Done, Verdict: registry.Pass, Summary: "ok", Finished: r.now, Output: "out.md"}
	}
	m := map[int]*registry.Worker{
		1: done(1),             // plan: before the target, left alone
		2: done(2, 1),          // implement: the target
		3: done(3, 2),          // build, a run step
		4: done(4, 3),          // lint, an agent step
		5: done(5, 4),          // ok, a judge
		6: done(6, 1),          // docs: not after the target
		7: done(7, 5, 6),       // review: x
		8: {ID: 8, Chain: "d"}, // another chain
	}
	m[3].Run, m[3].Pid, m[3].GoOn = "make", 42, true
	m[5].Judge = &registry.Judge{From: []int{4}, Winner: 4}
	m[6].Run = "true"
	x := m[7]
	x.Verdict, x.OnFail = registry.Fail, &registry.OnFail{Resume: 2, Brief: "Fix {{s7.final}}; {{s3.final}} has the log.", Max: 3}
	x.Turn, x.Replies = 1, 1 // a reply to a follow-up in the round before: not this round's
	m[2].Turn, m[2].Replies = 2, 2
	round1 := map[int]string{}
	for _, id := range []int{2, 3, 4, 5, 6, 7} {
		round1[id] = r.p.FinalPath(*m[id])
		registry.WriteFile(round1[id], []byte("round 1\n"))
	}
	r.w.loop(x, m)
	if x.State != registry.Waiting || !slices.Equal(x.After, []int{5, 6}) || x.RunFrom != 1 || x.Verdict != "" || x.Summary != "" || !x.Finished.IsZero() ||
		x.Note != "round 2: waiting on 2 task · claude to revise; 2 checks 3, 5 again first; not run again: step s4 (worker 4)" {
		t.Errorf("x = %+v", x)
	}
	// The target, x and the checks run again start over in a new run, whose
	// final is a file of its own: the revision names round 1's, which stay
	// where they were. The agent step it does not run again keeps its run.
	if tg := m[2]; tg.RunFrom != 2 || tg.Verdict != "" || tg.Summary != "" || tg.State != registry.Running || tg.Output != "out.md" ||
		len(tg.Inbox) != 1 || !strings.HasPrefix(tg.Inbox[0], "Fix "+round1[7]+"; "+round1[3]+" has the log.") {
		t.Errorf("target = %+v", tg)
	}
	for _, id := range []int{2, 3, 4, 5, 6, 7} {
		o := m[id]
		_, err := os.Stat(r.p.FinalPath(*o))
		if again := id != 4 && id != 6; again != (o.Runs == 2) || again != errors.Is(err, os.ErrNotExist) || again != (r.p.PrevFinal(*o) == round1[id]) {
			t.Errorf("worker %d: run %d, final %v, previous %q", id, o.Runs, err, r.p.PrevFinal(*o))
		}
		if got := r.read(round1[id]); got != "round 1\n" {
			t.Errorf("worker %d: round 1's final reads %q", id, got)
		}
	}
	for _, id := range []int{3, 5} {
		if o := m[id]; o.State != registry.Pending || o.Verdict != "" || o.Summary != "" || !o.Finished.IsZero() || o.Output != "" || o.Pid != 0 || o.GoOn ||
			!strings.Contains(o.Note, "runs again for 7 task · claude round 2") {
			t.Errorf("worker %d = %+v", id, o)
		}
	}
	if j := m[5].Judge; j.Winner != 0 || !slices.Equal(j.From, []int{4}) {
		t.Errorf("judge = %+v", j)
	}
	// No edge is added: being after it, they wait for the revision as they
	// are (admit), and the judge for the check before it.
	if !slices.Equal(m[3].After, []int{2}) || !slices.Equal(m[5].After, []int{4}) {
		t.Errorf("after: build %v, judge %v", m[3].After, m[5].After)
	}
	for _, id := range []int{1, 4, 6} {
		if m[id].State != registry.Done {
			t.Errorf("worker %d was reset: %+v", id, m[id])
		}
	}
}

// A loop queues the judges between the target and the reviewer again
// without touching their After: verdict or pick, each judges what it judged
// in the previous round, read back from the registry as the watcher (and a later retry)
// sees it. The reviewer, a verdict judge itself, also judges the revision.
func TestLoopKeepsWhatAJudgeJudges(t *testing.T) {
	for _, pick := range []bool{false, true} {
		t.Run("pick="+strconv.FormatBool(pick), func(t *testing.T) {
			r := newRig(t)
			for _, w := range []registry.Worker{
				{Step: "draft"},
				{Step: "check", Run: "true", After: []int{1}},
				{Step: "report", After: []int{2}},
				{Step: "judge_report", After: []int{3}, Judge: &registry.Judge{From: []int{3}, Pick: pick}},
				{Step: "review", After: []int{4}, Judge: &registry.Judge{From: []int{4}},
					OnFail: &registry.OnFail{Resume: 1, Max: 3, Brief: "revise"}},
			} {
				w.Chain, w.Role, w.Harness, w.State, w.Verdict = "c", "task", "claude", registry.Done, registry.Pass
				if _, err := r.s.Create(w); err != nil {
					t.Fatal(err)
				}
			}
			judges := func() (judge, review []int) {
				all, err := r.s.List()
				if err != nil {
					t.Fatal(err)
				}
				ids := func(id int) []int {
					st, _, err := r.w.judgeState(all[slices.IndexFunc(all, func(w registry.Worker) bool { return w.ID == id })], all)
					if err != nil {
						t.Fatal(err)
					}
					var out []int
					for _, u := range st.Upstream {
						out = append(out, u.Worker)
					}
					return out
				}
				return ids(4), ids(5)
			}
			before, _ := judges()
			err := r.s.Tx(func(m map[int]*registry.Worker) error {
				m[5].Verdict = registry.Fail
				r.w.loop(m[5], m)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if j := r.state(4, registry.Pending); !slices.Equal(j.After, []int{3}) || !slices.Equal(j.Judge.From, []int{3}) {
				t.Fatalf("judge 4: After %v, Judge %+v", j.After, j.Judge)
			}
			judge, review := judges()
			if !slices.Equal(judge, before) || !slices.Equal(judge, []int{3}) {
				t.Errorf("after the loop judge 4 judges %v, before %v", judge, before)
			}
			if !slices.Equal(review, []int{1, 4}) {
				t.Errorf("the reviewer judges %v, want the revision too: [1 4]", review)
			}
			// routr retry leaves After and Judge as they are (but Winner
			// and Accept): it still judges the same.
			if _, err := r.s.Update(4, func(x *registry.Worker) { x.State, x.Judge.Winner, x.Judge.Accept = registry.Pending, 0, false }); err != nil {
				t.Fatal(err)
			}
			if judge, _ := judges(); !slices.Equal(judge, []int{3}) {
				t.Errorf("after a retry judge 4 judges %v", judge)
			}
		})
	}
}
