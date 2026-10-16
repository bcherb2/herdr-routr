package registry

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Retention bounds what the registry keeps of finished workers, so
// registry.json and runs/ do not grow for good. Every write applies it, in
// the same transaction, to the registry as that write left it: a worker a
// command has just brought back to work (`routr send` to a closed worker)
// is never dropped, and one that was dropped cannot be brought back (its
// id is not given out again).
//
// A worker may go once it is terminal, has no follow-up queued or in
// flight, has no pane, and no active worker (or one with a pane) names it
// in After or OnFail.Resume. A chain goes as one: only when every worker in
// it may go, and it counts as old as its newest worker. What may go is
// dropped once untouched (Updated) for Keep; beyond Max workers, the least
// recently touched of it goes first. Its run directory, its worktree
// (removeTrees: one with changes in it stays, as do branches with commits)
// and the work directory of a chain that has gone whole are removed after
// the write; nothing else is (harness sessions and outputs stay).
type Retention struct {
	Keep time.Duration // 0 = 14 days
	Max  int           // 0 = 200
	Off  bool          // keep everything
}

// Retention defaults.
const (
	defaultKeep = 14 * 24 * time.Hour
	defaultMax  = 200
)

// retention is a Store's policy and where its run and work directories
// and worktrees are.
type retention struct {
	Retention
	runs  string
	work  string
	trees string
}

// pruned is what a write's retention dropped, for removing from disk once
// the write is committed.
type pruned struct {
	workers []Worker
	chains  []string // chains no worker is left in (Worker.ChainRef)
}

// retain drops what r lets go from f and returns the dropped workers.
func (f *file) retain(now time.Time, r Retention) []Worker {
	if r.Off {
		return nil
	}
	keep, limit := r.Keep, r.Max
	if keep <= 0 {
		keep = defaultKeep
	}
	if limit <= 0 {
		limit = defaultMax
	}
	cutoff := now.Add(-keep)
	if len(f.Workers) <= limit && !slices.ContainsFunc(f.Workers, func(w Worker) bool { return w.Updated.Before(cutoff) }) {
		return nil
	}

	held := map[int]bool{} // active or with a pane, or named by one that is
	for _, w := range f.Workers {
		if !w.Active() && w.Live == nil {
			continue
		}
		held[w.ID] = true
		for _, id := range w.After {
			held[id] = true
		}
		if w.OnFail != nil {
			held[w.OnFail.Resume] = true
		}
	}

	// A unit is a chain, or a worker outside any; one held worker holds it.
	type unit struct {
		ids    []int
		newest time.Time
		held   bool
	}
	type key struct {
		chain string
		id    int // outside a chain
	}
	units := map[key]*unit{}
	for _, w := range f.Workers {
		k := key{chain: w.Chain}
		if w.Chain == "" {
			k.id = w.ID
		}
		u := units[k]
		if u == nil {
			u = &unit{}
			units[k] = u
		}
		u.ids = append(u.ids, w.ID)
		if w.Updated.After(u.newest) {
			u.newest = w.Updated
		}
		u.held = u.held || held[w.ID]
	}

	drop := map[int]bool{}
	var spare []*unit // may go, but not old enough yet
	for _, u := range units {
		switch {
		case u.held:
		case u.newest.Before(cutoff):
			for _, id := range u.ids {
				drop[id] = true
			}
		default:
			spare = append(spare, u)
		}
	}
	if over := len(f.Workers) - len(drop) - limit; over > 0 {
		slices.SortFunc(spare, func(a, b *unit) int {
			if c := a.newest.Compare(b.newest); c != 0 {
				return c
			}
			return slices.Min(a.ids) - slices.Min(b.ids)
		})
		for _, u := range spare {
			if over <= 0 {
				break
			}
			for _, id := range u.ids {
				drop[id] = true
			}
			over -= len(u.ids)
		}
	}
	if len(drop) == 0 {
		return nil
	}

	var dropped []Worker
	f.Workers = slices.DeleteFunc(f.Workers, func(w Worker) bool {
		if drop[w.ID] {
			dropped = append(dropped, w)
		}
		return drop[w.ID]
	})
	f.prune(now) // their directory locks
	return dropped
}

// removeRuns removes the run directories of dropped workers under runs (an
// absolute path, else nothing is removed), except one that holds the
// worker's output file. It is best effort: a run
// directory left behind costs disk, not correctness, and the write that
// dropped its worker has already been committed.
func removeRuns(runs string, dropped []Worker) {
	if !filepath.IsAbs(runs) {
		return
	}
	for _, w := range dropped {
		if w.ID <= 0 {
			continue
		}
		dir := filepath.Join(runs, strconv.Itoa(w.ID))
		if out := w.Output; out != "" && !w.Tabless() { // a run step's output is its own run.log, a judge's another's
			if !filepath.IsAbs(out) {
				out = filepath.Join(w.Cwd, out)
			}
			if within(dir, out) {
				continue
			}
		}
		os.RemoveAll(dir)
	}
}

// goneChains are the chains of dropped that no worker in kept is in.
func goneChains(kept, dropped []Worker) []string {
	left := map[string]bool{}
	for _, w := range kept {
		left[w.ChainRef()] = true
	}
	var out []string
	for _, w := range dropped {
		if c := w.ChainRef(); !left[c] {
			left[c] = true
			out = append(out, c)
		}
	}
	return out
}

// removeWork removes the work directories of chains under work (an
// absolute path, else nothing is removed), best effort as removeRuns.
func removeWork(work string, chains []string) {
	if !filepath.IsAbs(work) {
		return
	}
	for _, c := range chains {
		if dir := filepath.Join(work, c); c != "" && filepath.Dir(dir) == work {
			os.RemoveAll(dir)
		}
	}
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
