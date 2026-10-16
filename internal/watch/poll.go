package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// pollLive reads every worker that has a pane and acts on settled phases.
func (w *Watcher) pollLive(ctx context.Context) error {
	all, err := w.store.List()
	if err != nil {
		return err
	}
	for _, wk := range all {
		if wk.Live != nil && wk.State != registry.Cancelled && wk.State != registry.Starting && !wk.Queued() {
			w.poll(ctx, wk)
		}
	}
	return nil
}

func (w *Watcher) poll(ctx context.Context, wk registry.Worker) {
	d, h := w.driver(wk), w.handle(wk)
	st, err := d.Poll(ctx, h)
	if outOfReach(err) {
		w.unreachable(wk.Socket, err)
		if w.gaveUp(wk.Socket) {
			w.lost(wk)
		}
		return
	}
	if err != nil {
		w.Log.Printf("%s: poll: %v", wk.Label(), err)
		return
	}
	w.reachable(wk.Socket)
	if (st.Session != "" && st.Session != wk.Session) || (st.Transcript != "" && st.Transcript != wk.Transcript) {
		got, err := w.store.Update(wk.ID, func(x *registry.Worker) {
			x.Session = first(st.Session, x.Session)
			x.Transcript = first(st.Transcript, x.Transcript)
		})
		if err == nil {
			wk = got
		}
	}
	if st.Phase == worker.Asking && wk.State != registry.Blocked {
		w.answered(wk.ID)
	}
	phase, settled := w.observe(wk.ID, st)
	if !settled {
		return
	}
	if phase != worker.Working && phase != worker.Asking && registry.HasDone(w.runDir(wk)) {
		w.finish(ctx, wk, d, h)
		return
	}
	switch phase {
	case worker.Asking:
		w.block(ctx, wk, st)
	case worker.Working:
		if wk.State == registry.Blocked || wk.State == registry.MaybeDone || (wk.State == registry.NeedsYou && wk.TurnError != "") {
			w.working(wk)
		}
	case worker.Idle:
		w.idle(ctx, wk, d, h)
	case worker.Exited:
		w.exited(ctx, wk, d, h)
	case worker.Gone:
		w.gone(wk)
	}
}

// set changes a worker the watcher is looking at, unless a CLI command
// cancelled it or took its pane away meanwhile.
func (w *Watcher) set(wk registry.Worker, fn func(*registry.Worker)) (registry.Worker, bool) {
	got, err := w.store.Modify(wk.ID, func(x *registry.Worker) error {
		if x.State == registry.Cancelled || !samePane(x.Live, wk.Live) {
			return errStale
		}
		fn(x)
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		return wk, false
	}
	return got, true
}

func samePane(a, b *registry.Live) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Pane == b.Pane && a.Socket == b.Socket
}

