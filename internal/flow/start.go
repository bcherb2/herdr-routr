package flow

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// Creator adds workers to the registry. Create assigns the ID.
type Creator interface {
	Create(w registry.Worker) (registry.Worker, error)
	Update(id int, fn func(*registry.Worker)) (registry.Worker, error)
}

// A fan-out copy carries its item in Params under these keys. A declared
// param cannot contain a dot, so they never clash with one; RefsFor moves
// them into Refs.Item. A foreach copy has path and name; a models copy has
// model and name.
const (
	itemPath  = "item.path"
	itemName  = "item.name"
	itemModel = "item.model"
)

// askVerdict is added to the brief of a step that must end with a verdict.
const askVerdict = "\n\nFinish with a verdict: pass if it holds up, fail if it does not (routr done --verdict pass|fail)."

// Start queues f as chain: one pending worker per step, or per matched file
// for a foreach step, in step order, with After and OnFail.Resume mapped
// from step ids to the ids Create assigned. params must hold every declared
// param and nothing else. work is the chain's scratch directory, {{work}}
// (absolute; "" = none). Foreach globs are relative to cwd. A foreach step
// without after is globbed now, so its files must already exist; one with
// after (Step.Runtime) is one worker with a Fanout, which the watcher
// expands (Expand) once the steps before it are done. A foreach step
// with allow_empty that matches nothing now is one worker, queued done,
// with a Fanout and no copies (registry.Worker.Empty). A worktree step's
// workers get a Checkout from HEAD of cwd's checkout, read now, so that all
// of them start from the same commit; the watcher names and makes each
// worktree. Its output and items use {{work}} absolute: their cwd will be
// the worktree, the work directory stays in the main repo.
//
// Each worker gets Chain, Step, Role ("task" if unset), Brief (the raw
// template), Params, Output (rendered: it may use params, chain, work and
// item), Gate, Interactive, the step's harness/model/effort pins, its
// answer as Answerer, its write as WritePin, its env and args as they are
// written, OnFail, Parallel, Cwd and state pending. A run step's
// worker has role "run", Run (the raw template), Expect, Timeout, Write and
// the step's env, and no brief; after one worktree step (treeStep) it has
// TreeOf, that step's worker, and the watcher moves its Cwd and Tree into
// that worktree when it admits it. A judge step's has role "judge", its
// brief as Jev's instructions and Judge, with From = its After; it does not
// write. Role defaults (Write, Permissions, unpinned harness, a default
// Output or Gate), the Answerer of a step without answer, and config's
// [env] and args under the step's are left to the Creator.
//
// Nothing is created if the params, a glob or an output is wrong. If Create
// fails partway, the workers already created are cancelled and the error
// says so.
func (f Flow) Start(c Creator, chain string, params map[string]string, cwd, work string) ([]registry.Worker, error) {
	if chain == "" {
		return nil, fmt.Errorf("flow %s: no chain id", f.Name)
	}
	if err := f.Check(params); err != nil {
		return nil, err
	}
	refs := Refs{Params: params, Chain: chain, Work: inDir(cwd, work)}
	var base registry.Checkout
	if slices.ContainsFunc(f.Steps, func(s Step) bool { return s.Worktree }) {
		var err error
		if base, err = registry.Head(cwd); err != nil {
			return nil, fmt.Errorf("flow %s: worktree = true needs a git repo: %w", f.Name, err)
		}
	}
	var queue []queued
	for i, s := range f.Steps {
		r := refs
		if s.Worktree {
			// Its cwd is its worktree, so the work directory, in the
			// main repo, is absolute in its output and its items.
			r.Work = work
		}
		ws, err := s.workers(chain, r, cwd)
		if err != nil {
			return nil, fmt.Errorf("flow %s step %s: %w", f.Name, s.ID, err)
		}
		for _, w := range ws {
			queue = append(queue, queued{i, w})
		}
	}
	ids := map[string][]int{}
	var created []registry.Worker
	for _, q := range queue {
		s, w := f.Steps[q.step], q.w
		for _, a := range s.After {
			w.After = append(w.After, ids[a]...)
		}
		if o := s.OnFail; o != nil {
			w.OnFail = &registry.OnFail{Resume: ids[o.Resume][0], Brief: o.Brief, Max: o.Max}
		}
		if w.Judge != nil {
			w.Judge.From = slices.Clone(w.After)
		}
		if t := f.treeStep(s); w.Run != "" && t != "" {
			w.TreeOf = ids[t][0]
		}
		if s.Worktree && !w.Empty() {
			co := base
			w.Checkout = &co
		}
		got, err := c.Create(w)
		if err != nil {
			err = fmt.Errorf("flow %s step %s: %w", f.Name, s.ID, err)
			return nil, errors.Join(err, cancel(c, created, err))
		}
		created = append(created, got)
		ids[s.ID] = append(ids[s.ID], got.ID)
	}
	return created, nil
}

