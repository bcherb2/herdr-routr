package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/watch"
)

// target is what a command acts on: a worker id or a chain name.
type target struct {
	id    int
	chain string
	turn  int // wait for the reply to this follow-up, not for the worker to finish
}

func (t target) String() string {
	if t.chain != "" {
		return t.chain
	}
	return strconv.Itoa(t.id)
}

// parseTarget reads ID, CHAIN, or - (an id from stdin).
func (a *app) parseTarget(s string, chainOK bool) (target, error) {
	if s == "-" {
		tok, err := a.readToken("worker id")
		if err != nil {
			return target{}, err
		}
		s = tok
	}
	if id, err := strconv.Atoi(s); err == nil {
		if id <= 0 {
			return target{}, usagef("%q is not a worker id", s)
		}
		return target{id: id}, nil
	}
	if !chainOK {
		return target{}, usagef("%q is not a worker id", s)
	}
	return target{chain: s}, nil
}

func (a *app) wait(args []string) error {
	fs, c := a.flags("wait")
	timeout := fs.Duration("timeout", DefaultTimeout, "give up waiting after this long (exit 3)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("give one worker id, chain name, or -")
	}
	t, err := a.parseTarget(pos[0], true)
	if err != nil {
		return err
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	return a.waitFor(p, t, *timeout, c.json)
}

// outcome is where a wait ends: code < 0 keeps waiting.
type outcome struct {
	code    int
	text    string
	workers []registry.Worker
}

var keepWaiting = outcome{code: -1}

// waitFor re-reads the registry until t finishes or needs you, or timeout.
// It restarts the watcher if it died meanwhile.
func (a *app) waitFor(p registry.Paths, t target, timeout time.Duration, asJSON bool) error {
	store := p.Store()
	begin := a.now()
	var ensured time.Time
	for {
		all, err := store.List()
		if err != nil {
			return err
		}
		o, err := a.judge(p, t, all)
		if err != nil {
			return err
		}
		if o.code < 0 && a.now().Sub(begin) >= timeout {
			o = outcome{code: exitRunning, text: a.stillRunning(t, all)}
		}
		if o.code >= 0 {
			if asJSON {
				if err := a.printViews(p, t, all); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(a.stdout, strings.TrimRight(o.text, "\n"))
			}
			if o.code == exitOK {
				return nil
			}
			return exitCode(o.code)
		}
		if a.now().Sub(ensured) >= 5*time.Second {
			a.ensureWatcher(p)
			ensured = a.now()
		}
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(a.poll):
		}
	}
}

func (a *app) judge(p registry.Paths, t target, all []registry.Worker) (outcome, error) {
	m := map[int]registry.Worker{}
	for _, w := range all {
		m[w.ID] = w
	}
	if t.chain == "" {
		w, ok := m[t.id]
		if !ok {
			return outcome{}, fmt.Errorf("no worker %d (routr list --all)", t.id)
		}
		if t.turn > 0 {
			return a.judgeTurn(p, w, t.turn), nil
		}
		return a.judgeOne(p, w, m), nil
	}
	var members []registry.Worker
	for _, w := range all {
		if w.Chain == t.chain {
			members = append(members, w)
		}
	}
	if len(members) == 0 {
		return outcome{}, fmt.Errorf("no worker or chain %q (routr list --all)", t.chain)
	}
	return a.judgeChain(p, members, m), nil
}

func (a *app) judgeOne(p registry.Paths, w registry.Worker, m map[int]registry.Worker) outcome {
	if o, stop := a.stopped(p, w); stop {
		return o
	}
	if o, stop := a.upstream(p, w, m); stop {
		return o
	}
	if w.TurnsPending() || !(w.Succeeded() || w.State == registry.MaybeDone) {
		return keepWaiting
	}
	return outcome{code: exitOK, text: a.resultText(p, w), workers: []registry.Worker{w}}
}

