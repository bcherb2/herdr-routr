package main

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/menu"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/route"
	"github.com/bcherb2/herdr-routr/internal/watch"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// oneWorker parses a command whose only positional is a worker id.
func (a *app) oneWorker(name string, args []string, extra func(fs flagSet)) (registry.Paths, registry.Worker, *common, error) {
	fs, c := a.flags(name)
	if extra != nil {
		extra(fs)
	}
	pos, err := parse(fs, args)
	if err != nil {
		return registry.Paths{}, registry.Worker{}, c, err
	}
	if len(pos) != 1 {
		return registry.Paths{}, registry.Worker{}, c, usagef("give one worker id")
	}
	t, err := a.parseTarget(pos[0], false)
	if err != nil {
		return registry.Paths{}, registry.Worker{}, c, err
	}
	p, err := a.paths(c)
	if err != nil {
		return p, registry.Worker{}, c, err
	}
	w, err := p.Store().Get(t.id)
	if errors.Is(err, registry.ErrNotFound) {
		err = fmt.Errorf("no worker %d (routr list --all)", t.id)
	}
	return p, w, c, err
}

// flagSet is the part of *flag.FlagSet the extra flags need.
type flagSet interface {
	Bool(name string, value bool, usage string) *bool
}

func (a *app) result(args []string) error {
	var path *bool
	p, w, c, err := a.oneWorker("result", args, func(fs flagSet) {
		path = fs.Bool("path", false, "print only the path of the file holding the result: the final message, or the newer follow-up reply")
	})
	if err != nil {
		return err
	}
	if *path && c.json {
		return usagef("--path prints a path, not JSON; --json alone gives final_path")
	}
	if c.json {
		return a.printJSON(a.view(p, w))
	}
	cur := p.Current(w)
	if !hasResult(cur) {
		return a.noResult(p, w)
	}
	if *path {
		fmt.Fprintln(a.stdout, cur.Path())
		return nil
	}
	fmt.Fprint(a.stdout, a.resultText(p, w))
	return nil
}

// noResult is `routr result` for a worker with no final message and no
// reply in this run: why, by its state, in `routr wait`'s words
// (stopState), and what to do next, with the final message of its previous
// run if it had one (Paths.PrevFinal). A foreach step lists its copies.
// Only a worker still at work sends you to routr wait, and is an error.
func (a *app) noResult(p registry.Paths, w registry.Worker) error {
	var b strings.Builder
	o, stop := a.stopped(p, w)
	if stop {
		b.WriteString(strings.TrimRight(o.text, "\n") + "\n")
	}
	if w.Fanout != nil && len(w.Fanout.Items) > 0 {
		fmt.Fprintf(&b, "%s is a foreach step; its copies:\n", w.Label())
		if err := a.copyLines(&b, p, w.Fanout.Items); err != nil {
			return err
		}
	}
	prev := ""
	if f := p.PrevFinal(w); f != "" {
		prev = fmt.Sprintf("previous run's final message: %s\n", f)
	}
	active := !stop && w.Active()
	switch {
	case active && b.Len() > 0:
		fmt.Fprintf(&b, "%sRun: routr wait %d\n", prev, w.ID)
		fmt.Fprint(a.stdout, b.String())
		return exitCode(exitError)
	case active:
		why := a.age(w)
		if w.State == registry.Running && !w.Started.IsZero() {
			why = "running for " + why
		}
		fmt.Fprint(a.stdout, prev)
		return fmt.Errorf("worker %d has no final message yet (%s). Run: routr wait %d", w.ID, why, w.ID)
	case b.Len() == 0:
		fmt.Fprintf(&b, "%s: %s; it wrote no final message\n", w.Label(), oneLine(w))
	}
	fmt.Fprint(a.stdout, b.String()+prev)
	return nil
}

// failedNext is what to do about failed worker w, from its record alone:
// one that never started is started again with its brief, one with no
// session to resume (a run or judge step, or a harness that recorded none)
// is run again, and one with a session is resumed in it (resumable).
func failedNext(w registry.Worker) (next string, resumable bool) {
	switch {
	case w.Started.IsZero():
		return fmt.Sprintf("It never started. routr retry %d starts it again with its brief", w.ID), false
	case w.Session == "":
		return fmt.Sprintf("routr retry %d runs it again", w.ID), false
	}
	return fmt.Sprintf("routr send %d … resumes its session", w.ID), true
}

// copyLines writes one line per foreach copy: its label, state, verdict
// and output.
func (a *app) copyLines(b *strings.Builder, p registry.Paths, ids []int) error {
	all, err := p.Store().List()
	if err != nil {
		return err
	}
	m := map[int]registry.Worker{}
	for _, w := range all {
		m[w.ID] = w
	}
	for _, id := range ids {
		c, ok := m[id]
		if !ok {
			fmt.Fprintf(b, "  %d: gone (dropped by retention)\n", id)
			continue
		}
		line := string(c.State)
		if c.Verdict != "" {
			line += ", verdict " + string(c.Verdict)
		}
		if c.Output != "" {
			line += ", output " + a.rel(c.Output, c.Cwd)
		}
		fmt.Fprintf(b, "  %s: %s\n", c.Label(), line)
	}
	return nil
}

