package watch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/jev"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// Judge steps (skills/routr/flows.md, "Judge steps"): a worker with Judge set asks
// Jev instead of a harness. Its state is the results of the workers it
// comes after (each foreach copy on its own) and the files its read names;
// its question is its rendered brief, with pass and fail as the options, or
// with a pick, one option per upstream agent worker: run steps and judges
// among them are context (a check on an attempt), not candidates. It is admitted as a
// read-only worker is, then ask builds the state and calls Jev in a
// goroutine, a job as a run step's command is: tendRuns records the answer
// on the next tick (judged), and a watcher that stopped mid-call leaves it
// to the next, which asks again.
//
// Jev never passes on its own account: no key, an error after the client's
// retries, or a choice below min_confidence needs you. routr approve then
// takes Jev's last choice as it is (Judge.Accept), or asks again if it had
// none.

// Decider is a Jev call; *jev.Client satisfies it.
type Decider interface {
	Decide(ctx context.Context, state any, questions map[string]jev.Choice) (jev.Decision, error)
}

// JevKeyEnv names the OpenRouter key a judge asks Jev with.
const JevKeyEnv = "OPENROUTER_API_KEY"

// Judge step limits.
var (
	jevBudget  = 80_000          // bytes of file text in a state, split fairly between the files; Jev reads ~32K tokens
	jevSummary = 300             // bytes of a worker's summary in a pick option
	jevCommand = 2000            // bytes of a run step's command in a state
	jevTimeout = 3 * time.Minute // for one decision, the client's retries included
)

// What the options mean to Jev when the step does not say.
const (
	defaultPass = "the work holds up: the chain can go on as it is"
	defaultFail = "the work does not hold up: it must be revised first"
)

var errNoKey = errors.New("no Jev key: " + JevKeyEnv + " is not set where the watcher runs (export it, or set it in config [env])")

// JudgeState is the state a judge sends Jev, and writes to state.json.
type JudgeState struct {
	Judge    JudgeInfo  `json:"judge"`
	Upstream []Upstream `json:"upstream"`        // the results it judges, in worker id order
	Empty    []string   `json:"empty,omitempty"` // foreach steps it judges that matched no files (allow_empty): they have no results
	Files    []File     `json:"files,omitempty"` // what its read names
}

// JudgeInfo says which judge asks.
type JudgeInfo struct {
	Chain string `json:"chain"`
	Step  string `json:"step,omitempty"`
	Mode  string `json:"mode"`  // "verdict" or "pick"
	Round int    `json:"round"` // from 1; a loop asks again a round up
}