// judgeTurn ends a wait for the reply to follow-up turn.
func (a *app) judgeTurn(p registry.Paths, w registry.Worker, turn int) outcome {
	if w.Replies >= turn {
		return outcome{code: exitOK, text: readFile(p.TurnFile(w.ID, turn)), workers: []registry.Worker{w}}
	}
	if w.State == registry.Blocked || w.State == registry.Cancelled || (w.State == registry.Failed && len(w.Inbox) == 0) ||
		(w.State == registry.NeedsYou && len(w.Inbox) == 0 && w.Turn < turn) {
		if o, stop := a.stopState(p, w); stop {
			return o
		}
	}
	return keepWaiting
}

func (a *app) judgeChain(p registry.Paths, members []registry.Worker, m map[int]registry.Worker) outcome {
	finished := true
	for _, w := range members {
		if o, stop := a.stopped(p, w); stop {
			return o
		}
		if o, stop := a.upstream(p, w, m); stop {
			return o
		}
		if w.TurnsPending() || !(w.Succeeded() || w.State == registry.MaybeDone) {
			finished = false
		}
	}
	if !finished {
		return keepWaiting
	}
	var b strings.Builder
	for _, w := range members {
		fmt.Fprintf(&b, "%s: %s\n", w.Label(), oneLine(w))
	}
	fmt.Fprintf(&b, "\n%s", a.resultText(p, lastStep(members)))
	return outcome{code: exitOK, text: b.String(), workers: members}
}

// lastStep is the chain's last worker by id, leaving out the copies a
// foreach worker expanded into while the chain ran: they come after the
// steps queued with them.
func lastStep(members []registry.Worker) registry.Worker {
	copies := map[int]bool{}
	for _, w := range members {
		if w.Fanout != nil {
			for _, id := range w.Fanout.Items {
				copies[id] = true
			}
		}
	}
	for i := len(members) - 1; i > 0; i-- {
		if !copies[members[i].ID] {
			return members[i]
		}
	}
	return members[0]
}

// oneLine is a chain step's summary line.
func oneLine(w registry.Worker) string {
	parts := []string{string(w.State)}
	if w.Verdict != "" {
		parts = append(parts, "verdict "+string(w.Verdict))
	}
	s := strings.Join(parts, ", ")
	if w.Summary != "" {
		s += " — " + w.Summary
	}
	return s
}

// stopped reports whether w ended a wait: it needs you, failed or was
// cancelled. A follow-up queued or awaiting its reply keeps it going (a
// failed worker is resumed for it), unless it is blocked on a question or
// was cancelled.
func (a *app) stopped(p registry.Paths, w registry.Worker) (outcome, bool) {
	if w.TurnsPending() && w.State != registry.Blocked && w.State != registry.Cancelled {
		return outcome{}, false
	}
	return a.stopState(p, w)
}

// upstream reports whether a queued w waits on a worker it comes after
// (transitively) that stopped, that went idle without `routr done`
// (maybe_done), or that halts the steps after it (a fail without
// on_fail): they cannot start until you act.
func (a *app) upstream(p registry.Paths, w registry.Worker, m map[int]registry.Worker) (outcome, bool) {
	if !w.Queued() {
		return outcome{}, false
	}
	for _, up := range registry.Ancestors(w, m) {
		o, stop := a.stopped(p, up)
		if !stop && up.State == registry.MaybeDone && !up.TurnsPending() {
			o, stop = outcome{code: exitNeedsYou, workers: []registry.Worker{up}, text: a.resultText(p, up)}, true
		}
		if !stop && up.Halts() {
			o, stop = outcome{code: exitNeedsYou, workers: []registry.Worker{up}, text: fmt.Sprintf(
				"needs you: %s ended with a fail verdict, so the steps after it do not start. "+
					"routr approve %d lets the chain go on past it\n\n%s", up.Label(), up.ID, a.resultText(p, up))}, true
		}
		if stop {
			o.text = fmt.Sprintf("%s waits on %s:\n%s", w.Label(), up.Label(), o.text)
			return o, true
		}
	}
	return outcome{}, false
}

