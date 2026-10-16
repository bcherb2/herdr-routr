package watch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// startQueued admits queued workers one by one (the lock is taken in id
// order) and starts the admitted ones concurrently: a harness takes seconds
// to come up.
func (w *Watcher) startQueued(ctx context.Context) error {
	all, err := w.store.List()
	if err != nil {
		return err
	}
	m := byID(all)
	var admitted []registry.Worker
	for _, wk := range all {
		if wk.Queued() {
			if got, ok := w.admit(wk, m); ok {
				admitted = append(admitted, got)
				m[got.ID] = got // it takes a parallel slot from here on
			}
		}
	}
	var wg sync.WaitGroup
	for _, wk := range admitted {
		wg.Go(func() { w.start(ctx, wk, all) })
	}
	wg.Wait()
	return nil
}

// admit moves wk to starting if every worker it comes after, directly or
// through others, is done (and none of them halts it: a fail without
// on_fail, until you approve past it), its gate
// is open, its step has a parallel slot free and (for a
// write role) it holds the directory lock; else to waiting or gated with a
// note that says what it waits for. A worker with a Checkout gets its worktree named first
// (Paths.Place), so that it locks its own worktree, not the main one; a
// run step with TreeOf moves into that worker's worktree (a pick judge's:
// its winner's) the same way. A
// runtime foreach worker is never admitted: fanout expands it, or ends it
// once its copies are done. Since it waits on every worker upstream, not
// only its After, a review loop's revision holds back every step after it
// that has not started: the reviewer and the checks it queued again too.
func (w *Watcher) admit(wk registry.Worker, m map[int]registry.Worker) (registry.Worker, bool) {
	for _, id := range wk.After {
		if _, ok := m[id]; !ok {
			return wk, w.park(wk, registry.NeedsYou, fmt.Sprintf("it comes after worker %d, which does not exist; routr cancel %d and start it again", id, wk.ID))
		}
	}
	for _, d := range registry.Ancestors(wk, m) { // nearest first
		switch {
		case d.Halts():
			return wk, w.park(wk, registry.Waiting, fanNote(wk, haltNote(wk, d)))
		case d.Succeeded():
			continue
		case d.Stuck():
			return wk, w.park(wk, registry.Waiting, fanNote(wk, fmt.Sprintf("waiting on %s, which is %s", d.Label(), d.State)))
		default:
			return wk, w.park(wk, registry.Waiting, fanNote(wk, fmt.Sprintf("waiting on %s (%s)", d.Label(), d.State)))
		}
	}
	if wk.Fanout != nil {
		return wk, w.fanout(wk)
	}
	if wk.Gate == "you" && !wk.GateOpen {
		return wk, w.park(wk, registry.Gated, fmt.Sprintf("waiting at the gate: routr approve %d", wk.ID))
	}
	if busy := running(wk, m); wk.Parallel > 0 && len(busy) >= wk.Parallel {
		return wk, w.park(wk, registry.Waiting, fmt.Sprintf("waiting for a slot: step %s runs %d at a time (running: %s)", wk.Step, wk.Parallel, idList(busy)))
	}
	if wk.TreeOf != 0 {
		// A run step after a worktree step runs in that worktree, and
		// takes its lock, not the main tree's.
		d, ok := m[wk.TreeOf]
		if ok && d.Judge != nil && d.Judge.Winner != 0 {
			d, ok = m[d.Judge.Winner]
		}
		if !ok || d.Checkout == nil || d.Checkout.Path == "" {
			return wk, w.park(wk, registry.NeedsYou, fmt.Sprintf("it runs in the worktree of worker %d, which has none; routr cancel %d", wk.TreeOf, wk.ID))
		}
		if wk.Cwd != d.Cwd || wk.Tree != d.Tree {
			got, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
				if !x.Queued() {
					return errStale
				}
				x.Cwd, x.Tree = d.Cwd, d.Tree
				return nil
			})
			if err != nil {
				if !errors.Is(err, errStale) {
					w.Log.Printf("%s: %v", wk.Label(), err)
				}
				return wk, false
			}
			wk = got
		}
	}
	if c := wk.Checkout; c != nil && c.Path == "" {
		// Name its worktree first: the directory lock is that worktree's.
		got, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
			if !x.Queued() || x.Checkout == nil || x.Checkout.Path != "" {
				return errStale
			}
			w.Paths.Place(x)
			return nil
		})
		if err != nil {
			if !errors.Is(err, errStale) {
				w.Log.Printf("%s: %v", wk.Label(), err)
			}
			return wk, false
		}
		wk = got
	}
	got, holder, err := w.store.Claim(wk.ID, func(x *registry.Worker) error {
		if !x.Queued() {
			return errStale
		}
		x.State, x.Note, x.Error = registry.Starting, "", ""
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		return wk, false
	}
	if holder != wk.ID {
		return wk, w.park(wk, registry.Waiting, fmt.Sprintf("waiting for the directory lock on %s (held by worker %d)", wk.Worktree(), holder))
	}
	return got, true
}

