package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/route"
	"github.com/bcherb2/herdr-routr/internal/watch"
	"github.com/bcherb2/herdr-routr/internal/watch/watchtest"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// cli runs routr commands in-process against a temp repo and a fake driver.
type cli struct {
	t       *testing.T
	fake    *watchtest.Fake
	dir     string
	p       registry.Paths
	env     map[string]string
	spawned atomic.Int32
	reach   error            // what connecting to herdr gives
	unready map[string]error // harness → why it can't run workers
	build   buildInfo        // what the binary says it was built from
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	dir := t.TempDir()
	c := &cli{t: t, fake: &watchtest.Fake{}, dir: dir, p: registry.Paths{Dir: filepath.Join(dir, ".routr")}}
	c.env = map[string]string{
		"ROUTR_STATE_DIR":        c.p.Dir,
		"HERDR_SOCKET_PATH":      "/tmp/fake-herdr.sock",
		"HERDR_WORKSPACE_ID":     "w1",
		"HERDR_PANE_ID":          "w1:p1",
		"XDG_CONFIG_HOME":        filepath.Join(dir, "xdg"),
		"CLAUDECODE":             "1",
		"CLAUDE_CODE_SESSION_ID": "sess-main",
	}
	return c
}

func (c *cli) app(stdin string, stdout, stderr *bytes.Buffer) *app {
	return &app{
		ctx: context.Background(), stdin: strings.NewReader(stdin), stdout: stdout, stderr: stderr,
		getenv: func(k string) string { return c.env[k] }, cwd: c.dir, bin: "/usr/local/bin/routr", build: c.build,
		dial:  func(string) worker.Driver { return c.fake },
		reach: func(context.Context, string) error { return c.reach },
		spawn: func(registry.Paths) error { c.spawned.Add(1); return nil },
		ready: func(_ context.Context, h string) error { return c.unready[h] },
		now:   time.Now,
		poll:  5 * time.Millisecond,
	}
}

// run runs one command and returns its exit code, stdout and stderr.
func (c *cli) run(stdin string, args ...string) (int, string, string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	code := c.app(stdin, &out, &errb).run(args)
	return code, out.String(), errb.String()
}

func (c *cli) mustRun(stdin string, args ...string) string {
	c.t.Helper()
	code, out, errOut := c.run(stdin, args...)
	if code != 0 {
		c.t.Fatalf("routr %v: exit %d\n%s%s", args, code, out, errOut)
	}
	return out
}

// watcher runs a fast in-process watcher until the test ends.
func (c *cli) watcher() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := &watch.Watcher{Paths: c.p, Dial: func(string) worker.Driver { return c.fake }, Bin: "/usr/local/bin/routr",
		Settle: 20 * time.Millisecond, Interval: 5 * time.Millisecond, IdleExit: time.Hour}
	go func() { defer close(done); w.Run(ctx) }()
	c.t.Cleanup(func() { cancel(); <-done })
}

func (c *cli) worker(id int) registry.Worker {
	c.t.Helper()
	w, err := c.p.Store().Get(id)
	if err != nil {
		c.t.Fatal(err)
	}
	return w
}

// chain is chain name's workers, in id order.
func (c *cli) chain(name string) []registry.Worker {
	c.t.Helper()
	all, err := c.p.Store().List()
	if err != nil {
		c.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(w registry.Worker) bool { return w.Chain != name })
}

// until polls cond for up to 5s.
func (c *cli) until(what string, cond func() bool) {
	c.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	c.t.Fatalf("timed out waiting for %s", what)
}

func TestStartPrintsTheIDAlone(t *testing.T) {
	c := newCLI(t)
	code, out, errOut := c.run("", "start", "--role", "review", "Red-team PLAN.md")
	if code != 0 || out != "1\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "queued worker 1 red-team-plan-md · codex") {
		t.Errorf("stderr = %q", errOut)
	}
	w := c.worker(1)
	if w.Harness != "codex" || w.Effort != "high" || w.Write || !w.WantVerdict || w.Preamble == "" || w.State != registry.Pending ||
		w.Socket != "/tmp/fake-herdr.sock" || w.Workspace != "w1" || w.Cwd != c.dir || w.Permissions != "default" {
		t.Errorf("worker = %+v", w)
	}
	if w.Caller == nil || w.Caller.Harness != "claude" || w.Caller.Session != "sess-main" {
		t.Errorf("caller = %+v", w.Caller)
	}
	if c.spawned.Load() != 1 {
		t.Error("start did not start the watcher")
	}
	if b, _ := os.ReadFile(filepath.Join(c.p.Dir, ".gitignore")); string(b) != "/state/\n/runs/\n/work/\n/trees/\n/.gitignore\n" {
		t.Errorf(".gitignore = %q", b)
	}
}

// Pi runs without approvals, so its workers get bypass unless a flag or
// config says otherwise.
func TestPiCallersWorkersBypass(t *testing.T) {
	c := newCLI(t)
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, "OK permissions default for workers started without --permissions (no flag:") {
		t.Errorf("doctor from claude:\n%s", out)
	}
	c.env["PI_CODING_AGENT"] = "true" // set by pi for every command its model runs; CLAUDECODE stays inherited
	c.mustRun("", "start", "Fix it")
	c.mustRun("", "start", "--permissions", "auto", "Fix it")
	c.mustRun("", "start", "--caller", "claude", "Fix it")
	chain := strings.TrimSpace(c.mustRun("", "flow", "start", "plan-redteam", "--task", "t"))
	for id, want := range map[int]string{1: "bypass", 2: "auto", 3: "default"} {
		if w := c.worker(id); w.Permissions != want {
			t.Errorf("worker %d: permissions %q, want %q (caller %+v)", id, w.Permissions, want, w.Caller)
		}
	}
	ws, _ := c.p.Store().List()
	for _, w := range ws {
		if w.Chain == chain && w.Permissions != "bypass" {
			t.Errorf("flow step %s: permissions %q", w.Step, w.Permissions)
		}
	}
	if out := c.mustRun("", "roles"); !strings.HasPrefix(out, "caller: pi · workers started without --permissions get: bypass\n") {
		t.Errorf("roles from pi:\n%s", out)
	}
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, "OK caller pi") ||
		!strings.Contains(out, "OK permissions bypass for workers started without --permissions (the mode pi, the caller, runs in)") {
		t.Errorf("doctor from pi:\n%s", out)
	}
	if err := os.WriteFile(c.p.Config(), []byte("permissions = \"default\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.Atoi(strings.TrimSpace(c.mustRun("", "start", "Fix it")))
	if w := c.worker(id); w.Permissions != "default" {
		t.Errorf("with config permissions = default: %q", w.Permissions)
	}
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, "OK permissions default for workers started without --permissions (config permissions)") {
		t.Errorf("doctor with config:\n%s", out)
	}
}

func TestPipeJoinsTheUpstreamChain(t *testing.T) {
	c := newCLI(t)
	c.mustRun("", "start", "--role", "plan", "--in", "DECISIONS.md", "Write PLAN.md for {{in}}")
	out := c.mustRun("1\n", "start", "--after", "-", "--role", "review", "Red-team {{prev.output}}")
	if out != "2\n" {
		t.Fatalf("stdout = %q", out)
	}
	one, two := c.worker(1), c.worker(2)
	if one.Chain != "c1" || two.Chain != "c1" || len(two.After) != 1 || two.After[0] != 1 || one.Params["in"] != "DECISIONS.md" || one.Output != "PLAN.md" {
		t.Errorf("1 = %+v\n2 = %+v", one, two)
	}
	c.mustRun("2", "start", "--after", "-", "--role", "implement", "Implement {{plan.output}}")
	if w := c.worker(3); w.Chain != "c1" || w.Gate != "you" || !w.Write {
		t.Errorf("3 = %+v", w)
	}
	if code, _, errOut := c.run("1", "start", "--after", "-", "-"); code != exitUsage || !strings.Contains(errOut, "not both") {
		t.Errorf("brief and --after both from stdin: exit %d %s", code, errOut)
	}
	if code, _, errOut := c.run("", "start", "--after", "-", "x {{prev.output}}"); code != exitError || !strings.Contains(errOut, "command before the pipe failed") {
		t.Errorf("empty pipe: exit %d %s", code, errOut)
	}
	if code, _, errOut := c.run("", "start", "--after", "9", "x"); code != exitError || !strings.Contains(errOut, "--after 9") {
		t.Errorf("unknown upstream: exit %d %s", code, errOut)
	}
}

func TestStartRejectsWhatCannotWork(t *testing.T) {
	c := newCLI(t)
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"start", "Fix {{task}}"}, exitUsage, "--param task="},
		{[]string{"start", "Review {{prev.output}}"}, exitUsage, "needs --after"},
		{[]string{"start", "--role", "nope", "x"}, exitError, "unknown role"},
		{[]string{"start", "--gate", "ci", "x"}, exitUsage, "only gate"},
		{[]string{"start", "--param", "a.b=1", "x"}, exitUsage, "key=value"},
		{[]string{"start", "--harness", "auto", "x"}, exitError, "Jev"},
		{[]string{"start"}, exitUsage, "one brief"},
		{[]string{"start", "--bogus", "x"}, exitUsage, "bogus"},
		{[]string{"start", "--title", "!!", "x"}, exitUsage, "--title"},
		{[]string{"start", "--param", "work=x", "x"}, exitUsage, "{{work}} is routr's"},
		{[]string{"start", "--chain", "../up", "x"}, exitUsage, ".routr/work/../up"},
		{[]string{"start", "--chain", "a..b", "x"}, exitUsage, "it names git branches"},
		{[]string{"flow", "start", "plan-redteam", "--task", "t", "--chain", "x.lock"}, exitUsage, "it names git branches"},
	} {
		if code, _, errOut := c.run("", tt.args...); code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: exit %d %q, want %d with %q", tt.args, code, errOut, tt.code, tt.want)
		}
	}
	delete(c.env, "HERDR_SOCKET_PATH")
	if code, _, errOut := c.run("", "start", "x"); code != exitError || !strings.Contains(errOut, "HERDR_SOCKET_PATH") {
		t.Errorf("outside herdr: exit %d %s", code, errOut)
	}
	c.mustRun("", "start", "--socket", "/s", "--param", "task=calc", "Fix {{task}} in {{work}} of {{chain}}")
}

func TestStartTitles(t *testing.T) {
	c := newCLI(t)
	for i, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"start", "--title", "Fix the login race", "Look into auth.go"}, "fix-the-login-race"},
		{[]string{"start", "\n\n# Review the plan\nin detail"}, "review-the-plan"},
		{[]string{"start", "--param", "in=a.md", "{{in}}\nTighten {{in}} up"}, "tighten-up"},
		{[]string{"start", "Name dispatched workspaces readably"}, "name-dispatched-wor~"},
		{[]string{"start", "{{task}}", "--param", "task=x"}, ""},
	} {
		c.mustRun("", tt.args...)
		w := c.worker(i + 1)
		if w.Title != tt.want {
			t.Errorf("%v: title %q, want %q", tt.args, w.Title, tt.want)
		}
	}
	if w := c.worker(5); w.Label() != "5 task · claude" {
		t.Errorf("untitled label %q", w.Label())
	}
	c.watcher()
	c.until("worker 1 started", func() bool { return c.worker(1).Live != nil })
	if ps := c.fake.Panes(1); len(ps) != 1 || ps[0].Spec.Label != "1 fix-the-login-race · claude" {
		t.Errorf("tabs %+v", ps)
	}
	if out := c.mustRun("", "list"); !strings.Contains(out, "TITLE") || !strings.Contains(out, "fix-the-login-race") {
		t.Errorf("list:\n%s", out)
	}
}

// Codex runs `routr start` in its sandbox, which may not connect to herdr;
// a watcher spawned there would fail the worker. With no watcher running,
// start, flow start and send refuse there, queueing nothing.
func TestSandboxedCommandsQueueNothing(t *testing.T) {
	c := newCLI(t)
	id := c.mustRun("", "start", "x") // queued while herdr was reachable
	c.reach = &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.EPERM)}
	for _, args := range [][]string{
		{"start", "y"},
		{"flow", "start", "plan-redteam", "--task", "t"},
		{"send", strings.TrimSpace(id), "more"},
	} {
		code, _, errOut := c.run("", args...)
		if code != exitError || !strings.Contains(errOut, "nothing was queued") || !strings.Contains(errOut, "outside the sandbox") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	if all, _ := c.p.Store().List(); len(all) != 1 || len(all[0].Inbox) != 0 {
		t.Errorf("queued in the sandbox: %+v", all)
	}
	c.reach = &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if code, _, errOut := c.run("", "start", "y"); code != exitError || !strings.Contains(errOut, "Is its server running?") {
		t.Errorf("herdr down: exit %d %q", code, errOut)
	}
	// A watcher already running outside the sandbox does the herdr work.
	release, err := watch.Hold(c.p)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	c.mustRun("", "start", "y")
}

func TestWaitTimesOutWithAHint(t *testing.T) {
	c := newCLI(t)
	c.mustRun("", "start", "x")
	code, out, _ := c.run("", "wait", "1", "--timeout", "30ms")
	if code != exitRunning || out != "still running: 1 x · claude (pending). Run: routr wait 1\n" {
		t.Errorf("exit %d, %q", code, out)
	}
	code, out, _ = c.run("1\n", "wait", "-", "--timeout", "1ms")
	if code != exitRunning || !strings.Contains(out, "routr wait 1") {
		t.Errorf("wait -: exit %d %q", code, out)
	}
}

func TestEndToEndWithTheWatcher(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	c.mustRun("", "start", "--output", "NOTES.md", "Fix calc.py")
	c.until("worker 1 running", func() bool { return c.worker(1).State == registry.Running })

	// The worker writes its output, runs `routr done`, then ends its turn.
	os.WriteFile(filepath.Join(c.dir, "NOTES.md"), []byte("notes"), 0o644)
	c.env["ROUTR_ID"], c.env["ROUTR_RUN_DIR"] = "1", c.p.RunDir(1)
	out := c.mustRun("", "done", "--output", "NOTES.md", "fixed the sign")
	if !strings.Contains(out, "End your turn") {
		t.Errorf("done said %q", out)
	}
	delete(c.env, "ROUTR_ID")
	delete(c.env, "ROUTR_RUN_DIR")
	c.fake.Finish(1, "Changed a - b to a + b.")

	out = c.mustRun("", "wait", "1")
	if out != "fixed the sign\n\nChanged a - b to a + b.\noutput: NOTES.md\n" {
		t.Errorf("wait printed %q", out)
	}
	if got := c.mustRun("", "result", "1"); got != out {
		t.Errorf("result printed %q", got)
	}

	// A follow-up resumes the closed session; wait prints the reply.
	c.mustRun("", "send", "1", "why that line?")
	c.until("the follow-up prompt", func() bool { return len(c.fake.Prompts(1)) == 2 })
	c.fake.Finish(1, "Because it subtracted.")
	out = c.mustRun("", "wait", "1")
	if !strings.HasPrefix(out, "reply to follow-up 1:\nBecause it subtracted.") {
		t.Errorf("after send: %q", out)
	}
	if w := c.worker(1); w.State != registry.Done || w.Live != nil {
		t.Errorf("after the reply the worker is done and its tab closed: %+v", w)
	}
	var v view
	if err := json.Unmarshal([]byte(c.mustRun("", "result", "1", "--json")), &v); err != nil || v.RunDir != c.p.RunDir(1) || v.FinalPath != c.final(1) || v.Reply != "Because it subtracted." || v.Final == "" {
		t.Errorf("result --json: %+v %v", v, err)
	}
}

func TestSendWaitAndPipeToWait(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	id := strings.TrimSpace(c.mustRun("", "start", "go"))
	c.until("running", func() bool { return c.worker(1).State == registry.Running })
	c.fake.Finish(1, "first turn over") // idle, so the follow-up can go in
	go func() {
		c.until("the follow-up prompt", func() bool { return len(c.fake.Prompts(1)) == 2 })
		c.fake.Finish(1, "pong")
	}()
	out := c.mustRun("", "send", "--wait", id, "ping")
	if !strings.Contains(out, "pong") {
		t.Errorf("send --wait: %q", out)
	}
}

// A worker whose start failed never ran a harness: there is no session to
// send to, and routr send says what starts it instead.
func TestSendRefusesAWorkerThatNeverStarted(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{Harness: "claude", Brief: "go", State: registry.Failed, Error: "could not start claude: herdr agent.start: agent_pane_busy\nmore"})
	code, _, errOut := c.run("", "send", "1", "why?")
	if code != exitError || !strings.Contains(errOut, "worker 1 failed (could not start claude: herdr agent.start: agent_pane_busy) and has no session to send to") ||
		!strings.Contains(errOut, "It never started. routr retry 1 starts it again with its brief") {
		t.Errorf("send: exit %d\n%s", code, errOut)
	}
	if w := c.worker(1); len(w.Inbox) != 0 {
		t.Errorf("queued anyway: %+v", w)
	}
}