// working moves a blocked or maybe_done worker, or one an API error left
// needing you (and that you set going again in its tab), back to running. A write
// worker idle for longer than registry.MaybeDoneLock has let its directory
// lock go; it takes it back if it is free, and otherwise works on with a
// note, since routr can't stop a harness mid-turn.
func (w *Watcher) working(wk registry.Worker) {
	run := func(x *registry.Worker) {
		if x.State == registry.NeedsYou {
			x.Error, x.TurnError = "", ""
		}
		x.State, x.Note, x.Idle = registry.Running, "", time.Time{}
		x.ClearQuestion()
	}
	if !wk.Write {
		w.set(wk, run)
		return
	}
	_, holder, err := w.store.Claim(wk.ID, func(x *registry.Worker) error {
		if x.State == registry.Cancelled || !samePane(x.Live, wk.Live) {
			return errStale
		}
		run(x)
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		return
	}
	if holder != wk.ID {
		w.set(wk, func(x *registry.Worker) {
			run(x)
			x.Note = fmt.Sprintf("working again after going idle, while worker %d holds the directory lock", holder)
		})
		w.Log.Printf("%s: working again without the directory lock (worker %d holds it)", wk.Label(), holder)
	}
}

// block records a settled question, with its menu's options and
// fingerprint, herdr's sequence number and the tool call it is about, and
// hands it to Jev if Jev answers the worker's questions (jevAsk). A
// blocked worker whose screen settled on another menu is recorded afresh:
// herdr can report blocked, with one sequence number, across two prompts
// (a question answered in its tab and the next one right after), and an
// answer decided away from the screen must be about the question on it.
// A screen with no menu leaves the record as it is: one can scroll out of
// view while the same question waits.
func (w *Watcher) block(ctx context.Context, wk registry.Worker, st worker.Status) {
	switch {
	case wk.State == registry.Running, wk.State == registry.MaybeDone:
	case wk.State == registry.Blocked && st.Fingerprint == wk.Fingerprint:
		w.jevAsk(ctx, wk) // the question it recorded, still on screen
		return
	case wk.State == registry.Blocked && st.Fingerprint != "": // another menu
	default:
		return
	}
	q, call := question(st), w.pending(wk, st)
	got, ok := w.set(wk, func(x *registry.Worker) {
		x.State, x.Question, x.Note = registry.Blocked, q, ""
		x.Options, x.Fingerprint, x.BlockSeq, x.Pending, x.HandedTo = st.Options, st.Fingerprint, st.Seq, call, ""
	})
	if !ok {
		return
	}
	w.asked(wk.ID)
	about := ""
	if call != nil {
		about = " (" + call.Summary + ")"
	}
	w.Log.Printf("%s: blocked: %s%s", wk.Label(), firstLine(q), about)
	w.jevAsk(ctx, got)
}

// question is the screen's question with its numbered options; the herdr
// driver already includes them, so they are added only when missing.
func question(st worker.Status) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(st.Question))
	if len(st.Options) > 0 && strings.Contains(st.Question, "1. "+st.Options[0]) {
		return b.String()
	}
	for i, o := range st.Options {
		fmt.Fprintf(&b, "\n%d. %s", i+1, o)
	}
	return strings.TrimSpace(b.String())
}

// idle handles a settled idle phase without done.json: a turn an API error
// ended, the reply to a follow-up, an interactive worker waiting on you, or
// a worker that stopped without `routr done` (maybe_done, or done if its
// output file changed). A reply it can't read yet holds it (usable).
func (w *Watcher) idle(ctx context.Context, wk registry.Worker, d worker.Driver, h worker.Handle) {
	reply, err := d.LastReply(ctx, h)
	var failed *worker.TurnError
	if errors.As(err, &failed) {
		w.turnFailed(ctx, wk, d, h, failed.Message)
		return
	}
	if err == nil && (wk.TurnError != "" || wk.Retries > 0) {
		got, ok := w.set(wk, func(x *registry.Worker) { x.TurnError, x.Retries, x.Retry = "", 0, time0 })
		if !ok {
			return
		}
		wk = got
	}
	replied := false
	if wk.Turn > wk.Replies {
		text, ok := w.usable(wk, reply, err)
		if !ok {
			return
		}
		w.writeRun(wk, w.Paths.TurnFile(wk.ID, wk.Turn), text)
		got, ok := w.set(wk, func(x *registry.Worker) {
			x.Replied()
			x.TurnError, x.Retries, x.Retry = "", 0, time0 // also after an unreadable reply
		})
		if !ok {
			return
		}
		w.Log.Printf("%s: replied to turn %d", wk.Label(), wk.Turn)
		wk, replied = got, true
	}
	if len(wk.Inbox) > 0 {
		// A follow-up goes next (deliverInbox). Until it has been sent, this
		// idle is the end of an earlier turn: a review loop's revision must
		// not finish on the output of the round before.
		return
	}
	if replied && wk.State.Terminal() {
		// It answered a follow-up and is finished again. A tab is open only
		// while its worker has work; the next routr send resumes the session.
		w.closeTab(ctx, wk, d)
		return
	}
	switch wk.State {
	case registry.Running, registry.Blocked, registry.MaybeDone:
	default:
		return
	}
	if wk.Interactive && wk.State != registry.MaybeDone {
		if wk.State == registry.Blocked {
			w.set(wk, func(x *registry.Worker) { x.State = registry.Running; x.ClearQuestion() })
		}
		return // you're talking to it; it ends with `routr done`
	}
	if w.outputChanged(wk) {
		w.finish(ctx, wk, d, h)
		return
	}
	if wk.State == registry.MaybeDone {
		return
	}
	reply, ok := w.usable(wk, reply, err)
	if !ok {
		return
	}
	w.writeRun(wk, w.Paths.FinalPath(wk), reply)
	w.set(wk, func(x *registry.Worker) {
		x.State, x.Idle = registry.MaybeDone, w.now()
		x.ClearQuestion()
		x.TurnError, x.Retries, x.Retry = "", 0, time0
		x.Note = fmt.Sprintf("idle without running routr done; routr result %d has its last reply. routr send %d … to go on, or routr close %d", wk.ID, wk.ID, wk.ID)
	})
	w.Log.Printf("%s: maybe done", wk.Label())
}