// Check reports what keeps the flow from starting with params: a bad flow,
// or a param missing or unknown. Start checks too; a caller checks first to
// give out a chain name only for a flow that can start.
func (f Flow) Check(params map[string]string) error {
	if err := f.validate(); err != nil {
		return err
	}
	if err := f.checkParams(params); err != nil {
		return fmt.Errorf("flow %s: %w", f.Name, err)
	}
	return nil
}

type queued struct {
	step int
	w    registry.Worker
}

func (f Flow) checkParams(params map[string]string) error {
	var bad error
	for _, p := range f.Params {
		if strings.TrimSpace(params[p]) == "" {
			bad = errors.Join(bad, fmt.Errorf("missing param %q", p))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(params)) {
		if !slices.Contains(f.Params, k) {
			bad = errors.Join(bad, fmt.Errorf("unknown param %q (params: %s)", k, strings.Join(f.Params, ", ")))
		}
	}
	return bad
}

// treeStep is the step whose worktree run step s runs in: the one step
// in its after with a worktree, if it has one (a worktree step that is
// not a fan-out, or a pick judge over worktree steps); "" when none does,
// or more than one, or that one is a fan-out step with a worktree per
// copy.
func (f Flow) treeStep(s Step) string {
	var found []Step
	for _, a := range s.After {
		if i := slices.IndexFunc(f.Steps, func(t Step) bool { return t.ID == a }); i >= 0 && f.treed(f.Steps[i]) {
			found = append(found, f.Steps[i])
		}
	}
	if len(found) != 1 || found[0].fanout() {
		return ""
	}
	return found[0].ID
}

// treed reports whether step t's workers run in worktrees: a worktree
// step, or a pick judge whose winner is always a worktree step's worker.
func (f Flow) treed(t Step) bool {
	if t.Worktree {
		return true
	}
	if t.Judge == "" || !t.Pick {
		return false
	}
	for _, a := range t.After {
		if i := slices.IndexFunc(f.Steps, func(u Step) bool { return u.ID == a }); i >= 0 {
			if u := f.Steps[i]; u.Run == "" && u.Judge == "" && !u.Worktree {
				return false
			}
		}
	}
	return true
}

// workers builds the step's workers, without After and OnFail. refs has
// the params, chain and work.
func (s Step) workers(chain string, refs Refs, cwd string) ([]registry.Worker, error) {
	params := refs.Params
	if s.Run != "" {
		// It takes the directory lock: it should see a tree no writer is
		// changing. After a worktree step, Start sets TreeOf, and the
		// tree is that worktree.
		return []registry.Worker{{
			Chain: chain, Step: s.ID, Role: "run", Run: s.Run, Expect: registry.Verdict(s.Expect), Timeout: s.timeout(),
			Env: maps.Clone(s.Env), Cwd: cwd, Write: true, Params: maps.Clone(params), Gate: s.Gate, State: registry.Pending,
		}}, nil
	}
	if s.Judge != "" {
		return []registry.Worker{{
			Chain: chain, Step: s.ID, Role: "judge", Brief: s.Brief,
			Cwd: cwd, Params: maps.Clone(params), Gate: s.Gate, State: registry.Pending,
			Judge: &registry.Judge{Pick: s.Pick, Pass: s.Pass, Fail: s.Fail, MinConfidence: s.minConfidence(), Read: slices.Clone(s.Read)},
		}}, nil
	}
	items := []*Item{nil}
	if s.Runtime() {
		// Its output is rendered per copy; a param that takes it outside
		// the directory is wrong for every copy, so it fails now.
		if _, err := output(s.Output, refs, &Item{Path: "x", Name: "x"}); err != nil {
			return nil, err
		}
	} else if s.Foreach != "" {
		var err error
		if items, err = glob(s.Foreach, refs, cwd, s.maxItems(), s.AllowEmpty); err != nil {
			return nil, err
		}
	} else if s.Models != nil {
		// The list is known when the flow starts, so its copies are too:
		// no runtime expansion, unlike a foreach with after. The models
		// replace the no-item entry: every copy has one.
		items = nil
		for _, m := range s.Models {
			items = append(items, &Item{Model: m, Name: slug(m)})
		}
	}
	role := s.Role
	if role == "" {
		role = "task"
	}
	brief := s.Brief
	if s.Verdict || s.OnFail != nil {
		brief = strings.TrimRight(brief, "\n") + askVerdict
	}
	if len(items) == 0 {
		// allow_empty, and nothing matched: one worker, queued done, so
		// that the steps after it have a step to wait on and refer to.
		return []registry.Worker{{
			Chain: chain, Step: s.ID, Role: role, Harness: s.Harness, Model: s.Model, Effort: s.Effort, Answerer: s.Answer,
			Env: maps.Clone(s.Env), Args: slices.Clone(s.Args), Cwd: cwd, Brief: brief, Params: maps.Clone(params), State: registry.Done,
			Fanout:  &registry.Fanout{Glob: s.Foreach, Output: s.Output, MaxItems: s.maxItems(), AllowEmpty: true},
			Summary: EmptyNote(s.Foreach), Note: EmptyNote(s.Foreach),
		}}, nil
	}
	ws := make([]registry.Worker, 0, len(items))
	for _, it := range items {
		w := registry.Worker{
			Chain: chain, Step: s.ID, Role: role,
			Harness: s.Harness, Model: s.Model, Effort: s.Effort, Answerer: s.Answer,
			Env: maps.Clone(s.Env), Args: slices.Clone(s.Args), Cwd: cwd, Brief: brief, Params: maps.Clone(params),
			WritePin: s.Write, Interactive: s.Interactive, Gate: s.Gate, State: registry.Pending,
		}
		if s.fanout() {
			w.Parallel = s.parallel()
		}
		if s.Runtime() {
			w.Fanout = &registry.Fanout{Glob: s.Foreach, Output: s.Output, MaxItems: s.maxItems(), AllowEmpty: s.AllowEmpty}
			ws = append(ws, w)
			continue
		}
		if it != nil {
			if w.Params == nil {
				w.Params = map[string]string{}
			}
			if it.Path != "" {
				w.Params[itemPath], w.Params[itemName] = it.Path, it.Name
			} else {
				w.Params[itemModel], w.Params[itemName] = it.Model, it.Name
				w.Model = it.Model
			}
		}
		out, err := output(s.Output, refs, it)
		if err != nil {
			return nil, err
		}
		w.Output = out
		ws = append(ws, w)
	}
	return ws, nil
}

// output renders an output template with refs and item it; "" stays "".
// It must stay inside the directory, or inside the work directory.
func output(tmpl string, refs Refs, it *Item) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	refs.Item = it
	out, err := Render(tmpl, refs)
	if err != nil {
		return "", fmt.Errorf("output: %w", err)
	}
	if !filepath.IsLocal(out) && !(filepath.IsAbs(refs.Work) && within(refs.Work, out)) {
		return "", fmt.Errorf("output %q must be a relative path inside the directory", out)
	}
	return out, nil
}

