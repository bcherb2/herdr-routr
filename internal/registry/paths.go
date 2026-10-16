package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StateDirEnv overrides where the .routr directory is (tests, and workers:
// the watcher exports it so `routr done` finds the root from anywhere).
const StateDirEnv = "ROUTR_STATE_DIR"

// Paths locates one repo's .routr directory and everything in it.
type Paths struct {
	Dir       string    // <root>/.routr
	Retention Retention // what its Stores keep of finished workers (retain.go)
}

// Find returns the .routr directory for work in cwd: $ROUTR_STATE_DIR (read
// with getenv; relative to cwd) if set, else <root>/.routr, where root is the
// parent of git's common dir (so a repo's worktrees share one root), else
// cwd outside git.
func Find(cwd string, getenv func(string) string) (Paths, error) {
	if d := getenv(StateDirEnv); d != "" {
		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}
		abs, err := filepath.Abs(d)
		return Paths{Dir: abs}, err
	}
	root, err := RepoRoot(cwd)
	if err != nil {
		return Paths{}, err
	}
	return Paths{Dir: filepath.Join(root, ".routr")}, nil
}

// At returns the .routr directory of root (the `--root` flag).
func At(root string) (Paths, error) {
	abs, err := filepath.Abs(root)
	return Paths{Dir: filepath.Join(abs, ".routr")}, err
}

// RepoRoot is the directory that holds .routr for work in cwd.
func RepoRoot(cwd string) (string, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	common, err := git(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return cwd, nil // not in a repo, or no git
	}
	if strings.HasSuffix(common, string(filepath.Separator)+".git") {
		return filepath.Dir(common), nil
	}
	// A submodule or unusual layout: fall back to the checkout's top.
	if top, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		return top, nil
	}
	return cwd, nil
}

// Tree is the directory lock's key for work in cwd: its worktree's top
// directory, else cwd. Two worktrees of one repo lock separately.
func Tree(cwd string) string {
	cwd, _ = filepath.Abs(cwd)
	if top, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		return top
	}
	return cwd
}

func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Root is the repo root: the parent of Dir.
func (p Paths) Root() string { return filepath.Dir(p.Dir) }

// Registry is the registry file.
func (p Paths) Registry() string { return filepath.Join(p.Dir, "state", "registry.json") }

// SetAside lists registry files that were unreadable and set aside
// (Store.load), oldest first. The workers they held are no longer tracked.
func (p Paths) SetAside() []string {
	m, _ := filepath.Glob(p.Registry() + ".corrupt-*")
	return m
}

// WatchLock is held by the live watcher for its whole life.
func (p Paths) WatchLock() string { return filepath.Join(p.Dir, "state", "watch.lock") }

// WatchLog is where a detached watcher writes.
func (p Paths) WatchLog() string { return filepath.Join(p.Dir, "state", "watch.log") }

// Config is the repo's config overrides.
func (p Paths) Config() string { return filepath.Join(p.Dir, "config.toml") }

// Flows is the repo's flow directory.
func (p Paths) Flows() string { return filepath.Join(p.Dir, "flows") }

// RunDir is worker id's run directory.
func (p Paths) RunDir(id int) string { return filepath.Join(p.Dir, "runs", strconv.Itoa(id)) }

// WorkDir is chain's scratch directory, {{work}} in its templates.
func (p Paths) WorkDir(chain string) string { return filepath.Join(p.Dir, "work", chain) }

// Store opens the registry, with p's Retention.
func (p Paths) Store() *Store {
	return &Store{Path: p.Registry(), retain: &retention{p.Retention, filepath.Join(p.Dir, "runs"), filepath.Join(p.Dir, "work"), p.Trees()}}
}

// ignored are the lines of .routr/.gitignore: what routr writes for itself,
// and the .gitignore itself, so that .routr/ stays out of git status until it
// holds a config.toml or flows/ (a .gitignore already committed stays tracked).
var ignored = []string{"/state/", "/runs/", "/work/", "/trees/", "/.gitignore"}