// A failed worker that started but has no session (its harness recorded
// none) has nothing to resume either: routr send refuses it with what its
// record says to do, routr retry, instead of queueing a follow-up that
// would be dropped.
func TestSendRefusesAFailedWorkerWithNoSession(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{Harness: "codex", Brief: "go", State: registry.Failed, Started: time.Now(), Error: "the harness exited before it ran routr done"})
	code, _, errOut := c.run("", "send", "1", "why?")
	if code != exitError || !strings.Contains(errOut, "worker 1 failed (the harness exited before it ran routr done) and has no session to send to") ||
		!strings.Contains(errOut, "routr retry 1 runs it again") {
		t.Errorf("send: exit %d\n%s", code, errOut)
	}
	if w := c.worker(1); len(w.Inbox) != 0 {
		t.Errorf("queued anyway: %+v", w)
	}
}

// routr retry queues failed workers again in their place: one that never
// started gets its brief afresh, one that ran is rerun in its session, and
// the workers after them go on as before.
func TestRetry(t *testing.T) {
	c := newCLI(t)
	now := time.Now()
	c.seed(
		registry.Worker{Chain: "c", Step: "a", Harness: "claude", Brief: "Do a.", State: registry.Failed,
			Error: "could not start claude: busy", Session: "phantom"},
		registry.Worker{Chain: "c", Step: "b", Harness: "claude", Brief: "Do b.", After: []int{1}, State: registry.Waiting, Note: "waiting on 1 task · claude, which is failed"},
		registry.Worker{Chain: "r", Step: "plan", Harness: "claude", Brief: "Plan.", State: registry.Done, Started: now, Finished: now, Session: "sess-p"},
		registry.Worker{Chain: "r", Step: "review", Harness: "claude", Brief: "Review it.", Round: 2, Started: now, Session: "sess-r",
			State: registry.Failed, Error: "could not start claude: dialog", Live: &registry.Live{Pane: "p1"}},
	)
	c.fake.Start(context.Background(), worker.Spec{ID: 4}) // the tab its failed start left: p1

	for _, tt := range []struct{ target, want string }{
		{"3", "worker 3 is done"},
		{"2", "worker 2 is waiting, not failed"},
		{"9", "no worker 9"},
	} {
		if code, _, errOut := c.run("", "retry", tt.target); code != exitError || !strings.Contains(errOut, tt.want) {
			t.Errorf("retry %s: exit %d\n%s", tt.target, code, errOut)
		}
	}
	if w := c.worker(2); w.State != registry.Waiting {
		t.Errorf("a refused retry changed worker 2: %s", w.State)
	}

	c.mustRun("", "retry", "1")
	if w := c.worker(1); w.State != registry.Pending || w.Session != "" || w.Error != "" {
		t.Errorf("retried first start: %+v", w)
	}
	c.mustRun("", "retry", "r") // only its failed worker
	if w := c.worker(3); w.State != registry.Done {
		t.Errorf("retry r touched its done worker: %s", w.State)
	}
	if w := c.worker(4); w.State != registry.Pending || w.Session != "sess-r" || w.Live != nil || c.fake.Open(4) != 0 {
		t.Errorf("retried rerun: %+v, %d tabs open", w, c.fake.Open(4))
	}
	if code, _, errOut := c.run("", "retry", "r"); code != exitError || !strings.Contains(errOut, "nothing to retry in r") {
		t.Errorf("retry r again: exit %d\n%s", code, errOut)
	}
	if b, _ := os.ReadFile(c.p.WatchLog()); !strings.Contains(string(b), "queued 1 task · claude again by routr retry") {
		t.Errorf("watch.log:\n%s", b)
	}

	c.watcher()
	c.until("worker 1 running", func() bool { return c.worker(1).State == registry.Running })
	if p := c.fake.Panes(1); len(p) != 1 || p[0].Spec.Session != "" || !strings.HasPrefix(p[0].Prompts[0], "Do a.") {
		t.Errorf("worker 1 restarted as: %+v", p)
	}
	c.until("worker 4 running", func() bool { return c.worker(4).State == registry.Running })
	if p := c.fake.Panes(4); len(p) != 2 || p[1].Spec.Session != "sess-r" || !strings.HasPrefix(p[1].Prompts[0], "The work was revised; review it again (round 2)") {
		t.Errorf("worker 4 rerun as: %+v", p)
	}
	c.mustRun("", "done", "--id", "1", "did a")
	c.fake.Finish(1, "did a")
	c.until("worker 2 running after it", func() bool { return c.worker(2).State == registry.Running })
}

// routr retry starts a new run: what the failed one concluded (its final,
// verdict, summary, finish time) becomes history, and replies so far are
// its. routr result then has no result of this run and points to the
// previous run's final, which stays where it was. A second retry does the
// same for the next one.
func TestRetryStartsANewRun(t *testing.T) {
	c := newCLI(t)
	now := time.Now()
	c.seed(registry.Worker{Harness: "claude", Brief: "Do it.", State: registry.Failed, Started: now, Finished: now, Session: "sess-1",
		Summary: "old success", Verdict: registry.Pass, Error: "could not resume its session", Turn: 1, Replies: 1, Interactive: true})
	registry.WriteFile(c.final(1), []byte("old success\n"))
	os.Chtimes(c.final(1), now.Add(-time.Minute), now.Add(-time.Minute))
	registry.WriteFile(c.p.TurnFile(1, 1), []byte("OLD REPLY\n"))

	if code, out, _ := c.run("", "result", "1"); code != exitOK || !strings.HasPrefix(out, "failed: 1 task · claude: could not resume its session\nroutr send 1 … resumes its session\n\nreply to follow-up 1:\nOLD REPLY") {
		t.Errorf("result before retry: exit %d\n%s", code, out)
	}
	for n := 1; n <= 2; n++ {
		c.mustRun("", "retry", "1")
		w := c.worker(1)
		if w.Verdict != "" || w.Summary != "" || !w.Finished.IsZero() || w.Error != "" || w.Session != "sess-1" || w.RunFrom != 1 {
			t.Errorf("retry %d kept the old run's result: %+v", n, w)
		}
		archived := filepath.Join(c.p.RunDir(1), fmt.Sprintf("final-%d.md", n))
		if _, err := os.Stat(c.final(1)); w.Runs != n+1 || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("retry %d: run %d has a final: %v", n, w.Runs, err)
		}
		if b, _ := os.ReadFile(filepath.Join(c.p.RunDir(1), "final-1.md")); string(b) != "old success\n" {
			t.Errorf("run 1's final = %q", b)
		}
		code, out, errOut := c.run("", "result", "1")
		if code != exitError || strings.Contains(out, "OLD REPLY") || strings.Contains(out, "verdict") ||
			!strings.Contains(out, "previous run's final message: "+archived+"\n") ||
			!strings.Contains(errOut, "worker 1 has no final message yet (pending). Run: routr wait 1") {
			t.Errorf("result after retry %d: exit %d\nstdout %q\nstderr %q", n, code, out, errOut)
		}
		c.p.Store().Update(1, func(w *registry.Worker) { w.State, w.Error = registry.Failed, "failed again" })
		registry.WriteFile(c.final(1), []byte("second run\n"))
	}
}

// A judge a review loop queued again fails and is retried: it still comes
// after what it did, and still judges only what it judged.
func TestRetryKeepsWhatAJudgeJudges(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{State: registry.Done}, registry.Worker{State: registry.Done, Run: "true", After: []int{1}},
		registry.Worker{State: registry.Done, After: []int{2}},
		registry.Worker{State: registry.Failed, After: []int{3}, Error: "interrupted judge",
			Judge: &registry.Judge{From: []int{3}, Winner: 3}})
	c.mustRun("", "retry", "4")
	if w := c.worker(4); w.State != registry.Pending || !slices.Equal(w.After, []int{3}) ||
		!slices.Equal(w.Judge.From, []int{3}) || w.Judge.Winner != 0 {
		t.Errorf("after retry: %+v, judge %+v", w, w.Judge)
	}
}

// An interactive worker stays running once it has answered a follow-up, so
// `routr result` took the reply it had just settled for an earlier run's:
// labelled previous, exit 1 with Run: routr wait.
func TestResultCurrentInteractiveReply(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	c.mustRun("", "start", "--interactive", "Discuss it")
	c.until("started", func() bool { return c.worker(1).Live != nil })
	c.fake.Finish(1, "ready")
	c.mustRun("", "send", "1", "answer this")
	c.until("turn delivered", func() bool { return c.worker(1).Turn == 1 })
	c.fake.Finish(1, "CURRENT ANSWER")
	c.until("reply settled", func() bool { return c.worker(1).Replies == 1 })
	code, out, errOut := c.run("", "result", "1")
	if w := c.worker(1); w.State != registry.Running || code != exitOK || errOut != "" || out != "reply to follow-up 1:\nCURRENT ANSWER\n" {
		t.Errorf("current reply: state %s, exit %d\nstdout %q\nstderr %q", w.State, code, out, errOut)
	}
}

// The reply to a follow-up is a result only if that follow-up came in this
// run (after RunFrom); without one routr result points to the previous
// run's final. RunFrom 0, a record from before routr retry, takes every
// reply as this run's.
func TestLatestReplyIsThisRuns(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{Harness: "claude", State: registry.Running, Started: time.Now(), Interactive: true, Turn: 1, Replies: 1, RunFrom: 1})
	c.p.Store().Update(1, func(w *registry.Worker) { w.Runs = 2 })
	registry.WriteFile(c.p.TurnFile(1, 1), []byte("OLD REPLY\n"))
	registry.WriteFile(filepath.Join(c.p.RunDir(1), "final-1.md"), []byte("old final\n"))
	if cur := c.p.Current(c.worker(1)); cur != (registry.Result{}) {
		t.Errorf("a reply up to RunFrom or the run before's final is this run's: %+v", cur)
	}
	code, out, errOut := c.run("", "result", "1")
	if code != exitError || strings.Contains(out, "OLD REPLY") || !strings.Contains(out, "previous run's final message: ") ||
		!strings.Contains(out, "final-1.md") || !strings.Contains(errOut, "worker 1 has no final message yet") {
		t.Errorf("reply before RunFrom: exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	c.p.Store().Update(1, func(w *registry.Worker) { w.RunFrom = 0 })
	if cur := c.p.Current(c.worker(1)); cur.Turn != 1 || cur.Reply != "OLD REPLY\n" {
		t.Errorf("RunFrom 0: %+v", cur)
	}
	if code, out, _ := c.run("", "result", "1"); code != exitOK || out != "reply to follow-up 1:\nOLD REPLY\n" {
		t.Errorf("RunFrom 0: exit %d %q", code, out)
	}
}

// routr result --path prints the file holding what routr result prints:
// the final message, or the reply when that is the current result. With
// neither it is routr result: the same exit code and the same text.
func TestResultPath(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{Harness: "claude", State: registry.Done, Started: time.Now(), Runs: 1},
		registry.Worker{Harness: "claude", State: registry.Running, Started: time.Now(), Interactive: true, Runs: 1},
		registry.Worker{Harness: "claude", State: registry.Done, Started: time.Now(), Runs: 1},
		registry.Worker{Harness: "claude", State: registry.Failed, Error: "it broke", Started: time.Now(), Session: "s", Runs: 1})
	registry.WriteFile(c.final(1), []byte("FINAL\n"))
	os.Chtimes(c.final(1), time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	if code, out, errOut := c.run("", "result", "1", "--path"); code != exitOK || out != c.final(1)+"\n" || errOut != "" {
		t.Errorf("final: exit %d %q %q", code, out, errOut)
	}
	c.p.Store().Update(1, func(w *registry.Worker) { w.Turn, w.Replies = 1, 1 })
	registry.WriteFile(c.p.TurnFile(1, 1), []byte("REPLY\n"))
	if code, out, _ := c.run("", "result", "--path", "1"); code != exitOK || out != c.p.TurnFile(1, 1)+"\n" {
		t.Errorf("reply: exit %d %q", code, out)
	}
	for id := 2; id <= 4; id++ {
		code, out, errOut := c.run("", "result", strconv.Itoa(id))
		pcode, pout, perr := c.run("", "result", strconv.Itoa(id), "--path")
		if pcode != code || pout != out || perr != errOut {
			t.Errorf("worker %d with no result: --path exit %d %q %q, result exit %d %q %q", id, pcode, pout, perr, code, out, errOut)
		}
	}
	if code, _, errOut := c.run("", "result", "1", "--path", "--json"); code != exitUsage || !strings.Contains(errOut, "--path") {
		t.Errorf("--path --json: exit %d %q", code, errOut)
	}
}

// A review loop's next round starts the reviewer over: its fail verdict
// and final become history. Once the reviewer, running again, settled a
// follow-up, routr result must not print the old verdict under the new reply
// as if it were this round's. It has no verdict until it gives
// one in this round; before its reply it points to the last round's final.
// The path each revision was handed still reads that round's review after
// the rounds that follow.
func TestLoopStartsTheReviewerOver(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{State: registry.Done, Harness: "claude", Session: "sess-1", Started: time.Now(), Chain: "c", Step: "draft"},
		registry.Worker{State: registry.Pending, Harness: "claude", Brief: "Review it.", Chain: "c", Step: "review",
			Interactive: true, After: []int{1}, OnFail: &registry.OnFail{Resume: 1, Max: 5, Brief: "Fix {{review.final}}."}})
	c.watcher()
	c.until("reviewer started", func() bool { return c.worker(2).Live != nil })
	for round := 2; round <= 3; round++ {
		c.mustRun("", "done", "--id", "2", "--verdict", "fail", fmt.Sprintf("revise round %d", round-1))
		c.fake.Finish(2, "review done")
		c.until("next round", func() bool { return c.worker(2).Round == round && c.worker(2).State == registry.Waiting })
		archived := filepath.Join(c.p.RunDir(2), fmt.Sprintf("final-%d.md", round-1))
		if w := c.worker(2); w.Verdict != "" || w.Summary != "" {
			t.Errorf("round %d kept the reviewer's verdict: %+v", round, w)
		}
		if code, out, _ := c.run("", "result", "2"); code != exitError || !strings.Contains(out, "previous run's final message: "+archived) || strings.Contains(out, "verdict") {
			t.Errorf("round %d, waiting: exit %d\n%s", round, code, out)
		}
		if js := c.mustRun("", "result", "2", "--json"); strings.Contains(js, `"reply"`) || strings.Contains(js, `"final"`) || strings.Contains(js, `"final_path"`) {
			t.Errorf("round %d, waiting, --json has the round before's result:\n%s", round, js)
		}
		c.until("revision delivered", func() bool { return c.worker(1).Turn == round-1 })
		if p := c.fake.Panes(1); !strings.Contains(strings.Join(p[len(p)-1].Prompts, "\n"), "Fix "+archived+".") {
			t.Errorf("round %d: the revision does not point to the review it answers (%s): %q", round, archived, p[len(p)-1].Prompts)
		}
		c.mustRun("", "done", "--id", "1", "revised")
		c.fake.Finish(1, "revised")
		c.until("reviewer running again", func() bool { return c.worker(2).State == registry.Running && c.worker(2).Live != nil })
		c.fake.Finish(2, "ready")
		c.mustRun("", "send", "2", "answer this")
		c.until("follow-up delivered", func() bool { return c.worker(2).Turn == round-1 })
		c.fake.Finish(2, "CURRENT REVIEW REPLY")
		c.until("follow-up settled", func() bool { return c.worker(2).Replies == round-1 })
		code, out, errOut := c.run("", "result", "2")
		want := fmt.Sprintf("reply to follow-up %d:\nCURRENT REVIEW REPLY\n", round-1)
		if w := c.worker(2); w.State != registry.Running || code != exitOK || errOut != "" || out != want {
			t.Errorf("round %d, current reply: state %s, exit %d\nstdout %q\nstderr %q", round, w.State, code, out, errOut)
		}
	}
	c.mustRun("", "done", "--id", "2", "--verdict", "pass", "good now")
	c.fake.Finish(2, "good now")
	c.until("reviewer done", func() bool { return c.worker(2).State == registry.Done })
	if code, out, _ := c.run("", "result", "2"); code != exitOK || !strings.HasSuffix(out, "\nverdict: pass\n") || strings.Contains(out, "previous") {
		t.Errorf("verdict of this round: exit %d\n%s", code, out)
	}
	for round := 1; round <= 2; round++ {
		if got := readFile(filepath.Join(c.p.RunDir(2), fmt.Sprintf("final-%d.md", round))); !strings.HasPrefix(got, fmt.Sprintf("revise round %d\n", round)) {
			t.Errorf("round %d's final now reads %q", round, got)
		}
	}
}