// haltNote says that wk waits on d, a step that failed without on_fail
// and halts (registry Halts), and how to go on past it.
func haltNote(wk, d registry.Worker) string {
	why, more := fmt.Sprintf("failed (%s)", d.Summary), "Its log is "+relTo(wk.Cwd, d.Output)
	switch {
	case d.Judge != nil:
		why, more = "judged "+strings.TrimPrefix(d.Summary, "Jev: "), fmt.Sprintf("routr result %d says why", d.ID)
	case d.Run == "":
		why, more = fmt.Sprintf("gave a fail verdict (%s)", d.Summary), "Its output says why"
		if d.Output != "" {
			more += ": " + relTo(wk.Cwd, absIn(d.Cwd, d.Output))
		}
	}
	return fmt.Sprintf("waits: %s %s; routr approve %d lets the chain go on past it. %s", d.Label(), why, d.ID, more)
}

// running lists the other copies of wk's foreach step that hold a parallel
// slot: starting, running or blocked on a question.
func running(wk registry.Worker, m map[int]registry.Worker) []int {
	if wk.Parallel <= 0 || wk.Chain == "" {
		return nil
	}
	var ids []int
	for id, o := range m {
		if id == wk.ID || o.Chain != wk.Chain || o.Step != wk.Step || o.Fanout != nil {
			continue
		}
		switch o.State {
		case registry.Starting, registry.Running, registry.Blocked:
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// fanout handles a runtime foreach worker whose After are done. The first
// time, it expands into one copy per matched file (flow.Expand), adds them
// to its After and waits on them. Once they are done too, it is done, as a
// join; it never opens a tab. No match or too many need you: a planner that
// wrote nothing is usually a bug, and nothing is started. With allow_empty,
// no match is done at once, with no copies.
func (w *Watcher) fanout(wk registry.Worker) bool {
	if wk.Fanout.Items != nil {
		now := w.now()
		_, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
			if !x.Queued() {
				return errStale
			}
			x.State, x.Finished, x.Note = registry.Done, now, ""
			x.Summary = fmt.Sprintf("expanded into %s (%s), all done", count(len(x.Fanout.Items), "worker"), idList(x.Fanout.Items))
			return nil
		})
		if err != nil && !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		if err == nil {
			w.Log.Printf("%s: done: every copy is done", wk.Label())
		}
		return false
	}
	copies, err := flow.Expand(wk, w.Paths.WorkDir(wk.ChainRef()))
	if err != nil {
		w.Log.Printf("%s: foreach: %v", wk.Label(), err)
		return w.park(wk, registry.NeedsYou, fmt.Sprintf("%v; nothing was started. Have the right files written, then routr approve %d to look again, or routr cancel %s", err, wk.ID, wk.Chain))
	}
	if len(copies) == 0 { // allow_empty
		now, note := w.now(), flow.EmptyNote(wk.Fanout.Glob)
		_, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
			if !x.Queued() || x.Fanout == nil || x.Fanout.Items != nil {
				return errStale
			}
			x.State, x.Finished, x.Output, x.Note, x.Summary = registry.Done, now, "", note, note
			return nil
		})
		if err != nil && !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		if err == nil {
			w.Log.Printf("%s: done: %s", wk.Label(), note)
		}
		return false
	}
	created, err := w.store.Spawn(wk.ID, copies, func(x *registry.Worker, created []registry.Worker) error {
		if !x.Queued() || x.Fanout == nil || x.Fanout.Items != nil {
			return errStale
		}
		for _, c := range created {
			x.Fanout.Items = append(x.Fanout.Items, c.ID)
		}
		x.After = append(x.After, x.Fanout.Items...)
		x.State, x.Output = registry.Waiting, "" // each copy has its own output
		x.Note = fanNote(*x, "its copies are queued")
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		return false
	}
	ids := make([]int, len(created))
	for i, c := range created {
		ids[i] = c.ID
	}
	w.Log.Printf("%s: expanded into %s (%s)", wk.Label(), count(len(ids), "worker"), idList(ids))
	return false
}

