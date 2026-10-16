package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The directory lock lets at most one write worker work in a worktree
// (Worker.Worktree) at a time. It is kept in registry.json and taken in the
// same transaction as the change that needs it, and a holder counts only
// while it still needs the lock (needsLock): a worker that finished, failed,
// closed or was cancelled frees it without anyone releasing it, so a crash
// between two steps cannot leave a stale lock behind.

var errLocked = errors.New("the directory lock is held")

// Claim applies fn to worker id and, for a write worker, gives it the
// directory lock of its worktree, in one transaction. If another worker
// holds the lock, fn is not run, nothing changes, and holder names that
// worker; otherwise holder is id. A non-nil error from fn leaves the
// registry unchanged and is returned as is.
func (s *Store) Claim(id int, fn func(*Worker) error) (w Worker, holder int, err error) {
	holder = id
	err = s.update(func(f *file) error {
		i := f.index(id)
		if i < 0 {
			return fmt.Errorf("worker %d: %w", id, ErrNotFound)
		}
		x := f.Workers[i]
		if x.Write {
			if h := f.holder(x.Worktree(), s.now()); h != 0 && h != id {
				holder = h
				return errLocked
			}
		}
		before, _ := json.Marshal(x)
		if err := fn(&x); err != nil {
			return err
		}
		if after, _ := json.Marshal(x); string(after) != string(before) {
			x.Updated = s.now()
		}
		f.Workers[i], w = x, x
		if x.Write {
			f.prune(s.now())
			if f.Locks == nil {
				f.Locks = map[string]int{}
			}
			f.Locks[x.Worktree()] = id
		}
		return nil
	})
	if errors.Is(err, errLocked) {
		return w, holder, nil
	}
	return w, holder, err
}

// Holders returns each locked worktree's holder.
func (s *Store) Holders() (map[string]int, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	out, now := map[string]int{}, s.now()
	for tree := range f.Locks {
		if h := f.holder(tree, now); h != 0 {
			out[tree] = h
		}
	}
	return out, nil
}

// holder is the worker that holds tree's lock at now, or 0.
func (f *file) holder(tree string, now time.Time) int {
	id, ok := f.Locks[tree]
	if !ok {
		return 0
	}
	if i := f.index(id); i >= 0 && f.Workers[i].needsLock(now) {
		return id
	}
	return 0
}

// prune drops the locks whose holder no longer needs them.
func (f *file) prune(now time.Time) {
	for tree := range f.Locks {
		if f.holder(tree, now) == 0 {
			delete(f.Locks, tree)
		}
	}
}

// needsLock reports whether a worker that took the directory lock still
// holds it: a write worker at work (LocksDir), or with a follow-up queued or
// in flight.
func (w Worker) needsLock(now time.Time) bool {
	return w.Write && w.State != Cancelled && (w.LocksDir(now) || w.TurnsPending())
}
