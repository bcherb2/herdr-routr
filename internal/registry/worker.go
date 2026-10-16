package registry

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Label names the worker in tabs and messages: "3 review · codex", or with
// a title "3 fix-login-race · codex"; a run step "5 check · run", a judge
// "6 ship · jev".
func (w Worker) Label() string {
	if w.Tabless() {
		kind := "run"
		if w.Judge != nil {
			kind = "jev"
		}
		if name := cmp.Or(w.Title, w.Step); name != "" {
			return strconv.Itoa(w.ID) + " " + name + " · " + kind
		}
		return strconv.Itoa(w.ID) + " " + kind
	}
	name := w.Title
	if name == "" {
		name = w.Role
	}
	s := strconv.Itoa(w.ID) + " " + name
	if w.Harness != "" {
		s += " · " + w.Harness
	}
	return s
}

// TitleMax bounds a title, so a tab label stays short enough to read in
// herdr's tab bar.
const TitleMax = 20

// Title turns s into a worker title: lower case, each run of anything but
// letters and digits one "-", at most TitleMax characters. A longer one is
// cut and ends in "~", so a cut is always routr's and visible, never herdr's.
func Title(s string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(s) {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
		gap = false
	}
	t := []rune(b.String())
	if len(t) <= TitleMax {
		return string(t)
	}
	return strings.TrimSuffix(string(t[:TitleMax-1]), "-") + "~"
}

// Tag is harness · model · effort, skipping what is unset; "sh" for a run
// step, "jev" for a judge.
func (w Worker) Tag() string {
	switch {
	case w.Run != "":
		return "sh"
	case w.Judge != nil:
		return "jev"
	}
	var parts []string
	for _, p := range []string{w.Harness, w.Model, w.Effort} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}

// Tabless reports whether the watcher does the worker's work itself, with
// no tab or session: a run step's command, a judge's Jev call.
func (w Worker) Tabless() bool { return w.Run != "" || w.Judge != nil }

// Succeeded reports whether the worker finished its work: done, or closed
// after it was done. Steps that come after it may start.
func (w Worker) Succeeded() bool {
	return w.State == Done || (w.State == Closed && !w.Finished.IsZero())
}

// Halts reports whether the worker stops the steps after it: whatever kind
// of step it is, it finished with a fail verdict and no on_fail, and you
// have not approved past it (GoOn). It is done, its fail the answer, but
// the steps after it do not start until routr approve.
func (w Worker) Halts() bool {
	return w.Succeeded() && w.Verdict == Fail && w.OnFail == nil && !w.GoOn
}

// NewRun starts w over in its session: what its last run concluded becomes
// history. Its next run writes a final message of its own (Runs, so the
// last one stays where it was, Paths.PrevFinal), the replies so far are an
// earlier run's (RunFrom), and its verdict, summary, finish time, approval
// past a halt and error are cleared; a run or judge step's output and a
// judge's pick and approval too, as it decides them again. So what is
// current (Paths.Current) is always this run's: routr retry, and a review
// loop for its target, its reviewer and the checks it runs again. It
// touches no file.
func (w *Worker) NewRun() {
	w.Runs++
	w.RunFrom = w.Turn
	w.Verdict, w.Summary, w.Finished, w.GoOn, w.Error = "", "", time.Time{}, false, ""
	if w.Tabless() {
		w.Output = ""
		if w.Judge != nil {
			w.Judge.Winner, w.Judge.Accept = 0, false
		}
	}
}

// Stuck reports whether the worker ended without finishing: steps after it
// cannot start until you step in.
func (w Worker) Stuck() bool {
	return w.State.Terminal() && !w.Succeeded()
}

// Ancestors are w's After, transitively, nearest first: every worker w
// comes after, directly or through others, once each. Ids not in m are
// left out.
func Ancestors(w Worker, m map[int]Worker) []Worker {
	var out []Worker
	seen := map[int]bool{w.ID: true}
	queue := slices.Clone(w.After)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if up, ok := m[id]; ok {
			out = append(out, up)
			queue = append(queue, up.After...)
		}
	}
	return out
}

// Queued reports whether the worker has not started yet.
func (w Worker) Queued() bool {
	switch w.State {
	case Pending, Waiting, Gated:
		return true
	}
	return false
}