// unreadWait is how long a reply that can't be read (a transcript not
// written or found yet) holds a worker before routr settles without it.
const unreadWait = time.Minute

// usable is the reply to settle wk's turn with, given LastReply's error. A
// reply that can't be read holds the worker (ok false) rather than settle
// the turn with an empty reply; after unreadWait it settles with a note
// saying so.
func (w *Watcher) usable(wk registry.Worker, reply string, err error) (string, bool) {
	if err == nil {
		return reply, true
	}
	since, first := w.unreadSince(wk.ID)
	if first {
		w.Log.Printf("%s: can't read its reply yet: %v", wk.Label(), err)
	}
	if w.now().Sub(since) < unreadWait {
		return "", false
	}
	w.Log.Printf("%s: gave up reading its reply: %v", wk.Label(), err)
	return fmt.Sprintf("routr could not read this reply: %v. routr show %d shows its tab.\n", err, wk.ID), true
}

// outputChanged reports whether wk's declared output was written since its
// last prompt.
func (w *Watcher) outputChanged(wk registry.Worker) bool {
	if wk.Output == "" {
		return false
	}
	fi, err := os.Stat(absIn(wk.Cwd, wk.Output))
	if err != nil {
		return false
	}
	since := wk.Prompted
	if since.IsZero() {
		since = wk.Started
	}
	return fi.ModTime().After(since)
}