// A review loop starts its target over even where the target's run
// directory is read-only: the new run needs no file moved, so routr result
// never shows the previous round's final as this round's. Were a move
// needed, it would fail there and the round would go on with the old final
// in the current slot (exit 0, OLD DRAFT SUMMARY). The path the round handed out
// still reads that round's final.
func TestLoopRoundKeepsItsFinal(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{State: registry.Done, Harness: "claude", Session: "sess-1", Started: time.Now(), Chain: "c", Step: "draft"},
		registry.Worker{State: registry.Pending, Harness: "claude", Brief: "Review it.", Chain: "c", Step: "review", After: []int{1},
			OnFail: &registry.OnFail{Resume: 1, Max: 3, Brief: "Fix {{review.final}}; {{draft.final}} was the draft."}})
	round1 := c.final(1)
	if err := registry.WriteFile(round1, []byte("OLD DRAFT SUMMARY")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.p.RunDir(1), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(c.p.RunDir(1), 0o700) })
	c.watcher()
	c.until("reviewer started", func() bool { return c.worker(2).State == registry.Running })
	c.mustRun("", "done", "--id", "2", "--verdict", "fail", "revise again")
	c.fake.Finish(2, "review done")
	c.until("new round", func() bool { return c.worker(2).Round == 2 })
	code, out, errOut := c.run("", "result", "1")
	if code == exitOK || strings.Contains(out, "OLD DRAFT SUMMARY") || !strings.Contains(out, "previous run's final message: "+round1+"\n") {
		t.Errorf("result of the target in round 2: exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	if got := readFile(round1); got != "OLD DRAFT SUMMARY" {
		t.Errorf("round 1's final reads %q", got)
	}
	c.until("revision delivered", func() bool { return c.worker(1).Turn == 1 })
	if p := c.fake.Panes(1); !strings.Contains(strings.Join(p[len(p)-1].Prompts, "\n"), "; "+round1+" was the draft.") {
		t.Errorf("the revision does not name round 1's draft %s: %q", round1, p[len(p)-1].Prompts)
	}
}

// routr result --json after a successful retry has neither the failed
// run's final nor its reply: the reply came in a run before RunFrom.
func TestResultJSONAfterRetry(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{State: registry.Failed, Started: time.Now(), Session: "s1", Turn: 1, Replies: 1})
	if err := registry.WriteFile(c.final(1), []byte("OLD FINAL")); err != nil {
		t.Fatal(err)
	}
	if err := registry.WriteFile(c.p.TurnFile(1, 1), []byte("OLD REPLY WITH VERDICT FAIL")); err != nil {
		t.Fatal(err)
	}
	c.mustRun("", "retry", "1")
	out := c.mustRun("", "result", "1", "--json")
	if strings.Contains(out, `"reply"`) || strings.Contains(out, "OLD") || strings.Contains(out, `"final_path"`) {
		t.Errorf("result --json after retry:\n%s", out)
	}
	if out := c.mustRun("", "list", "--json", "--final"); strings.Contains(out, "OLD") {
		t.Errorf("list --json --final after retry:\n%s", out)
	}
}

// A record from before runs were numbered (no runs) keeps its final.md:
// routr result and --json still read it, and its first new run points
// back to it.
func TestLegacyRecordReadsFinalMd(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{State: registry.Failed, Started: time.Now(), Session: "s1", Error: "it broke", Verdict: registry.Fail})
	c.p.Store().Update(1, func(w *registry.Worker) { w.Runs = 0 })
	legacy := filepath.Join(c.p.RunDir(1), registry.FinalFile)
	if err := registry.WriteFile(legacy, []byte("LEGACY FINAL\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(c.p.Registry()); strings.Contains(string(b), `"runs"`) {
		t.Fatalf("the record has a runs field:\n%s", b)
	}
	if code, out, _ := c.run("", "result", "1"); code != exitOK || out != "failed: 1 task: it broke\nroutr send 1 … resumes its session\n\nLEGACY FINAL\nverdict: fail\n" {
		t.Errorf("legacy result: exit %d\n%q", code, out)
	}
	var v view
	if err := json.Unmarshal([]byte(c.mustRun("", "result", "1", "--json")), &v); err != nil || v.FinalPath != legacy || v.Final != "LEGACY FINAL\n" {
		t.Errorf("legacy --json: %v, %+v", err, v)
	}
	c.mustRun("", "retry", "1")
	if w := c.worker(1); w.Runs != 1 || c.final(1) == legacy {
		t.Errorf("after retry: run %d, final %s", w.Runs, c.final(1))
	}
	if code, out, _ := c.run("", "result", "1"); code == exitOK || strings.Contains(out, "LEGACY FINAL") || !strings.Contains(out, "previous run's final message: "+legacy+"\n") {
		t.Errorf("legacy after retry: exit %d\n%q", code, out)
	}
}

// final is the path of worker id's final message in its current run.
func (c *cli) final(id int) string { return c.p.FinalPath(c.worker(id)) }

// seed writes workers straight into the registry.
func (c *cli) seed(ws ...registry.Worker) {
	c.t.Helper()
	s := c.p.Store()
	for _, w := range ws {
		if w.Role == "" {
			w.Role = "task"
		}
		if w.Cwd == "" {
			w.Cwd = c.dir
		}
		if _, err := s.Create(w); err != nil {
			c.t.Fatal(err)
		}
	}
}

// roles names the model each role picks, the harness's own when none is
// pinned, and the models --model takes on each harness, from route's table,
// so a caller need not guess one.
func TestRolesListsModelAliases(t *testing.T) {
	c := newCLI(t)
	out := c.mustRun("", "roles")
	for _, want := range []string{
		"caller: claude · workers started without --permissions get: default\n",
		"claude · default model ", "claude · sonnet · low",
		"effort low (research): fast and cheap; pass --effort high for open-ended brainstorming or synthesis.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("roles lacks %q:\n%s", want, out)
		}
	}
	for _, h := range route.Defaults() {
		for m := range h.Models {
			if m != route.PiDefaultModel && !strings.Contains(out, m) {
				t.Errorf("roles lacks %s's model %q:\n%s", h.Kind, m, out)
			}
		}
	}
	if !strings.Contains(out, "--model: claude haiku|opus|sonnet · codex ") || !strings.Contains(out, "pi provider/id") {
		t.Errorf("roles lacks the --model line:\n%s", out)
	}
	_, help, _ := c.run("", "start", "--help")
	if !strings.Contains(help, "claude haiku|opus|sonnet") {
		t.Errorf("start --help: --model names no models:\n%s", help)
	}
}

// routr answer --expect presses nothing on a question other than the one
// decided, and --by records who chose, on the worker and in watch.log. An
// answer by agent or jev needs --expect; the user's does not, whoever runs
// routr.
func TestAnswerExpectAndBy(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	c.mustRun("", "start", "Look into auth.go")
	c.until("worker 1 started", func() bool { return c.worker(1).Live != nil })
	c.fake.Ask(1, "Allow curl?", "Yes", "No")
	c.until("worker 1 blocked", func() bool { return c.worker(1).State == registry.Blocked })

	var listed []map[string]any
	json.Unmarshal([]byte(c.mustRun("", "list", "--json")), &listed)
	fp := watchtest.Fingerprint("Allow curl?", []string{"Yes", "No"})
	if len(listed) != 1 || listed[0]["fingerprint"] != fp || listed[0]["block_seq"] == nil || len(listed[0]["options"].([]any)) != 2 {
		t.Fatalf("list --json: %v", listed)
	}
	log := func() string { b, _ := os.ReadFile(c.p.WatchLog()); return string(b) }
	for _, tt := range []struct {
		args []string
		code int
		want string // on stderr
	}{
		{[]string{"answer", "1", "2", "--by", "someone"}, exitUsage, `--by "someone": use human, agent or jev`},
		// An answer decided away from the screen names the question it decided.
		{[]string{"answer", "1", "1", "--by", "agent"}, exitUsage, "--by agent needs --expect FP: pass the worker's fingerprint from the [routr] message, routr wait or routr list --json"},
		{[]string{"answer", "1", "1", "--by", "jev"}, exitUsage, "--by jev needs --expect FP"},
		{[]string{"answer", "1", "2", "--expect", "0123456789abcdef", "--by", "jev"}, exitNeedsYou,
			"1 look-into-auth-go · claude asks another question now, so no option was chosen. On its screen (fingerprint " + fp + "):\n" +
				"Allow curl?\nDecide again, then: routr answer 1 N --expect " + fp + " --by jev"},
		{[]string{"answer", "1", "3", "--expect", fp, "--by", "jev"}, exitError, "no option 3"},
	} {
		if code, _, errOut := c.run("", tt.args...); code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: exit %d %q; want %d with %q", tt.args, code, errOut, tt.code, tt.want)
		}
	}
	if w := c.worker(1); len(c.fake.Panes(1)[0].Answers) != 0 || w.State != registry.Blocked || w.AnsweredBy != "" {
		t.Fatalf("a refused answer pressed or recorded something: %+v, answers %v", w, c.fake.Panes(1)[0].Answers)
	}
	if !strings.Contains(log(), "1 look-into-auth-go · claude: answer 2 by jev refused") {
		t.Errorf("watch.log:\n%s", log())
	}

	out := c.mustRun("", "answer", "1", "2", "--expect", fp, "--by", "jev")
	w := c.worker(1)
	if !strings.Contains(out, "answered 1 look-into-auth-go · claude with option 2 (No), by jev") || w.State != registry.Running ||
		w.Answer != "2. No" || w.AnsweredBy != "jev" || w.Question != "" || w.Fingerprint != "" || !slices.Equal(c.fake.Panes(1)[0].Answers, []int{2}) {
		t.Errorf("answer: %q %+v", out, w)
	}
	if !strings.Contains(log(), "1 look-into-auth-go · claude: answered 2. No, by jev") {
		t.Errorf("watch.log:\n%s", log())
	}

	// Run by an agent, neither flag is required: a model that
	// ignores who answers would type --by human anyway. --expect still
	// guards the question changing on screen.
	c.fake.Ask(1, "Allow wget?", "Yes", "No")
	c.until("worker 1 blocked again", func() bool { return c.worker(1).State == registry.Blocked })
	fp = watchtest.Fingerprint("Allow wget?", []string{"Yes", "No"})
	if code, _, errOut := c.run("", "answer", "1", "1", "--expect", "0123456789abcdef"); code != exitNeedsYou ||
		!strings.Contains(errOut, "Decide again, then: routr answer 1 N --expect "+fp+" --by human") {
		t.Errorf("answer --expect on a changed question: exit %d %q", code, errOut)
	}
	if w := c.worker(1); len(c.fake.Panes(1)[0].Answers) != 1 || w.State != registry.Blocked {
		t.Fatalf("a refused answer pressed or recorded something: %+v, answers %v", w, c.fake.Panes(1)[0].Answers)
	}
	c.mustRun("", "answer", "1", "1", "--expect", fp)
	if w := c.worker(1); w.Answer != "1. Yes" || w.AnsweredBy != "human" {
		t.Errorf("answered by an agent with --expect: %+v", w)
	}
	c.fake.Ask(1, "Allow scp?", "Yes", "No")
	c.until("worker 1 blocked a third time", func() bool { return c.worker(1).State == registry.Blocked })
	c.mustRun("", "answer", "1", "2")
	if w := c.worker(1); w.Answer != "2. No" || w.AnsweredBy != "human" {
		t.Errorf("answered by an agent with no flags: %+v", w)
	}

	// From a plain shell, the user answers what they see: --by human is the
	// default, and --expect is theirs to leave out.
	delete(c.env, "CLAUDECODE")
	c.fake.Ask(1, "Allow ssh?", "Yes", "No")
	c.until("worker 1 blocked a fourth time", func() bool { return c.worker(1).State == registry.Blocked })
	c.mustRun("", "answer", "1", "2")
	if w := c.worker(1); w.Answer != "2. No" || w.AnsweredBy != "human" {
		t.Errorf("answered from a shell: %+v", w)
	}
}

// An answer by agent or jev may not pick an option that saves a rule or
// lasts past this call, since that decides more than the one question:
// nothing is pressed, watch.log says so, and the error names the options it
// may pick. The user may pick it.
func TestAnswerLeavesLastingOptionsToTheUser(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	c.mustRun("", "start", "Look into auth.go")
	c.until("worker 1 started", func() bool { return c.worker(1).Live != nil })
	opts := []string{"Yes", "Yes, and don't ask again for: curl", "No"}
	c.fake.Ask(1, "Do you want to proceed?", opts...)
	c.until("worker 1 blocked", func() bool { return c.worker(1).State == registry.Blocked })
	fp := watchtest.Fingerprint("Do you want to proceed?", opts)
	log := func() string { b, _ := os.ReadFile(c.p.WatchLog()); return string(b) }

	const refusal = "option 2 (Yes, and don't ask again for: curl) saves a rule or allows more than this call, so only the user may pick it (--by human). " +
		"Nothing was pressed. Pick one that answers only this call (1. Yes; 3. No), or ask the user"
	for _, args := range [][]string{
		{"answer", "1", "2", "--expect", fp, "--by", "agent"},
		{"answer", "1", "2", "--expect", fp, "--by", "jev"},
	} {
		if code, _, errOut := c.run("", args...); code != exitError || !strings.Contains(errOut, refusal) {
			t.Errorf("%v: exit %d %q; want %d with %q", args, code, errOut, exitError, refusal)
		}
	}
	if w := c.worker(1); len(c.fake.Panes(1)[0].Answers) != 0 || w.State != registry.Blocked || w.AnsweredBy != "" {
		t.Fatalf("a refused answer pressed or recorded something: %+v, answers %v", w, c.fake.Panes(1)[0].Answers)
	}
	for _, by := range []string{"agent", "jev"} {
		if want := `1 look-into-auth-go · claude: answer 2 by ` + by + ` refused: "Yes, and don't ask again for: curl" decides more than this call`; !strings.Contains(log(), want) {
			t.Errorf("watch.log lacks %q:\n%s", want, log())
		}
	}

	c.mustRun("", "answer", "1", "2", "--expect", fp, "--by", "human")
	if w := c.worker(1); w.Answer != "2. Yes, and don't ask again for: curl" || w.AnsweredBy != "human" || !slices.Equal(c.fake.Panes(1)[0].Answers, []int{2}) {
		t.Errorf("the user's answer: %+v", w)
	}
}

func TestWaitStopsWhenItNeedsYou(t *testing.T) {
	c := newCLI(t)
	c.seed(
		registry.Worker{Chain: "a", State: registry.Done, Summary: "planned", Finished: time.Now()},
		registry.Worker{Chain: "a", State: registry.Gated, Gate: "you", After: []int{1}},
		registry.Worker{Chain: "a", State: registry.Waiting, After: []int{2}},
		registry.Worker{State: registry.Blocked, Question: "Allow?\n1. Yes\n2. No", Live: &registry.Live{Pane: "p1"}},
		registry.Worker{Chain: "b", State: registry.Failed, Error: "the harness exited"},
		registry.Worker{State: registry.Blocked, Question: "Do you want to proceed?\n1. Yes\n2. No", Live: &registry.Live{Pane: "p2"},
			Pending: &registry.ToolCall{Tool: "Bash", Summary: "Bash: rm -rf build", Input: `{"command":"rm -rf build"}`}},
	)
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"wait", "2"}, exitNeedsYou, "gated: 2 task waits for your approval. Run: routr approve 2"},
		{[]string{"wait", "3"}, exitNeedsYou, "3 task waits on 2 task:\ngated:"},
		{[]string{"wait", "a"}, exitNeedsYou, "routr approve 2"},
		{[]string{"wait", "4"}, exitNeedsYou, "blocked: 4 task asks:\nAllow?\n1. Yes\n2. No\nThe user answers this question, not you"},
		// What "Do you want to proceed?" is about.
		{[]string{"wait", "6"}, exitNeedsYou, "blocked: 6 task asks:\nDo you want to proceed?\n1. Yes\n2. No\nIt asks about Bash: rm -rf build\nThe user answers this question, not you"},
		{[]string{"wait", "b"}, exitFailed, "failed: 5 task: the harness exited"},
		{[]string{"wait", "nope"}, exitError, ""},
	} {
		code, out, _ := c.run("", tt.args...)
		if code != tt.code || !strings.Contains(out, tt.want) {
			t.Errorf("%v: exit %d %q; want %d with %q", tt.args, code, out, tt.code, tt.want)
		}
	}
	var listed []map[string]any
	json.Unmarshal([]byte(c.mustRun("", "list", "--json")), &listed)
	if p, _ := listed[5]["pending"].(map[string]any); len(listed) != 6 || p["summary"] != "Bash: rm -rf build" || p["tool"] != "Bash" || p["input"] != `{"command":"rm -rf build"}` {
		t.Errorf("list --json: %v", listed)
	}
	if _, ok := listed[3]["pending"]; ok {
		t.Errorf("a question about no tool call has a pending one: %v", listed[3])
	}
	out := c.mustRun("", "approve", "a")
	if !strings.Contains(out, "2 task: gate open") || !c.worker(2).GateOpen || c.worker(3).GateOpen {
		t.Errorf("approve a: %q", out)
	}
	if code, out, _ := c.run("", "wait", "2", "--timeout", "1s"); code == exitNeedsYou {
		t.Errorf("wait after approve stopped at the open gate: %q", out)
	}
	if code, _, errOut := c.run("", "approve", "a"); code != exitError || !strings.Contains(errOut, "nothing to approve") {
		t.Errorf("approve again: %d %s", code, errOut)
	}
	c.fake.Start(context.Background(), worker.Spec{ID: 4}) // pane p1
	c.fake.Ask(4, "Allow?", "Yes", "No")
	out = c.mustRun("", "answer", "4", "2", "--by", "human", "--expect", watchtest.Fingerprint("Allow?", []string{"Yes", "No"}))
	if w := c.worker(4); w.State != registry.Running || !strings.Contains(out, "option 2") {
		t.Errorf("answer: %+v %q", w, out)
	}
}

