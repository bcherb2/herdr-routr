package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrNotFound is returned for an unknown worker.
var ErrNotFound = errors.New("no such worker")

// Store is the registry file. Every routr process opens its own Store on the
// same Path; writes take an flock on a sibling lock file and replace the file
// atomically. The zero value with no Path keeps workers in memory (tests).
// A Store from Paths.Store applies the Paths' Retention on every write; any
// other keeps every worker.
type Store struct {
	Path string
	Now  func() time.Time

	mu     sync.Mutex
	mem    file
	retain *retention // nil = keep everything
}

// file is the on-disk format.
type file struct {
	Version int            `json:"version"`
	NextID  int            `json:"next_id"`
	Workers []Worker       `json:"workers"`
	Locks   map[string]int `json:"locks,omitempty"`  // worktree → the worker that took its directory lock (lock.go)
	Chains  map[string]int `json:"chains,omitempty"` // flow name → the last chain number NewChain gave out
}

// version is the format this routr writes; it refuses a newer file rather
// than misread it. A field added since reads as its zero value from a file
// that lacks it, and that zero must mean what the older routr meant.
const version = 1

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create adds w with the next id, in its first run (Runs); Created and
// Updated are set to now.
func (s *Store) Create(w Worker) (Worker, error) {
	err := s.update(func(f *file) error {
		f.NextID++
		w.ID, w.Runs = f.NextID, 1
		w.Created, w.Updated = s.now(), s.now()
		f.Workers = append(f.Workers, w)
		return nil
	})
	return w, err
}

