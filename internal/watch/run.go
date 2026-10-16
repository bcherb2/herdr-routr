package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// Run steps (skills/routr/flows.md, "Run steps"): a worker with Run set runs a shell
// command instead of a harness, and its exit status is its verdict. It is
// admitted as any write worker is (it takes the directory lock), then
// launch starts `sh -c` in its own process group with stdout and stderr in
// run.log, and a goroutine waits for it. The goroutine touches neither the
// registry nor the clock; the next tick records how the command ended
// (tendRuns), and follows on_fail as for a review step. A fail without
// on_fail halts the steps after it.
//
// The worker records the command's pid and when that process started
// (PidAt): together they name it for good, where a pid alone may have been
// reused. A watcher that stopped mid-command leaves the worker running with
// no process of the next watcher's behind it; the next one runs it again
// (rerun), first killing the old process group if its leader is still the
// recorded process. Nothing the command started decides that, so a server
// it detached from its group (setsid, a compiler cache's) outlives it
// without holding up the next run. A group whose leader is gone may be the
// command's leftovers or a later process's that took its pid: routr cannot
// tell, so the step fails saying how to look.

// Run step timing.
var (
	runGrace   = 5 * time.Second // after SIGTERM on a timeout, SIGKILL
	runDrain   = 2 * time.Second // after the command, how long its output may still come
	runFree    = 5 * time.Second // after killing a stopped watcher's run, how long it may take to go
	tailLines  = 40              // of run.log, in final.md
	maxRunHalf = 512 << 10       // run.log keeps the first and the last this many bytes
)

// job is work this watcher started in a goroutine: a run step's command
// (proc), a judge's Jev call (ask), or Jev deciding a worker's question
// (decision). Its goroutine touches neither the registry nor the clock;
// tendRuns records how it ended.
type job interface {
	ended() <-chan struct{}                                     // closed once it has ended
	stop()                                                      // ends it now, if it still goes
	owns(wk registry.Worker) bool                               // wk is still the run it was started for
	record(ctx context.Context, w *Watcher, wk registry.Worker) // records how it ended
}

// proc is a command this watcher started.
type proc struct {
	pid     int
	at      uint64 // when pid started: procStat's start
	timeout time.Duration
	done    chan struct{} // closed when res is set

	mu       sync.Mutex
	timedOut bool
	res      runResult
}

// runResult is how a command ended.
type runResult struct {
	exit   int    // its exit status; -1 when a signal ended it
	signal string // the signal that ended it, if one did
	took   time.Duration
	err    error // waiting for it failed
}

// launch starts a run step admitted to starting: renders its command,
// writes it as brief.md and runs it.
func (w *Watcher) launch(ctx context.Context, wk registry.Worker, all []registry.Worker) {
	w.makeWork(wk, wk.Run)
	cmdline, err := renderRun(wk, all, w.Paths)
	if err != nil {
		w.fail(ctx, wk, fmt.Sprintf("its command has bad refs: %v. routr cancel %d, then start it again with a command that fits", err, wk.ID))
		return
	}
	dir := w.runDir(wk)
	w.writeRun(wk, filepath.Join(dir, registry.BriefFile), cmdline+"\n")
	p, err := w.exec(wk, cmdline)
	if err != nil {
		w.fail(ctx, wk, fmt.Sprintf("could not run its command: %v", err))
		return
	}
	w.mu.Lock()
	w.jobs[wk.ID] = p
	w.mu.Unlock()
	now := w.now()
	if _, ok := w.starting(wk, func(x *registry.Worker) {
		x.State, x.Pid, x.PidAt, x.Prompted = registry.Running, p.pid, p.at, now
		x.Output = filepath.Join(dir, registry.RunLog)
		x.Note = fmt.Sprintf("running its command (pid %d), for at most %s", p.pid, p.timeout)
		if x.Started.IsZero() {
			x.Started = now
		}
	}); !ok { // cancelled while it started
		w.Log.Printf("%s: changed while starting; stopping its command", wk.Label())
		p.kill()
		return
	}
	w.Log.Printf("%s: running %s (pid %d)", wk.Label(), firstLine(cmdline), p.pid)
}