func (a *app) send(args []string) error {
	fs, c := a.flags("send")
	wait := fs.Bool("wait", false, "wait for the reply")
	timeout := fs.Duration("timeout", DefaultTimeout, "with --wait: give up after this long (exit 3)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("give a worker id and the text (or - to read it from stdin)")
	}
	t, err := a.parseTarget(pos[0], false)
	if err != nil {
		return err
	}
	text := pos[1]
	if text == "-" {
		if text, err = a.readAll(); err != nil {
			return err
		}
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	if cur, err := p.Store().Get(t.id); err == nil {
		socket := cur.Socket
		if cur.Live != nil {
			socket = cur.Live.Socket
		}
		if err := a.canWatch(p, socket); err != nil {
			return err
		}
	}
	turn := 0
	w, err := p.Store().Modify(t.id, func(w *registry.Worker) error {
		switch {
		case w.Fanout != nil:
			return fmt.Errorf("worker %d is a foreach step; it runs no harness of its own. Send to one of its copies (routr list)", w.ID)
		case w.Run != "":
			return fmt.Errorf("worker %d is a run step; it runs a command, not a harness, so it has no session to send to", w.ID)
		case w.Judge != nil:
			return fmt.Errorf("worker %d is a judge step; it asks Jev, not a harness, so it has no session to send to", w.ID)
		case w.Queued() || w.State == registry.Starting:
			return fmt.Errorf("worker %d has not started yet; its brief is queued. Wait for it (routr wait %d) or cancel it and start another", w.ID, w.ID)
		case w.State == registry.Cancelled:
			return fmt.Errorf("worker %d was cancelled; start a new one", w.ID)
		case w.State == registry.Failed:
			if next, ok := failedNext(*w); !ok {
				why, _, _ := strings.Cut(w.Error, "\n")
				return fmt.Errorf("worker %d failed (%s) and has no session to send to. %s, in its place in the chain", w.ID, why, next)
			}
		}
		turn = w.Queue(text)
		return nil
	})
	if errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("no worker %d (routr list --all)", t.id)
	}
	if err != nil {
		return err
	}
	a.ensureWatcher(p)
	how := "into its tab"
	if w.Live == nil {
		how = "after resuming its session in a new tab"
	}
	fmt.Fprintf(a.stderr, "routr: queued follow-up %d for %s; it goes %s\n", turn, w.Label(), how)
	if !*wait {
		if c.json {
			return a.printJSON(a.view(p, w))
		}
		fmt.Fprintln(a.stdout, w.ID)
		return nil
	}
	return a.waitFor(p, target{id: w.ID, turn: turn}, *timeout, c.json)
}