// Spawn adds ws with the next ids, each in its first run (Runs), and
// applies fn to worker id with the
// workers as created, in one transaction: a foreach worker and the copies
// it expands into land together. A non-nil error from fn leaves the
// registry unchanged and is returned as is.
func (s *Store) Spawn(id int, ws []Worker, fn func(w *Worker, created []Worker) error) ([]Worker, error) {
	var out []Worker
	err := s.update(func(f *file) error {
		i := f.index(id)
		if i < 0 {
			return fmt.Errorf("worker %d: %w", id, ErrNotFound)
		}
		out = make([]Worker, len(ws))
		for k, w := range ws {
			f.NextID++
			w.ID, w.Runs = f.NextID, 1
			w.Created, w.Updated = s.now(), s.now()
			out[k] = w
		}
		w := f.Workers[i]
		if err := fn(&w, out); err != nil {
			return err
		}
		w.Updated = s.now()
		f.Workers[i] = w
		f.Workers = append(f.Workers, out...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Update applies fn to worker id, stamps Updated and returns the result.
func (s *Store) Update(id int, fn func(*Worker)) (Worker, error) {
	return s.Modify(id, func(w *Worker) error { fn(w); return nil })
}

// Modify is Update where fn may refuse: a non-nil error from fn leaves the
// registry unchanged and is returned as is.
func (s *Store) Modify(id int, fn func(*Worker) error) (Worker, error) {
	var out Worker
	err := s.update(func(f *file) error {
		i := f.index(id)
		if i < 0 {
			return fmt.Errorf("worker %d: %w", id, ErrNotFound)
		}
		w := f.Workers[i]
		if err := fn(&w); err != nil {
			return err
		}
		w.Updated = s.now()
		f.Workers[i], out = w, w
		return nil
	})
	return out, err
}

// Tx runs fn over every worker, by id, under one lock: for changes that must
// land together (a review loop moves two workers). Workers fn changes get a
// new Updated. A non-nil error from fn leaves the registry unchanged.
func (s *Store) Tx(fn func(byID map[int]*Worker) error) error {
	return s.update(func(f *file) error {
		byID := make(map[int]*Worker, len(f.Workers))
		before := make(map[int]string, len(f.Workers))
		for i := range f.Workers {
			w := &f.Workers[i]
			byID[w.ID] = w
			b, _ := json.Marshal(w)
			before[w.ID] = string(b)
		}
		if err := fn(byID); err != nil {
			return err
		}
		for _, w := range byID {
			if b, _ := json.Marshal(w); string(b) != before[w.ID] {
				w.Updated = s.now()
			}
		}
		return nil
	})
}

// NewChain gives out a chain name for a run of flow name: name-N, with N
// above every number given out before or in use, in one transaction, so two
// `routr flow start` at once never share a chain.
func (s *Store) NewChain(name string) (string, error) {
	var out string
	err := s.update(func(f *file) error {
		n := f.Chains[name]
		for _, w := range f.Workers {
			if num, ok := strings.CutPrefix(w.Chain, name+"-"); ok {
				if k, err := strconv.Atoi(num); err == nil {
					n = max(n, k)
				}
			}
		}
		if f.Chains == nil {
			f.Chains = map[string]int{}
		}
		f.Chains[name] = n + 1
		out = name + "-" + strconv.Itoa(n+1)
		return nil
	})
	return out, err
}

// Get returns worker id.
func (s *Store) Get(id int) (Worker, error) {
	f, err := s.read()
	if err != nil {
		return Worker{}, err
	}
	if i := f.index(id); i >= 0 {
		return f.Workers[i], nil
	}
	return Worker{}, fmt.Errorf("worker %d: %w", id, ErrNotFound)
}

// List returns every worker, by id.
func (s *Store) List() ([]Worker, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	out := slices.Clone(f.Workers)
	slices.SortFunc(out, func(a, b Worker) int { return a.ID - b.ID })
	return out, nil
}

// Chain returns the workers of chain, by id.
func (s *Store) Chain(chain string) ([]Worker, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(w Worker) bool { return w.Chain != chain }), nil
}

func (f *file) index(id int) int {
	return slices.IndexFunc(f.Workers, func(w Worker) bool { return w.ID == id })
}

// update runs fn on the current registry under the lock, applies the
// retention and saves it; then, with the lock released, it removes the run
// directories and worktrees of the workers retention dropped, and the work
// directories of the chains it dropped whole.
func (s *Store) update(fn func(*file) error) error {
	dropped, err := s.commit(fn)
	if err == nil && len(dropped.workers) > 0 {
		removeRuns(s.retain.runs, dropped.workers)
		removeTrees(s.retain.trees, dropped.workers)
		removeWork(s.retain.work, dropped.chains)
	}
	return err
}

func (s *Store) commit(fn func(*file) error) (pruned, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Path == "" {
		f := s.mem.clone()
		if err := fn(&f); err != nil {
			return pruned{}, err
		}
		dropped := s.applyRetention(&f)
		s.mem = f
		return dropped, nil
	}
	unlock, err := lockFile(s.Path+".lock", syscall.LOCK_EX)
	if err != nil {
		return pruned{}, err
	}
	defer unlock()
	f, err := s.load()
	if err != nil {
		return pruned{}, err
	}
	if err := fn(&f); err != nil {
		return pruned{}, err
	}
	dropped := s.applyRetention(&f)
	return dropped, s.save(f)
}

func (s *Store) applyRetention(f *file) pruned {
	if s.retain == nil {
		return pruned{}
	}
	ws := f.retain(s.now(), s.retain.Retention)
	return pruned{ws, goneChains(f.Workers, ws)}
}

func (s *Store) read() (file, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Path == "" {
		return s.mem.clone(), nil
	}
	unlock, err := lockFile(s.Path+".lock", syscall.LOCK_SH)
	if err != nil {
		return file{}, err
	}
	defer unlock()
	return s.load()
}

// clone deep-copies f through JSON, so callers never share slices or maps
// with the in-memory registry.
func (f file) clone() file {
	b, _ := json.Marshal(f)
	var out file
	json.Unmarshal(b, &out)
	return out
}

// lockFile takes an flock on path (created if missing) and returns its
// release. The registry itself is replaced by rename, so it cannot carry the
// lock.
func lockFile(path string, how int) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), how); err != nil {
		lf.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	}, nil
}

// load reads the registry. A missing file is an empty registry; an
// unreadable one is set aside (…corrupt-<unix>, see SetAside) rather than
// stall the watcher and every command. Either way ids go on above the
// highest run directory, so a run directory is never reused.
func (s *Store) load() (file, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return s.empty(), nil
	}
	if err != nil {
		return file{}, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", s.Path, s.now().Unix())
		// A reader beside this one (both under the shared lock) may have
		// set it aside first.
		if rerr := os.Rename(s.Path, aside); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return file{}, fmt.Errorf("%s is unreadable (%v) and could not be moved aside: %w", s.Path, err, rerr)
		}
		return s.empty(), nil
	}
	if f.Version > version {
		return file{}, fmt.Errorf("%s has format version %d; this routr reads up to %d: upgrade routr", s.Path, f.Version, version)
	}
	for _, w := range f.Workers { // ids are never reused
		f.NextID = max(f.NextID, w.ID)
	}
	return f, nil
}

// empty is a new registry whose ids start above the highest run directory.
func (s *Store) empty() file {
	f := file{Version: version}
	if s.retain == nil {
		return f
	}
	entries, _ := os.ReadDir(s.retain.runs)
	for _, e := range entries {
		if id, err := strconv.Atoi(e.Name()); err == nil && e.IsDir() {
			f.NextID = max(f.NextID, id)
		}
	}
	return f
}

func (s *Store) save(f file) error {
	f.Version = version
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".registry-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // a crash after the rename must not leave it empty
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}