// Init creates the state and runs directories and a .gitignore that keeps
// them, work/, trees/ and itself out of git (config.toml and flows/ stay in).
// A line an older .gitignore lacks is added to it.
func (p Paths) Init() error {
	for _, d := range []string{filepath.Join(p.Dir, "state"), filepath.Join(p.Dir, "runs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	gi := filepath.Join(p.Dir, ".gitignore")
	b, err := os.ReadFile(gi)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	for line := range strings.Lines(string(b)) {
		have[strings.TrimSpace(line)] = true
	}
	var add strings.Builder
	for _, l := range ignored {
		if !have[l] {
			add.WriteString(l + "\n")
		}
	}
	if add.Len() == 0 {
		return nil
	}
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		b = append(b, '\n')
	}
	return WriteFile(gi, append(b, add.String()...))
}

// Run files, in RunDir.
const (
	BriefFile = "brief.md"    // the prompt as sent
	DoneFile  = "done.json"   // written by `routr done`
	FinalFile = "final.md"    // the final message of a record from before runs were numbered (FinalPath)
	TurnsDir  = "turns"       // turns/N.md: replies to follow-ups
	RunLog    = "run.log"     // a run step's command's output, stdout and stderr
	JevFile   = "jev.json"    // a judge's Jev decision, or why it has none
	StateFile = "state.json"  // the state a judge sent Jev
	HookFile  = "hooks.jsonl" // a Claude worker's PermissionRequest hook records (routr hook)
)

// TurnFile is the reply to follow-up turn n.
func (p Paths) TurnFile(id, n int) string {
	return filepath.Join(p.RunDir(id), TurnsDir, strconv.Itoa(n)+".md")
}

// FinalPath is where w's final message for its current run goes:
// final-<Runs>.md in its run directory; final.md (FinalFile) for a record
// from before runs were numbered (Runs 0). The dash keeps it apart from the
// final.<n>.md that routr of that time moved an earlier run's final.md to.
func (p Paths) FinalPath(w Worker) string { return p.finalOf(w.ID, w.Runs) }

func (p Paths) finalOf(id, run int) string {
	if run == 0 {
		return filepath.Join(p.RunDir(id), FinalFile)
	}
	return filepath.Join(p.RunDir(id), "final-"+strconv.Itoa(run)+".md")
}

// PrevFinal is the final message of w's run before this one (NewRun), if it
// wrote one: final-<Runs-1>.md, or final.md for the first run after a
// record from before runs were numbered; "" if none.
func (p Paths) PrevFinal(w Worker) string {
	if w.Runs == 0 {
		return ""
	}
	f := p.finalOf(w.ID, w.Runs-1)
	if _, err := os.Stat(f); err != nil {
		return ""
	}
	return f
}

// Result is what w's current run has to show (Paths.Current).
type Result struct {
	Final     string // the text of its final message
	FinalPath string // where that is; "" while this run has none
	Reply     string // the reply to its last follow-up (Turn)
	ReplyPath string // where that is: Turn's turn file; "" while Turn is 0
	Turn      int    // the follow-up Reply answers; 0 = none counts
}

// Path is the file holding r: the reply's when there is one, else the
// final message's; "" when r is empty.
func (r Result) Path() string {
	if r.Turn > 0 {
		return r.ReplyPath
	}
	return r.FinalPath
}

// Current is w's result in its current run, the one reader of it: its
// final message (FinalPath), and the reply to its last follow-up if that
// follow-up came in this run (after RunFrom) and the reply is newer than
// the final message. What an earlier run concluded is never in it: NewRun
// gives the next run a final file of its own and moves RunFrom up.
func (p Paths) Current(w Worker) Result {
	var r Result
	final := p.FinalPath(w)
	ff, ferr := os.Stat(final)
	if ferr == nil {
		b, _ := os.ReadFile(final)
		r.Final, r.FinalPath = string(b), final
	}
	if w.Replies <= w.RunFrom {
		return r
	}
	turn := p.TurnFile(w.ID, w.Replies)
	tf, err := os.Stat(turn)
	if err != nil || ferr == nil && ff.ModTime().After(tf.ModTime()) {
		return r
	}
	b, _ := os.ReadFile(turn)
	r.Reply, r.ReplyPath, r.Turn = string(b), turn, w.Replies
	return r
}

// HookLog is where worker id's Claude PermissionRequest hook appends.
func (p Paths) HookLog(id int) string { return filepath.Join(p.RunDir(id), HookFile) }
