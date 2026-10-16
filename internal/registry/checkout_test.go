package registry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepo makes a git repo with one commit in a temp directory; git runs
// git there.
func gitRepo(t *testing.T) (dir string, git func(args ...string) string) {
	t.Helper()
	dir = t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "first")
	return dir, git
}

func TestHeadAndPlace(t *testing.T) {
	repo, git := gitRepo(t)
	os.Mkdir(filepath.Join(repo, "sub"), 0o755)
	c, err := Head(filepath.Join(repo, "sub"))
	if err != nil || c.Base != git("rev-parse", "HEAD") || c.Repo != repo || c.Sub != "sub" {
		t.Fatalf("head = %+v, %v", c, err)
	}
	if c, err := Head(repo); err != nil || c.Sub != "" {
		t.Errorf("head at the top = %+v, %v", c, err)
	}
	if _, err := Head(t.TempDir()); err == nil || !strings.Contains(err.Error(), "is not in a git repo") {
		t.Errorf("no repo: %v", err)
	}
	empty := t.TempDir()
	exec.Command("git", "-C", empty, "init", "-q").Run()
	if _, err := Head(empty); err == nil || !strings.Contains(err.Error(), "has no commit yet") {
		t.Errorf("no commit: %v", err)
	}
	os.MkdirAll(filepath.Join(repo, ".routr", "state"), 0o755)
	os.WriteFile(filepath.Join(repo, ".routr", "config.toml"), []byte("\n"), 0o644)
	if Dirty(filepath.Join(repo, "sub")) {
		t.Error("a clean repo is dirty (.routr counts)")
	}
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644)
	if !Dirty(filepath.Join(repo, "sub")) {
		t.Error("an untracked file is not dirty")
	}

	p := Paths{Dir: filepath.Join(repo, ".routr")}
	w := Worker{ID: 7, Chain: "best-1", Step: "try", Cwd: filepath.Join(repo, "sub"), Checkout: &c}
	p.Place(&w)
	path := filepath.Join(repo, ".routr", "trees", "best-1", "try-7")
	if c.Path != path || c.Branch != "routr/best-1/try-7" || w.Cwd != filepath.Join(path, "sub") || w.Tree != path || w.Worktree() != path {
		t.Errorf("placed: %+v, cwd %s tree %s", c, w.Cwd, w.Tree)
	}
}

func TestMakeWorktree(t *testing.T) {
	repo, git := gitRepo(t)
	base, err := Head(repo)
	if err != nil {
		t.Fatal(err)
	}
	// The main tree moves on: the worktree starts from base all the same.
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a2\n"), 0o644)
	git("commit", "-q", "-am", "second")
	p := Paths{Dir: filepath.Join(repo, ".routr")}
	w := Worker{ID: 3, Chain: "c", Step: "a", Checkout: &base}
	p.Place(&w)
	c := *w.Checkout
	if err := MakeWorktree(c); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitIn(t, c.Path, "rev-parse", "HEAD")); got != c.Base {
		t.Errorf("worktree HEAD %s, want base %s", got, c.Base)
	}
	if b, _ := os.ReadFile(filepath.Join(c.Path, "a.txt")); string(b) != "a\n" {
		t.Errorf("a.txt in the worktree = %q", b)
	}
	if err := MakeWorktree(c); err != nil {
		t.Errorf("again, as a watcher that stopped after making it: %v", err)
	}
	// Another worker's name on the same branch, or a directory in the way.
	other := c
	other.Path = filepath.Join(p.Trees(), "c", "x-9")
	if err := MakeWorktree(other); err == nil || !strings.Contains(err.Error(), "git worktree add -b routr/c/a-3") || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("branch exists: %v", err)
	}
	other = c
	other.Path, other.Branch = t.TempDir(), "routr/c/y-9"
	if err := MakeWorktree(other); err == nil || !strings.Contains(err.Error(), "already exists and is not its worktree") {
		t.Errorf("dir in the way: %v", err)
	}
	if err := MakeWorktree(Checkout{Base: c.Base, Repo: repo}); err == nil {
		t.Error("made one without a name")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// Retention removes a dropped chain's worktrees, but never work it cannot
// tell is merged: a worktree with changes stays, and so does a branch with
// commits on it.
func TestRetentionRemovesWorktrees(t *testing.T) {
	repo, git := gitRepo(t)
	p := Paths{Dir: filepath.Join(repo, ".routr"), Retention: Retention{Keep: time.Hour}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	now := t0
	s := p.Store()
	s.Now = clock(&now)
	base, err := Head(repo)
	if err != nil {
		t.Fatal(err)
	}
	made := func(step string) Worker {
		t.Helper()
		co := base
		id := seed(t, s, p, Worker{Chain: "old", Step: step, State: Done, Checkout: &co}, nil)
		w, err := s.Update(id, func(w *Worker) { p.Place(w); w.Checkout.Made = true })
		if err != nil {
			t.Fatal(err)
		}
		if err := MakeWorktree(*w.Checkout); err != nil {
			t.Fatal(err)
		}
		return w
	}
	clean, dirty, committed := made("clean"), made("dirty"), made("committed")
	os.WriteFile(filepath.Join(dirty.Checkout.Path, "new.txt"), []byte("wip\n"), 0o644)
	os.WriteFile(filepath.Join(committed.Checkout.Path, "a.txt"), []byte("better\n"), 0o644)
	gitIn(t, committed.Checkout.Path, "commit", "-q", "-am", "attempt")
	gone := made("gone")
	os.RemoveAll(gone.Checkout.Path) // removed by hand

	now = now.Add(2 * time.Hour)
	seed(t, s, p, Worker{State: Pending}, nil) // a write: retention drops chain old
	if got := ids(t, s); len(got) != 1 {
		t.Fatalf("kept %v", got)
	}
	branches := git("branch", "--list", "routr/*")
	for _, c := range []struct {
		w      Worker
		dir    bool
		branch bool
	}{{clean, false, false}, {dirty, true, true}, {committed, false, true}, {gone, false, false}} {
		_, err := os.Stat(c.w.Checkout.Path)
		if (err == nil) != c.dir {
			t.Errorf("%s: worktree there = %v, want %v", c.w.Step, err == nil, c.dir)
		}
		if strings.Contains(branches, c.w.Checkout.Branch) != c.branch {
			t.Errorf("%s: branch kept = %v, want %v (branches %q)", c.w.Step, !c.branch, c.branch, branches)
		}
	}
	if list := git("worktree", "list"); strings.Contains(list, "clean-") || strings.Contains(list, "gone-") || !strings.Contains(list, "dirty-") {
		t.Errorf("git worktree list:\n%s", list)
	}
}
