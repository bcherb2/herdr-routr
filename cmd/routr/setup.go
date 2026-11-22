package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// pluginRootEnv is the variable herdr sets for a plugin's build and actions:
// the directory the plugin is installed in.
const pluginRootEnv = "HERDR_PLUGIN_ROOT"

// setup puts routr's own pieces in place: this binary on PATH, the user
// config, and the watchers on an old binary restarted. It checks herdr and
// the harnesses and prints the command that adds each front end, but it
// installs and registers nothing outside routr. It exits 1 only when one of
// its own pieces could not be put in place.
func (a *app) setup(args []string) error {
	fs, c := a.flags("setup")
	binDir := fs.String("bin-dir", "", "directory to link routr into (default ~/.local/bin)")
	dry := fs.Bool("dry-run", false, "print what setup would do, and change nothing")
	socket := fs.String("socket", "", "herdr socket (default $HERDR_SOCKET_PATH)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	var checks []check
	add := func(name, status, detail, fix string) {
		checks = append(checks, check{name, status, detail, fix})
	}
	// A herdr or a harness setup cannot reach fails doctor, not setup: what
	// setup puts in place is there all the same.
	soft := func(name, status, detail, fix string) {
		if status == "fail" {
			status = "warn"
		}
		add(name, status, detail, fix)
	}

	root, from := a.sourceRoot()
	if root != "" {
		add("source", "ok", root+" ("+from+")", "")
	}
	dir := *binDir
	if dir == "" {
		if home := a.getenv("HOME"); home != "" {
			dir = filepath.Join(home, ".local", "bin")
		}
	}
	a.linkBinary(add, dir, *dry)
	at := root
	if a.pluginInstall(root) {
		at = a.linkPlugin(add, root, *dry)
	}
	a.setupConfig(add, *dry)
	if s := first(*socket, a.getenv(herdr.SocketEnv)); s != "" {
		a.checkHerdr(soft, s, false)
	} else if p, ok := a.lookPath("herdr"); ok {
		add("herdr", "ok", fmt.Sprintf("found at %s; %s is unset, so its server is not checked from here", p, herdr.SocketEnv),
			"routr doctor from a herdr pane checks it")
	} else {
		add("herdr", "warn", "herdr is not on PATH: routr starts every worker in a herdr tab",
			fmt.Sprintf("install herdr %s or later (https://herdr.dev)", herdr.MinVersion))
	}
	a.frontEnds(add, root, at)
	a.restartWatchers(add, a.proc, *dry)
	if *dry {
		add("next", "ok", "dry run: nothing was changed", "run routr setup without --dry-run")
	} else {
		add("next", "ok", "routr is set up", "from a herdr pane in a repo, run routr doctor")
	}
	return a.report(c, checks)
}

// sourceRoot is the tree the front ends are read from, and where it came
// from: $HERDR_PLUGIN_ROOT when herdr runs setup for its plugin, else the
// checkout install.sh built this binary from, else the tree around a binary
// in bin/ (where the plugin's build step puts it), else this binary's directory.
func (a *app) sourceRoot() (root, from string) {
	switch {
	case a.getenv(pluginRootEnv) != "":
		return a.getenv(pluginRootEnv), "$" + pluginRootEnv
	case a.build.Src != "":
		return a.build.Src, "the checkout this binary was built from"
	case a.bin != "":
		dir := filepath.Dir(a.bin)
		if up := filepath.Dir(dir); filepath.Base(dir) == "bin" && isDir(filepath.Join(up, "skills", "routr")) {
			return up, "the tree around this binary"
		}
		return dir, "this binary's directory"
	}
	return "", ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// linkBinary makes dir/routr a symlink to this binary: created, repointed,
// or left as it is. A file there that is not a symlink is never replaced.
func (a *app) linkBinary(add func(name, status, detail, fix string), dir string, dry bool) {
	if dir == "" {
		add("link", "fail", "HOME is unset, so there is no default bin directory", "pass --bin-dir DIR")
		return
	}
	if a.bin == "" {
		add("link", "fail", "cannot tell where this routr binary is", "run setup by the binary's full path")
		return
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		add("link", "fail", err.Error(), "pass --bin-dir DIR")
		return
	}
	dest := filepath.Join(dir, "routr")
	fi, err := os.Lstat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !dry {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				add("link", "fail", err.Error(), "pass --bin-dir DIR")
				return
			}
			if err := os.Symlink(a.bin, dest); err != nil {
				add("link", "fail", err.Error(), "pass --bin-dir DIR")
				return
			}
		}
		add("link", "ok", fmt.Sprintf("%s %s → %s", pick(dry, "would link", "linked"), dest, a.bin), "")
	case err != nil:
		add("link", "fail", err.Error(), "pass --bin-dir DIR")
		return
	case fi.Mode()&fs.ModeSymlink != 0:
		old, _ := os.Readlink(dest)
		if resolved, _ := filepath.EvalSymlinks(dest); resolved == a.bin {
			add("link", "ok", fmt.Sprintf("%s links to %s already", dest, a.bin), "")
			break
		}
		if !dry {
			if err := replaceLink(a.bin, dest); err != nil {
				add("link", "fail", err.Error(), "pass --bin-dir DIR")
				return
			}
		}
		add("link", "ok", fmt.Sprintf("%s %s from %s to %s", pick(dry, "would repoint", "repointed"), dest, old, a.bin), "")
	default:
		if self, err := os.Stat(a.bin); err == nil && os.SameFile(fi, self) {
			add("link", "ok", dest+" is this binary", "")
			break
		}
		add("link", "fail", dest+" exists and is not a symlink; left alone",
			"move it aside and run routr setup again, or pass --bin-dir DIR")
		return
	}
	a.checkOnPath(add, dir, dest)
}