// `routr wait` on a blocked worker tells the main agent who answers it, and
// how. human: the user, in its tab or by telling it an option, which it
// passes with --by human and the menu's fingerprint; without a menu, only
// its tab. agent: the main agent, after reading the brief and the project
// docs, unless the call cannot be undone, goes beyond the brief or the docs
// don't settle it; never an option that saves a rule. A question with no
// menu is the user's whoever answers. jev: while Jev decides, wait goes on;
// once it hands the question over, its note comes first, then the text for
// whoever it went to; once it answers, the worker runs on.
func TestWaitSaysWhoAnswers(t *testing.T) {
	c := newCLI(t)
	menu := func(answerer string) registry.Worker {
		return registry.Worker{State: registry.Blocked, Answerer: answerer, Live: &registry.Live{Pane: "p"},
			Question: "Do you want to proceed?\n1. Yes\n2. No", Options: []string{"Yes", "No"}, Fingerprint: "6a128b4273b95faa",
			Pending: &registry.ToolCall{Tool: "Bash", Summary: "Bash: rm -rf build"}}
	}
	prose := func(answerer string) registry.Worker {
		return registry.Worker{State: registry.Blocked, Answerer: answerer, Question: "Which file should I start with?", Live: &registry.Live{Pane: "p"}}
	}
	rule := menu("agent")
	rule.Question = "Do you want to proceed?\n1. Yes\n2. Yes, and don't ask again for: rm commands in /repo\n3. No"
	rule.Options = []string{"Yes", "Yes, and don't ask again for: rm commands in /repo", "No"}
	const unsure = "Jev did not answer it: it picked option 1 (Yes) with confidence 0.62, below min_confidence 0.8."
	handed := func(to string) registry.Worker {
		w := menu("jev")
		w.HandedTo, w.Note = to, unsure
		return w
	}
	answered := registry.Worker{State: registry.Running, Answerer: "jev", Live: &registry.Live{Pane: "p"}, Answer: "1. Yes", AnsweredBy: "jev"}
	c.seed(menu("human"), prose(""), menu("agent"), menu("jev"), prose("agent"), rule, handed("human"), handed("agent"), answered)
	os.WriteFile(filepath.Join(c.dir, "SPEC.md"), []byte("# Spec\n"), 0o644)
	registry.WriteFile(filepath.Join(c.p.RunDir(3), registry.BriefFile), []byte("Clean the build.\n"))
	human := func(id int) string {
		return fmt.Sprintf("blocked: %d task asks:\nDo you want to proceed?\n1. Yes\n2. No\nIt asks about Bash: rm -rf build\n"+
			"The user answers this question, not you: do not choose an option yourself, even an obvious one.\n"+
			"Tell the user what it asks. They answer in its tab (routr show %d), or tell you an option.\n"+
			"If they tell you option N, run: routr answer %d N --by human --expect 6a128b4273b95faa\n", id, id, id)
	}
	noMenu := func(id int) string {
		return fmt.Sprintf("blocked: %d task asks:\nWhich file should I start with?\n"+
			"The user answers this question, not you: do not choose an option yourself, even an obvious one.\n"+
			"Tell the user what it asks. They answer in its tab: routr show %d\n", id, id)
	}
	agent := func(id int, read, options, lasting string) string {
		return fmt.Sprintf("blocked: %d task asks:\nDo you want to proceed?\n%sIt asks about Bash: rm -rf build\n"+
			"You answer this question, not the user.\n%s"+
			"Answer it yourself only if what it asks about is allowed by the docs, within its brief, and can be undone "+
			"(deleting files, pushing, sending or installing cannot), and the docs and brief settle which option is right.\n%s"+
			"To answer, run: routr answer %d N --by agent --expect 6a128b4273b95faa, then tell the user in one line what you chose and why.\n"+
			"Otherwise ask the user: say what it asks and why you did not decide. They answer in its tab (routr show %d), or tell you an option N: "+
			"then run routr answer %d N --by human --expect 6a128b4273b95faa\n", id, options, read, lasting, id, id, id)
	}
	afterJev := func(text string) string { // Jev's note first, then whoever it handed the question to
		head, rest, _ := strings.Cut(text, "It asks about Bash: rm -rf build\n")
		return head + "It asks about Bash: rm -rf build\n" + unsure + "\n" + rest
	}
	for _, tt := range []struct {
		id   string
		code int
		want string
	}{
		{"1", exitNeedsYou, human(1)},
		{"2", exitNeedsYou, noMenu(2)},
		{"3", exitNeedsYou, agent(3, "Before you decide, read the project docs, even when the answer looks obvious: /repo/SPEC.md\nIts brief: /repo/.routr/runs/3/brief.md\n",
			"1. Yes\n2. No\n", "")},
		// Jev decides: nothing for the main agent to do, so wait goes on.
		{"4", exitRunning, "still running: 4 task (Jev decides its question). Run: routr wait 4\n"},
		{"5", exitNeedsYou, noMenu(5)},
		{"6", exitNeedsYou, agent(6, "Before you decide, read the project docs, even when the answer looks obvious: /repo/SPEC.md\n",
			"1. Yes\n2. Yes, and don't ask again for: rm commands in /repo\n3. No\n",
			"Do not pick option 2: it saves a rule or lasts past this call, so only the user may.\n")},
		{"7", exitNeedsYou, afterJev(human(7))},
		{"8", exitNeedsYou, afterJev(agent(8, "Before you decide, read the project docs, even when the answer looks obvious: /repo/SPEC.md\n", "1. Yes\n2. No\n", ""))},
		// Jev answered: the worker runs on.
		{"9", exitRunning, "still running: 9 task (running). Run: routr wait 9\n"},
	} {
		if code, out, _ := c.run("", "wait", tt.id, "--timeout", "30ms"); code != tt.code || strings.ReplaceAll(out, c.dir, "/repo") != tt.want {
			t.Errorf("wait %s: exit %d\n%s\nwant exit %d\n%s", tt.id, code, out, tt.code, tt.want)
		}
	}
	if out := c.mustRun("", "list"); !strings.Contains(out, "blocked, Jev decides") || strings.Count(out, "Jev decides") != 1 {
		t.Errorf("list:\n%s", out)
	}

	// With no docs and the brief not sent, nothing to read first; with a
	// config that does not load, the command that says what to read.
	os.Remove(filepath.Join(c.dir, "SPEC.md"))
	if _, out, _ := c.run("", "wait", "6"); !strings.Contains(out, "\nYou answer this question, not the user.\nAnswer it yourself only if") {
		t.Errorf("nothing to read:\n%s", out)
	}
	os.MkdirAll(c.p.Dir, 0o755)
	os.WriteFile(c.p.Config(), []byte("[questions]\nanswer = \"sometimes\"\n"), 0o644)
	if _, out, _ := c.run("", "wait", "6"); !strings.Contains(out, "\nYou answer this question, not the user.\nBefore you decide, run routr show 6 --context: it says which project docs and brief to read.\n") {
		t.Errorf("a config that does not load:\n%s", out)
	}
}

// routr show ID --context prints what a worker's question is decided with,
// from the registry and files alone (no pane, no herdr): the question, the
// tool call it is about, the brief as sent, and the project docs from config
// [questions] context as paths with sizes, a missing one named. A worker
// that asks nothing gets the rest; a run step asks nothing at all.
func TestShowContext(t *testing.T) {
	c := newCLI(t)
	c.env["HERDR_PANE_ID"] = ""
	os.MkdirAll(c.p.Dir, 0o755)
	os.WriteFile(c.p.Config(), []byte("[questions]\nanswer = \"agent\"\ncontext = [\"SPEC.md\", \"docs/gone.md\"]\n"), 0o644)
	os.WriteFile(filepath.Join(c.dir, "SPEC.md"), []byte("# Spec\n"), 0o644)
	c.seed(registry.Worker{Chain: "fix-3", Step: "implement", Role: "implement", Harness: "claude", Answerer: "agent", State: registry.Blocked,
		Live: &registry.Live{Pane: "p"}, Question: "Do you want to proceed?\n1. Yes\n2. No", Options: []string{"Yes", "No"}, Fingerprint: "6a128b4273b95faa",
		Pending: &registry.ToolCall{Tool: "Bash", Summary: "Bash: go test ./...", Input: `{"command":"go test ./..."}`}},
		registry.Worker{State: registry.Running, Brief: "Summarize the diff"},
		registry.Worker{State: registry.Running, Run: "go test ./..."})
	registry.WriteFile(filepath.Join(c.p.RunDir(1), registry.BriefFile), []byte("Fix the flaky test.\n"))

	want := `1 implement · claude: role implement, chain fix-3, step implement, in /repo
Who answers its questions: agent

It asks:
Do you want to proceed?
1. Yes
2. No
fingerprint: 6a128b4273b95faa
It asks about Bash: go test ./...
input:
{"command":"go test ./..."}

Its brief (/repo/.routr/runs/1/brief.md):
Fix the flaky test.

Project docs: read them before you decide.
/repo/SPEC.md (7 bytes)
Missing: docs/gone.md, listed in [questions] context but not in /repo
`
	if out := strings.ReplaceAll(c.mustRun("", "show", "1", "--context"), c.dir, "/repo"); out != want {
		t.Errorf("show 1 --context:\n%s\nwant\n%s", out, want)
	}
	var js watch.QuestionContext
	if err := json.Unmarshal([]byte(c.mustRun("", "show", "1", "--context", "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.Worker != 1 || js.State != registry.Blocked || js.Answerer != "agent" || js.Chain != "fix-3" || js.Step != "implement" ||
		!slices.Equal(js.Options, []string{"Yes", "No"}) || js.Fingerprint != "6a128b4273b95faa" || js.Pending == nil || js.Pending.Summary != "Bash: go test ./..." ||
		js.Brief != "Fix the flaky test." || js.BriefPath != filepath.Join(c.p.RunDir(1), registry.BriefFile) || js.Root != c.dir ||
		!slices.Equal(js.Docs, []watch.Doc{{Path: filepath.Join(c.dir, "SPEC.md"), Size: 7}}) || !slices.Equal(js.Missing, []string{"docs/gone.md"}) {
		t.Errorf("show 1 --context --json: %+v", js)
	}
	// who answers it and how, a line each: what routr wait prints, for front ends to put in their wake
	if len(js.Answer) < 2 || js.Answer[0] != "You answer this question, not the user." || js.Answer[1] != "Before you decide, read the project docs, even when the answer looks obvious: "+filepath.Join(c.dir, "SPEC.md") {
		t.Errorf("show 1 --context --json: answer %q", js.Answer)
	}
	if code, out, _ := c.run("", "wait", "1"); code != exitNeedsYou || !strings.Contains(out, "\n"+strings.Join(js.Answer, "\n")) {
		t.Errorf("wait 1: exit %d, does not print the answer lines:\n%s\nwant\n%s", code, out, strings.Join(js.Answer, "\n"))
	}
	var nothing watch.QuestionContext
	if err := json.Unmarshal([]byte(c.mustRun("", "show", "2", "--context", "--json")), &nothing); err != nil || nothing.Answer != nil {
		t.Errorf("show 2 --context --json, which asks nothing: answer %q (%v)", nothing.Answer, err)
	}

	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"show", "2", "--context"}, exitOK, "It asks nothing now (running).\n\nIts brief:\nSummarize the diff\n"},
		{[]string{"show", "3", "--context"}, exitError, "worker 3 is a run step; it runs a command and asks no questions"},
		{[]string{"show", "1", "--context", "--focus"}, exitUsage, "takes no --focus"},
		{[]string{"show", "1"}, exitError, "HERDR_PANE_ID is unset"}, // without --context it shows the pane
	} {
		code, out, errOut := c.run("", tt.args...)
		if code != tt.code || !strings.Contains(out+errOut, tt.want) {
			t.Errorf("%v: exit %d %q %q; want %d with %q", tt.args, code, out, errOut, tt.code, tt.want)
		}
	}
}

// Who answers a worker's questions is decided when it is queued: --answer,
// then a flow step's answer, then config [questions] answer, then human.
// flow start --answer is for every step, over a step's own answer. It is
// stored on the worker as answerer; a run or judge step has none.
func TestWhoAnswersIsSetPerRunStepAndConfig(t *testing.T) {
	c := newCLI(t)
	file := filepath.Join(t.TempDir(), "f.toml")
	os.WriteFile(file, []byte(`[flow.f]
[[flow.f.step]]
id = "dig"
answer = "jev"
brief = "Dig."
[[flow.f.step]]
id = "fix"
after = ["dig"]
brief = "Fix."
[[flow.f.step]]
id = "check"
after = ["fix"]
run = "true"
`), 0o644)
	answerers := func(out string) string {
		t.Helper()
		var got []string
		for _, w := range c.chain(strings.TrimSpace(out)) {
			got = append(got, w.Step+"="+w.Answerer)
		}
		return strings.Join(got, " ")
	}
	id := func(out string) registry.Worker {
		t.Helper()
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("start printed %q", out)
		}
		return c.worker(n)
	}
	if w := id(c.mustRun("", "start", "Fix it")); w.Answerer != "human" {
		t.Errorf("default: %q", w.Answerer)
	}
	if w := id(c.mustRun("", "start", "--answer", "jev", "Fix it")); w.Answerer != "jev" {
		t.Errorf("--answer jev: %q", w.Answerer)
	}
	if got := answerers(c.mustRun("", "flow", "start", "--file", file)); got != "dig=jev fix=human check=" {
		t.Errorf("flow, no config: %s", got)
	}
	if err := os.WriteFile(c.p.Config(), []byte("[questions]\nanswer = \"agent\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := id(c.mustRun("", "start", "Fix it")); w.Answerer != "agent" {
		t.Errorf("config agent: %q", w.Answerer)
	}
	if w := id(c.mustRun("", "start", "--answer", "human", "Fix it")); w.Answerer != "human" {
		t.Errorf("--answer human over config agent: %q", w.Answerer)
	}
	if got := answerers(c.mustRun("", "flow", "start", "--file", file)); got != "dig=jev fix=agent check=" {
		t.Errorf("flow, config agent: the step over config: %s", got)
	}
	if got := answerers(c.mustRun("", "flow", "start", "--file", file, "--answer", "human")); got != "dig=human fix=human check=" {
		t.Errorf("flow --answer human: the flag over the step and config: %s", got)
	}

	var listed []map[string]any
	json.Unmarshal([]byte(c.mustRun("", "list", "--json")), &listed)
	if len(listed) != 13 || listed[0]["answerer"] != "human" || listed[1]["answerer"] != "jev" || listed[3]["answerer"] != "human" {
		t.Fatalf("list --json: %v", listed)
	}
	if _, ok := listed[4]["answerer"]; ok {
		t.Errorf("a run step has an answerer: %v", listed[4])
	}

	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"start", "--answer", "me", "x"}, exitUsage, `--answer "me": use human, agent or jev`},
		{[]string{"flow", "start", "--file", file, "--answer", "everyone"}, exitUsage, `--answer "everyone": use human, agent or jev`},
		{[]string{"start", "--run", "true", "--answer", "jev"}, exitUsage, "--answer: a --run step runs a command, not a harness"},
	} {
		if code, _, errOut := c.run("", tt.args...); code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: exit %d %q, want %d with %q", tt.args, code, errOut, tt.code, tt.want)
		}
	}
	if err := os.WriteFile(c.p.Config(), []byte("[questions]\nanswer = \"bot\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := c.run("", "start", "x"); code != exitError || !strings.Contains(errOut, `questions.answer = "bot": use one of human, agent, jev`) {
		t.Errorf("a bad config: exit %d %s", code, errOut)
	}
}