// inDir is path relative to dir when it lies inside dir, else as it is.
func inDir(dir, path string) string {
	if dir == "" || !filepath.IsAbs(path) || !within(dir, path) {
		return path
	}
	rel, _ := filepath.Rel(dir, path)
	return rel
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Expand builds the copies of fan, a worker with a Fanout whose After are
// done: one per file its glob matches now in fan.Cwd, in name order, without
// ids. work is its chain's scratch directory, {{work}} (absolute). Each copy
// is fan with its item in Params, its own Output, no Fanout and state
// pending; it keeps fan's After, role settings, brief and pins. More
// matches than MaxItems or an output outside the directory is an error,
// and nothing is built; so is no match, unless Fanout.AllowEmpty, which
// builds no copies.
func Expand(fan registry.Worker, work string) ([]registry.Worker, error) {
	f := fan.Fanout
	if f == nil {
		return nil, fmt.Errorf("worker %d is not a foreach step", fan.ID)
	}
	params := map[string]string{}
	for k, v := range fan.Params {
		if k != itemPath && k != itemName {
			params[k] = v
		}
	}
	refs := Refs{Params: params, Chain: fan.ChainRef(), Work: inDir(fan.Cwd, work)}
	if fan.Checkout != nil {
		refs.Work = work // as in Start
	}
	items, err := glob(f.Glob, refs, fan.Cwd, f.MaxItems, f.AllowEmpty)
	if err != nil {
		return nil, err
	}
	ws := make([]registry.Worker, 0, len(items))
	for _, it := range items {
		w := fan
		w.ID, w.Fanout, w.State, w.Note, w.Error, w.Summary = 0, nil, registry.Pending, "", "", ""
		w.After = slices.Clone(fan.After)
		w.Args, w.Env = slices.Clone(fan.Args), maps.Clone(fan.Env)
		if fan.Checkout != nil {
			co := *fan.Checkout // each copy its own worktree, from the same commit
			w.Checkout = &co
		}
		w.Params = maps.Clone(params)
		w.Params[itemPath], w.Params[itemName] = it.Path, it.Name
		if f.Output != "" {
			if w.Output, err = output(f.Output, refs, it); err != nil {
				return nil, err
			}
		}
		ws = append(ws, w)
	}
	return ws, nil
}

// glob expands a foreach pattern into at most limit items: files only,
// sorted, with paths as written in the pattern (relative to cwd unless it
// is absolute). No match is an error unless empty allows it.
func glob(tmpl string, refs Refs, cwd string, limit int, empty bool) ([]*Item, error) {
	pat, err := Render(tmpl, refs)
	if err != nil {
		return nil, fmt.Errorf("foreach: %w", err)
	}
	full := pat
	if !filepath.IsAbs(pat) {
		full = filepath.Join(cwd, pat)
	}
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, fmt.Errorf("foreach %q: %w", pat, err)
	}
	var items []*Item
	for _, m := range matches {
		if fi, err := os.Stat(m); err != nil || fi.IsDir() {
			continue
		}
		path := m
		if !filepath.IsAbs(pat) {
			if path, err = filepath.Rel(cwd, m); err != nil {
				return nil, fmt.Errorf("foreach %q: %w", pat, err)
			}
		}
		items = append(items, &Item{Path: path, Name: filepath.Base(m)})
	}
	if len(items) == 0 && !empty {
		return nil, fmt.Errorf("foreach %q matched no files in %s", pat, cwd)
	}
	if len(items) > limit {
		return nil, fmt.Errorf("foreach %q matched %d files in %s, more than max_items (%d)", pat, len(items), cwd, limit)
	}
	return items, nil
}