// Upstream is one worker's result.
type Upstream struct {
	ID         string   `json:"id"` // <step>-<worker id>; in a pick, an agent worker's is its option
	Worker     int      `json:"worker"`
	Step       string   `json:"step,omitempty"`
	Item       string   `json:"item,omitempty"` // a foreach copy's file
	Role       string   `json:"role"`
	Harness    string   `json:"harness,omitempty"` // harness · model · effort; sh for a run step
	After      []string `json:"after,omitempty"`   // the ids of the entries it came after: what a check checked
	Command    string   `json:"command,omitempty"` // a run step: its command as run
	Verdict    string   `json:"verdict,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	OutputPath string   `json:"output_path,omitempty"` // relative to the judge's cwd when inside it
	Output     string   `json:"output,omitempty"`      // the output file's text, cut to fit
	Final      string   `json:"final,omitempty"`       // its final.md, cut to fit
}

// File is a file's text as Jev gets it: one a judge's read names, or a
// question's project doc.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// jevRecord is jev.json: what was asked and what Jev said, or why it said
// nothing.
type jevRecord struct {
	Question      string            `json:"question"`
	Instructions  string            `json:"instructions"`
	Options       map[string]string `json:"options"`
	MinConfidence float64           `json:"min_confidence"`
	Decision      *jev.Decision     `json:"decision,omitempty"`
	Error         string            `json:"error,omitempty"`
	Accepted      bool              `json:"accepted,omitempty"` // you took a choice below min_confidence (routr approve)
	Only          bool              `json:"only,omitempty"`     // one candidate: it won without asking Jev
}

// option is one choice a judge offers Jev.
type option struct {
	key  string
	desc string
	wk   registry.Worker // a pick's candidate
}

// ask is a judge's Jev call in flight.
type ask struct {
	at     time.Time // the Prompted it was started for
	cancel context.CancelFunc
	done   chan struct{} // closed when what follows is set
	rec    jevRecord
	opts   []option
	ups    []registry.Worker
	err    error
}

func (a *ask) ended() <-chan struct{} { return a.done }
func (a *ask) stop() {
	if a.cancel != nil {
		a.cancel()
	}
}
func (a *ask) owns(wk registry.Worker) bool {
	return wk.State == registry.Running && wk.Judge != nil && wk.Prompted.Equal(a.at)
}
func (a *ask) record(_ context.Context, w *Watcher, wk registry.Worker) { w.judged(wk, a) }

// ask starts a judge admitted to starting: renders its brief and state,
// writes them as brief.md and state.json, and asks Jev in a goroutine (or,
// approved by you, takes the choice in jev.json).
func (w *Watcher) ask(ctx context.Context, wk registry.Worker, all []registry.Worker) {
	w.makeWork(wk, wk.Brief)
	brief, err := flow.Render(wk.Brief, Refs(wk, all, w.Paths, wk.Cwd)) // Jev opens no files: paths as in its state
	if err != nil {
		w.fail(ctx, wk, fmt.Sprintf("its brief has bad refs: %v. routr cancel %d, then start it again with a brief that fits", err, wk.ID))
		return
	}
	state, ups, err := w.judgeState(wk, all)
	if err != nil {
		w.fail(ctx, wk, err.Error())
		return
	}
	dir := w.runDir(wk)
	w.writeRun(wk, filepath.Join(dir, registry.BriefFile), strings.TrimSpace(brief)+"\n")
	if b, err := json.MarshalIndent(state, "", "  "); err == nil {
		w.writeRun(wk, filepath.Join(dir, registry.StateFile), string(b)+"\n")
	}
	j := wk.Judge
	a := &ask{at: w.now().Round(0), done: make(chan struct{}), ups: ups, opts: options(wk, ups, state)}
	a.rec = jevRecord{Question: "verdict", Instructions: strings.TrimSpace(brief), Options: map[string]string{},
		MinConfidence: cmp.Or(j.MinConfidence, registry.DefaultMinConfidence)}
	if j.Pick {
		a.rec.Question = "pick"
	}
	for _, o := range a.opts {
		a.rec.Options[o.key] = o.desc
	}
	note := fmt.Sprintf("asking Jev about %s", count(len(ups), "upstream result"))
	if j.Pick {
		note = fmt.Sprintf("asking Jev to pick one of %s", count(len(a.opts), "upstream result"))
	}
	switch dec := w.decider(); {
	case j.Accept && a.load(dir):
		note = "taking Jev's last choice, as you approved"
		close(a.done)
	case len(a.opts) == 1:
		a.rec.Only = true
		a.rec.Decision = &jev.Decision{Answers: map[string]jev.Answer{a.rec.Question: {
			Choice: a.opts[0].key, Probabilities: map[string]float64{a.opts[0].key: 1}, Confidence: 1}}}
		close(a.done)
	case dec == nil:
		a.err = errNoKey
		close(a.done)
	default:
		cctx, cancel := context.WithTimeout(ctx, jevTimeout)
		a.cancel = cancel
		q := map[string]jev.Choice{a.rec.Question: {Instructions: a.rec.Instructions, Options: maps.Clone(a.rec.Options)}}
		go func() {
			defer cancel()
			d, err := dec.Decide(cctx, state, q)
			a.rec.Decision, a.err = &d, err
			if err != nil {
				a.rec.Decision = nil
			}
			close(a.done)
		}()
	}
	w.mu.Lock()
	w.jobs[wk.ID] = a
	w.mu.Unlock()
	if _, ok := w.starting(wk, func(x *registry.Worker) {
		x.State, x.Prompted, x.Note = registry.Running, a.at, note
		if x.Started.IsZero() {
			x.Started = a.at
		}
	}); !ok { // cancelled while it started
		a.stop()
		return
	}
	w.Log.Printf("%s: %s", wk.Label(), note)
}

// decider is the Jev to ask: Watcher.Jev, or a client with the key from
// config [env] or the watcher's environment; nil without a key.
func (w *Watcher) decider() Decider {
	if w.Jev != nil {
		return w.Jev
	}
	w.mu.Lock()
	key := w.tabEnv[JevKeyEnv]
	w.mu.Unlock()
	if key = cmp.Or(key, os.Getenv(JevKeyEnv)); key == "" {
		return nil
	}
	return &jev.Client{APIKey: key}
}

// load takes the decision in dir's jev.json, which you approved; false if
// it holds none (Jev failed, or there was no key).
func (a *ask) load(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, registry.JevFile))
	if err != nil {
		return false
	}
	var rec jevRecord
	if json.Unmarshal(b, &rec) != nil || rec.Decision == nil || rec.Question != a.rec.Question {
		return false
	}
	if _, ok := rec.Decision.Answers[rec.Question]; !ok {
		return false
	}
	a.rec.Decision, a.rec.Accepted = rec.Decision, true
	return true
}

// upstream are the workers judge wk judges, in id order: those it was
// queued after (judgedIDs), each foreach copy on its own.
func upstream(wk registry.Worker, m map[int]registry.Worker) []registry.Worker {
	var out []registry.Worker
	seen := map[int]bool{}
	add := func(id int) {
		if x, ok := m[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, x)
		}
	}
	for _, id := range judgedIDs(wk) {
		if x, ok := m[id]; ok && x.Fanout != nil {
			for _, c := range x.Fanout.Items {
				add(c)
			}
			continue
		}
		add(id)
	}
	slices.SortFunc(out, func(a, b registry.Worker) int { return a.ID - b.ID })
	return out
}

// judgedIDs are the ids of the workers judge wk judges: Judge.From, and for
// a verdict, from round 2 on, the worker its own loop sent back to revise.
func judgedIDs(wk registry.Worker) []int {
	ids := slices.Clone(wk.Judge.From)
	if !wk.Judge.Pick && wk.OnFail != nil && wk.Round >= 2 {
		ids = append(ids, wk.OnFail.Resume)
	}
	return ids
}

// emptied are the steps of the foreach workers judge wk judges that
// matched no files (allow_empty), so upstream has none of their results.
func emptied(wk registry.Worker, m map[int]registry.Worker) []string {
	var out []string
	for _, id := range judgedIDs(wk) {
		if x, ok := m[id]; ok && x.Empty() && !slices.Contains(out, x.Step) {
			out = append(out, x.Step)
		}
	}
	return out
}

// cameAfter are the workers u came after, a runtime foreach worker's copies in
// its place.
func cameAfter(u registry.Worker, m map[int]registry.Worker) []int {
	var out []int
	for _, id := range u.After {
		if x, ok := m[id]; ok && x.Fanout != nil {
			out = append(out, x.Fanout.Items...)
			continue
		}
		out = append(out, id)
	}
	return out
}

// candidate reports whether a pick may choose u: an agent worker, not a
// run step or a judge, which are context.
func candidate(u registry.Worker) bool { return !u.Tabless() && u.Fanout == nil }

func optionKey(u registry.Worker) string {
	return fmt.Sprintf("%s-%d", cmp.Or(u.Step, "worker"), u.ID)
}

// judgeState builds what wk sends Jev, with the text of every file cut so
// that all of it fits in jevBudget.
func (w *Watcher) judgeState(wk registry.Worker, all []registry.Worker) (JudgeState, []registry.Worker, error) {
	m := byID(all)
	ups, empty := upstream(wk, m), emptied(wk, m)
	switch {
	case len(ups) == 0 && wk.Judge.Pick && len(empty) > 0:
		return JudgeState{}, nil, fmt.Errorf("it has no upstream worker to pick from: %s matched no files. routr cancel %d", strings.Join(empty, ", "), wk.ID)
	case len(ups) == 0 && len(empty) == 0:
		return JudgeState{}, nil, fmt.Errorf("it has no upstream worker to judge. routr cancel %d", wk.ID)
	case wk.Judge.Pick && !slices.ContainsFunc(ups, candidate):
		return JudgeState{}, nil, fmt.Errorf("it has no upstream agent worker to pick from, only run steps and judges. routr cancel %d", wk.ID)
	}
	mode := "verdict"
	if wk.Judge.Pick {
		mode = "pick"
	}
	st := JudgeState{Judge: JudgeInfo{Chain: wk.ChainRef(), Step: wk.Step, Mode: mode, Round: max(wk.Round, 1)}, Empty: empty}
	var texts []*blob
	var slots []*string
	take := func(b *blob, to *string) {
		texts, slots = append(texts, b), append(slots, to)
	}
	in := map[int]string{} // the entries, by worker id
	for _, u := range ups {
		in[u.ID] = optionKey(u)
	}
	st.Upstream = make([]Upstream, len(ups))
	for i, u := range ups {
		x := &st.Upstream[i]
		*x = Upstream{ID: optionKey(u), Worker: u.ID, Step: u.Step, Role: u.Role,
			Harness: u.Tag(), Verdict: string(u.Verdict), Summary: u.Summary}
		if it := flow.ItemOf(u); it != nil {
			x.Item = it.Name
		}
		for _, a := range cameAfter(u, m) {
			if k, ok := in[a]; ok {
				x.After = append(x.After, k)
			}
		}
		if u.Run != "" {
			b, _ := os.ReadFile(filepath.Join(w.Paths.RunDir(u.ID), registry.BriefFile))
			x.Command = cutEnd(strings.TrimSpace(string(b)), jevCommand)
		}
		if u.Output != "" {
			out := absIn(u.Cwd, u.Output)
			x.OutputPath = relTo(wk.Cwd, out)
			take(readBlob(out), &x.Output)
		}
		if cur := w.Paths.Current(u); cur.FinalPath != "" {
			take(textBlob(cur.Final), &x.Final)
		} else {
			take(&blob{head: "[routr: it has no final message in this run]"}, &x.Final)
		}
	}
	refs := Refs(wk, all, w.Paths, wk.Cwd)
	st.Files = make([]File, len(wk.Judge.Read))
	for i, r := range wk.Judge.Read {
		path, err := flow.Render(r, refs)
		if err != nil {
			return JudgeState{}, nil, fmt.Errorf("its read has bad refs: %v. routr cancel %d, then start it again", err, wk.ID)
		}
		abs := absIn(wk.Cwd, path)
		if !under(w.Paths.Root(), abs) && !under(wk.Worktree(), abs) {
			return JudgeState{}, nil, fmt.Errorf("read %s lies outside the repo; a judge reads only files inside it. routr cancel %d", path, wk.ID)
		}
		st.Files[i].Path = path
		take(readBlob(abs), &st.Files[i].Content)
	}
	for i, n := range fair(texts, jevBudget) {
		*slots[i] = texts[i].cut(n)
	}
	return st, ups, nil
}

// options are the choices wk offers Jev: pass and fail, or one per upstream
// agent worker (candidate).
func options(wk registry.Worker, ups []registry.Worker, st JudgeState) []option {
	j := wk.Judge
	if !j.Pick {
		return []option{{key: "pass", desc: cmp.Or(j.Pass, defaultPass)}, {key: "fail", desc: cmp.Or(j.Fail, defaultFail)}}
	}
	var opts []option
	for i, u := range ups {
		if !candidate(u) {
			continue
		}
		what := "worker " + fmt.Sprint(u.ID)
		if u.Step != "" {
			what += ", step " + u.Step
		}
		if it := flow.ItemOf(u); it != nil {
			what += ", item " + it.Name
		}
		if t := u.Tag(); t != "" {
			what += " (" + u.Role + " on " + t + ")"
		} else {
			what += " (" + u.Role + ")"
		}
		if s := strings.TrimSpace(u.Summary); s != "" {
			what += ": " + cutEnd(s, jevSummary)
		}
		opts = append(opts, option{key: st.Upstream[i].ID, wk: u,
			desc: fmt.Sprintf("the result of %s. It is the upstream entry with id %s.", what, st.Upstream[i].ID)})
	}
	return opts
}

// judged records a judge's answer: jev.json, final.md, and then the
// verdict (a pick: pass, and the winner's output as its own) and on_fail,
// as for a review step; a fail without on_fail halts the steps after it, as
// any step's does. No answer, or one below min_confidence, needs you.
func (w *Watcher) judged(wk registry.Worker, a *ask) {
	if !a.owns(wk) {
		return
	}
	dir := w.runDir(wk)
	ans, err := a.answer()
	if err != nil {
		a.rec.Error = err.Error()
	}
	if b, err := json.MarshalIndent(a.rec, "", "  "); err == nil {
		w.writeRun(wk, filepath.Join(dir, registry.JevFile), string(b)+"\n")
	}
	low := err == nil && !a.rec.Accepted && ans.Confidence < a.rec.MinConfidence
	var win option
	for _, o := range a.opts {
		if o.key == ans.Choice {
			win = o
		}
	}
	winOut := ""
	if wk.Judge.Pick && err == nil {
		winOut = absIn(win.wk.Cwd, win.wk.Output)
		if winOut == "" {
			winOut = w.Paths.FinalPath(win.wk)
		}
	}
	w.writeRun(wk, w.Paths.FinalPath(wk), a.final(w, wk, ans, err, low, winOut))
	now := w.now()
	var me registry.Worker
	var said string // its summary as it finished: a review loop starts it over
	txErr := w.store.Tx(func(m map[int]*registry.Worker) error {
		x := m[wk.ID]
		if x == nil || !a.owns(*x) {
			return errStale
		}
		x.Judge.Accept, x.Error = false, ""
		switch {
		case err != nil:
			x.State, x.Verdict, x.Summary = registry.NeedsYou, "", ""
			x.Note = fmt.Sprintf("Jev could not decide (%v), so nothing passed. routr approve %d asks again; routr cancel %s stops here", err, x.ID, cancelRef(*x))
		case low:
			x.State, x.Verdict = registry.NeedsYou, ""
			x.Summary = fmt.Sprintf("Jev is unsure: %s", choiceLine(ans, a.rec.Question))
			x.Note = fmt.Sprintf("Jev is unsure: it chose %s (%s), confidence %.2f, below min_confidence %.2g. routr approve %d takes its choice; to decide otherwise, routr cancel %s and go on by hand",
				ans.Choice, probs(ans), ans.Confidence, a.rec.MinConfidence, x.ID, cancelRef(*x))
		default:
			x.State, x.Finished, x.Note = registry.Done, now, ""
			x.Verdict, x.Summary = registry.Verdict(ans.Choice), "Jev: "+choiceLine(ans, a.rec.Question)
			if x.Judge.Pick {
				x.Verdict, x.Output, x.Judge.Winner = registry.Pass, winOut, win.wk.ID
			}
			said = x.Summary
			w.loop(x, m)
		}
		me = *x
		return nil
	})
	if txErr != nil {
		if !errors.Is(txErr, errStale) {
			w.Log.Printf("%s: %v", wk.Label(), txErr)
		}
		return
	}
	w.Log.Printf("%s: %s: %s", wk.Label(), me.State, first(me.Summary, said, a.rec.Error))
}

// answer is Jev's answer to the judge's question, checked against the
// options it was given.
func (a *ask) answer() (jev.Answer, error) {
	if a.err != nil {
		return jev.Answer{}, a.err
	}
	if a.rec.Decision == nil {
		return jev.Answer{}, errors.New("jev: no decision")
	}
	ans, ok := a.rec.Decision.Answers[a.rec.Question]
	if !ok {
		return jev.Answer{}, fmt.Errorf("jev: no answer to %q", a.rec.Question)
	}
	if _, ok := a.rec.Options[ans.Choice]; !ok {
		return jev.Answer{}, fmt.Errorf("jev chose %q, which is no option (%s)", ans.Choice, strings.Join(slices.Sorted(maps.Keys(a.rec.Options)), ", "))
	}
	return ans, nil
}

// final is a judge's final.md: what Jev chose, with every option's
// probability, what it judged and what it cost.
func (a *ask) final(w *Watcher, wk registry.Worker, ans jev.Answer, err error, low bool, winOut string) string {
	var b strings.Builder
	switch {
	case err != nil:
		fmt.Fprintf(&b, "Jev could not decide: %v\n", err)
	case low:
		fmt.Fprintf(&b, "Jev is unsure: %s, below min_confidence %.2g; routr approve %d takes its choice\n", choiceLine(ans, a.rec.Question), a.rec.MinConfidence, wk.ID)
	case a.rec.Only:
		fmt.Fprintf(&b, "Picked %s, the only candidate, without asking Jev\n", ans.Choice)
	case a.rec.Accepted:
		fmt.Fprintf(&b, "Jev: %s; below min_confidence %.2g, taken as you approved\n", choiceLine(ans, a.rec.Question), a.rec.MinConfidence)
	default:
		fmt.Fprintf(&b, "Jev: %s\n", choiceLine(ans, a.rec.Question))
	}
	if winOut != "" {
		for _, o := range a.opts {
			if o.key == ans.Choice {
				fmt.Fprintf(&b, "winner: %s, worker %d, output %s\n", o.key, o.wk.ID, relTo(wk.Cwd, winOut))
			}
		}
	}
	if err == nil {
		b.WriteString("\n")
		keys := slices.SortedFunc(maps.Keys(a.rec.Options), func(x, y string) int {
			return cmp.Or(cmp.Compare(ans.Probabilities[y], ans.Probabilities[x]), cmp.Compare(x, y))
		})
		for _, k := range keys {
			fmt.Fprintf(&b, "  %-12s %.2f  %s\n", k, ans.Probabilities[k], firstLine(a.rec.Options[k]))
		}
	}
	fmt.Fprintf(&b, "\nasked: %s\n", firstLine(a.rec.Instructions))
	judged := make([]string, len(a.ups))
	for i, u := range a.ups {
		judged[i] = u.Label()
		if u.Output != "" {
			judged[i] += " (" + relTo(wk.Cwd, absIn(u.Cwd, u.Output)) + ")"
		}
	}
	fmt.Fprintf(&b, "judged: %s\n", strings.Join(judged, "; "))
	if d := a.rec.Decision; d != nil && d.ID != "" {
		fmt.Fprintf(&b, "decision %s (%s), $%.8f, %d input tokens\n", d.ID, d.Model, d.Usage.Cost, d.Usage.InputTokens)
	}
	dir := relTo(wk.Cwd, w.runDir(wk))
	fmt.Fprintf(&b, "the state Jev saw: %s; its answer: %s\n", filepath.Join(dir, registry.StateFile), filepath.Join(dir, registry.JevFile))
	return b.String()
}

// choiceLine is "fail (p 0.83, confidence 0.83)", or for a pick "picked
// attempt-4 (…)".
func choiceLine(ans jev.Answer, question string) string {
	s := fmt.Sprintf("%s (p %.2f, confidence %.2f)", ans.Choice, ans.Probabilities[ans.Choice], ans.Confidence)
	if question == "pick" {
		return "picked " + s
	}
	return s
}

// probs is "fail 0.55, pass 0.45", most likely first.
func probs(ans jev.Answer) string {
	keys := slices.SortedFunc(maps.Keys(ans.Probabilities), func(x, y string) int {
		return cmp.Or(cmp.Compare(ans.Probabilities[y], ans.Probabilities[x]), cmp.Compare(x, y))
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %.2f", k, ans.Probabilities[k])
	}
	return strings.Join(parts, ", ")
}

// cancelRef is what routr cancel takes to stop wk's chain: its chain, or
// outside one its id.
func cancelRef(wk registry.Worker) string {
	return cmp.Or(wk.Chain, fmt.Sprint(wk.ID))
}

// under reports whether path is dir or inside it.
func under(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// blob is a file's text for a state: all of it, or its first and last
// jevBudget/2 bytes when it is longer.
type blob struct {
	head, tail string
	size       int // of the whole file
}

// readBlob reads path into a blob; one it can't read says why.
func readBlob(path string) *blob {
	f, err := os.Open(path)
	if err != nil {
		return &blob{head: fmt.Sprintf("[routr: could not read it: %v]", err)}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return &blob{head: fmt.Sprintf("[routr: could not read it: %s is a directory or unreadable]", path)}
	}
	half := int64(jevBudget / 2)
	size := fi.Size()
	if size <= 2*half {
		b, err := io.ReadAll(f)
		if err != nil {
			return &blob{head: fmt.Sprintf("[routr: could not read it: %v]", err)}
		}
		return textBlob(string(b))
	}
	head, tail := make([]byte, half), make([]byte, half)
	if _, err := io.ReadFull(f, head); err != nil {
		return &blob{head: fmt.Sprintf("[routr: could not read it: %v]", err)}
	}
	if _, err := f.ReadAt(tail, size-half); err != nil {
		return &blob{head: fmt.Sprintf("[routr: could not read it: %v]", err)}
	}
	return &blob{head: string(head), tail: string(tail), size: int(size)}
}

// textBlob is all of s, split in the middle on a rune boundary.
func textBlob(s string) *blob {
	n := len(s) / 2
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return &blob{head: s[:n], tail: s[n:], size: len(s)}
}

func (b *blob) len() int { return len(b.head) + len(b.tail) }

// cut is b's text in at most n bytes (plus a marker): its head and tail,
// with a line saying how much was cut between them, on rune boundaries.
func (b *blob) cut(n int) string {
	if b.len() <= n && b.size <= b.len() {
		return b.head + b.tail
	}
	h := cutEnd(b.head, n/2)
	t := cutStart(b.tail, n-len(h))
	return h + fmt.Sprintf("\n[routr: %d bytes cut here]\n", b.size-len(h)-len(t)) + t
}

// cutEnd is the longest prefix of s of at most n bytes that ends on a rune
// boundary.
func cutEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutStart is the longest suffix of s of at most n bytes that starts on a
// rune boundary.
func cutStart(s string, n int) string {
	i := max(len(s)-n, 0)
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// fair splits budget between blobs: each gets what it has, or an equal
// share of what is left when it has more (max-min fairness), so short
// files go whole and long ones are cut alike.
func fair(blobs []*blob, budget int) []int {
	order := make([]int, len(blobs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(x, y int) int { return blobs[x].len() - blobs[y].len() })
	out := make([]int, len(blobs))
	left := budget
	for k, i := range order {
		share := left / (len(order) - k)
		out[i] = min(blobs[i].len(), share)
		left -= out[i]
	}
	return out
}