// pluginLink is the path a herdr plugin install of routr is linked to, so
// that the front ends added from it outlive an update: herdr builds each
// version in a new directory and removes the old one, and the plugin's
// build step (scripts/plugin-build.sh) repoints this link to the new one.
func (a *app) pluginLink() string {
	if d := a.getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "routr", "plugin")
	}
	if h := a.getenv("HOME"); h != "" {
		return filepath.Join(h, ".local", "share", "routr", "plugin")
	}
	return ""
}

// linkPlugin makes pluginLink a symlink to root, a herdr plugin install, and
// returns the path to add the front ends from: the link, or root when it
// could not be made. A file there that is not a symlink is never replaced,
// nor a link that points anywhere but a herdr install of routr.
func (a *app) linkPlugin(add func(name, status, detail, fix string), root string, dry bool) string {
	link := a.pluginLink()
	if link == "" {
		add("plugin", "fail", "HOME is unset, so there is no path to link this plugin install to", "set HOME and run routr setup again")
		return root
	}
	why := "the front ends are added from it, and plugin updates repoint it"
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !dry {
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				add("plugin", "fail", err.Error(), "fix it, or set XDG_DATA_HOME, and run routr setup again")
				return root
			}
			if err := os.Symlink(root, link); err != nil {
				add("plugin", "fail", err.Error(), "fix it, or set XDG_DATA_HOME, and run routr setup again")
				return root
			}
		}
		add("plugin", "ok", fmt.Sprintf("%s %s → %s: %s", pick(dry, "would link", "linked"), link, root, why), "")
	case err != nil:
		add("plugin", "fail", err.Error(), "fix it, or set XDG_DATA_HOME, and run routr setup again")
		return root
	case fi.Mode()&fs.ModeSymlink != 0:
		old, _ := os.Readlink(link)
		if linksTo(link, root) {
			add("plugin", "ok", fmt.Sprintf("%s links to %s already: %s", link, root, why), "")
			break
		}
		if !a.pluginInstall(old) {
			add("plugin", "fail", fmt.Sprintf("%s links to %s, not to a herdr install of routr; left alone", link, old),
				"remove it and run routr setup again")
			return root
		}
		if !dry {
			if err := replaceLink(root, link); err != nil {
				add("plugin", "fail", err.Error(), "fix it, or set XDG_DATA_HOME, and run routr setup again")
				return root
			}
		}
		add("plugin", "ok", fmt.Sprintf("%s %s from %s to %s: %s", pick(dry, "would repoint", "repointed"), link, old, root, why), "")
	default:
		add("plugin", "fail", link+" exists and is not a symlink; left alone", "move it aside and run routr setup again")
		return root
	}
	return link
}