// EmptyNote says that a foreach step with allow_empty matched nothing.
func EmptyNote(glob string) string {
	return fmt.Sprintf("foreach %s matched no files; allow_empty, so nothing ran", glob)
}

// cancel marks created workers cancelled after a failed start.
func cancel(c Creator, created []registry.Worker, cause error) error {
	if len(created) == 0 {
		return nil
	}
	var bad error
	var done []string
	for _, w := range created {
		_, err := c.Update(w.ID, func(w *registry.Worker) {
			w.State = registry.Cancelled
			w.Error = "flow start failed: " + cause.Error()
		})
		if err != nil {
			bad = errors.Join(bad, fmt.Errorf("could not cancel worker %d: %w", w.ID, err))
			continue
		}
		done = append(done, strconv.Itoa(w.ID))
	}
	if len(done) > 0 {
		bad = errors.Join(fmt.Errorf("cancelled workers %s", strings.Join(done, ", ")), bad)
	}
	return bad
}

// RefsFor builds the refs for rendering w's brief, or the on_fail brief w
// sends when it fails, from the registry records in all:
//
//   - Params: w's params; Item: w's item, if it is a foreach copy; Chain:
//     w.ChainRef(). Work is left to the caller, which knows the directory.
//   - Steps: every step of w's chain queued up to and including w, by step
//     id. A foreach step has Outputs (every copy's, in id order, including
//     the copies a runtime foreach queued up to w expanded into later); any
//     other step has the Output, Final (final(w), its current run's final
//     message) and Verdict of its latest worker, and Outputs = [Output]; Worktree and Branch of
//     its Checkout (a pick judge: its winner's). The runtime foreach worker
//     itself (Fanout) is no step's output.
//     A foreach step that matched no files (allow_empty) is Empty.
//   - Steps["prev"]: the worker w.After[0] (all copies of its step, if it
//     is a foreach copy or a runtime foreach).
//
// A missing value is left empty, so Render reports it.
func RefsFor(w registry.Worker, all []registry.Worker, final func(registry.Worker) string) Refs {
	r := Refs{Params: map[string]string{}, Steps: map[string]StepRef{}, Chain: w.ChainRef()}
	for k, v := range w.Params {
		if k != itemPath && k != itemName && k != itemModel {
			r.Params[k] = v
		}
	}
	if p, ok := w.Params[itemPath]; ok {
		r.Item = &Item{Path: p, Name: w.Params[itemName]}
	} else if m, ok := w.Params[itemModel]; ok {
		r.Item = &Item{Model: m, Name: w.Params[itemName]}
	}
	all = slices.SortedFunc(slices.Values(all), func(a, b registry.Worker) int { return a.ID - b.ID })
	if w.Chain != "" {
		fanned := map[string]bool{} // runtime foreach steps queued up to w: their copies come later
		for _, x := range all {
			if x.Chain == w.Chain && x.Fanout != nil && x.ID <= w.ID {
				fanned[x.Step] = true
			}
		}
		groups := map[string][]registry.Worker{}
		for _, x := range all {
			if x.Chain == w.Chain && x.Step != "" && x.Fanout == nil && (x.ID <= w.ID || fanned[x.Step]) {
				groups[x.Step] = append(groups[x.Step], x)
			}
		}
		for step, g := range groups {
			r.Steps[step] = stepRef(g, all, final)
		}
		for _, x := range all {
			if _, ok := groups[x.Step]; x.Chain == w.Chain && x.ID <= w.ID && x.Empty() && !ok {
				r.Steps[x.Step] = StepRef{Empty: true}
			}
		}
	}
	if len(w.After) > 0 {
		i := slices.IndexFunc(all, func(x registry.Worker) bool { return x.ID == w.After[0] })
		if i >= 0 {
			p, g := all[i], []registry.Worker{all[i]}
			if (isCopy(p) || p.Fanout != nil) && p.Chain != "" {
				g = slices.DeleteFunc(slices.Clone(all), func(x registry.Worker) bool {
					return x.Chain != p.Chain || x.Step != p.Step || x.Fanout != nil
				})
			}
			switch {
			case len(g) > 0:
				r.Steps["prev"] = stepRef(g, all, final)
			case p.Empty():
				r.Steps["prev"] = StepRef{Empty: true}
			}
		}
	}
	return r
}