// fanNote prefixes a waiting note of a runtime foreach worker with where it
// stands.
func fanNote(wk registry.Worker, note string) string {
	f := wk.Fanout
	switch {
	case f == nil:
		return note
	case f.Items == nil:
		return fmt.Sprintf("foreach %s, once the steps before it are done: %s", f.Glob, note)
	}
	return fmt.Sprintf("expanded into %s (%s); %s", count(len(f.Items), "worker"), idList(f.Items), note)
}

// idList is "5, 6, 7".
func idList(ids []int) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.Itoa(id)
	}
	return strings.Join(s, ", ")
}

func count(n int, noun string) string {
	return fmt.Sprintf("%d %s%s", n, noun, plural(n))
}

// park sets a queued worker's state and note if they changed. It returns
// false, for admit's convenience.
func (w *Watcher) park(wk registry.Worker, st registry.State, note string) bool {
	if wk.State == st && wk.Note == note {
		return false
	}
	_, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
		if !x.Queued() {
			return errStale
		}
		x.State, x.Note = st, note
		return nil
	})
	if err != nil && !errors.Is(err, errStale) {
		w.Log.Printf("%s: %v", wk.Label(), err)
	}
	return false
}

// start renders the brief, opens the tab (or resumes the session of a
// worker that ran before) and sends the prompt. A run step runs its command
// instead (launch), a judge asks Jev (ask).
func (w *Watcher) start(ctx context.Context, wk registry.Worker, all []registry.Worker) {
	switch {
	case wk.Run != "":
		w.launch(ctx, wk, all)
		return
	case wk.Judge != nil:
		w.ask(ctx, wk, all)
		return
	}
	w.makeWork(wk, wk.Brief)
	if c := wk.Checkout; c != nil && !c.Made {
		var ok bool
		if wk, ok = w.makeTree(ctx, wk); !ok {
			return
		}
	}
	brief, err := Render(wk, all, w.Paths)
	if err != nil {
		w.fail(ctx, wk, fmt.Sprintf("its brief has bad refs: %v. routr cancel %d, then start it again with a brief that fits", err, wk.ID))
		return
	}
	// A worker that ran before resumes its session: a review loop's next
	// round, or routr retry after it failed. Only the loop's is a revision.
	rerun := !wk.Started.IsZero() && wk.Session != ""
	text := Compose(wk, w.Paths, brief, w.Bin)
	if rerun && wk.Round > 1 {
		text = Revised(wk, w.Paths, brief, w.Bin)
	}
	w.writeRun(wk, filepath.Join(w.runDir(wk), registry.BriefFile), text)

	d := w.driver(wk)
	h := w.handle(wk)
	if wk.Live == nil {
		spec, err := w.spec(wk)
		if err != nil {
			w.fail(ctx, wk, err.Error())
			return
		}
		if rerun {
			spec.Session = wk.Session
		}
		if h, err = d.Start(ctx, spec); err != nil {
			switch {
			case herdr.Unreachable(err):
				w.unreachable(wk.Socket, err)
				w.discard(ctx, wk, h)
				w.requeue(wk, fmt.Sprintf("waiting for herdr: %v", err))
			default:
				w.fail(ctx, wk, fmt.Sprintf("could not start %s: %v", wk.Harness, err), h)
			}
			return
		}
		w.reachable(wk.Socket)
		// Record the tab before the prompt: a watcher that dies from here on
		// leaves a worker the next one can pick up (recoverStarting), and a
		// tab that routr cancel can close.
		if _, ok := w.starting(wk, func(x *registry.Worker) { record(x, h) }); !ok {
			w.Log.Printf("%s: changed while starting; closing its tab", wk.Label())
			w.discard(ctx, wk, h)
			return
		}
	}
	w.prompt(ctx, wk, d, h, text)
}