// `routr send 5 "retry"; routr wait 5` on a failed worker exited 5 at once:
// a queued or unanswered follow-up keeps a wait going. And a maybe_done
// upstream made `wait` on the steps after it run into its timeout.
func TestWaitFollowUpsAndMaybeDoneUpstream(t *testing.T) {
	c := newCLI(t)
	c.seed(
		registry.Worker{State: registry.Failed, Error: "the harness exited", Inbox: []string{"retry"}},
		registry.Worker{State: registry.NeedsYou, Note: "cap", Finished: time.Now(), Turn: 1},
		registry.Worker{Chain: "m", State: registry.MaybeDone, Note: "idle without running routr done"},
		registry.Worker{Chain: "m", State: registry.Waiting, After: []int{3}},
		registry.Worker{Chain: "m", State: registry.Waiting, After: []int{4}},
	)
	registry.WriteFile(c.final(3), []byte("I wrote PLAN.md, I think."))
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"wait", "1", "--timeout", "20ms"}, exitRunning, "still running: 1 task (follow-up pending)"},
		{[]string{"wait", "2", "--timeout", "20ms"}, exitRunning, "still running"},
		{[]string{"wait", "5", "--timeout", "5s"}, exitNeedsYou, "5 task waits on 3 task:\nmaybe done: 3 task went idle without running routr done"},
		{[]string{"wait", "m", "--timeout", "5s"}, exitNeedsYou, "I wrote PLAN.md, I think."},
	} {
		code, out, _ := c.run("", tt.args...)
		if code != tt.code || !strings.Contains(out, tt.want) {
			t.Errorf("%v: exit %d %q; want %d with %q", tt.args, code, out, tt.code, tt.want)
		}
	}
	// Once the follow-up is dropped (the resume failed), it stops.
	c.p.Store().Update(1, func(w *registry.Worker) { w.Inbox = nil })
	if code, out, _ := c.run("", "wait", "1"); code != exitFailed || !strings.Contains(out, "failed: 1 task") {
		t.Errorf("wait 1 after: %d %q", code, out)
	}
}

func TestWaitOnAFinishedChain(t *testing.T) {
	c := newCLI(t)
	c.seed(
		registry.Worker{Chain: "c", Role: "plan", State: registry.Done, Summary: "plan written", Output: "PLAN.md", Finished: time.Now()},
		registry.Worker{Chain: "c", Role: "review", State: registry.Done, Verdict: registry.Pass, Summary: "no blockers", Finished: time.Now()},
	)
	registry.WriteFile(c.final(2), []byte("no blockers\n\nShip it."))
	out := c.mustRun("", "wait", "c")
	want := "1 plan: done — plan written\n2 review: done, verdict pass — no blockers\n\nno blockers\n\nShip it.\nverdict: pass\n"
	if out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
	var vs []view
	if err := json.Unmarshal([]byte(c.mustRun("", "wait", "c", "--json")), &vs); err != nil || len(vs) != 2 {
		t.Errorf("wait --json: %v %v", vs, err)
	}
}

func TestReviewLoopCapThenApprove(t *testing.T) {
	c := newCLI(t)
	c.seed(registry.Worker{Role: "review", State: registry.NeedsYou, Verdict: registry.Fail, Finished: time.Now(), Note: "the review loop hit its cap"})
	if code, out, _ := c.run("", "wait", "1"); code != exitNeedsYou || !strings.Contains(out, "cap") {
		t.Errorf("wait: %d %q", code, out)
	}
	c.mustRun("", "approve", "1")
	if w := c.worker(1); w.State != registry.Done {
		t.Errorf("approve at the cap accepts it: %s", w.State)
	}
}

func TestDone(t *testing.T) {
	c := newCLI(t)
	c.env["ROUTR_ID"] = "4"
	if code, _, errOut := c.run("", "done", "--verdict", "maybe", "x"); code != exitUsage || !strings.Contains(errOut, "pass or fail") {
		t.Errorf("bad verdict: %d %s", code, errOut)
	}
	if code, _, errOut := c.run("", "done", "--output", "MISSING.md", "x"); code != exitError || !strings.Contains(errOut, "write it first") {
		t.Errorf("missing output: %d %s", code, errOut)
	}
	c.mustRun("summary from stdin\n", "done", "--verdict", "fail", "-")
	d, ok, err := registry.TakeDone(c.p.RunDir(4))
	if !ok || err != nil || d.Message != "summary from stdin" || d.Verdict != registry.Fail {
		t.Errorf("done.json = %+v %v %v", d, ok, err)
	}
	delete(c.env, "ROUTR_ID")
	if code, _, errOut := c.run("", "done", "x"); code != exitUsage || !strings.Contains(errOut, "ROUTR_ID") {
		t.Errorf("no id: %d %s", code, errOut)
	}
	c.mustRun("", "done", "--id", "7", "by id")
	if !registry.HasDone(c.p.RunDir(7)) {
		t.Error("--id")
	}
	// A relative --output from a worker that has since cd'd out of where
	// it started: found there, in ROUTR_CWD.
	sub := filepath.Join(c.dir, "sub")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "OUT.md"), []byte("x"), 0o644)
	c.env["ROUTR_ID"], c.env["ROUTR_CWD"] = "8", sub
	c.mustRun("", "done", "--output", "OUT.md", "after a cd")
	if d, ok, _ := registry.TakeDone(c.p.RunDir(8)); !ok || d.Output != filepath.Join(sub, "OUT.md") {
		t.Errorf("relative output after a cd: %+v", d)
	}
	// One in the shell's cwd still wins.
	os.WriteFile(filepath.Join(c.dir, "OUT.md"), []byte("y"), 0o644)
	c.mustRun("", "done", "--output", "OUT.md", "in the cwd")
	if d, _, _ := registry.TakeDone(c.p.RunDir(8)); d.Output != filepath.Join(c.dir, "OUT.md") {
		t.Errorf("relative output in the cwd: %+v", d)
	}
}

func TestListKeepsActiveChains(t *testing.T) {
	c := newCLI(t)
	c.seed(
		registry.Worker{Chain: "c", Step: "plan", Role: "plan", State: registry.Done},
		registry.Worker{Chain: "c", Step: "review", Role: "review", State: registry.Running, After: []int{1}},
		registry.Worker{Chain: "d", Step: "plan", Role: "plan", State: registry.Done},
	)
	var vs []view
	if err := json.Unmarshal([]byte(c.mustRun("", "list", "--json", "--since", "0s")), &vs); err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, v := range vs {
		ids = append(ids, v.ID)
	}
	if !slices.Equal(ids, []int{1, 2}) {
		t.Errorf("list --since 0s = %v, want the finished step of the active chain and not the finished chain", ids)
	}
}

func TestListCloseCancel(t *testing.T) {
	c := newCLI(t)
	c.seed(
		registry.Worker{Chain: "c", Step: "plan", Role: "plan", Harness: "claude", Model: "opus", State: registry.Running, Write: true, Tree: c.dir,
			Live: &registry.Live{Pane: "p1"}, Started: time.Now(), Output: filepath.Join(c.dir, "PLAN.md")},
		registry.Worker{Chain: "c", Role: "review", State: registry.Waiting, After: []int{1}, Note: "waiting on 1 plan (running)"},
		registry.Worker{Role: "research", State: registry.Done, Finished: time.Now(), Summary: "found 3 tools"},
	)
	c.fake.Start(context.Background(), worker.Spec{ID: 1}) // pane p1
	out := c.mustRun("", "list", "--since", "0s")
	if !strings.Contains(out, "c/plan") || !strings.Contains(out, "claude · opus") || !strings.Contains(out, "PLAN.md") ||
		!strings.Contains(out, "waiting on 1 plan") || strings.Contains(out, "found 3 tools") {
		t.Errorf("list:\n%s", out)
	}
	if out := c.mustRun("", "list"); !strings.Contains(out, "found 3 tools") {
		t.Errorf("list keeps what finished in the last hour:\n%s", out)
	}
	if out := c.mustRun("", "list", "--all", "--since", "0s"); !strings.Contains(out, "found 3 tools") {
		t.Errorf("list --all:\n%s", out)
	}
	js := c.mustRun("", "list", "--json", "--since", "0s")
	var vs []view
	if err := json.Unmarshal([]byte(js), &vs); err != nil || len(vs) != 2 {
		t.Errorf("list --json: %d %v", len(vs), err)
	}
	if strings.Contains(js, "0001-01-01") || strings.Contains(js, `"finished"`) {
		t.Errorf("zero times in --json:\n%s", js)
	}
	// Every listed worker's whole final message made list --json, which the
	// Pi extension polls, grow without bound: only its path, unless --final.
	registry.WriteFile(c.final(1), []byte("a long final message"))
	js = c.mustRun("", "list", "--json", "--since", "0s")
	if strings.Contains(js, `"final":`) || !strings.Contains(js, `"final_path": "`+c.final(1)) {
		t.Errorf("list --json:\n%s", js)
	}
	if js := c.mustRun("", "list", "--json", "--final", "--since", "0s"); !strings.Contains(js, `"final": "a long final message"`) {
		t.Errorf("list --json --final:\n%s", js)
	}

	if code, _, errOut := c.run("", "show", "2"); code != exitError || !strings.Contains(errOut, "no open tab") {
		t.Errorf("show without tab: %d %s", code, errOut)
	}
	c.mustRun("", "show", "1")
	if p := c.fake.Panes(1)[0]; p.Shown != "w1:p1" {
		t.Errorf("shown beside %q", p.Shown)
	}
	c.mustRun("", "hide", "1")

	c.mustRun("", "cancel", "c")
	if w := c.worker(1); w.State != registry.Cancelled || w.Live != nil || c.fake.Open(1) != 0 {
		t.Errorf("cancel closes and marks: %+v", w)
	}
	if w := c.worker(2); w.State != registry.Cancelled {
		t.Errorf("cancel c cancels queued steps too: %s", w.State)
	}
	if b, _ := os.ReadFile(c.p.WatchLog()); !strings.Contains(string(b), "1 plan · claude: cancelled by routr cancel") {
		t.Errorf("watch.log:\n%s", b)
	}
	if code, _, _ := c.run("", "cancel", "c"); code != exitError {
		t.Error("cancelling a finished chain is an error")
	}
	if code, _, errOut := c.run("", "close", "3"); code != 0 || errOut != "" {
		t.Errorf("close a done worker: %d %s", code, errOut)
	}
}

// close and cancel during a follow-up settle the turn in flight; left in
// flight, later follow-ups are never sent, the worker stays active and
// `wait` runs into its timeout.
func TestCloseAndCancelSettleAFollowUpInFlight(t *testing.T) {
	c := newCLI(t)
	inFlight := func(pane string, rest registry.State) registry.Worker {
		return registry.Worker{State: registry.Running, Rest: rest, Finished: time.Now(), Turn: 1,
			Live: &registry.Live{Socket: "/tmp/fake-herdr.sock", Pane: pane}}
	}
	c.seed(inFlight("p1", registry.Done), inFlight("p2", ""))
	c.fake.Start(context.Background(), worker.Spec{ID: 1}) // p1
	c.fake.Start(context.Background(), worker.Spec{ID: 2}) // p2

	c.mustRun("", "close", "1")
	w := c.worker(1)
	if w.State != registry.Done || w.Replies != 1 || w.Active() || w.Live != nil || c.fake.Open(1) != 0 {
		t.Errorf("after close: %+v", w)
	}
	if got := readFile(c.p.TurnFile(1, 1)); !strings.Contains(got, "closed by routr close") {
		t.Errorf("turns/1.md = %q", got)
	}
	if code, out, _ := c.run("", "wait", "1", "--timeout", "1s"); code != 0 || !strings.Contains(out, "closed by routr close") {
		t.Errorf("wait after close: %d %q", code, out)
	}

	c.mustRun("", "cancel", "2")
	if w := c.worker(2); w.State != registry.Cancelled || w.Replies != 1 || w.Active() || c.fake.Open(2) != 0 {
		t.Errorf("after cancel: %+v", w)
	}
}

// A flow started with --chain gets a chain of its own: a name in use, or
// whose work directory has files, is refused. A pipe joins one on purpose.
func TestFlowStartRefusesAChainInUse(t *testing.T) {
	c := newCLI(t)
	if out := c.mustRun("", "flow", "start", "plan-redteam", "--task", "t", "--chain", "auth"); out != "auth\n" {
		t.Fatalf("flow start printed %q", out)
	}
	if code, _, errOut := c.run("", "flow", "start", "plan-redteam", "--task", "t", "--chain", "auth"); code != exitError ||
		!strings.Contains(errOut, "chain auth is in use (worker 1 is in it); give a new --chain name, or leave --chain out") {
		t.Errorf("reused chain: %d %s", code, errOut)
	}
	c.mustRun("", "start", "--chain", "auth", "--after", "2", "Follow up.")
	if w := c.worker(3); w.Chain != "auth" {
		t.Errorf("a pipe joins the chain: %+v", w)
	}
	work := filepath.Join(c.dir, ".routr", "work", "old")
	os.MkdirAll(work, 0o755)
	if out := c.mustRun("", "flow", "start", "plan-redteam", "--task", "t", "--chain", "old"); out != "old\n" {
		t.Errorf("an empty work directory is fresh: %q", out)
	}
	os.MkdirAll(filepath.Join(c.dir, ".routr", "work", "left"), 0o755)
	os.WriteFile(filepath.Join(c.dir, ".routr", "work", "left", "SEC.md"), []byte("old findings"), 0o644)
	if code, _, errOut := c.run("", "flow", "start", "plan-redteam", "--task", "t", "--chain", "left"); code != exitError ||
		!strings.Contains(errOut, "work directory "+filepath.Join(c.dir, ".routr", "work", "left")+" holds files of an earlier run") {
		t.Errorf("leftover files: %d %s", code, errOut)
	}
	if w, _ := c.p.Store().List(); len(w) != 5 {
		t.Errorf("a refused start queued workers: %d in all", len(w))
	}
}

// A foreach step with allow_empty that matches nothing at start is queued
// done, without the role's default output, and flow start says so.
func TestFlowStartEmptyForeach(t *testing.T) {
	c := newCLI(t)
	os.MkdirAll(filepath.Join(c.p.Dir, "flows"), 0o755)
	os.WriteFile(filepath.Join(c.p.Dir, "flows", "e.toml"), []byte(`
[flow.e]
[[flow.e.step]]
id = "dig"
role = "plan"
foreach = "in/*.md"
allow_empty = true
brief = "Do {{item.path}}."
[[flow.e.step]]
id = "join"
after = ["dig"]
brief = "Merge {{dig.outputs}}."
`), 0o644)
	code, out, errOut := c.run("", "flow", "start", "e")
	if code != 0 || out != "e-1\n" || !strings.Contains(errOut, "routr: step dig is done: foreach in/*.md matched no files; allow_empty, so nothing ran\n") {
		t.Fatalf("flow start: %d %q %s", code, out, errOut)
	}
	if w := c.worker(1); w.State != registry.Done || !w.Empty() || w.Output != "" {
		t.Errorf("dig = %+v", w)
	}
}

// A flow from --file (a path, or - for stdin) is for that one command: it
// is checked as a flow file is, may share a name with a built-in without
// replacing it, never shows in flow list, and is kept in the chain's work
// directory as flow.toml.
func TestFlowStartFile(t *testing.T) {
	c := newCLI(t)
	scratch := `[flow.plan-redteam]
description = "a scratch variant"
params = ["task"]
[[flow.plan-redteam.step]]
id = "dig"
role = "research"
model = "haiku"
brief = "Look into {{task}}."
`
	path := filepath.Join(t.TempDir(), "wide.toml")
	os.WriteFile(path, []byte(scratch), 0o644)

	if out := c.mustRun("", "flow", "show", "--file", path); !strings.Contains(out, "a scratch variant") || !strings.Contains(out, "from: "+path) {
		t.Errorf("flow show --file:\n%s", out)
	}
	code, out, errOut := c.run("", "flow", "start", "--file", path, "--task", "queues")
	if code != 0 || out != "plan-redteam-1\n" || !strings.Contains(errOut, "flow.toml") {
		t.Fatalf("flow start --file: %d %q %s", code, out, errOut)
	}
	if all, _ := c.p.Store().List(); len(all) != 1 || all[0].Model != "haiku" || all[0].Step != "dig" {
		t.Fatalf("queued %+v", all)
	}
	if b, err := os.ReadFile(filepath.Join(c.p.WorkDir("plan-redteam-1"), "flow.toml")); err != nil || string(b) != scratch {
		t.Errorf("work/plan-redteam-1/flow.toml = %q, %v", b, err)
	}
	if out := c.mustRun("", "flow", "list"); strings.Contains(out, "scratch") {
		t.Errorf("flow list shows the scratch flow:\n%s", out)
	}
	c.mustRun("", "flow", "start", "plan-redteam", "--task", "t")
	if w := c.worker(3); w.Step != "redteam" {
		t.Errorf("the built-in plan-redteam after a scratch one: worker 3 = %+v", w)
	}

	two := scratch + `
[flow.other]
[[flow.other.step]]
id = "s"
brief = "other"
`
	if code, _, errOut := c.run(two, "flow", "start", "--file", "-", "--task", "t"); code != exitUsage || !strings.Contains(errOut, "other, plan-redteam") {
		t.Errorf("two flows, no name: %d %s", code, errOut)
	}
	if code, _, errOut := c.run(two, "flow", "start", "nope", "--file", "-"); code != exitError || !strings.Contains(errOut, `no flow "nope" in stdin`) {
		t.Errorf("a name not in the file: %d %s", code, errOut)
	}
	if out := c.mustRun(two, "flow", "start", "other", "--file", "-"); out != "other-1\n" {
		t.Errorf("flow start other --file -: %q", out)
	}
	bad := "[flow.x]\n[[flow.x.step]]\nid = \"s\"\nbrief = \"{{nope.output}}\"\n"
	before, _ := c.p.Store().List()
	if code, _, errOut := c.run(bad, "flow", "start", "--file", "-"); code != exitError || !strings.Contains(errOut, "flows stdin") {
		t.Errorf("a bad scratch flow: %d %s", code, errOut)
	}
	if code, _, errOut := c.run("", "flow", "start", "--file", "-"); code != exitError || !strings.Contains(errOut, "defines no flow") {
		t.Errorf("an empty file: %d %s", code, errOut)
	}
	if after, _ := c.p.Store().List(); len(after) != len(before) {
		t.Errorf("a refused scratch flow queued workers")
	}
	if code, _, errOut := c.run("", "flow", "start", "plan-redteam", "other", "--file", path); code != exitUsage {
		t.Errorf("two names: %d %s", code, errOut)
	}
}