// finish records a finished worker: done.json if there is one, final.md
// from its message and the harness's last reply, the review loop, and
// closes its tab unless a follow-up waits to go into it.
func (w *Watcher) finish(ctx context.Context, wk registry.Worker, d worker.Driver, h worker.Handle) {
	inFlight := wk.Turn > wk.Replies
	reply := ""
	if wk.Live != nil {
		var err error
		reply, err = d.LastReply(ctx, h)
		// A follow-up's reply is its turn file, so one that can't be read
		// holds it (before done.json is taken, so the next poll gets here
		// again); final.md has done.json's message to go on.
		var failed *worker.TurnError
		if err != nil && !errors.As(err, &failed) && inFlight {
			var ok bool
			if reply, ok = w.usable(wk, reply, err); !ok {
				return
			}
		}
	}
	rec, ok, takeErr := registry.TakeDone(w.runDir(wk))
	if takeErr != nil {
		w.Log.Printf("%s: %v", wk.Label(), takeErr)
	}
	w.writeRun(wk, w.Paths.FinalPath(wk), joinFinal(rec.Message, reply))
	if inFlight {
		w.writeRun(wk, w.Paths.TurnFile(wk.ID, wk.Turn), reply)
	}
	now := w.now()
	var me registry.Worker
	var verdict registry.Verdict // as it finished: a review loop starts it over
	err := w.store.Tx(func(m map[int]*registry.Worker) error {
		x := m[wk.ID]
		if x == nil || x.State == registry.Cancelled || x.Queued() || !samePane(x.Live, wk.Live) {
			return errStale
		}
		x.State, x.Finished, x.Note, x.Error, x.Rest = registry.Done, now, "", "", ""
		x.ClearQuestion()
		x.TurnError, x.Retries, x.Retry = "", 0, time0
		switch {
		case takeErr != nil:
			x.Note = fmt.Sprintf("its done.json was unreadable; routr result %d has its final message", wk.ID)
		case ok:
			x.Summary, x.Verdict = rec.Message, rec.Verdict
			if rec.Output != "" {
				x.Output = rec.Output
			}
		default:
			x.Note = "its output file changed but it did not run routr done"
		}
		if inFlight {
			x.Replied()
		}
		verdict = x.Verdict
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
	w.Log.Printf("%s: %s", wk.Label(), strings.TrimSpace(string(me.State)+" "+string(verdict)))
	// A reviewer the loop sent back to wait for the revision reruns in a new
	// tab (resuming its session); its old one closes, even with follow-ups
	// queued, so that nothing in it can move a queued worker.
	if me.Live != nil && (me.Queued() || len(me.Inbox) == 0) {
		w.closeTab(ctx, me, d)
	}
}

func joinFinal(message, reply string) string {
	message, reply = strings.TrimSpace(message), strings.TrimSpace(reply)
	switch {
	case message == "" || strings.Contains(reply, message):
		return reply + "\n"
	case reply == "":
		return message + "\n"
	}
	return message + "\n\n" + reply + "\n"
}

// loop follows x's on_fail after a failing verdict: the worker it names gets
// the rendered brief in its inbox and goes back to running, and x, one
// round up, waits for it as for every worker upstream of it (admit): the
// flow checks that on_fail resumes a step x comes after. The run and judge
// steps between the two (between) are queued again, so that x sees them
// check the revision: being after it, they wait for it and for the checks
// before them the same way. An agent step between them is not, and x's
// note names it. At the cap x needs you.
func (w *Watcher) loop(x *registry.Worker, m map[int]*registry.Worker) {
	of := x.OnFail
	if x.Verdict != registry.Fail || of == nil {
		return
	}
	round := max(x.Round, 1)
	if round >= of.Max {
		x.State = registry.NeedsYou
		x.Note = fmt.Sprintf("the review loop hit its cap of %d round%s with a fail verdict. routr result %d has it; routr approve %d accepts it and lets the chain go on, routr send %d … revises by hand", of.Max, plural(of.Max), x.ID, x.ID, of.Resume)
		return
	}
	target := m[of.Resume]
	if target == nil || target.Tabless() {
		x.State, x.Note = registry.NeedsYou, fmt.Sprintf("on_fail resumes worker %d, which does not exist or has no session (a run or judge step)", of.Resume)
		return
	}
	if !reaches(m, x.ID, target.ID, map[int]bool{}) {
		x.State, x.Note = registry.NeedsYou, fmt.Sprintf("on_fail resumes worker %d, which it does not come after, so it would not wait for the revision", of.Resume)
		return
	}
	all := make([]registry.Worker, 0, len(m))
	for _, o := range m {
		all = append(all, *o)
	}
	// Rendered before the new runs start, so the finals it names are the
	// failing round's: each run keeps its own (registry NewRun).
	brief, err := flow.Render(of.Brief, Refs(*x, all, w.Paths, "")) // paths absolute, as in Render
	if err != nil {
		x.State, x.Note = registry.NeedsYou, fmt.Sprintf("its on_fail brief has bad refs: %v", err)
		return
	}
	target.NewRun()
	x.NewRun()
	x.State, x.Round = registry.Waiting, round+1
	x.Note = fmt.Sprintf("round %d: waiting on %s to revise", round+1, target.Label())
	var again []int
	var stale []string
	for _, o := range between(*target, *x, m) {
		if !o.Tabless() {
			stale = append(stale, fmt.Sprintf("step %s (worker %d)", o.Step, o.ID))
			continue
		}
		o.NewRun()
		o.State, o.Pid, o.PidAt = registry.Pending, 0, 0
		o.Note = fmt.Sprintf("runs again for %s round %d, once %s has revised", x.Label(), round+1, target.Label())
		again = append(again, o.ID)
	}
	// It resumes its session, so no preamble; but it must run routr done
	// again, or a revision that leaves its output as it was ends maybe_done
	// and x waits on it for good.
	turn := target.Queue(strings.TrimSpace(brief) + "\n\n" + Finish(*target, w.Paths, w.Bin))
	target.Revisions = append(slices.Clone(target.Revisions), turn)
	target.State, target.Rest, target.Note = registry.Running, "", fmt.Sprintf("revising for %s, round %d", x.Label(), round)
	if len(again) > 0 {
		x.Note += fmt.Sprintf("; %s %s again first", count(len(again), "check"), idList(again))
	}
	if len(stale) > 0 {
		x.Note += fmt.Sprintf("; not run again: %s", strings.Join(stale, ", "))
	}
}

// between is the workers of x's chain that x comes after, directly or
// through others, and that come after target the same way: the steps
// between a review loop's target and its reviewer x, in id order.
func between(target, x registry.Worker, m map[int]*registry.Worker) []*registry.Worker {
	up := map[int]bool{}
	for todo := slices.Clone(x.After); len(todo) > 0; {
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if o := m[id]; o != nil && !up[id] && o.Chain == x.Chain {
			up[id] = true
			todo = append(todo, o.After...)
		}
	}
	var out []*registry.Worker
	for id := range up {
		if id != target.ID && reaches(m, id, target.ID, map[int]bool{}) {
			out = append(out, m[id])
		}
	}
	slices.SortFunc(out, func(a, b *registry.Worker) int { return a.ID - b.ID })
	return out
}

// reaches reports whether worker id comes after worker to, directly or
// through others.
func reaches(m map[int]*registry.Worker, id, to int, seen map[int]bool) bool {
	o := m[id]
	if o == nil || seen[id] {
		return false
	}
	seen[id] = true
	for _, a := range o.After {
		if a == to || reaches(m, a, to, seen) {
			return true
		}
	}
	return false
}

// exited handles a harness that quit while its pane stayed: the pane is
// closed, and a worker that was still at work failed. A follow-up it was
// answering is settled unanswered, and a finished worker stays finished.
func (w *Watcher) exited(ctx context.Context, wk registry.Worker, d worker.Driver, h worker.Handle) {
	working := !wk.State.Terminal() && !(wk.Turn > wk.Replies && wk.Rest != "")
	if working {
		if reply, _ := d.LastReply(ctx, h); reply != "" {
			w.writeRun(wk, w.Paths.FinalPath(wk), reply)
		}
	}
	if err := d.Close(ctx, h); err != nil {
		w.Log.Printf("%s: close: %v", wk.Label(), err)
	}
	w.dropPane(wk, "the harness exited before it replied", func(x *registry.Worker) {
		if !x.State.Terminal() {
			x.State = registry.Failed
			x.ClearQuestion()
			x.Error = "the harness exited before it ran routr done"
		}
	})
	w.Log.Printf("%s: harness exited", wk.Label())
}

// gone handles a pane that disappeared: the worker is closed, its session
// kept for `routr send`.
func (w *Watcher) gone(wk registry.Worker) {
	w.dropPane(wk, "the tab closed before it replied", func(x *registry.Worker) {
		if !x.State.Terminal() {
			x.State = registry.Closed
			x.ClearQuestion()
			x.Note = fmt.Sprintf("its tab was closed; routr send %d … resumes the session", wk.ID)
		}
	})
	w.Log.Printf("%s: tab gone", wk.Label())
}

// closeTab closes wk's pane; the session stays resumable.
func (w *Watcher) closeTab(ctx context.Context, wk registry.Worker, d worker.Driver) {
	if err := d.Close(ctx, w.handle(wk)); err != nil {
		w.Log.Printf("%s: close: %v", wk.Label(), err)
	}
	w.dropPane(wk, "its tab was closed before it replied", nil)
}

// dropPane records that wk has no pane any more (registry DropPane), then
// applies then, unless a CLI command moved the worker meanwhile.
func (w *Watcher) dropPane(wk registry.Worker, why string, then func(*registry.Worker)) {
	w.set(wk, func(x *registry.Worker) {
		if err := x.DropPane(w.Paths, why); err != nil {
			w.Log.Printf("%s: %v", wk.Label(), err)
		}
		if then != nil {
			then(x)
		}
	})
	w.forget(wk.ID)
}

var time0 time.Time

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