func pick(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

// replaceLink points the symlink dest at target in one rename, so that
// nothing ever finds dest missing.
func replaceLink(target, dest string) error {
	tmp := filepath.Join(filepath.Dir(dest), ".routr-link-"+strconv.Itoa(os.Getpid()))
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// checkOnPath warns when dir is not on PATH, or when another routr comes
// before dest there.
func (a *app) checkOnPath(add func(name, status, detail, fix string), dir, dest string) {
	if !slices.ContainsFunc(filepath.SplitList(a.getenv("PATH")), func(d string) bool { return d != "" && sameDir(d, dir) }) {
		add("path", "warn", dir+" is not on PATH, so a shell or harness does not find routr by name",
			fmt.Sprintf("add it in your shell's startup file: export PATH=%s:$PATH", shellQuote(dir)))
		return
	}
	p, ok := a.lookPath("routr")
	if !ok || p == dest {
		add("path", "ok", dir+" is on PATH", "")
		return
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved == a.bin {
		add("path", "ok", dir+" is on PATH, and "+p+" before it is this routr too", "")
		return
	}
	add("path", "warn", fmt.Sprintf("the routr found first on PATH is %s, not %s", p, dest),
		"remove "+p+", or put "+dir+" before its directory in PATH")
}

func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// lookPath finds an executable on this invocation's PATH.
func (a *app) lookPath(name string) (string, bool) {
	for _, d := range filepath.SplitList(a.getenv("PATH")) {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// setupConfig is routr config init.
func (a *app) setupConfig(add func(name, status, detail, fix string), dry bool) {
	user := userConfig(a.getenv)
	if dry {
		if exists(user) {
			add("config", "ok", user+" exists; it would be left alone", "")
		} else {
			add("config", "ok", "would write "+user+", every key commented out", "")
		}
		return
	}
	created, err := a.initConfig()
	switch {
	case err != nil:
		add("config", "fail", err.Error(), "fix it, or set XDG_CONFIG_HOME, and run routr setup again")
	case created:
		add("config", "ok", "wrote "+user+": every key commented out; uncomment one to change it", "")
	default:
		add("config", "ok", user+" exists; left alone", "routr config example prints the template")
	}
}

// frontEnds checks each harness and names the command that adds routr's
// front end to it from root, by the path at: root, or the link to it that
// follows plugin updates. Each front end ships the routr skill.
func (a *app) frontEnds(add func(name, status, detail, fix string), root, at string) {
	codexHome := first(a.getenv("CODEX_HOME"), filepath.Join(a.getenv("HOME"), ".codex"))
	type frontEnd struct {
		dir, what string // dir under root; what it is
		cmd       func(dir string) string
	}
	ends := map[string]frontEnd{
		"claude": {"frontend", "the Claude Code plugin", func(string) string {
			return "claude plugin marketplace add " + shellQuote(at) + " && claude plugin install routr@routr"
		}},
		"codex": {"skills/routr", "the routr skill", func(d string) string {
			skills := filepath.Join(codexHome, "skills")
			return "mkdir -p " + shellQuote(skills) + " && ln -s " + shellQuote(d) + " " + shellQuote(filepath.Join(skills, "routr"))
		}},
		"pi": {"pi", "the Pi extension", func(d string) string { return "pi install " + shellQuote(d) }},
	}
	skill := filepath.Join(codexHome, "skills", "routr")
	old := linkTarget(skill)
	found := 0
	for _, h := range config.Harnesses {
		p, ok := a.lookPath(h)
		if !ok {
			add(h, "warn", h+" is not on PATH: routr cannot start "+h+" workers", "install it and log in if you want them; setup installs nothing")
			continue
		}
		found++
		fe := ends[h]
		d := filepath.Join(at, fe.dir)
		switch {
		case root == "" || !exists(filepath.Join(root, fe.dir)):
			add(h, "warn", fmt.Sprintf("found at %s, but %s has no %s/ to add %s from", p, root, fe.dir, fe.what),
				"run setup from a routr checkout's binary (install.sh builds one) or from the herdr plugin")
		case h == "codex" && old != d && a.pluginInstall(old):
			// A link straight into a herdr install breaks when an update
			// removes that install.
			add(h, "warn", fmt.Sprintf("found at %s, but %s links to %s, %s", p, skill, old,
				pick(exists(old), "in a herdr install of routr that its next update removes", "which is gone: herdr removed that install when it updated routr")),
				pick(at != root, "to link the path that follows updates, run: ", "to relink it, run: ")+"ln -sfn "+shellQuote(d)+" "+shellQuote(skill))
		case h == "codex" && linksTo(skill, d):
			add(h, "ok", fmt.Sprintf("found at %s; %s is linked into %s", p, fe.what, filepath.Join(codexHome, "skills")), "")
		default:
			add(h, "ok", "found at "+p, "to add "+fe.what+", run: "+fe.cmd(d))
		}
		if h == "pi" {
			if _, ok := a.lookPath("node"); !ok {
				add("node", "warn", "node is not on PATH: the Pi extension needs Node 22.19 or newer", "install Node before adding the extension")
			}
		}
	}
	if found == 0 {
		add("harnesses", "warn", "found none of "+strings.Join(config.Harnesses, ", ")+": routr has nothing to start workers on",
			"install one and log in to it, then run routr setup again")
	}
}

// linkTarget is where the symlink link points, made absolute, or "" when
// it is not a symlink.
func linkTarget(link string) string {
	t, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(link), t)
	}
	return filepath.Clean(t)
}

func linksTo(link, target string) bool {
	l, err1 := filepath.EvalSymlinks(link)
	t, err2 := filepath.EvalSymlinks(target)
	return err1 == nil && err2 == nil && l == t
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./+-:=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// staleWatcher is a watcher still running a binary that has been replaced
// at this binary's path.
type staleWatcher struct {
	pid  int
	dir  string   // its working directory
	env  []string // its environment, which workers' tabs inherit
	busy bool     // a worker is in flight
}

// staleWatchers finds them under proc (/proc). A watcher runs until it has
// been idle for 10m, so after an update it can keep running the old binary.
// None are found where there is no proc (macOS).
func staleWatchers(proc, bin string) []staleWatcher {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	var out []staleWatcher
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		at := func(f string) string { return filepath.Join(proc, e.Name(), f) }
		if exe, _ := os.Readlink(at("exe")); exe != bin+" (deleted)" {
			continue
		}
		cmdline, _ := os.ReadFile(at("cmdline"))
		if !strings.Contains(" "+string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})), " watch --foreground ") {
			continue
		}
		dir, err := os.Readlink(at("cwd"))
		if err != nil {
			continue
		}
		environ, _ := os.ReadFile(at("environ"))
		env := strings.FieldsFunc(string(environ), func(r rune) bool { return r == 0 })
		out = append(out, staleWatcher{pid: pid, dir: dir, env: env, busy: inFlight(dir, env)})
	}
	return out
}