// flow show --toml prints the flow as written, ready to copy into a
// scratch file.
func TestFlowShowTOML(t *testing.T) {
	c := newCLI(t)
	out := c.mustRun("", "flow", "show", "deep-research", "--toml")
	if !strings.HasPrefix(out, "[flow.deep-research]\n") || strings.Contains(out, "[flow.fix-until-green]") || !strings.Contains(out, "[[flow.deep-research.step]]") {
		t.Errorf("flow show --toml:\n%s", out)
	}
	if code, _, errOut := c.run(out, "flow", "show", "--file", "-"); code != 0 {
		t.Errorf("its output is not a flow file: %s", errOut)
	}
}

func TestMisc(t *testing.T) {
	c := newCLI(t)
	if code, _, errOut := c.run("", "flow", "nope"); code != exitUsage || !strings.Contains(errOut, "list, show or start") {
		t.Errorf("flow nope: %d %s", code, errOut)
	}
	if out := c.mustRun("", "flow", "list"); !strings.Contains(out, "plan-redteam") || !strings.Contains(out, "plan-to-impl") {
		t.Errorf("flow list:\n%s", out)
	}
	if out := c.mustRun("", "flow", "show", "plan-redteam"); !strings.Contains(out, "redteam") {
		t.Errorf("flow show:\n%s", out)
	}
	if code, _, errOut := c.run("", "flow", "start", "plan-redteam"); code != exitError || !strings.Contains(errOut, "task") {
		t.Errorf("flow start without task: %d %s", code, errOut)
	}
	if out := c.mustRun("", "flow", "start", "plan-redteam", "--task", "add OAuth"); out != "plan-redteam-1\n" {
		t.Errorf("flow start printed %q", out)
	}
	plan, red := c.worker(1), c.worker(2)
	if plan.Chain != "plan-redteam-1" || plan.Step != "plan" || !plan.Write || plan.Harness != "claude" || plan.Socket == "" || plan.Output != "PLAN.md" {
		t.Errorf("plan = %+v", plan)
	}
	if red.Harness != "codex" || red.Write || !red.WantVerdict || red.After[0] != 1 || red.Preamble == "" {
		t.Errorf("redteam = %+v", red)
	}
	if out := c.mustRun("", "flow", "start", "plan-redteam", "--task", "again"); out != "plan-redteam-2\n" {
		t.Errorf("second chain %q", out)
	}
	if code, _, _ := c.run("", "frobnicate"); code != exitUsage {
		t.Error("unknown command")
	}
	out := c.mustRun("", "roles")
	for _, want := range []string{"caller: claude", "review", "codex", "avoid caller", "implement", "you"} {
		if !strings.Contains(out, want) {
			t.Errorf("roles lacks %q:\n%s", want, out)
		}
	}
	before := c.spawned.Load()
	c.mustRun("", "watch")
	if c.spawned.Load() != before+1 {
		t.Error("watch did not spawn")
	}
}

// candidateSteps starts flow name and checks that each of steps is a hunt
// worker whose composed brief does not ask for an approval verdict: finding
// candidates is the work, so a list of them is no fail, and the merge after
// them must still start.
func candidateSteps(t *testing.T, c *cli, name, param string, steps []string, merge string) {
	t.Helper()
	ws := c.chain(strings.TrimSpace(c.mustRun("", "flow", "start", name, "--param", param+"=x")))
	byStep := map[string]registry.Worker{}
	for _, w := range ws {
		byStep[w.Step] = w
	}
	var ids []int
	for _, s := range steps {
		w, ok := byStep[s]
		if !ok {
			t.Fatalf("%s has no step %s", name, s)
		}
		ids = append(ids, w.ID)
		brief := watch.Compose(w, c.p, w.Brief, "/usr/local/bin/routr")
		if w.Role != "hunt" || w.Write || w.WantVerdict || w.Effort != "high" || strings.Contains(brief, "--verdict") ||
			!strings.Contains(brief, "candidate") {
			t.Errorf("%s %s: role %s write %v verdict %v effort %q\n%s", name, s, w.Role, w.Write, w.WantVerdict, w.Effort, brief)
		}
	}
	if m := byStep[merge]; !reflect.DeepEqual(m.After, ids) {
		t.Errorf("%s %s waits on %v, not the hunters %v", name, merge, m.After, ids)
	}
}

func TestSecurityHuntersDoNotRequestApprovalVerdicts(t *testing.T) {
	c := newCLI(t)
	candidateSteps(t, c, "security-review", "scope", []string{"inject", "access", "robust"}, "merge")
	// A review that judges the work still asks for one.
	red := c.chain(strings.TrimSpace(c.mustRun("", "flow", "start", "plan-redteam", "--task", "t")))[1]
	if brief := watch.Compose(red, c.p, red.Brief, "/usr/local/bin/routr"); red.Role != "review" || !red.WantVerdict ||
		!strings.Contains(brief, "--verdict pass|fail") {
		t.Errorf("redteam: role %s verdict %v\n%s", red.Role, red.WantVerdict, brief)
	}
}

func TestReviewLensesDoNotRequestApprovalVerdicts(t *testing.T) {
	candidateSteps(t, newCLI(t), "adversarial-review", "target", []string{"correctness", "design", "risk", "premortem"}, "merge")
}

// Any step's fail without on_fail halts the steps after it, so no built-in
// flow asks an agent step for a verdict, by its role, its brief or verdict
// = true, without on_fail unless nothing comes after it.
func TestBuiltinAgentVerdictsHaltNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	set, err := flow.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range set.Names() {
		steps := set[name].Steps
		for _, s := range steps {
			role := cmp.Or(s.Role, "task")
			if s.Run != "" || s.Judge != "" || s.OnFail != nil ||
				!(cfg.Roles[role].Verdict || s.Verdict || strings.Contains(s.Brief, "--verdict")) {
				continue
			}
			for _, next := range steps {
				if slices.Contains(next.After, s.ID) {
					t.Errorf("%s: step %s (role %s) is asked for a verdict with no on_fail, so its fail would halt %s", name, s.ID, role, next.ID)
				}
			}
		}
	}
}

func TestParseInterspersedFlags(t *testing.T) {
	a := newCLI(t).app("", &bytes.Buffer{}, &bytes.Buffer{})
	fs, c := a.flags("x")
	n := fs.Int("n", 0, "")
	pos, err := parse(fs, []string{"a", "--n", "3", "b", "--json", "--", "--not-a-flag"})
	if err != nil || *n != 3 || !c.json || strings.Join(pos, ",") != "a,b,--not-a-flag" {
		t.Errorf("pos %v n %d json %v err %v", pos, *n, c.json, err)
	}
}

// Probe from the review: a corrupt registry made `list --json` print [] with
// no word of it, and the next worker took id 1 again.
func TestCorruptRegistryWarnsAndKeepsIDs(t *testing.T) {
	c := newCLI(t)
	if err := c.p.Init(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(c.p.RunDir(4), 0o755)
	os.WriteFile(c.p.Registry(), []byte("{not json"), 0o600)
	code, out, errOut := c.run("", "list", "--json")
	if code != 0 || out != "[]\n" || !strings.Contains(errOut, "registry was unreadable and was set aside as "+c.p.Registry()+".corrupt-") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if out := c.mustRun("", "start", "x"); out != "5\n" {
		t.Errorf("start after corruption printed %q, want id 5", out)
	}
	os.Remove(c.p.SetAside()[0])
	if _, _, errOut := c.run("", "list"); strings.Contains(errOut, "warning") {
		t.Errorf("warns after the file was deleted: %q", errOut)
	}
}

// `routr CMD --help` (and `routr help CMD`) lists every flag CMD defines,
// with its description: SKILL.md sends agents there for the details.
func TestHelpDescribesEveryFlag(t *testing.T) {
	c := newCLI(t)
	for name := range commands {
		for _, args := range helpArgs(name) {
			var out, errb bytes.Buffer
			err := commands[name].run(c.app("", &out, &errb), args)
			var help helpError
			if !errors.As(err, &help) {
				t.Errorf("%s %v: %v, want a helpError", name, args, err)
				continue
			}
			for _, how := range [][]string{append([]string{name}, args...), append([]string{"help", name}, args[:len(args)-1]...)} {
				code, stdout, stderr := c.run("", how...)
				if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "usage: "+commands[name].usage+"\n\n"+commands[name].summary+"\n") {
					t.Errorf("routr %v: exit %d, stdout %q, stderr %q", how, code, stdout, stderr)
					continue
				}
				help.fs.VisitAll(func(f *flag.Flag) {
					if !strings.Contains(stdout, "--"+f.Name) || !strings.Contains(stdout, firstLine(f.Usage)) {
						t.Errorf("routr %v does not describe --%s:\n%s", how, f.Name, stdout)
					}
				})
			}
		}
	}
	if _, stdout, _ := c.run("", "start", "--help"); !strings.Contains(stdout, "(default 9m0s)") || !strings.Contains(stdout, "examples:\n  routr start") {
		t.Errorf("start --help:\n%s", stdout)
	}
}