// renderRun fills wk's command from the registry: params as they are,
// every other ref quoted for sh (flow.RenderShell).
func renderRun(wk registry.Worker, all []registry.Worker, p registry.Paths) (string, error) {
	return flow.RenderShell(wk.Run, Refs(wk, all, p, wk.Cwd))
}

// exec starts cmdline for wk with sh -c, in wk's cwd and its own process
// group, with the environment a worker's tab gets, stdout and stderr into
// run.log.
func (w *Watcher) exec(wk registry.Worker, cmdline string) (*proc, error) {
	dir := w.runDir(wk)
	f, err := os.Create(filepath.Join(dir, registry.RunLog))
	if err != nil {
		return nil, err
	}
	out, in, err := os.Pipe()
	if err != nil {
		f.Close()
		return nil, err
	}
	cmd := exec.Command("sh", "-c", cmdline)
	cmd.Dir = wk.Cwd
	cmd.Env = w.runEnv(wk)
	cmd.Stdout, cmd.Stderr = in, in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		out.Close()
		in.Close()
		return nil, err
	}
	in.Close()
	timeout := wk.Timeout
	if timeout <= 0 {
		timeout = registry.DefaultRunTimeout
	}
	p := &proc{pid: cmd.Process.Pid, timeout: timeout, done: make(chan struct{})}
	if st, err := procStat(p.pid); err == nil { // not yet waited for, so there even if it has exited
		p.at = st.start
	}
	go p.wait(cmd, out, &runLog{f: f})
	return p, nil
}

// runEnv is the watcher's environment, the worker's env (envOf) and the
// ROUTR_* a worker's tab gets.
func (w *Watcher) runEnv(wk registry.Worker) []string {
	env := os.Environ()
	for k, v := range w.envOf(wk) {
		env = append(env, k+"="+v)
	}
	return append(env, "ROUTR_ID="+strconv.Itoa(wk.ID), registry.StateDirEnv+"="+w.Paths.Dir,
		"ROUTR_RUN_DIR="+w.runDir(wk), "ROUTR_BIN="+w.Bin, "ROUTR_CWD="+wk.Cwd)
}

// wait waits for the command, stopping it at its timeout; then kills what
// it left running in its group, and closes run.log.
func (p *proc) wait(cmd *exec.Cmd, out *os.File, log *runLog) {
	begin := time.Now()
	copied := make(chan struct{})
	go func() { io.Copy(log, out); close(copied) }()
	var kill *time.Timer
	timer := time.AfterFunc(p.timeout, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.timedOut = true
		killGroup(p.pid, syscall.SIGTERM)
		kill = time.AfterFunc(runGrace, func() { killGroup(p.pid, syscall.SIGKILL) })
	})
	err := cmd.Wait()
	took := time.Since(begin)
	timer.Stop()
	killGroup(p.pid, syscall.SIGKILL)
	select {
	case <-copied:
	case <-time.After(runDrain): // something outside its group holds the pipe
		out.Close()
		<-copied
	}
	out.Close()
	log.Close()

	res := runResult{exit: -1, took: took}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		res.err = err
	}
	if st := cmd.ProcessState; st != nil {
		res.exit = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.signal = ws.Signal().String()
		}
	}
	p.mu.Lock()
	if kill != nil {
		kill.Stop()
	}
	p.res = res
	p.mu.Unlock()
	close(p.done)
}

// kill stops the command now, if it still runs.
func (p *proc) kill() {
	select {
	case <-p.done:
	default:
		killGroup(p.pid, syscall.SIGKILL)
	}
}