// ItemOf is the item of a fan-out copy w; nil for any other worker.
func ItemOf(w registry.Worker) *Item {
	if p, ok := w.Params[itemPath]; ok {
		return &Item{Path: p, Name: w.Params[itemName]}
	}
	if m, ok := w.Params[itemModel]; ok {
		return &Item{Model: m, Name: w.Params[itemName]}
	}
	return nil
}

func isCopy(w registry.Worker) bool {
	_, path := w.Params[itemPath]
	_, model := w.Params[itemModel]
	return path || model
}

// stepRef summarizes a step's workers, sorted by id; all is every worker,
// for a pick's winner.
func stepRef(g []registry.Worker, all []registry.Worker, final func(registry.Worker) string) StepRef {
	if isCopy(g[0]) {
		var ref StepRef
		for _, x := range g {
			if x.Output != "" {
				ref.Outputs = append(ref.Outputs, x.Output)
			}
		}
		return ref
	}
	last := g[len(g)-1]
	ref := StepRef{Output: last.Output, Verdict: string(last.Verdict)}
	if final != nil {
		ref.Final = final(last)
	}
	if last.Output != "" {
		ref.Outputs = []string{last.Output}
	}
	co := last.Checkout
	if j := last.Judge; j != nil && j.Winner != 0 {
		if i := slices.IndexFunc(all, func(x registry.Worker) bool { return x.ID == j.Winner }); i >= 0 {
			co = all[i].Checkout
		}
	}
	if co != nil {
		ref.Worktree, ref.Branch = co.Path, co.Branch
	}
	return ref
}