// `routr help` lists every command with what it does; asked for, it goes to
// stdout, and bare `routr` (a usage error) to stderr.
func TestHelpListsEveryCommand(t *testing.T) {
	c := newCLI(t)
	if len(commandOrder) != len(commands) {
		t.Errorf("commandOrder has %d commands, commands %d", len(commandOrder), len(commands))
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		code, stdout, stderr := c.run("", args...)
		if code != 0 || stderr != "" {
			t.Fatalf("routr %v: exit %d, stderr %q", args, code, stderr)
		}
		for name, cmd := range commands {
			if len(cmd.summary) < 20 || !strings.Contains(stdout, "  "+cmd.usage+"\n      "+cmd.summary+"\n") {
				t.Errorf("routr %v: %s is missing or has no summary:\n%s", args, name, stdout)
			}
		}
	}
	if code, stdout, stderr := c.run(""); code != exitUsage || stdout != "" || !strings.Contains(stderr, commands["start"].summary) {
		t.Errorf("bare routr: exit %d, stdout %q", code, stdout)
	}
	if code, stdout, stderr := c.run("", "help", "nosuch"); code != exitUsage || stdout != "" || !strings.Contains(stderr, `unknown command "nosuch"`) {
		t.Errorf("routr help nosuch: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// helpArgs are the --help command lines for command name (flow has one per
// subcommand).
func helpArgs(name string) [][]string {
	if name == "flow" {
		return [][]string{{"list", "--help"}, {"show", "--help"}, {"start", "--help"}}
	}
	return [][]string{{"--help"}}
}

func firstLine(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// skills/routr/cli.md is the written contract: it names every flag.
func TestCLIDocNamesEveryFlag(t *testing.T) {
	doc, err := os.ReadFile("../../skills/routr/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	c := newCLI(t)
	for name := range commands {
		for _, args := range helpArgs(name) {
			var help helpError
			if !errors.As(commands[name].run(c.app("", &bytes.Buffer{}, &bytes.Buffer{}), args), &help) {
				continue // TestHelpDescribesEveryFlag reports it
			}
			help.fs.VisitAll(func(f *flag.Flag) {
				if !bytes.Contains(doc, []byte("--"+f.Name)) {
					t.Errorf("skills/routr/cli.md does not mention %s's --%s", help.fs.Name(), f.Name)
				}
			})
		}
	}
}

// The --json contract: every command given --json prints exactly one JSON
// value on stdout and nothing else (notes go to stderr).
func TestEveryCommandHonorsJSON(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	covered := map[string]bool{}
	run := func(want int, args ...string) map[string]any {
		t.Helper()
		covered[args[0]] = true
		code, out, errOut := c.run("", append(args, "--json")...)
		if code != want {
			t.Fatalf("routr %v --json: exit %d, want %d\nstdout %q\nstderr %q", args, code, want, out, errOut)
		}
		dec := json.NewDecoder(strings.NewReader(out))
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("routr %v --json printed no JSON: %v\n%s", args, err, out)
		}
		if dec.More() || strings.TrimSpace(out[dec.InputOffset():]) != "" {
			t.Fatalf("routr %v --json printed more than one JSON value:\n%s", args, out)
		}
		m, _ := v.(map[string]any)
		return m
	}
	action := func(m map[string]any, want string, ids ...int) {
		t.Helper()
		ws, _ := m["workers"].([]any)
		var got []int
		for _, w := range ws {
			got = append(got, int(w.(map[string]any)["id"].(float64)))
		}
		if m["action"] != want || !slices.Equal(got, ids) {
			t.Errorf("%s: %v", want, m)
		}
	}

	run(0, "start", "Look into auth.go")
	c.until("worker 1 started", func() bool { return c.worker(1).Live != nil })
	action(run(0, "show", "1"), "show", 1)
	action(run(0, "hide", "1"), "hide", 1)
	c.fake.Ask(1, "Allow curl?", "Yes", "No")
	c.until("worker 1 blocked", func() bool { return c.worker(1).State == registry.Blocked })
	action(run(0, "answer", "1", "1", "--by", "human", "--expect", watchtest.Fingerprint("Allow curl?", []string{"Yes", "No"})), "answer", 1)
	c.fake.Finish(1, "found it")
	c.until("worker 1 maybe done", func() bool { return c.worker(1).State == registry.MaybeDone })
	run(0, "send", "1", "why?")
	c.until("the follow-up sent", func() bool { w := c.worker(1); return w.Turn == 1 && len(w.Inbox) == 0 })
	run(0, "done", "--id", "1", "found it")
	c.fake.Finish(1, "because")
	run(0, "wait", "1", "--timeout", "10s")
	run(0, "result", "1")
	action(run(0, "close", "1"), "close", 1)

	run(0, "start", "--gate", "you", "Fix it")
	action(run(0, "approve", "2"), "approve", 2)
	run(0, "start", "--after", "2", "Check it")
	action(run(0, "cancel", "3"), "cancel", 3)
	c.seed(registry.Worker{Harness: "claude", Brief: "Try again", State: registry.Failed, Error: "could not start claude: busy"})
	action(run(0, "retry", "4"), "retry", 4)
	run(0, "list")
	run(0, "roles")
	run(0, "flow", "list")
	run(0, "flow", "show", "research")
	run(0, "flow", "start", "plan-redteam", "--task", "x")
	run(0, "watch")
	run(0, "doctor")
	run(0, "version")
	for name := range commands {
		if !covered[name] {
			t.Errorf("routr %s --json is not covered", name)
		}
	}
}

func TestDoctor(t *testing.T) {
	c := newCLI(t)
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	out := squeeze(c.mustRun("", "doctor"))
	for _, want := range []string{"OK herdr reachable at /tmp/fake-herdr.sock", "OK watcher not running", "OK codex installed", "OK caller claude sess-main"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}

	c.unready = map[string]error{"codex": errors.New("codex: not logged in")}
	if err := c.p.Init(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.p.Registry(), []byte("{not json"), 0o600)
	out = squeeze(c.mustRun("", "doctor"))
	for _, want := range []string{"WARN codex codex: not logged in → install or log in to codex", "WARN registry an unreadable registry was set aside"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}

	c.unready = map[string]error{"claude": errors.New("x"), "codex": errors.New("x"), "pi": errors.New("x")}
	c.reach = &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.EPERM)}
	code, out, _ := c.run("", "doctor", "--json")
	var got struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != exitError || got.OK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	failed := map[string]string{}
	for _, ch := range got.Checks {
		if ch.Status == "fail" {
			failed[ch.Check] = ch.Fix
		}
	}
	if !strings.Contains(failed["herdr"], "outside the sandbox") || failed["harnesses"] == "" || len(failed) != 2 {
		t.Errorf("failed checks %v", failed)
	}
}

// doctor in a directory routr has not been used in leaves it as it was.
func TestDoctorCreatesNothing(t *testing.T) {
	c := newCLI(t)
	out := c.mustRun("", "doctor")
	if _, err := os.Stat(c.p.Dir); !os.IsNotExist(err) || !strings.Contains(out, "routr start creates it") {
		t.Errorf("stat %s: %v\n%s", c.p.Dir, err, out)
	}
}

func TestRuntimeForeachFromTheCLI(t *testing.T) {
	c := newCLI(t)
	c.watcher()
	flows := filepath.Join(c.dir, ".routr", "flows", "orch.toml")
	os.MkdirAll(filepath.Dir(flows), 0o755)
	os.WriteFile(flows, []byte(`
[flow.orch]
params = ["task"]
[[flow.orch.step]]
id = "plan"
brief = "Split {{task}} into tasks/*.md."
[[flow.orch.step]]
id = "dig"
role = "research"
after = ["plan"]
foreach = "tasks/*.md"
brief = "Do {{item.path}}."
[[flow.orch.step]]
id = "join"
after = ["dig"]
brief = "Merge {{dig.outputs}}."
`), 0o644)
	code, out, errOut := c.run("", "flow", "start", "orch", "--task", "x")
	if code != 0 || out != "orch-1\n" || !strings.Contains(errOut, "step dig: one worker per file of tasks/*.md, once the steps before it are done") {
		t.Fatalf("flow start: %d %q %q", code, out, errOut)
	}
	if fi, err := os.Stat(c.p.WorkDir("orch-1")); err != nil || !fi.IsDir() {
		t.Errorf("flow start made no work directory: %v", err)
	}
	c.until("the planner running", func() bool { return c.worker(1).State == registry.Running })
	os.MkdirAll(filepath.Join(c.dir, "tasks"), 0o755)
	os.WriteFile(filepath.Join(c.dir, "tasks", "a.md"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(c.dir, "tasks", "b.md"), []byte("b"), 0o644)
	c.env["ROUTR_ID"], c.env["ROUTR_RUN_DIR"] = "1", c.p.RunDir(1)
	c.mustRun("", "done", "split into 2")
	delete(c.env, "ROUTR_ID")
	delete(c.env, "ROUTR_RUN_DIR")
	c.fake.Finish(1, "")
	c.until("both copies running", func() bool {
		a, errA := c.p.Store().Get(4)
		b, errB := c.p.Store().Get(5)
		return errA == nil && errB == nil && a.State == registry.Running && b.State == registry.Running
	})

	if code, _, errOut := c.run("", "send", "2", "hi"); code != exitError || !strings.Contains(errOut, "worker 2 is a foreach step") {
		t.Errorf("send to the foreach worker: %d %s", code, errOut)
	}
	if out := c.mustRun("", "list"); !strings.Contains(out, "expanded into 2 workers (4, 5)") {
		t.Errorf("list:\n%s", out)
	}
	out = c.mustRun("", "cancel", "orch-1")
	for id := 2; id <= 5; id++ {
		if w := c.worker(id); w.State != registry.Cancelled || w.Live != nil {
			t.Errorf("after cancel, worker %d = %s (live %v)", id, w.State, w.Live)
		}
	}
	if c.fake.Open(4)+c.fake.Open(5) != 0 || !strings.Contains(out, "cancelled 4 research") {
		t.Errorf("cancel left tabs open, or said %q", out)
	}
	if code, _, _ := c.run("", "wait", "orch-1"); code != exitFailed {
		t.Errorf("wait on the cancelled chain: %d", code)
	}
}

// `routr result` on a worker with no final message said "has no final
// message yet … Run: routr wait N" whatever its state, though a failed,
// cancelled or blocked worker never gets one; and a failed worker's old
// final.md read as its result.
func TestResultSaysWhyByState(t *testing.T) {
	c := newCLI(t)
	now := time.Now()
	c.seed(
		registry.Worker{State: registry.Failed, Error: "worker: start 1 task: agent_pane_busy"},
		registry.Worker{State: registry.Failed, Started: now, Error: "could not resume its session"},
		registry.Worker{State: registry.Failed, Started: now, Session: "s3", Error: "the API went away"},
		registry.Worker{State: registry.Cancelled, Started: now},
		registry.Worker{State: registry.NeedsYou, Started: now, Note: "the review loop hit its cap of 2 rounds"},
		registry.Worker{Chain: "f", Step: "dig", State: registry.Waiting, After: []int{7, 8}, Fanout: &registry.Fanout{Glob: "t/*.md", MaxItems: 4, Items: []int{7, 8}}},
		registry.Worker{Chain: "f", Step: "dig", Title: "a", State: registry.Done, Finished: now, Verdict: registry.Pass, Output: filepath.Join(c.dir, "t/a.out")},
		registry.Worker{Chain: "f", Step: "dig", Title: "b", State: registry.Running, Started: now},
		registry.Worker{State: registry.Running, Started: now.Add(-time.Minute)},
		registry.Worker{State: registry.Gated, Gate: "you"},
		registry.Worker{State: registry.Blocked, Started: now, Question: "Delete build/?\n1. Yes\n2. No"},
		registry.Worker{Chain: "r", Step: "plan", State: registry.Done, Started: now, Finished: now, Turn: 1, Replies: 1, Revisions: []int{1}},
		registry.Worker{Chain: "r", Step: "review", Role: "review", State: registry.Waiting, After: []int{12}, Round: 2, OnFail: &registry.OnFail{Resume: 12, Brief: "fix it", Max: 3}},
		registry.Worker{State: registry.Done, Finished: now, Summary: "nothing to change"},
	)
	registry.WriteFile(c.final(2), []byte("round 1 plan"))
	registry.WriteFile(c.final(12), []byte("plan v1"))
	registry.WriteFile(c.p.TurnFile(12, 1), []byte("plan v2"))
	for _, tt := range []struct {
		id   int
		code int
		want []string
		not  []string
	}{
		{1, 0, []string{"failed: 1 task: worker: start 1 task: agent_pane_busy\n", "It never started. routr retry 1 starts it again"}, []string{"routr send", "routr wait"}},
		{2, 0, []string{"failed: 2 task: could not resume its session\nroutr retry 2 runs it again\n\nround 1 plan\n"}, []string{"routr wait"}},
		{3, 0, []string{"failed: 3 task: the API went away\n", "routr send 3 … resumes its session"}, []string{"retry", "routr wait"}},
		{4, 0, []string{"cancelled: 4 task\n"}, []string{"routr wait"}},
		{5, 0, []string{"needs you: 5 task: the review loop hit its cap of 2 rounds"}, []string{"routr wait"}},
		{6, 1, []string{"6 task is a foreach step; its copies:\n", "  7 a: done, verdict pass, output t/a.out\n", "  8 b: running\n", "Run: routr wait 6\n"}, nil},
		{9, 1, []string{"worker 9 has no final message yet (running for 1m", "Run: routr wait 9"}, nil},
		{10, 0, []string{"gated: 10 task waits for your approval. Run: routr approve 10"}, []string{"routr wait"}},
		{11, 0, []string{"blocked: 11 task asks:\nDelete build/?", "The user answers this question"}, []string{"routr wait"}},
		{12, 0, []string{"revision for 13 review (on_fail round 1):\nplan v2"}, []string{"follow-up"}},
		{14, 0, []string{"14 task: done — nothing to change; it wrote no final message\n"}, []string{"routr wait"}},
	} {
		code, out, errOut := c.run("", "result", strconv.Itoa(tt.id))
		got := out + errOut
		if code != tt.code {
			t.Errorf("result %d: exit %d, want %d\n%s", tt.id, code, tt.code, got)
		}
		for _, w := range tt.want {
			if !strings.Contains(got, w) {
				t.Errorf("result %d lacks %q:\n%s", tt.id, w, got)
			}
		}
		for _, w := range tt.not {
			if strings.Contains(got, w) {
				t.Errorf("result %d says %q:\n%s", tt.id, w, got)
			}
		}
	}
}

// A failed worker that saved a final message or a reply printed its error
// and that text, but not what to do next: failedNext came only with no
// result. The state line and next step come first, then the result, so a
// reader that clips the result (a Pi wake) keeps them.
func TestFailedResultSaysWhatNext(t *testing.T) {
	c := newCLI(t)
	now := time.Now()
	c.seed(
		registry.Worker{State: registry.Failed, Started: now, Error: "the harness exited before it ran routr done"},
		registry.Worker{State: registry.Failed, Started: now, Session: "s2", Error: "could not resume its session", Turn: 1, Replies: 1},
	)
	registry.WriteFile(c.final(1), []byte("SAVED FINAL\n"))
	registry.WriteFile(c.p.TurnFile(2, 1), []byte("SAVED REPLY\n"))
	for _, tt := range []struct {
		id         int
		next, body string
	}{
		{1, "routr retry 1 runs it again", "SAVED FINAL"},
		{2, "routr send 2 … resumes its session", "reply to follow-up 1:\nSAVED REPLY"},
	} {
		id := strconv.Itoa(tt.id)
		code, out, _ := c.run("", "result", id)
		head := fmt.Sprintf("failed: %d task: %s\n%s\n\n", tt.id, c.worker(tt.id).Error, tt.next)
		if code != exitOK || !strings.HasPrefix(out, head) || !strings.Contains(out, tt.body) {
			t.Errorf("result %d: exit %d\n%s\nwant it to start %q and hold %q", tt.id, code, out, head, tt.body)
		}
		if code, out, _ := c.run("", "wait", id); code != exitFailed || !strings.Contains(out, tt.next) {
			t.Errorf("wait %d: exit %d\n%s", tt.id, code, out)
		}
	}
}

// The lifecycle behind it: the harness replies, then exits before routr
// done; the watcher saves the reply and records Failed. routr result must
// still say how to resume it.
func TestExitedWorkerResultKeepsRecovery(t *testing.T) {
	c := newCLI(t)
	c.mustRun("", "start", "work")
	c.watcher()
	c.until("started", func() bool { return c.worker(1).State == registry.Running })
	c.fake.Finish(1, "last partial reply")
	c.fake.SetPhase(1, worker.Exited)
	c.until("exited", func() bool { return c.worker(1).State == registry.Failed })
	code, out, _ := c.run("", "result", "1")
	if code != exitOK || !strings.Contains(out, "routr send 1 … resumes its session") || !strings.Contains(out, "last partial reply") {
		t.Errorf("result: exit %d\n%s", code, out)
	}
}

// What to do about a failed worker comes from its record alone, never from
// its error text: one that never started is started again, one with no
// session is run again, and one with a session is resumed. routr wait and
// routr result say the same. An error that names a command (an old
// record's) no longer hides the line.
func TestFailedNextFromTheRecord(t *testing.T) {
	c := newCLI(t)
	now := time.Now()
	c.seed(
		registry.Worker{State: registry.Failed, Error: "could not start claude: agent_pane_busy. routr retry 1 starts it again"},
		registry.Worker{State: registry.Failed, Started: now, Error: "the harness exited before it ran routr done"},
		registry.Worker{State: registry.Failed, Started: now, Session: "s3", Error: "the harness exited before it ran routr done"},
		registry.Worker{State: registry.Failed, Started: now, Run: "make test", Error: "its command exited 2"},
	)
	for _, tt := range []struct {
		id   int
		want string
		not  string
	}{
		{1, "\nIt never started. routr retry 1 starts it again with its brief", "routr send"},
		{2, "\nroutr retry 2 runs it again", "routr send"},
		{3, "\nroutr send 3 … resumes its session", "retry"},
		{4, "\nroutr retry 4 runs it again", "routr send"},
	} {
		for _, cmd := range []string{"wait", "result"} {
			_, out, errOut := c.run("", cmd, strconv.Itoa(tt.id))
			got := out + errOut
			if !strings.Contains(got, tt.want) || strings.Contains(got, tt.not) {
				t.Errorf("%s %d: want %q, not %q:\n%s", cmd, tt.id, tt.want, tt.not, got)
			}
		}
	}
}

func TestForeachWorkerInWaitCancelApprove(t *testing.T) {
	c := newCLI(t)
	now := time.Now()
	c.seed(
		registry.Worker{Chain: "f", Step: "plan", State: registry.Done, Finished: now, Summary: "planned"},
		registry.Worker{Chain: "f", Step: "dig", Role: "research", State: registry.Done, Finished: now, After: []int{1, 4, 5},
			Fanout: &registry.Fanout{Glob: "t/*.md", MaxItems: 12, Items: []int{4, 5}}, Summary: "expanded into 2 workers (4, 5), all done"},
		registry.Worker{Chain: "f", Step: "join", State: registry.Done, Finished: now, After: []int{2}, Summary: "merged"},
		registry.Worker{Chain: "f", Step: "dig", Role: "research", State: registry.Done, Finished: now, After: []int{1}, Summary: "a"},
		registry.Worker{Chain: "f", Step: "dig", Role: "research", State: registry.Done, Finished: now, After: []int{1}, Summary: "b"},
		// A foreach worker that found no files, and its copies' ids are
		// still to come; and one whose copies run outside a chain target.
		registry.Worker{Chain: "g", Step: "dig", State: registry.NeedsYou, Fanout: &registry.Fanout{Glob: "t/*.md", MaxItems: 12},
			Note: `foreach "t/*.md" matched no files`},
		registry.Worker{Chain: "h", Step: "dig", State: registry.Waiting, Fanout: &registry.Fanout{Glob: "t/*.md", MaxItems: 12, Items: []int{8}}, After: []int{8}},
		registry.Worker{Chain: "h", Step: "dig", State: registry.Pending},
	)
	registry.WriteFile(c.final(3), []byte("merged all"))
	registry.WriteFile(c.final(5), []byte("copy b"))
	// The chain's last step is the join, not the copy with the highest id.
	if out := c.mustRun("", "wait", "f"); !strings.Contains(out, "2 research: done — expanded into 2 workers (4, 5), all done") || !strings.HasSuffix(out, "\nmerged all\n") {
		t.Errorf("wait f:\n%s", out)
	}
	if code, out, _ := c.run("", "wait", "g"); code != exitNeedsYou || !strings.Contains(out, "matched no files") {
		t.Errorf("wait g: %d %q", code, out)
	}
	if out := c.mustRun("", "approve", "g"); !strings.Contains(out, "6 task: looking for its files again") || c.worker(6).State != registry.Pending {
		t.Errorf("approve g: %q %s", out, c.worker(6).State)
	}
	// Cancelling the foreach worker by id cancels its copies.
	c.mustRun("", "cancel", "7")
	if c.worker(7).State != registry.Cancelled || c.worker(8).State != registry.Cancelled {
		t.Errorf("cancel 7: %s, copy %s", c.worker(7).State, c.worker(8).State)
	}
}

func TestRunStepFromTheCLI(t *testing.T) {
	c := newCLI(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"start", "--run", "true", "a brief"}, "not both"},
		{[]string{"start", "--run", "true", "--role", "review"}, "--role: a --run step runs a command"},
		{[]string{"start", "--run", "true", "--expect", "maybe"}, `--expect "maybe"`},
		{[]string{"start", "--run", "true", "--run-timeout", "0s"}, "--run-timeout 0s"},
		{[]string{"start", "--expect", "fail", "a brief"}, "go with --run"},
		{[]string{"start", "--run", " "}, "give the command"},
		{[]string{"start", "--run", "cat {{prev.output}}"}, "needs --after"},
	} {
		if code, _, errOut := c.run("", tt.args...); code != exitUsage || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: exit %d %q, want usage with %q", tt.args, code, errOut, tt.want)
		}
	}

	c.watcher()
	c.mustRun("", "start", "--run", "sleep 30", "--title", "slow")
	w := c.worker(1)
	if w.Role != "run" || w.Run != "sleep 30" || !w.Write || w.Timeout != registry.DefaultRunTimeout || w.Title != "slow" || w.Harness != "" {
		t.Errorf("worker = %+v", w)
	}
	c.until("the command running", func() bool { w = c.worker(1); return w.State == registry.Running })
	if out := c.mustRun("", "list"); !strings.Contains(out, "slow") || !strings.Contains(out, " run ") || !strings.Contains(out, " sh ") {
		t.Errorf("list:\n%s", out)
	}
	if code, _, errOut := c.run("", "send", "1", "hi"); code != exitError || !strings.Contains(errOut, "worker 1 is a run step") {
		t.Errorf("send: %d %s", code, errOut)
	}
	if code, _, errOut := c.run("", "close", "1"); code != exitError || !strings.Contains(errOut, "routr cancel 1 stops its command") {
		t.Errorf("close: %d %s", code, errOut)
	}
	if out := c.mustRun("", "cancel", "1"); !strings.Contains(out, "cancelled 1 slow · run") {
		t.Errorf("cancel: %q", out)
	}
	c.until("the command gone", func() bool { return syscall.Kill(-w.Pid, 0) != nil })

	// A pipe: a step, then a check on what it wrote.
	c.mustRun("", "start", "--run", "echo hello > out.txt", "--step", "write")
	code, out, errOut := c.run("2\n", "start", "--after", "-", "--run", "grep -q bye {{write.output}} || { echo no bye; exit 2; }")
	if code != 0 || out != "3\n" {
		t.Fatalf("pipe: %d %q %q", code, out, errOut)
	}
	// The failing check ends the chain: that is its answer.
	code, out, _ = c.run("", "wait", "3")
	if code != exitOK || !strings.Contains(out, "fail: exit 2 after") || !strings.Contains(out, "no bye") || !strings.HasSuffix(out, "verdict: fail\n") {
		t.Errorf("wait: %d\n%s", code, out)
	}
	if code, out, _ := c.run("", "wait", "c2"); code != exitOK || !strings.Contains(out, "3 grep-q-bye-echo-no~ · run: done, verdict fail — exit 2 after") {
		t.Errorf("wait c2: %d\n%s", code, out)
	}
	if out := c.mustRun("", "result", "3"); !strings.Contains(out, "fail: exit 2 after") || !strings.Contains(out, "output: .routr/runs/3/run.log") || !strings.HasSuffix(out, "verdict: fail\n") {
		t.Errorf("result:\n%s", out)
	}
	if cmd := readFile(filepath.Join(c.p.RunDir(3), "brief.md")); !strings.Contains(cmd, "grep -q bye .routr/runs/2/run.log ||") {
		t.Errorf("command = %q", cmd)
	}
	// A step piped after it, once it failed, waits for you.
	if code, out, errOut := c.run("3\n", "start", "--after", "-", "--run", "echo next", "--title", "next"); code != 0 || out != "4\n" {
		t.Fatalf("pipe after the fail: %d %q %q", code, out, errOut)
	}
	code, out, _ = c.run("", "wait", "c2")
	if code != exitNeedsYou || !strings.Contains(out, "4 next · run waits on 3 grep-q-bye-echo-no~ · run:\nneeds you:") ||
		!strings.Contains(out, "routr approve 3 lets the chain go on past it") || !strings.Contains(out, "no bye") {
		t.Errorf("wait c2 with a step after the fail: %d\n%s", code, out)
	}
	c.until("next parked", func() bool {
		return strings.HasPrefix(c.worker(4).Note, "waits: 3 grep-q-bye-echo-no~ · run failed (exit 2 after")
	})
	if out := c.mustRun("", "list"); !strings.Contains(out, "waits: 3 grep-q-bye-echo-no~ · run failed (exit 2") {
		t.Errorf("list:\n%s", out)
	}
	if out := c.mustRun("", "list", "--json"); strings.Count(out, `"halts": true`) != 1 {
		t.Errorf("list --json: want worker 3 alone to halt:\n%s", out)
	}
	if out := c.mustRun("", "approve", "c2"); !strings.Contains(out, "3 grep-q-bye-echo-no~ · run: the chain goes on past its fail") {
		t.Errorf("approve: %q", out)
	}
	if w := c.worker(3); w.State != registry.Done || w.Verdict != registry.Fail || !w.GoOn {
		t.Errorf("after approve: %+v", w)
	}
	if code, out, _ := c.run("", "wait", "4"); code != exitOK || !strings.Contains(out, "next") {
		t.Errorf("wait 4: %d\n%s", code, out)
	}
	if code, _, errOut := c.run("", "approve", "3"); code != exitError || !strings.Contains(errOut, "no step that failed with no on_fail") {
		t.Errorf("approve again: %d %s", code, errOut)
	}
}