func (p *proc) ended() <-chan struct{} { return p.done }
func (p *proc) stop()                  { p.kill() }
func (p *proc) owns(wk registry.Worker) bool {
	return wk.State == registry.Running && wk.Pid == p.pid
}
func (p *proc) record(_ context.Context, w *Watcher, wk registry.Worker) { w.ran(wk, p) }

func (p *proc) result() (runResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.res, p.timedOut
}

func killGroup(pid int, sig syscall.Signal) {
	if pid > 0 {
		syscall.Kill(-pid, sig)
	}
}

// groupAlive reports whether process group pid has a process left.
func groupAlive(pid int) bool {
	return pid > 0 && !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

// stat is what /proc/<pid>/stat says of a process.
type stat struct {
	pgrp  int    // its process group
	start uint64 // when it started, in clock ticks after boot: no later process with its pid has the same
}

// procStat reads /proc/<pid>/stat (Linux; elsewhere it fails). A process
// that has exited but is not yet waited for is still there.
func procStat(pid int) (stat, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return stat{}, err
	}
	// pid (comm) state ppid pgrp …, the start the 22nd field; comm may
	// hold anything, ')' too.
	i := bytes.LastIndexByte(b, ')')
	f := strings.Fields(string(b[i+1:]))
	if i < 0 || len(f) < 20 {
		return stat{}, fmt.Errorf("/proc/%d/stat: %q", pid, b)
	}
	pgrp, err := strconv.Atoi(f[2])
	if err != nil {
		return stat{}, err
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return stat{}, err
	}
	return stat{pgrp: pgrp, start: start}, nil
}

// remnant is what is left of a run a watcher no longer waits for.
type remnant int

const (
	runGone   remnant = iota // nothing of it, or its pid is another process's now (so its group went first)
	runLives                 // its leader is the recorded process: its group is the run's to kill
	runUnsure                // its leader is gone (or unknown) and its group has processes: the run's leftovers, or a later process's that took its pid
)

// oldRun says what is left of the run whose leader was pid, started at at
// (0: not known). Only its leader decides: a process the command detached
// from its group may live on, and is not the run.
func oldRun(pid int, at uint64) remnant {
	if pid <= 0 {
		return runGone
	}
	if st, err := procStat(pid); err == nil && at != 0 {
		if st.start != at {
			return runGone
		}
		if st.pgrp == pid {
			return runLives
		}
	}
	if groupAlive(pid) {
		return runUnsure
	}
	return runGone
}

// unsure says that processes are left in process group pid, which routr
// cannot tell for a run's, and how to look.
func unsure(pid int) string {
	return fmt.Sprintf("process group %d still has processes, and routr cannot tell whether they are its command's or a later process's that took its pid. Look (pgrep -ag %[1]d), and stop them if they are its (kill -- -%[1]d)", pid)
}

// StopRun kills the process group of run step wk if its command still
// runs: `routr cancel` uses it. The watcher does too, so a cancel that
// could not reach the process still stops it. A group whose leader is gone
// is not killed: StopRun says how to look.
func StopRun(_ registry.Paths, wk registry.Worker) error {
	if wk.Run == "" {
		return nil
	}
	switch oldRun(wk.Pid, wk.PidAt) {
	case runLives:
		killGroup(wk.Pid, syscall.SIGKILL)
	case runUnsure:
		return fmt.Errorf("%s: %s", wk.Label(), unsure(wk.Pid))
	}
	return nil
}