// Active reports whether the worker still has work in hand: not terminal,
// or a follow-up is queued or in flight.
func (w Worker) Active() bool {
	return !w.State.Terminal() || w.TurnsPending()
}

// TurnsPending reports whether a follow-up is queued or awaiting its reply.
func (w Worker) TurnsPending() bool { return len(w.Inbox) > 0 || w.Turn > w.Replies }

// ClearQuestion drops the question w was blocked on, with its menu, the
// tool call it was about and who Jev handed it to.
func (w *Worker) ClearQuestion() {
	w.Question, w.Options, w.Fingerprint, w.BlockSeq, w.Pending, w.HandedTo = "", nil, "", 0, nil, ""
}

// Answering is who answers the question w is blocked on: its Answerer
// (human when unset), or, for jev, whoever Jev handed the question to;
// "jev" while Jev decides.
func (w Worker) Answering() string {
	if w.Answerer == "jev" && w.HandedTo != "" {
		return w.HandedTo
	}
	return cmp.Or(w.Answerer, "human")
}

// MaybeDoneLock is how long a write worker that went idle without `routr
// done` keeps the directory lock. It may still go on working, but a worker
// that forgot `routr done` must not stall every writer in its tree.
const MaybeDoneLock = 2 * time.Minute

// LocksDir reports whether the worker should hold the directory lock at
// now: a write worker that is starting, working or waiting on a question,
// or went idle without `routr done` (maybe_done) less than MaybeDoneLock
// ago.
func (w Worker) LocksDir(now time.Time) bool {
	if !w.Write {
		return false
	}
	switch w.State {
	case Starting, Running, Blocked:
		return true
	case MaybeDone:
		return !w.Idle.IsZero() && now.Sub(w.Idle) < MaybeDoneLock
	}
	return false
}

// ChainRef is the chain w's {{chain}} and {{work}} name: its chain, or
// outside one "c<ID>", the name a pipe from it gives the chain it starts.
func (w Worker) ChainRef() string {
	if w.Chain != "" {
		return w.Chain
	}
	return "c" + strconv.Itoa(w.ID)
}

// Worktree is the directory lock's key: the worktree top of Cwd.
func (w Worker) Worktree() string {
	if w.Tree != "" {
		return w.Tree
	}
	return w.Cwd
}

// ReservedEnv are the variables routr sets in every worker's tab and run
// step, which a worker's env may not set.
var ReservedEnv = []string{"ROUTR_ID", "ROUTR_RUN_DIR", "ROUTR_BIN", "ROUTR_CWD", StateDirEnv}

// CheckEnv rejects a worker env (a flow step's env, --env) that sets one of
// ReservedEnv, or a name no shell exports.
func CheckEnv(env map[string]string) error {
	var bad []string
	for k := range env {
		switch {
		case slices.Contains(ReservedEnv, k):
			bad = append(bad, k+" is routr's own")
		case !envName(k):
			bad = append(bad, fmt.Sprintf("%q is no variable name", k))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	slices.Sort(bad)
	return errors.New(strings.Join(bad, "; "))
}

func envName(k string) bool {
	if k == "" || unicode.IsDigit(rune(k[0])) {
		return false
	}
	for _, r := range k {
		if r != '_' && (r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// managedArgs are the harness args routr sets itself to start, resume or
// drive a session, per harness; "" holds the ones no harness takes for
// anything else, checked for every harness and where it is not known yet.
var managedArgs = map[string][]string{
	"":       {"--session-id", "--session", "--resume", "--continue", "--print"},
	"claude": {"-r", "-c", "-p"},
	"codex":  {"resume"},
	"pi":     {"-r", "-c", "-p"},
}

// CheckArgs rejects extra harness args (a flow step's args, --arg) that
// routr manages for harness ("" = not known yet): the session flags it
// starts and resumes with, print mode, Codex's resume.
func CheckArgs(harness string, args []string) error {
	managed := slices.Concat(managedArgs[""], managedArgs[harness])
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if slices.Contains(managed, name) {
			return fmt.Errorf("%s is routr's: it starts and resumes the session itself", name)
		}
	}
	return nil
}