func TestFlowWithARunStep(t *testing.T) {
	c := newCLI(t)
	flows := filepath.Join(c.dir, ".routr", "flows", "fix.toml")
	os.MkdirAll(filepath.Dir(flows), 0o755)
	os.WriteFile(flows, []byte(`
[flow.fix]
params = ["check"]
[[flow.fix.step]]
id = "implement"
role = "implement"
brief = "Fix it."
[[flow.fix.step]]
id = "check"
after = ["implement"]
run = "{{check}}"
timeout = "5m"
on_fail = { resume = "implement", brief = "The check failed; see {{check.output}}.", max = 5 }
`), 0o644)
	if out := c.mustRun("", "flow", "show", "fix"); !strings.Contains(out, "step check: run · after implement · timeout 5m · on fail resume implement, max 5\n  run: {{check}}") {
		t.Errorf("flow show:\n%s", out)
	}
	code, out, errOut := c.run("", "flow", "start", "fix", "--param", "check=go test ./...")
	if code != 0 || out != "fix-1\n" || !strings.Contains(errOut, "queued 2 check · run (sh) step check") {
		t.Fatalf("flow start: %d %q %q", code, out, errOut)
	}
	w := c.worker(2)
	if w.Role != "run" || !w.Write || w.Timeout != 5*time.Minute || w.Socket != "/tmp/fake-herdr.sock" || w.Tree == "" || w.WantVerdict || w.Preamble != "" || w.OnFail == nil {
		t.Errorf("check = %+v", w)
	}
}

// write = false keeps a step that only writes into {{work}} off the
// directory lock, whatever its role; verdict = true reaches the worker.
func TestWriteFalseStepTakesNoLock(t *testing.T) {
	c := newCLI(t)
	flows := filepath.Join(c.dir, ".routr", "flows", "notes.toml")
	os.MkdirAll(filepath.Dir(flows), 0o755)
	os.WriteFile(flows, []byte(`
[flow.notes]
params = ["task"]
[[flow.notes.step]]
id = "plan"
output = "{{work}}/PLAN.md"
write = false
brief = "Plan {{task}}."
[[flow.notes.step]]
id = "implement"
role = "implement"
after = ["plan"]
verdict = true
brief = "Do {{plan.output}}."
`), 0o644)
	if out := c.mustRun("", "flow", "show", "notes"); !strings.Contains(out, "step plan: role task · write false · output {{work}}/PLAN.md") ||
		!strings.Contains(out, "step implement: role implement · after plan · verdict") {
		t.Errorf("flow show:\n%s", out)
	}
	c.mustRun("", "flow", "start", "notes", "--task", "x")
	if plan := c.worker(1); plan.Role != "task" || plan.Write {
		t.Errorf("plan = %+v", plan)
	}
	if impl := c.worker(2); !impl.Write || !impl.WantVerdict {
		t.Errorf("implement = %+v", impl)
	}
}

func TestFlowWithAJudgeStep(t *testing.T) {
	c := newCLI(t)
	flows := filepath.Join(c.dir, ".routr", "flows", "ship.toml")
	os.MkdirAll(filepath.Dir(flows), 0o755)
	os.WriteFile(flows, []byte(`
[flow.ship]
params = ["task"]
[[flow.ship.step]]
id = "implement"
role = "implement"
brief = "{{task}}"
[[flow.ship.step]]
id = "ok"
after = ["implement"]
judge = "jev"
brief = "Is {{task}} done?"
fail = "it needs more work"
on_fail = { resume = "implement", brief = "Not yet; see {{ok.final}}.", max = 2 }
`), 0o644)
	if out := c.mustRun("", "flow", "show", "ship"); !strings.Contains(out, "step ok: judge jev · after implement · on fail resume implement, max 2\n  brief: Is {{task}} done?\n  fail: it needs more work\n") {
		t.Errorf("flow show:\n%s", out)
	}
	code, out, errOut := c.run("", "flow", "start", "ship", "--task", "add login")
	if code != 0 || out != "ship-1\n" || !strings.Contains(errOut, "queued 2 ok · jev (jev) step ok") {
		t.Fatalf("flow start: %d %q %q", code, out, errOut)
	}
	w := c.worker(2)
	if w.Role != "judge" || w.Write || w.Harness != "" || w.WantVerdict || w.Preamble != "" || w.Judge == nil || w.Judge.Fail != "it needs more work" ||
		len(w.Judge.From) != 1 || w.Judge.From[0] != 1 || w.Socket != "/tmp/fake-herdr.sock" {
		t.Errorf("ok = %+v", w)
	}

	// Jev was unsure: approve takes its choice.
	c.p.Store().Update(2, func(x *registry.Worker) { x.State, x.Note = registry.NeedsYou, "Jev is unsure" })
	if code, _, errOut := c.run("", "send", "2", "hi"); code != exitError || !strings.Contains(errOut, "worker 2 is a judge step; it asks Jev") {
		t.Errorf("send: %d %s", code, errOut)
	}
	if code, _, errOut := c.run("", "close", "2"); code != exitError || !strings.Contains(errOut, "worker 2 is a judge step; it has no tab. routr cancel 2 stops it") {
		t.Errorf("close: %d %s", code, errOut)
	}
	if out := c.mustRun("", "approve", "2"); !strings.Contains(out, "2 ok · jev: Jev's choice taken (or asked again)") {
		t.Errorf("approve: %q", out)
	}
	if w := c.worker(2); w.State != registry.Pending || !w.Judge.Accept {
		t.Errorf("after approve: %+v %+v", w, w.Judge)
	}
}

func TestDoctorReportsTheJevKey(t *testing.T) {
	c := newCLI(t)
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, "WARN jev OPENROUTER_API_KEY is not set: a judge step") {
		t.Errorf("doctor:\n%s", out)
	}
	c.env["OPENROUTER_API_KEY"] = "sk-test"
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, "OK jev OPENROUTER_API_KEY is set") || strings.Contains(out, "sk-test") {
		t.Errorf("doctor:\n%s", out)
	}
}

// A flow with worktree steps, end to end: two writers start side by
// side, each in its own worktree; list and result show where; routr done
// from inside a worktree finds the main repo's registry, with or without
// ROUTR_STATE_DIR.
func TestFlowWorktrees(t *testing.T) {
	c := newCLI(t)
	if real, err := filepath.EvalSymlinks(c.dir); err == nil {
		c.dir = real
		c.p = registry.Paths{Dir: filepath.Join(real, ".routr")}
		c.env["ROUTR_STATE_DIR"] = c.p.Dir
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = c.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(c.dir, "main.go"), []byte("package main\n"), 0o644)
	git("add", "main.go")
	git("commit", "-q", "-m", "first")
	os.MkdirAll(filepath.Join(c.p.Dir, "flows"), 0o755)
	os.WriteFile(filepath.Join(c.p.Dir, "flows", "b.toml"), []byte(`
[flow.b]
[[flow.b.step]]
id = "a"
worktree = true
brief = "Try one way."
[[flow.b.step]]
id = "b"
worktree = true
brief = "Try another."
`), 0o644)
	os.WriteFile(filepath.Join(c.dir, "wip.txt"), []byte("not committed\n"), 0o644)
	code, out, errOut := c.run("", "flow", "start", "b")
	if code != 0 || out != "b-1\n" || !strings.Contains(errOut, "routr: the worktrees start from HEAD (") || !strings.Contains(errOut, "uncommitted changes here are not in them") {
		t.Fatalf("flow start: %d %q %s", code, out, errOut)
	}
	if w := c.worker(1); !w.Write || w.Checkout == nil || w.Checkout.Repo != c.dir || w.Checkout.Path != "" {
		t.Fatalf("a = %+v", w)
	}
	c.watcher()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if c.worker(1).State == registry.Running && c.worker(2).State == registry.Running {
			break
		}
	}
	if a, b := c.worker(1), c.worker(2); a.State != registry.Running || b.State != registry.Running {
		t.Fatalf("a = %s %s %s, b = %s %s %s", a.State, a.Note, a.Error, b.State, b.Note, b.Error)
	}
	a := c.worker(1)
	tree := filepath.Join(c.p.Dir, "trees", "b-1", "a-1")
	if a.Cwd != tree || !a.Checkout.Made {
		t.Fatalf("a = %+v", a)
	}
	if out := c.mustRun("", "list"); !strings.Contains(out, "worktrees:\n  1 a: .routr/trees/b-1/a-1 (branch routr/b-1/a-1)\n  2 b: .routr/trees/b-1/b-2 (branch routr/b-1/b-2)\n") {
		t.Errorf("list:\n%s", out)
	}

	// routr done in a's worktree, without ROUTR_STATE_DIR: git finds the
	// main repo.
	main := c.dir
	c.dir = tree
	delete(c.env, "ROUTR_STATE_DIR")
	c.env["ROUTR_ID"] = "1"
	os.WriteFile(filepath.Join(tree, "notes.md"), []byte("a's notes\n"), 0o644)
	c.mustRun("", "done", "--output", "notes.md", "tried a")
	c.dir = main
	c.env["ROUTR_STATE_DIR"] = c.p.Dir
	delete(c.env, "ROUTR_ID")
	if !registry.HasDone(c.p.RunDir(1)) {
		t.Fatal("routr done in the worktree wrote no done.json in the main repo's run dir")
	}
	c.fake.Finish(1, "Done.")
	c.until("a done", func() bool { return c.worker(1).State == registry.Done })
	if w := c.worker(1); w.Output != filepath.Join(tree, "notes.md") {
		t.Errorf("a's output = %q", w.Output)
	}
	if out := c.mustRun("", "result", "1"); !strings.Contains(out, "worktree: .routr/trees/b-1/a-1 (branch routr/b-1/a-1)") {
		t.Errorf("result:\n%s", out)
	}
}

func TestVersionPrintsBuildInfo(t *testing.T) {
	c := newCLI(t)
	if out := c.mustRun("", "version"); !strings.HasPrefix(out, "routr unknown") {
		t.Errorf("with no build info: %q", out)
	}
	c.build = buildInfo{Revision: "2a7d3e18d7de6741fcede75302909d7113b4e4f0", Time: "2026-10-02T20:00:00Z", Modified: true, Go: "go1.26.7", Src: "/src/routr"}
	out := c.mustRun("", "version")
	if !strings.HasPrefix(out, "routr 2a7d3e18d7de+dirty (2026-10-02T20:00:00Z) go1.26.7\n") || !strings.Contains(out, "source: /src/routr") {
		t.Errorf("version:\n%s", out)
	}
	var got buildInfo
	if err := json.Unmarshal([]byte(c.mustRun("", "version", "--json")), &got); err != nil || got != c.build {
		t.Errorf("version --json = %+v, %v; want %+v", got, err, c.build)
	}
	// the binary go test built: no VCS stamp, but it reads without failing
	if b := readBuild(); b.Go == "" {
		t.Errorf("readBuild() = %+v: no Go version", b)
	}
}

// git runs git in dir for a test and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// doctor's build line compares the binary's revision with its checkout.
func TestDoctorChecksTheBuild(t *testing.T) {
	src := t.TempDir()
	git(t, src, "init", "-q")
	commit := func(file, msg string) string {
		os.MkdirAll(filepath.Join(src, filepath.Dir(file)), 0o755)
		os.WriteFile(filepath.Join(src, file), []byte(msg), 0o644)
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", msg)
		return git(t, src, "rev-parse", "HEAD")
	}
	built := commit("cmd/routr/main.go", "first")
	c := newCLI(t)
	build := func() check {
		t.Helper()
		var got struct{ Checks []check }
		if err := json.Unmarshal([]byte(c.mustRun("", "doctor", "--json")), &got); err != nil {
			t.Fatal(err)
		}
		for _, ch := range got.Checks {
			if ch.Check == "build" {
				return ch
			}
		}
		t.Fatal("doctor has no build check")
		return check{}
	}

	if ch := build(); ch.Status != "ok" || !strings.Contains(ch.Detail, "no VCS revision") {
		t.Errorf("no build info: %+v", ch)
	}
	c.build = buildInfo{Revision: built}
	if ch := build(); ch.Status != "ok" || !strings.Contains(ch.Detail, "built outside install.sh") {
		t.Errorf("no source checkout: %+v", ch)
	}
	c.build.Src = src
	if ch := build(); ch.Status != "ok" || !strings.Contains(ch.Detail, "is current with "+src) {
		t.Errorf("current: %+v", ch)
	}
	commit("skills/routr/cli.md", "docs only")
	if ch := build(); ch.Status != "ok" {
		t.Errorf("a commit outside the build paths: %+v", ch)
	}
	commit("internal/watch/watch.go", "newer watcher")
	ch := build()
	if ch.Status != "warn" || !strings.Contains(ch.Detail, "1 commits behind "+src) || !strings.Contains(ch.Detail, "newer watcher") ||
		ch.Fix != "run "+src+"/install.sh" {
		t.Errorf("behind: %+v", ch)
	}
	c.build = buildInfo{Revision: git(t, src, "rev-parse", "HEAD"), Modified: true, Src: src}
	if ch := build(); ch.Status != "warn" || !strings.Contains(ch.Detail, "uncommitted changes") {
		t.Errorf("dirty: %+v", ch)
	}
	c.build = buildInfo{Revision: "0123456789abcdef0123456789abcdef01234567", Src: src}
	if ch := build(); ch.Status != "warn" || !strings.Contains(ch.Detail, "cannot compare") {
		t.Errorf("a revision the checkout lacks: %+v", ch)
	}
}