// stopState is stopped by w's state alone. It prints w's header; one that
// needs you also prints its result (body), if it has one.
func (a *app) stopState(p registry.Paths, w registry.Worker) (outcome, bool) {
	code := exitNeedsYou
	switch w.State {
	case registry.Failed, registry.Cancelled:
		code = exitFailed
	case registry.Blocked, registry.Gated, registry.NeedsYou, registry.Closed:
	default:
		return outcome{}, false
	}
	text := a.header(p, w)
	if text == "" {
		return outcome{}, false // Jev decides its question, or its tab closed after it finished
	}
	if w.State == registry.NeedsYou && hasResult(p.Current(w)) {
		text = a.resultText(p, w)
	}
	return outcome{code: code, text: text, workers: []registry.Worker{w}}, true
}

// header is w's state line and what to do next, or "" when its state needs
// no word (done, running): the one place w's state becomes text, for `wait`
// that stops on it and for `result` above w's body.
func (a *app) header(p registry.Paths, w registry.Worker) string {
	switch w.State {
	case registry.Blocked:
		if w.Answering() == "jev" {
			return "" // Jev decides; it answers or hands the question over in seconds
		}
		text := fmt.Sprintf("blocked: %s asks:\n%s\n", w.Label(), w.Question)
		if w.Pending != nil {
			text += "It asks about " + w.Pending.Summary + "\n"
		}
		return text + a.whoAnswers(p, w)
	case registry.Gated:
		return fmt.Sprintf("gated: %s waits for your approval. Run: routr approve %d", w.Label(), w.ID)
	case registry.NeedsYou:
		return fmt.Sprintf("needs you: %s: %s", w.Label(), first(w.Error, w.Note))
	case registry.Failed:
		next, _ := failedNext(w)
		return fmt.Sprintf("failed: %s: %s\n%s", w.Label(), w.Error, next)
	case registry.Cancelled:
		if w.Error != "" {
			return fmt.Sprintf("cancelled: %s: %s", w.Label(), w.Error)
		}
		return fmt.Sprintf("cancelled: %s", w.Label())
	case registry.Closed:
		if w.Succeeded() || w.TurnsPending() {
			return ""
		}
		return fmt.Sprintf("closed: %s's tab was closed before it finished. routr send %d … resumes it", w.Label(), w.ID)
	case registry.MaybeDone:
		return fmt.Sprintf("maybe done: %s went idle without running routr done; below is its last reply. "+
			"routr send %d … to have it go on (or run routr done)", w.Label(), w.ID)
	}
	return ""
}

// whoAnswers is who answers blocked worker w's question and how
// (watch.QuestionContext.WhoAnswers). With a config that does not load, the
// docs to read are unknown, so it names the command that says.
func (a *app) whoAnswers(p registry.Paths, w registry.Worker) string {
	cfg, err := a.loadConfig(p)
	if err != nil {
		qc := watch.BuildContext(p, []string{}, w)
		return strings.Join(qc.WhoAnswers(w, fmt.Sprintf("Before you decide, run routr show %d --context: it says which project docs and brief to read.", w.ID)), "\n")
	}
	return strings.Join(watch.BuildContext(p, cfg.Questions.Context, w).Answer, "\n")
}

func (a *app) stillRunning(t target, all []registry.Worker) string {
	var parts []string
	for _, w := range all {
		if (t.chain == "" && w.ID == t.id) || (t.chain != "" && w.Chain == t.chain && w.Active()) {
			parts = append(parts, fmt.Sprintf("%s (%s)", w.Label(), a.age(w)))
		}
	}
	return fmt.Sprintf("still running: %s. Run: routr wait %s", strings.Join(parts, ", "), t)
}

// age is how long a running worker has run, else its state.
func (a *app) age(w registry.Worker) string {
	if w.State == registry.Blocked && w.Answering() == "jev" {
		return "Jev decides its question"
	}
	if w.State != registry.Running || w.Started.IsZero() {
		if w.TurnsPending() {
			return "follow-up pending"
		}
		return string(w.State)
	}
	return a.now().Sub(w.Started).Truncate(time.Second).String()
}