// prompt sends a starting worker its brief and marks it running.
func (w *Watcher) prompt(ctx context.Context, wk registry.Worker, d worker.Driver, h worker.Handle, text string) {
	w.beforePrompt(ctx, d, h, wk.ID)
	if err := d.Prompt(ctx, h, text); err != nil {
		w.fail(ctx, wk, fmt.Sprintf("%s started but did not take the brief: %v", wk.Harness, err), h)
		return
	}
	w.running(ctx, wk, d, h)
}

// running marks a starting worker that has its brief as running.
func (w *Watcher) running(ctx context.Context, wk registry.Worker, d worker.Driver, h worker.Handle) {
	now := w.now()
	if _, ok := w.starting(wk, func(x *registry.Worker) {
		record(x, h)
		x.State, x.Prompted = registry.Running, now
		if x.Started.IsZero() {
			x.Started = now
		}
	}); !ok { // cancelled while it started
		w.Log.Printf("%s: changed while starting; closing its tab", wk.Label())
		w.discard(ctx, wk, h)
		return
	}
	w.Log.Printf("%s: started (%s)", wk.Label(), wk.Tag())
}

// starting applies fn to wk if it is still starting: a CLI command may have
// cancelled it meanwhile.
func (w *Watcher) starting(wk registry.Worker, fn func(*registry.Worker)) (registry.Worker, bool) {
	got, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
		if x.State != registry.Starting {
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

// discard closes a tab that was opened for wk but will not be used.
func (w *Watcher) discard(ctx context.Context, wk registry.Worker, h worker.Handle) {
	if h.Pane == "" {
		return
	}
	if err := w.driver(wk).Close(ctx, h); err != nil {
		w.Log.Printf("%s: close the unused tab %s: %v", wk.Label(), h.Pane, err)
	}
}

// recoverStarting picks up the workers a watcher left starting when it
// stopped. startQueued waits for the starts it began, so a worker that is
// starting when a tick begins was left by a watcher that is gone. One
// without a tab goes back to the queue. One with a tab runs if it has
// visibly taken its brief; else it gets the brief (again: a watcher that died
// right after sending it cannot tell).
func (w *Watcher) recoverStarting(ctx context.Context) error {
	all, err := w.store.List()
	if err != nil {
		return err
	}
	for _, wk := range all {
		if wk.State == registry.Starting {
			w.Log.Printf("%s: left starting by a watcher that stopped; picking it up", wk.Label())
			w.recover(ctx, wk)
		}
	}
	return nil
}

func (w *Watcher) recover(ctx context.Context, wk registry.Worker) {
	const again = "the watcher stopped while starting it; starting it again"
	if wk.Live == nil {
		w.requeue(wk, again)
		return
	}
	d, h := w.driver(wk), w.handle(wk)
	st, err := d.Poll(ctx, h)
	switch {
	case outOfReach(err):
		w.unreachable(wk.Socket, err) // the next tick tries again
	case err != nil:
		w.fail(ctx, wk, fmt.Sprintf("could not read its tab after the watcher stopped while starting it: %v", err))
	case st.Phase == worker.Gone:
		w.requeue(wk, again)
	case st.Phase == worker.Exited:
		w.fail(ctx, wk, fmt.Sprintf("%s exited while it started. routr cancel %d, then start it again", wk.Harness, wk.ID))
	case st.Phase == worker.Working || st.Phase == worker.Asking || registry.HasDone(w.runDir(wk)):
		w.running(ctx, wk, d, h)
	default:
		text, err := os.ReadFile(filepath.Join(w.runDir(wk), registry.BriefFile))
		if err != nil || len(text) == 0 {
			w.discard(ctx, wk, h)
			w.requeue(wk, again)
			return
		}
		w.prompt(ctx, wk, d, h, string(text))
	}
}

// makeTree makes wk's git worktree (registry.MakeWorktree) and records it.
// A worktree routr cannot make fails the worker: it never falls back to
// the main tree.
func (w *Watcher) makeTree(ctx context.Context, wk registry.Worker) (registry.Worker, bool) {
	c := wk.Checkout
	if err := registry.MakeWorktree(*c); err != nil {
		w.fail(ctx, wk, fmt.Sprintf("could not make its git worktree, so it did not start (it never runs in the main tree instead): %v", err))
		return wk, false
	}
	got, ok := w.starting(wk, func(x *registry.Worker) { x.Checkout.Made = true })
	if ok {
		w.Log.Printf("%s: made its worktree %s (branch %s, from %.12s)", wk.Label(), c.Path, c.Branch, c.Base)
	}
	return got, ok
}

// makeWork creates wk's work directory if tmpl refers to it ({{work}}): a
// flow's is made when it starts, a pipe's when a step first needs it.
func (w *Watcher) makeWork(wk registry.Worker, tmpl string) {
	if !slices.Contains(flow.Placeholders(tmpl), "work") {
		return
	}
	if err := os.MkdirAll(w.Paths.WorkDir(wk.ChainRef()), 0o755); err != nil {
		w.Log.Printf("%s: %v", wk.Label(), err)
	}
}

// spec is what the driver needs to open wk's tab: its env, frozen when it
// was queued, or for a record from before that, config [env] as it is now.
func (w *Watcher) spec(wk registry.Worker) (worker.Spec, error) {
	env := maps.Clone(wk.Env)
	if wk.Env == nil {
		env = map[string]string{}
	}
	if wk.Env == nil && w.Env != nil {
		e, err := w.Env()
		if err != nil {
			return worker.Spec{}, fmt.Errorf("config: %v", err)
		}
		maps.Copy(env, e)
	}
	env["ROUTR_ID"] = strconv.Itoa(wk.ID)
	env[registry.StateDirEnv] = w.Paths.Dir
	env["ROUTR_RUN_DIR"] = w.runDir(wk)
	env["ROUTR_BIN"] = w.Bin
	env["ROUTR_CWD"] = wk.Cwd
	return worker.Spec{
		ID: wk.ID, Label: wk.Label(), Harness: wk.Harness, Model: wk.Model, Effort: wk.Effort,
		Permissions: wk.Permissions, Args: wk.Args, MCP: wk.MCP, Cwd: wk.Cwd, Env: env, Workspace: wk.Workspace,
		Dirs: w.dirs(wk), HookLog: w.Paths.HookLog(wk.ID),
	}, nil
}

// dirs are the directories outside wk's cwd that it writes: its chain's
// work directory, when that lies outside (a worktree worker's always does,
// and it is made for it, since its outputs go there).
func (w *Watcher) dirs(wk registry.Worker) []string {
	work := w.Paths.WorkDir(wk.ChainRef())
	if under(wk.Cwd, work) {
		return nil
	}
	if wk.Checkout != nil {
		if err := os.MkdirAll(work, 0o755); err != nil {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
	}
	if fi, err := os.Stat(work); err != nil || !fi.IsDir() {
		return nil
	}
	return []string{work}
}

// requeue puts a starting worker back in the queue, without a tab, when its
// start could not go on through no fault of its own (herdr out of reach, a
// watcher that stopped): the next tick, or a watcher that can reach herdr,
// starts it.
func (w *Watcher) requeue(wk registry.Worker, note string) {
	w.starting(wk, func(x *registry.Worker) { x.State, x.Note, x.Live = registry.Pending, note, nil })
}

// fail marks a starting worker failed with why, recording its pane if it has
// one, so that you can look at it and routr cancel can close it. The
// handle's session is not recorded: a failed start may carry one the
// harness never created, and a worker that ran keeps the one it had. A
// worker a CLI command moved meanwhile is left alone, and the pane closed.
func (w *Watcher) fail(ctx context.Context, wk registry.Worker, why string, live ...worker.Handle) {
	var h worker.Handle
	if len(live) > 0 {
		h = live[0]
	}
	if _, ok := w.starting(wk, func(x *registry.Worker) {
		if h.Pane != "" {
			recordPane(x, h)
		}
		x.State, x.Error = registry.Failed, why
	}); !ok {
		w.Log.Printf("%s: changed while starting; its start failed, ignored: %s", wk.Label(), why)
		w.discard(ctx, wk, h)
		return
	}
	w.Log.Printf("%s: failed: %s", wk.Label(), why)
}