func (a *app) done(args []string) error {
	fs, c := a.flags("done")
	output := fs.String("output", "", "the file you wrote")
	verdict := fs.String("verdict", "", "pass or fail")
	idFlag := fs.Int("id", 0, "worker id (default $ROUTR_ID)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	msg := strings.Join(pos, " ")
	if msg == "-" {
		if msg, err = a.readAll(); err != nil {
			return err
		}
	}
	v := registry.Verdict(*verdict)
	if v != registry.NoVerdict && v != registry.Pass && v != registry.Fail {
		return usagef("--verdict %q: use pass or fail", *verdict)
	}
	id := *idFlag
	if id == 0 {
		if id, err = strconv.Atoi(a.getenv("ROUTR_ID")); err != nil {
			return usagef("not a routr worker: ROUTR_ID is unset; pass --id N")
		}
	}
	runDir := a.getenv("ROUTR_RUN_DIR")
	if runDir == "" || *idFlag != 0 {
		p, err := a.paths(c)
		if err != nil {
			return err
		}
		runDir = p.RunDir(id)
	}
	out := *output
	if out != "" {
		if !filepath.IsAbs(out) {
			out = doneOutput(out, a.cwd, a.getenv("ROUTR_CWD"))
		}
		if _, err := os.Stat(out); err != nil {
			return fmt.Errorf("no file at %s: write it first, or leave out --output", out)
		}
	}
	rec := registry.DoneRecord{Output: out, Verdict: v, Message: msg, At: a.now()}
	if err := registry.WriteDone(runDir, rec); err != nil {
		return err
	}
	if c.json {
		return a.printJSON(rec)
	}
	fmt.Fprintf(a.stdout, "routr: recorded worker %d as done. End your turn now with a short final message.\n", id)
	return nil
}

// doneOutput resolves a relative --output against the shell's cwd, or,
// when nothing is there, against ROUTR_CWD (the cwd the worker started
// in), so that a worker that cd'd into a subdirectory still finds it.
func doneOutput(out, cwd, start string) string {
	p := filepath.Join(cwd, out)
	if start == "" || fileAt(p) {
		return p
	}
	if q := filepath.Join(start, out); fileAt(q) {
		return q
	}
	return p
}

func fileAt(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// recentlyFinished is how long `routr list` keeps showing a finished worker.
const recentlyFinished = time.Hour

// list shows workers that are active (not terminal, or a follow-up is
// queued or in flight), have an open tab, changed within --since, or belong
// to a chain that is still active (so a long chain keeps its early steps); --all
// shows every worker the registry keeps (registry.Retention).
func (a *app) list(args []string) error {
	fs, c := a.flags("list")
	all := fs.Bool("all", false, "include every worker the registry keeps")
	since := fs.Duration("since", recentlyFinished, "also show workers that finished this recently")
	withText := fs.Bool("final", false, "with --json: include each worker's final message and latest reply")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	ws, err := p.Store().List()
	if err != nil {
		return err
	}
	// Usually the watcher finds the registry unreadable first, and it only
	// logs to watch.log.
	for _, aside := range p.SetAside() {
		fmt.Fprintf(a.stderr, "routr: warning: the registry was unreadable and was set aside as %s. The workers it held are no longer tracked (their tabs may still be open). Delete it once you have looked at it.\n", aside)
	}
	var shown []registry.Worker
	active := false
	live := map[string]bool{} // chains with an active worker: all their steps are shown
	for _, w := range ws {
		if w.Active() {
			active = true
			if w.Chain != "" {
				live[w.Chain] = true
			}
		}
	}
	for _, w := range ws {
		if *all || w.Active() || w.Live != nil || a.now().Sub(w.Updated) <= *since || live[w.Chain] {
			shown = append(shown, w)
		}
	}
	if active {
		a.ensureWatcher(p) // the next morning: pick up where the last watcher stopped
	}
	if c.json {
		vs := make([]view, 0, len(shown))
		for _, w := range shown {
			if *withText {
				vs = append(vs, a.view(p, w))
			} else {
				vs = append(vs, a.pathView(p, w, p.Current(w)))
			}
		}
		return a.printJSON(vs)
	}
	if len(shown) == 0 {
		fmt.Fprintln(a.stdout, "no active or recent workers (routr list --all shows every one)")
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tCHAIN/STEP\tROLE\tHARNESS\tSTATE\tAGE\tOUTPUT\tNOTE")
	for _, w := range shown {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, dash(w.Title), dash(chainStep(w)), w.Role, dash(w.Tag()),
			stateText(w), a.now().Sub(w.Created).Truncate(time.Second), dash(outputText(a, w)), noteText(w))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	head := false
	for _, w := range shown {
		if t := treeText(a, w); t != "" {
			if !head {
				fmt.Fprintln(a.stdout, "\nworktrees:")
				head = true
			}
			fmt.Fprintf(a.stdout, "  %d %s: %s\n", w.ID, cmp.Or(w.Step, w.Role), t)
		}
	}
	return nil
}

// treeText is where a worker's own worktree is, and its branch; "" for a
// worker without one, or before the watcher names it.
func treeText(a *app, w registry.Worker) string {
	c := w.Checkout
	if c == nil || c.Path == "" {
		return ""
	}
	s := a.rel(c.Path, w.Cwd) + " (branch " + c.Branch + ")"
	if !c.Made {
		s += ", not made yet"
	}
	return s
}

func chainStep(w registry.Worker) string {
	if w.Step == "" {
		return w.Chain
	}
	return w.Chain + "/" + w.Step
}

func stateText(w registry.Worker) string {
	s := string(w.State)
	if w.Verdict != "" {
		s += " (" + string(w.Verdict) + ")"
	}
	if w.State == registry.Blocked && w.Answering() == "jev" {
		s += ", Jev decides"
	}
	if w.Live != nil && w.State.Terminal() {
		s += ", tab open"
	}
	return s
}

func outputText(a *app, w registry.Worker) string {
	if w.Output == "" {
		return ""
	}
	return a.rel(w.Output, w.Cwd)
}

func noteText(w registry.Worker) string {
	s := first(w.Question, w.Error, w.Note, w.Summary)
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 80 {
		line = line[:77] + "..."
	}
	return line
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) show(args []string) error {
	var focus, asked *bool
	p, w, c, err := a.oneWorker("show", args, func(fs flagSet) {
		focus = fs.Bool("focus", false, "focus the worker's pane")
		asked = fs.Bool("context", false, "show no pane: print what its question is decided with, and the project docs to read")
	})
	if err != nil {
		return err
	}
	if *asked {
		if *focus {
			return usagef("--context prints and shows no pane, so it takes no --focus")
		}
		return a.showContext(p, w, c)
	}
	beside := a.getenv("HERDR_PANE_ID")
	if beside == "" {
		return errors.New("HERDR_PANE_ID is unset: run routr show from your herdr pane")
	}
	if w.Live == nil {
		return fmt.Errorf("worker %d has no open tab (%s). routr send %d … reopens it", w.ID, w.State, w.ID)
	}
	if err := a.dial(w.Live.Socket).Show(a.ctx, watch.Handle(w), beside, *focus); err != nil {
		return err
	}
	return a.acted(c, p, "show", []string{fmt.Sprintf("%s is beside your pane; routr hide %d puts it back", w.Label(), w.ID)}, w)
}

// showContext prints what worker w's question is decided with
// (watch.BuildContext), the project docs as paths for the agent to read. A
// worker that asks nothing now gets the rest, so the agent can look before
// a question comes, and one answered meanwhile is no error. A run, judge or
// foreach step never asks.
func (a *app) showContext(p registry.Paths, w registry.Worker, c *common) error {
	switch {
	case w.Run != "":
		return fmt.Errorf("worker %d is a run step; it runs a command and asks no questions", w.ID)
	case w.Judge != nil:
		return fmt.Errorf("worker %d is a judge step; it asks Jev and has no questions of its own", w.ID)
	case w.Fanout != nil:
		return fmt.Errorf("worker %d is a foreach step; it runs no harness of its own. Look at one of its copies (routr list)", w.ID)
	}
	cfg, err := a.loadConfig(p)
	if err != nil {
		return err
	}
	qc := watch.BuildContext(p, cfg.Questions.Context, w)
	if c.json {
		return a.printJSON(qc)
	}
	_, err = fmt.Fprint(a.stdout, qc.Text())
	return err
}

func (a *app) hide(args []string) error {
	p, w, c, err := a.oneWorker("hide", args, nil)
	if err != nil {
		return err
	}
	if w.Live == nil {
		return fmt.Errorf("worker %d has no open tab", w.ID)
	}
	if err := a.dial(w.Live.Socket).Hide(a.ctx, watch.Handle(w)); err != nil {
		return err
	}
	return a.acted(c, p, "hide", nil, w)
}

func (a *app) closeCmd(args []string) error {
	p, w, c, err := a.oneWorker("close", args, nil)
	if err != nil {
		return err
	}
	if w.Queued() {
		return fmt.Errorf("worker %d has not started; routr cancel %d removes it from the queue", w.ID, w.ID)
	}
	if w.Run != "" {
		return fmt.Errorf("worker %d is a run step; it has no tab. routr cancel %d stops its command", w.ID, w.ID)
	}
	if w.Judge != nil {
		return fmt.Errorf("worker %d is a judge step; it has no tab. routr cancel %d stops it", w.ID, w.ID)
	}
	var had registry.Worker // as it was, with the pane to close
	var dropErr error
	w, err = p.Store().Update(w.ID, func(x *registry.Worker) {
		had = *x
		dropErr = x.DropPane(p, "closed by routr close before it replied")
		if !x.State.Terminal() {
			x.State = registry.Closed
			x.ClearQuestion()
			x.Note = fmt.Sprintf("closed by routr close; routr send %d … resumes the session", x.ID)
		}
	})
	if err != nil {
		return err
	}
	if dropErr != nil {
		fmt.Fprintf(a.stderr, "routr: %v\n", dropErr)
	}
	if had.Live != nil {
		if err := a.dial(had.Live.Socket).Close(a.ctx, watch.Handle(had)); err != nil {
			fmt.Fprintf(a.stderr, "routr: closing the tab: %v\n", err)
		}
	}
	a.ensureWatcher(p)
	return a.acted(c, p, "close", []string{fmt.Sprintf("closed %s (%s); routr send %d … resumes it", w.Label(), w.State, w.ID)}, w)
}

// targets resolves ID|CHAIN to the matching workers, by id.
func (a *app) targets(name string, args []string) (registry.Paths, target, *common, error) {
	fs, c := a.flags(name)
	pos, err := parse(fs, args)
	if err != nil {
		return registry.Paths{}, target{}, c, err
	}
	if len(pos) != 1 {
		return registry.Paths{}, target{}, c, usagef("give one worker id or chain name")
	}
	t, err := a.parseTarget(pos[0], true)
	if err != nil {
		return registry.Paths{}, target{}, c, err
	}
	p, err := a.paths(c)
	return p, t, c, err
}

func (t target) match(w registry.Worker) bool {
	if t.chain != "" {
		return w.Chain == t.chain
	}
	return w.ID == t.id
}

func (a *app) approve(args []string) error {
	p, t, c, err := a.targets("approve", args)
	if err != nil {
		return err
	}
	var did []string
	var approved []int
	err = p.Store().Tx(func(m map[int]*registry.Worker) error {
		for _, id := range sortedIDs(m) {
			w := m[id]
			if !t.match(*w) {
				continue
			}
			switch {
			case w.State == registry.NeedsYou && w.Fanout != nil && w.Fanout.Items == nil:
				w.State, w.Note = registry.Pending, "approved by you: looking for its files again"
				did, approved = append(did, fmt.Sprintf("%s: looking for its files again", w.Label())), append(approved, id)
			case w.State == registry.NeedsYou && w.Judge != nil && w.Finished.IsZero():
				// Jev was unsure, or could not answer: take its last
				// choice, or ask again.
				w.State, w.Judge.Accept, w.Note = registry.Pending, true, "approved by you: Jev's last choice stands, or it is asked again if it made none"
				did, approved = append(did, fmt.Sprintf("%s: Jev's choice taken (or asked again)", w.Label())), append(approved, id)
			case w.State == registry.NeedsYou && !w.Finished.IsZero():
				w.State, w.Note = registry.Done, "approved by you as it is"
				did, approved = append(did, fmt.Sprintf("%s accepted as done", w.Label())), append(approved, id)
			case w.Halts():
				// Its fail stays on record; the steps after it may start.
				w.GoOn, w.Note = true, "approved by you: the chain goes on past its fail"
				did, approved = append(did, fmt.Sprintf("%s: the chain goes on past its fail", w.Label())), append(approved, id)
			case w.Queued() && w.Gate != "" && !w.GateOpen && (t.chain == "" || w.State == registry.Gated):
				// Pending, not gated, so that a `routr wait` right after
				// this doesn't stop at a gate the watcher has yet to see open.
				w.GateOpen = true
				if w.State == registry.Gated {
					w.State, w.Note = registry.Pending, "approved by you"
				}
				did, approved = append(did, fmt.Sprintf("%s: gate open", w.Label())), append(approved, id)
			}
		}
		if len(did) == 0 {
			return fmt.Errorf("nothing to approve in %s: no closed gate, no loop at its cap, no step that failed with no on_fail, no foreach that found the wrong files and no judge that needs you (routr list)", t)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, line := range did {
		a.logWatch(p, "%s (routr approve)", line)
	}
	a.ensureWatcher(p)
	ws, err := a.workers(p, approved)
	if err != nil {
		return err
	}
	return a.acted(c, p, "approve", []string{strings.Join(did, "; ")}, ws...)
}

// retry queues failed workers again in their place: the same id, chain,
// step, round and worktree, so the workers after them still wait on them,
// and a foreach parent on its copy. One that never started starts
// afresh with its brief; one that ran keeps its session, so it is rerun
// with the revised prompt. A tab its failure left open is closed.
func (a *app) retry(args []string) error {
	p, t, c, err := a.targets("retry", args)
	if err != nil {
		return err
	}
	all, err := p.Store().List()
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(all, func(w registry.Worker) bool { return t.match(w) && w.State == registry.Failed }); i >= 0 {
		if err := a.canWatch(p, all[i].Socket); err != nil {
			return err
		}
	}
	var said []string
	var ids []int
	var tabs []registry.Worker
	err = p.Store().Tx(func(m map[int]*registry.Worker) error {
		if t.chain == "" && m[t.id] == nil {
			return fmt.Errorf("no worker %d (routr list --all)", t.id)
		}
		for _, id := range sortedIDs(m) {
			w := m[id]
			switch {
			case !t.match(*w):
				continue
			case w.State != registry.Failed && t.chain == "":
				return notFailed(*w)
			case w.State != registry.Failed:
				continue
			}
			if w.Live != nil {
				tabs = append(tabs, *w)
			}
			requeue(w)
			said, ids = append(said, "queued "+w.Label()+" again"), append(ids, id)
		}
		if len(ids) == 0 {
			return fmt.Errorf("nothing to retry in %s: none of its workers failed (routr list)", t)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range tabs {
		if err := a.dial(w.Live.Socket).Close(a.ctx, watch.Handle(w)); err != nil {
			errs = append(errs, fmt.Errorf("close the tab of %s: %w", w.Label(), err))
		}
	}
	for _, line := range said {
		a.logWatch(p, "%s by routr retry", line)
	}
	a.ensureWatcher(p)
	ws, err := a.workers(p, ids)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	return errors.Join(append(errs, a.acted(c, p, "retry", said, ws...))...)
}

// requeue makes a failed worker pending again, as if just queued, but for
// the session of one that ran: start then reruns it in that session. What
// the failed run concluded becomes history (registry NewRun).
func requeue(w *registry.Worker) {
	w.NewRun()
	w.State, w.Note = registry.Pending, "queued again by routr retry"
	w.Live, w.Pid, w.PidAt = nil, 0, 0
	w.Inbox = nil // a dropped follow-up is gone; routr send it again
	w.TurnError, w.Retries, w.Retry = "", 0, time.Time{}
	w.ClearQuestion()
	if w.Started.IsZero() {
		w.Session, w.Transcript = "", "" // what a failed start named was never created
	}
}

// notFailed is retry's error for a worker that did not fail: what to do
// with it instead.
func notFailed(w registry.Worker) error {
	switch {
	case w.Succeeded():
		return fmt.Errorf("worker %d is done; nothing to retry. routr send %d … gives it a follow-up", w.ID, w.ID)
	case w.State == registry.Closed || w.State == registry.MaybeDone:
		return fmt.Errorf("worker %d is %s, not failed: routr send %d … gives it a follow-up in its session", w.ID, w.State, w.ID)
	}
	return fmt.Errorf("worker %d is %s, not failed; routr retry starts only a failed worker again (routr list)", w.ID, w.State)
}

// answer picks option N of the menu on a blocked worker's screen, and with
// --expect only if that menu is still the one with that fingerprint: an
// answer decided away from the screen must not land on another question.
// An answer by agent or jev must name the question it decided (--expect),
// and may not pick an option that saves a rule or lasts past this call
// (menu.Lasting): the user did not give it. Who runs routr does not matter:
// a model that would answer the user's question would also type --by human,
// so requiring the flags of an agent gated nothing. The answer and who
// chose it go on the worker and into watch.log.
func (a *app) answer(args []string) error {
	fs, c := a.flags("answer")
	expect := fs.String("expect", "", "press nothing unless the question on screen has this fingerprint (the worker's fingerprint in routr list --json); required with --by agent or jev")
	by := fs.String("by", "human", "who chose the option: human (the user), agent or jev; agent and jev may not pick one that saves a rule or lasts past this call")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("give a worker id and an option number")
	}
	if !slices.Contains(registry.Answerers, *by) {
		return usagef("--by %q: use human, agent or jev", *by)
	}
	if *by != "human" && *expect == "" {
		return usagef("--by %s needs --expect FP: pass the worker's fingerprint from the [routr] message, routr wait or routr list --json. "+
			"A question with no menu has no fingerprint; the user answers it", *by)
	}
	t, err := a.parseTarget(pos[0], false)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(pos[1])
	if err != nil || n <= 0 {
		return usagef("option %q: give the number shown on screen, from 1", pos[1])
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	w, err := p.Store().Get(t.id)
	if err != nil {
		return fmt.Errorf("no worker %d (routr list --all)", t.id)
	}
	if w.State != registry.Blocked || w.Live == nil {
		return fmt.Errorf("worker %d is %s, not blocked on a question", w.ID, w.State)
	}
	text, err := a.dial(w.Live.Socket).Answer(a.ctx, watch.Handle(w), n, *expect, *by != "human")
	var changed *worker.ChangedError
	var lasting *worker.LastingError
	switch {
	case errors.As(err, &changed):
		a.logWatch(p, "%s: answer %d by %s refused: the question on screen is not %s", w.Label(), n, *by, *expect)
		return a.refused(w, *by, changed)
	case errors.As(err, &lasting):
		a.logWatch(p, "%s: answer %d by %s refused: %q decides more than this call", w.Label(), n, *by, lasting.Options[n-1])
		return lastingRefused(lasting)
	case errors.Is(err, worker.ErrStillAsking):
		a.logWatch(p, "%s: pressed option %d (%s) by %s, but the question stayed on screen", w.Label(), n, text, *by)
		return fmt.Errorf("pressed option %d (%s) on %s's question, but it was still on screen seconds later. Look before you answer again: routr show %d", n, text, w.Label(), w.ID)
	case err != nil:
		return err
	}
	answer := fmt.Sprintf("%d. %s", n, text)
	got, err := p.Store().Modify(w.ID, func(x *registry.Worker) error {
		if x.State == registry.Blocked {
			x.State = registry.Running
			x.ClearQuestion()
		}
		x.Answer, x.AnsweredBy = answer, *by
		return nil
	})
	if err != nil {
		fmt.Fprintf(a.stderr, "routr: the answer went, but recording it failed: %v\n", err)
	} else {
		w = got
	}
	a.logWatch(p, "%s: answered %s, by %s", w.Label(), answer, *by)
	a.ensureWatcher(p)
	return a.acted(c, p, "answer", []string{fmt.Sprintf("answered %s with option %d (%s), by %s", w.Label(), n, text, *by)}, w)
}

// refused reports an answer that --expect stopped: what is on screen now.
// Another question needs deciding (exit 4); none means it is gone (exit 1).
func (a *app) refused(w registry.Worker, by string, now *worker.ChangedError) error {
	if now.Fingerprint == "" {
		return fmt.Errorf("worker %d shows no menu now: its question was answered or went away. No option was chosen. routr list shows where it is", w.ID)
	}
	again := fmt.Sprintf("routr answer %d N --expect %s --by %s", w.ID, now.Fingerprint, by)
	fmt.Fprintf(a.stderr, "routr answer: %s asks another question now, so no option was chosen. On its screen (fingerprint %s):\n%s\nDecide again, then: %s\n",
		w.Label(), now.Fingerprint, now.Question, again)
	return exitCode(exitNeedsYou)
}

// lastingRefused is the error for an answer by agent or jev that picked an
// option that saves a rule or lasts past this call: what it may pick.
func lastingRefused(e *worker.LastingError) error {
	var once []string
	for i, o := range e.Options {
		if !menu.Lasting(o) {
			once = append(once, fmt.Sprintf("%d. %s", i+1, o))
		}
	}
	instead := "Ask the user"
	if len(once) > 0 {
		instead = fmt.Sprintf("Pick one that answers only this call (%s), or ask the user", strings.Join(once, "; "))
	}
	return fmt.Errorf("option %d (%s) saves a rule or allows more than this call, so only the user may pick it (--by human). Nothing was pressed. %s",
		e.Option, e.Options[e.Option-1], instead)
}

// logWatch adds a line to watch.log; failing to is a warning.
func (a *app) logWatch(p registry.Paths, format string, args ...any) {
	if err := watch.Logf(p, format, args...); err != nil {
		fmt.Fprintf(a.stderr, "routr: could not write watch.log: %v\n", err)
	}
}

func (a *app) cancel(args []string) error {
	p, t, c, err := a.targets("cancel", args)
	if err != nil {
		return err
	}
	all, err := p.Store().List()
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	hit := toCancel(t, all, seen)
	if len(hit) == 0 {
		return fmt.Errorf("nothing to cancel in %s: it has finished (routr list --all)", t)
	}
	var errs []error
	var said []string
	var cancelled []registry.Worker
	for len(hit) > 0 {
		for _, w := range hit {
			// The pane to close is the one the registry has when the change
			// lands, not the one listed above: the watcher may have opened it
			// in between.
			var had registry.Worker
			now, err := p.Store().Update(w.ID, func(x *registry.Worker) {
				had = *x
				if err := x.DropPane(p, "cancelled by routr cancel before it replied"); err != nil {
					errs = append(errs, err)
				}
				x.Inbox = nil
				if !x.Succeeded() {
					x.State, x.Note = registry.Cancelled, "cancelled by routr cancel"
					x.ClearQuestion()
				}
			})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			cancelled = append(cancelled, now)
			a.logWatch(p, "%s: cancelled by routr cancel", w.Label())
			if err := watch.StopRun(p, had); err != nil { // a run step's command, if it runs
				errs = append(errs, err)
			}
			if had.Live != nil {
				if err := a.dial(had.Live.Socket).Close(a.ctx, watch.Handle(had)); err != nil {
					errs = append(errs, fmt.Errorf("close %s: %w", w.Label(), err))
				}
			}
			said = append(said, "cancelled "+w.Label())
		}
		// The watcher may have expanded a foreach worker into its copies
		// before it was cancelled: they go too. Once it is cancelled it
		// expands no more, so this ends.
		if all, err = p.Store().List(); err != nil {
			errs = append(errs, err)
			break
		}
		hit = toCancel(t, all, seen)
	}
	if t.chain == "" {
		for _, w := range all {
			if slices.Contains(w.After, t.id) && w.Queued() {
				said = append(said, fmt.Sprintf("%s comes after it and will not start; routr cancel %d, or routr cancel %s for the chain", w.Label(), w.ID, dash(w.Chain)))
			}
		}
	}
	a.ensureWatcher(p)
	return errors.Join(append(errs, a.acted(c, p, "cancel", said, cancelled...))...)
}

// toCancel lists what cancelling t cancels and seen does not hold yet: its
// workers, and the copies of a foreach worker of it, that are active or
// have a tab. It adds them to seen.
func toCancel(t target, all []registry.Worker, seen map[int]bool) []registry.Worker {
	copies := map[int]bool{}
	for _, w := range all {
		if t.match(w) && w.Fanout != nil {
			for _, id := range w.Fanout.Items {
				copies[id] = true
			}
		}
	}
	var hit []registry.Worker
	for _, w := range all {
		if !seen[w.ID] && (t.match(w) || copies[w.ID]) && (w.Active() || w.Live != nil) {
			seen[w.ID] = true
			hit = append(hit, w)
		}
	}
	return hit
}

// workers are the registry's workers with these ids, in that order.
func (a *app) workers(p registry.Paths, ids []int) ([]registry.Worker, error) {
	ws := make([]registry.Worker, 0, len(ids))
	for _, id := range ids {
		w, err := p.Store().Get(id)
		if err != nil {
			return nil, err
		}
		ws = append(ws, w)
	}
	return ws, nil
}

// acted reports what a command did to workers: each line of said as
// "routr: …", or with --json one object, {action, message, workers}, the
// workers as `list --json` shows them.
func (a *app) acted(c *common, p registry.Paths, action string, said []string, ws ...registry.Worker) error {
	if !c.json {
		for _, line := range said {
			if _, err := fmt.Fprintln(a.stdout, "routr: "+line); err != nil {
				return err
			}
		}
		return nil
	}
	vs := make([]view, 0, len(ws))
	for _, w := range ws {
		vs = append(vs, a.pathView(p, w, p.Current(w)))
	}
	return a.printJSON(struct {
		Action  string `json:"action"`
		Message string `json:"message,omitempty"`
		Workers []view `json:"workers"`
	}{action, strings.Join(said, "\n"), vs})
}

func sortedIDs(m map[int]*registry.Worker) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (a *app) roles(args []string) error {
	fs, c := a.flags("roles")
	caller := fs.String("caller", "", "the calling agent's harness (default: from its environment)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	cfg, err := a.applyConfig(p)
	if err != nil {
		return err
	}
	if *caller == "" {
		*caller = config.DetectCaller(a.getenv)
	}
	type row struct {
		Role        string   `json:"role"`
		Write       bool     `json:"write"`
		Interactive bool     `json:"interactive,omitempty"`
		Harness     string   `json:"harness,omitempty"`
		Model       string   `json:"model,omitempty"`
		Effort      string   `json:"effort,omitempty"`
		Gate        string   `json:"gate,omitempty"`
		Output      string   `json:"output,omitempty"`
		Prefer      []string `json:"prefer"`
		Avoid       string   `json:"avoid,omitempty"`
		Error       string   `json:"error,omitempty"`
		Preamble    string   `json:"preamble,omitempty"`
	}
	var rows []row
	for _, name := range cfg.RoleNames() {
		role := cfg.Roles[name]
		r := row{Role: name, Write: role.Writes(), Interactive: role.Interactive, Gate: role.Gate, Output: role.Output, Prefer: config.Candidates(role, *caller), Avoid: role.Avoid, Preamble: role.Preamble}
		res, err := cfg.Resolve(a.ctx, config.Request{Role: name, Caller: *caller}, a.ready)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Harness, r.Model, r.Effort = res.Harness, res.Model, res.Effort
		}
		rows = append(rows, r)
	}
	if c.json {
		return a.printJSON(rows)
	}
	// The mode is the workers', not the caller's own: callers read a bare
	// "permissions: default" as theirs.
	fmt.Fprintf(a.stdout, "caller: %s · workers started without --permissions get: %s\n", dash(*caller), cfg.PermissionsFor(*caller))
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tWRITES\tTALKS\tNOW PICKS\tPREFER\tGATE\tOUTPUT")
	var low []string
	for _, r := range rows {
		picks := strings.Join(slices.DeleteFunc([]string{r.Harness, cmp.Or(r.Model, "default model"), r.Effort}, func(s string) bool { return s == "" }), " · ")
		if r.Error != "" {
			picks = "none ready"
		}
		if r.Effort == "low" {
			low = append(low, r.Role)
		}
		prefer := strings.Join(r.Prefer, ",")
		if r.Avoid != "" {
			prefer += " (avoid " + r.Avoid + ")"
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\t%s\t%s\n", r.Role, r.Write, yesDash(r.Interactive), picks, prefer, dash(r.Gate), dash(r.Output))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "TALKS: the user talks to it in its tab; it ends only when it runs routr done, so a wait on it lasts until then.")
	if len(low) > 0 {
		fmt.Fprintf(a.stdout, "effort low (%s): fast and cheap; pass --effort high for open-ended brainstorming or synthesis.\n", strings.Join(low, ", "))
	}
	_, err = fmt.Fprintf(a.stdout, "--model: %s. default model: the harness's own.\n", modelHint())
	return err
}

// modelHint names the models --model takes on each harness, from route's
// table, so a caller need not guess one: a made-up dated id fails.
func modelHint() string {
	var hs []string
	for _, h := range route.Defaults() {
		var ms []string
		for m := range h.Models {
			if m != route.PiDefaultModel {
				ms = append(ms, m)
			}
		}
		slices.Sort(ms)
		if h.Kind == "pi" {
			ms = append(ms, "provider/id (e.g. openrouter/deepseek/deepseek-v4-flash)")
		}
		hs = append(hs, h.Kind+" "+strings.Join(ms, "|"))
	}
	return strings.Join(hs, " · ")
}

func (a *app) watchCmd(args []string) error {
	fs, c := a.flags("watch")
	fg := fs.Bool("foreground", false, "run here instead of detached")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	// With --json: {action, message, root, log}; a watcher run in the
	// foreground prints nothing on stdout.
	said := func(msg string) error {
		if !c.json {
			_, err := fmt.Fprintln(a.stdout, "routr: "+msg)
			return err
		}
		return a.printJSON(struct {
			Action  string `json:"action"`
			Message string `json:"message"`
			Root    string `json:"root"`
			Log     string `json:"log"`
		}{"watch", msg, p.Root(), p.WatchLog()})
	}
	if !*fg {
		if err := a.spawn(p); err != nil {
			return err
		}
		return said(fmt.Sprintf("watcher running for %s (log: %s)", p.Root(), p.WatchLog()))
	}
	release, err := watch.Hold(p)
	if errors.Is(err, watch.ErrRunning) {
		return said("a watcher is already running for " + p.Root())
	}
	if err != nil {
		return err
	}
	defer release()
	ctx, stop := signal.NotifyContext(a.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	w := &watch.Watcher{
		Paths: p,
		Dial:  a.dial,
		Env: func() (map[string]string, error) {
			cfg, err := a.loadConfig(p)
			return cfg.Env, err
		},
		Questions: func() (config.Questions, error) {
			cfg, err := a.loadConfig(p)
			return cfg.Questions, err
		},
		Bin:   a.bin,
		Build: a.build.short(),
		Log:   log.New(watch.LogTo(p, a.stderr), "", log.LstdFlags),
	}
	if err := w.Run(ctx); err != nil && !errors.Is(err, ctx.Err()) {
		return err
	}
	return nil
}

func yesDash(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}