// resultText is what `wait` and `result` print for a worker with a result: its
// header, if its state calls for one (a failed worker's error and next
// step), then its body.
func (a *app) resultText(p registry.Paths, w registry.Worker) string {
	var parts []string
	if h := strings.TrimRight(a.header(p, w), "\n"); h != "" {
		parts = append(parts, h)
	}
	if b := a.body(p, w); b != "" {
		parts = append(parts, b)
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// hasResult reports whether cur (Paths.Current) holds a final message or a
// reply of this run.
func hasResult(cur registry.Result) bool { return cur.FinalPath != "" || cur.Turn > 0 }

// body is w's current result (Paths.Current): its final message (or its
// latest follow-up reply, if newer), output, verdict and worktree.
func (a *app) body(p registry.Paths, w registry.Worker) string {
	var b strings.Builder
	cur := p.Current(w)
	body := cur.Final
	if cur.Turn > 0 {
		body = fmt.Sprintf("%s:\n%s", a.turnName(p, w, cur.Turn), cur.Reply)
	}
	b.WriteString(strings.TrimSpace(body))
	if w.Output != "" {
		fmt.Fprintf(&b, "\noutput: %s", a.rel(w.Output, w.Cwd))
	}
	if w.Verdict != "" {
		fmt.Fprintf(&b, "\nverdict: %s", w.Verdict)
	}
	if t := treeText(a, w); t != "" {
		fmt.Fprintf(&b, "\nworktree: %s", t)
	}
	return strings.TrimLeft(b.String(), "\n")
}

// turnName names w's follow-up turn: a `routr send` follow-up, or a review
// loop's revision (Revisions) for the reviewer in its chain whose on_fail
// resumes w.
func (a *app) turnName(p registry.Paths, w registry.Worker, turn int) string {
	i := slices.Index(w.Revisions, turn)
	if i < 0 {
		return fmt.Sprintf("reply to follow-up %d", turn)
	}
	var chain []registry.Worker
	if w.Chain != "" {
		chain, _ = p.Store().Chain(w.Chain) // unreadable: the label goes without its reviewer
	}
	for _, x := range chain {
		if x.OnFail != nil && x.OnFail.Resume == w.ID {
			return fmt.Sprintf("revision for %s (on_fail round %d)", x.Label(), i+1)
		}
	}
	return fmt.Sprintf("revision (on_fail round %d)", i+1)
}

// rel shows path relative to the caller's cwd when it is inside it.
func (a *app) rel(path, base string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	if r, err := filepath.Rel(a.cwd, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// view is a worker as --json prints it. Env is the names of its env only:
// a value may be a secret (an API key in config [env]).
type view struct {
	registry.Worker
	Env       []string `json:"env,omitempty"`
	RunDir    string   `json:"run_dir"`
	Final     string   `json:"final,omitempty"`      // the text of this run's final message
	FinalPath string   `json:"final_path,omitempty"` // this run's final message, once it exists
	Reply     string   `json:"reply,omitempty"`      // the latest follow-up reply, if it is this run's and newer than the final
	Halts     bool     `json:"halts,omitempty"`      // a fail without on_fail: what comes after it waits for routr approve
}

func (a *app) view(p registry.Paths, w registry.Worker) view {
	cur := p.Current(w)
	v := a.pathView(p, w, cur)
	v.Final, v.Reply = cur.Final, cur.Reply
	return v
}

// pathView is view without the texts, only where the final message of
// cur (Paths.Current) is: what `list --json` prints unless --final, since a
// front end polls it.
func (a *app) pathView(p registry.Paths, w registry.Worker, cur registry.Result) view {
	return view{Worker: w, Env: slices.Sorted(maps.Keys(w.Env)), RunDir: p.RunDir(w.ID), FinalPath: cur.FinalPath, Halts: w.Halts()}
}

func (a *app) printViews(p registry.Paths, t target, all []registry.Worker) error {
	var vs []view
	for _, w := range all {
		if (t.chain == "" && w.ID == t.id) || (t.chain != "" && w.Chain == t.chain) {
			vs = append(vs, a.view(p, w))
		}
	}
	if t.chain == "" && len(vs) == 1 {
		return a.printJSON(vs[0])
	}
	return a.printJSON(vs)
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