// tendRuns records the commands and Jev calls that ended since the last
// tick, stops the ones whose worker was cancelled meanwhile, and starts
// again a run step (rerun) or judge a stopped watcher left running: no job
// of this watcher is behind it.
func (w *Watcher) tendRuns(ctx context.Context) error {
	all, err := w.store.List()
	if err != nil {
		return err
	}
	m := byID(all)
	w.mu.Lock()
	jobs := make(map[int]job, len(w.jobs))
	for id, j := range w.jobs {
		jobs[id] = j
	}
	w.mu.Unlock()
	for id, j := range jobs {
		wk, ok := m[id]
		select {
		case <-j.ended():
			if ok {
				j.record(ctx, w, wk)
			}
			w.mu.Lock()
			delete(w.jobs, id)
			w.mu.Unlock()
		default:
			if !ok || !j.owns(wk) {
				j.stop() // cancelled (routr cancel kills a command too, if it can)
			}
		}
	}
	for _, wk := range all {
		if !wk.Tabless() || wk.State != registry.Running || jobs[wk.ID] != nil {
			continue
		}
		if wk.Run != "" {
			w.rerun(wk)
			continue
		}
		const again = "the watcher stopped while it asked Jev; it asks again"
		if _, ok := w.left(wk, func(x *registry.Worker) { x.State, x.Note = registry.Pending, again }); ok {
			w.Log.Printf("%s: %s", wk.Label(), again)
		}
	}
	return nil
}

// rerun queues again run step wk, which a stopped watcher left running. It
// stays running, so keeps the directory lock, until its old command is
// gone: if its leader is still the recorded process, its group is killed
// and rerun waits for it to go. A group left without its leader is not
// provably the run's, so the step fails saying how to look.
func (w *Watcher) rerun(wk registry.Worker) {
	if oldRun(wk.Pid, wk.PidAt) == runLives {
		got, ok := w.left(wk, func(x *registry.Worker) {
			x.Note = "the watcher stopped while its command ran; stopping what is left of it"
		})
		if !ok {
			return
		}
		w.Log.Printf("%s: an earlier run of its command still goes (pid %d); stopping it", wk.Label(), wk.Pid)
		killGroup(wk.Pid, syscall.SIGKILL)
		wk = got
		for deadline := time.Now().Add(runFree); oldRun(wk.Pid, wk.PidAt) != runGone && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if left := oldRun(wk.Pid, wk.PidAt); left != runGone {
		why := fmt.Sprintf("the watcher stopped while its command ran, and %s, then routr cancel %d and start it again", unsure(wk.Pid), wk.ID)
		if left == runLives {
			why = fmt.Sprintf("the watcher stopped while its command ran, and its old command (process group %d) outlived SIGKILL. Once it is gone (pgrep -ag %[1]d), routr cancel %d and start it again", wk.Pid, wk.ID)
		}
		if _, ok := w.left(wk, func(x *registry.Worker) { x.State, x.Error, x.Pid, x.PidAt = registry.Failed, why, 0, 0 }); ok {
			w.Log.Printf("%s: failed: %s", wk.Label(), why)
		}
		return
	}
	const again = "the watcher stopped while its command ran; it runs again"
	if _, ok := w.left(wk, func(x *registry.Worker) { x.State, x.Note, x.Pid, x.PidAt = registry.Pending, again, 0, 0 }); ok {
		w.Log.Printf("%s: %s", wk.Label(), again)
	}
}

// left applies fn to wk if it is still the run a stopped watcher left
// running.
func (w *Watcher) left(wk registry.Worker, fn func(*registry.Worker)) (registry.Worker, bool) {
	got, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
		if x.State != registry.Running || x.Pid != wk.Pid || !x.Prompted.Equal(wk.Prompted) {
			return errStale
		}
		fn(x)
		return nil
	})
	if err != nil && !errors.Is(err, errStale) {
		w.Log.Printf("%s: %v", wk.Label(), err)
	}
	return got, err == nil
}