// inFlight reports whether the repo a watcher in dir drives has a worker
// mid-turn: the watcher keeps what it has seen of its pane in memory.
func inFlight(dir string, env []string) bool {
	getenv := func(k string) string {
		for _, kv := range slices.Backward(env) {
			if v, ok := strings.CutPrefix(kv, k+"="); ok {
				return v
			}
		}
		return ""
	}
	p, err := registry.Find(dir, getenv)
	if err != nil {
		return true
	}
	p.Retention = registry.Retention{Off: true}
	ws, err := p.Store().List()
	if err != nil {
		return true
	}
	return slices.ContainsFunc(ws, func(w registry.Worker) bool {
		switch w.State {
		case registry.Starting, registry.Running, registry.Blocked, registry.MaybeDone:
			return true
		}
		return false
	})
}

// restartWatchers stops each idle watcher on an old binary and starts it
// again, from its directory and with its environment. One with a worker in
// flight is left to finish.
func (a *app) restartWatchers(add func(name, status, detail, fix string), proc string, dry bool) {
	if proc == "" {
		return
	}
	for _, w := range staleWatchers(proc, a.bin) {
		switch {
		case w.busy:
			add("watcher", "warn", fmt.Sprintf("the watcher for %s (pid %d) runs the old routr and has workers in flight; left running", w.dir, w.pid),
				fmt.Sprintf("it switches once it idles out, or stop it when they finish: kill %d", w.pid))
		case dry:
			add("watcher", "ok", fmt.Sprintf("would restart the watcher for %s (pid %d), which runs the old routr", w.dir, w.pid), "")
		default:
			if err := a.restartWatcher(w); err != nil {
				add("watcher", "warn", fmt.Sprintf("the watcher for %s (pid %d) runs the old routr: %v", w.dir, w.pid, err),
					"the next routr command there uses it; it switches once it idles out")
				continue
			}
			add("watcher", "ok", "restarted the watcher for "+w.dir+" on this routr", "")
		}
	}
}

func (a *app) restartWatcher(w staleWatcher) error {
	if err := syscall.Kill(w.pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("could not stop it: %w", err)
	}
	stopped := false
	for range 50 {
		if syscall.Kill(w.pid, 0) != nil {
			stopped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !stopped {
		return errors.New("it did not stop")
	}
	cmd := exec.Command(a.bin, "watch")
	cmd.Dir, cmd.Env = w.dir, w.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not start it again: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
