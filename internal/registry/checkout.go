package registry

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A worker's own git worktree (Checkout): where it goes, making it, and
// removing it when retention drops its chain.

// worktreeTimeout bounds one git worktree command: making one checks out
// every file of the commit.
var worktreeTimeout = 5 * time.Minute

// Trees is where the workers' worktrees go.
func (p Paths) Trees() string { return filepath.Join(p.Dir, "trees") }

// Place names w's worktree: <Trees>/<chain>/<step>-<id> on branch
// routr/<chain>/<step>-<id> (a worker id is never given out twice, so
// neither is reused), and moves w's Cwd and Tree into it. w must have a
// Checkout.
func (p Paths) Place(w *Worker) {
	c := w.Checkout
	name := cmp.Or(w.Step, "worker") + "-" + strconv.Itoa(w.ID)
	c.Path = filepath.Join(p.Trees(), w.ChainRef(), name)
	c.Branch = "routr/" + w.ChainRef() + "/" + name
	w.Cwd, w.Tree = filepath.Join(c.Path, c.Sub), c.Path
}

// Head is the commit HEAD points at in cwd's checkout, and that
// checkout's top and cwd's place in it: what a flow's worktrees start
// from. Uncommitted changes are not in it.
func Head(cwd string) (Checkout, error) {
	top, err := git(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return Checkout{}, fmt.Errorf("%s is not in a git repo (or git is missing): %w", cwd, gitErr(err))
	}
	head, err := git(cwd, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Checkout{}, fmt.Errorf("the repo at %s has no commit yet: %w", top, gitErr(err))
	}
	c := Checkout{Base: head, Repo: top}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		if rel, err := filepath.Rel(top, real); err == nil && rel != "." && filepath.IsLocal(rel) {
			c.Sub = rel
		}
	}
	return c, nil
}

// Dirty reports whether cwd's checkout has uncommitted changes (to tracked
// files, or files not ignored), which a worktree made from its HEAD does
// not have. routr's own .routr directory does not count.
func Dirty(cwd string) bool {
	out, err := git(cwd, "status", "--porcelain", "--untracked-files=normal", "--", ":/", ":(top,exclude).routr")
	return err == nil && out != ""
}

// MakeWorktree makes c's worktree: `git worktree add -b Branch Path Base`
// in Repo. One that is already there as c's worktree (a watcher stopped
// after making it) is fine; anything else at Path, or a Branch that
// exists, is an error, and nothing is made.
func MakeWorktree(c Checkout) error {
	if c.Path == "" || c.Branch == "" || c.Base == "" || c.Repo == "" {
		return errors.New("its worktree is not named yet")
	}
	if _, err := os.Lstat(c.Path); err == nil {
		top, err1 := git(c.Path, "rev-parse", "--show-toplevel")
		branch, err2 := git(c.Path, "symbolic-ref", "--short", "HEAD")
		if err1 == nil && err2 == nil && samePath(top, c.Path) && branch == c.Branch {
			return nil
		}
		return fmt.Errorf("%s already exists and is not its worktree on %s; move it away, then routr cancel and start the flow again", c.Path, c.Branch)
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	_, err := gitLong(c.Repo, "worktree", "add", "-b", c.Branch, c.Path, c.Base)
	if err != nil && strings.Contains(err.Error(), ".lock") {
		// Another worktree being made at the same moment held a git lock.
		time.Sleep(500 * time.Millisecond)
		_, err = gitLong(c.Repo, "worktree", "add", "-b", c.Branch, c.Path, c.Base)
	}
	if err != nil {
		return fmt.Errorf("git worktree add -b %s %s %s in %s: %w", c.Branch, c.Path, c.Base, c.Repo, err)
	}
	return nil
}

// removeTrees removes the worktrees of dropped workers that lie under
// trees (an absolute path, else nothing is removed), best effort as
// removeRuns. It is conservative: `git worktree remove` without --force
// leaves a worktree with uncommitted or untracked changes where it is,
// and the branch is deleted only if nothing was committed on it (it still
// points at Base). Work routr cannot tell is merged is never thrown away.
func removeTrees(trees string, dropped []Worker) {
	if !filepath.IsAbs(trees) {
		return
	}
	parents := map[string]bool{}
	for _, w := range dropped {
		c := w.Checkout
		if c == nil || c.Path == "" || !within(trees, c.Path) || c.Path == trees {
			continue
		}
		parents[filepath.Dir(c.Path)] = true
		if _, err := os.Lstat(c.Path); err != nil {
			gitLong(c.Repo, "worktree", "prune") // removed by hand: drop git's record of it
		} else if _, err := gitLong(c.Repo, "worktree", "remove", c.Path); err != nil {
			continue // changes in it, or the repo is gone: leave it
		}
		if c.Branch != "" && c.Base != "" {
			gitLong(c.Repo, "update-ref", "-d", "refs/heads/"+c.Branch, c.Base)
		}
	}
	for d := range parents {
		if d != trees && within(trees, d) {
			os.Remove(d) // the chain's directory, once empty
		}
	}
}

// gitLong is git with worktreeTimeout and git's own message in the error.
func gitLong(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitErr adds git's message to an error from git.
func gitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}

// samePath reports whether a and b name the same directory.
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