// ran records how wk's command ended: final.md, the verdict and Done, then
// on_fail. A fail without on_fail is done too, but halts the steps after
// it (registry Halts, admit): a program said no, and they should not go on
// regardless.
func (w *Watcher) ran(wk registry.Worker, p *proc) {
	res, timedOut := p.result()
	verdict, summary := judgeRun(wk, res, timedOut, p.timeout)
	log := filepath.Join(w.runDir(wk), registry.RunLog)
	w.writeRun(wk, w.Paths.FinalPath(wk), runFinal(wk, w.runDir(wk), res, summary, verdict))
	now := w.now()
	var me registry.Worker
	err := w.store.Tx(func(m map[int]*registry.Worker) error {
		x := m[wk.ID]
		if x == nil || x.State != registry.Running || x.Pid != p.pid {
			return errStale
		}
		x.State, x.Finished, x.Note, x.Error, x.Pid, x.PidAt = registry.Done, now, "", "", 0, 0
		x.Verdict, x.Summary, x.Output = verdict, summary, log
		w.loop(x, m)
		me = *x
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		return
	}
	w.Log.Printf("%s: %s: %s", wk.Label(), me.State, summary)
}

// judgeRun is the verdict of a command and a line saying why. A timeout or
// a signal is a fail whatever it expects: the command did not finish.
func judgeRun(wk registry.Worker, res runResult, timedOut bool, timeout time.Duration) (registry.Verdict, string) {
	took := res.took.Round(100 * time.Millisecond)
	switch {
	case timedOut:
		return registry.Fail, fmt.Sprintf("timed out after %s", timeout)
	case res.err != nil:
		return registry.Fail, fmt.Sprintf("could not wait for it: %v", res.err)
	case res.signal != "":
		return registry.Fail, fmt.Sprintf("killed by %s after %s", res.signal, took)
	}
	status := fmt.Sprintf("exit %d after %s", res.exit, took)
	switch failed := res.exit != 0; {
	case wk.Expect == registry.Fail && failed:
		return registry.Pass, status + ", failing as expected"
	case wk.Expect == registry.Fail:
		return registry.Fail, status + ", expected to fail"
	case failed:
		return registry.Fail, status
	}
	return registry.Pass, status
}

// runFinal is a run step's final.md: the command, how it ended and the end
// of its log.
func runFinal(wk registry.Worker, runDir string, res runResult, summary string, v registry.Verdict) string {
	var b strings.Builder
	cmd, _ := os.ReadFile(filepath.Join(runDir, registry.BriefFile))
	fmt.Fprintf(&b, "$ %s\n%s: %s\n", strings.TrimSpace(string(cmd)), v, summary)
	log, _ := os.ReadFile(filepath.Join(runDir, registry.RunLog))
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	if len(log) == 0 {
		b.WriteString("\nit printed nothing\n")
		return b.String()
	}
	if len(lines) > tailLines {
		fmt.Fprintf(&b, "\nthe last %d of its %d lines of output (all of it in %s):\n", tailLines, len(lines), registry.RunLog)
		lines = lines[len(lines)-tailLines:]
	} else {
		b.WriteString("\nits output:\n")
	}
	b.WriteString(strings.Join(lines, "\n") + "\n")
	return b.String()
}

// runLog is run.log: the first maxRunHalf bytes of the output, then, if
// there was more, a line saying how much was cut and the last maxRunHalf.
type runLog struct {
	f    *os.File
	head int
	tail []byte
	cut  int64
}

func (l *runLog) Write(b []byte) (int, error) {
	n := len(b)
	if room := maxRunHalf - l.head; room > 0 {
		k := min(room, len(b))
		if _, err := l.f.Write(b[:k]); err != nil {
			return 0, err
		}
		l.head += k
		b = b[k:]
	}
	l.tail = append(l.tail, b...)
	if over := len(l.tail) - maxRunHalf; over > maxRunHalf { // trim now and then, not on every write
		l.cut += int64(over)
		l.tail = bytes.Clone(l.tail[over:])
	}
	return n, nil
}

func (l *runLog) Close() error {
	if over := len(l.tail) - maxRunHalf; over > 0 {
		l.cut += int64(over)
		l.tail = l.tail[over:]
	}
	if l.cut > 0 {
		fmt.Fprintf(l.f, "\n[routr: %d bytes of output cut here]\n", l.cut)
	}
	_, err := l.f.Write(l.tail)
	return errors.Join(err, l.f.Close())
}
