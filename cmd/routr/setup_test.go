package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// setupCLI is a cli whose HOME, PATH and binary are fakes under its temp
// dir: PATH holds an executable stub for each of tools, and the routr
// binary lives in a source tree with each front end's directory.
func setupCLI(t *testing.T, tools ...string) (c *cli, bin, src string) {
	t.Helper()
	c = newCLI(t)
	home, path := filepath.Join(c.dir, "home"), filepath.Join(c.dir, "path")
	src = filepath.Join(c.dir, "src")
	for _, d := range []string{home, path, filepath.Join(src, "frontend"), filepath.Join(src, "pi"), filepath.Join(src, "skills", "routr")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(path, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin = filepath.Join(src, "routr")
	if err := os.WriteFile(bin, []byte("routr"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.env["HOME"] = home
	c.env["PATH"] = path + ":" + filepath.Join(home, ".local", "bin")
	return c, bin, src
}

// runSetup runs routr setup as bin, with watchers looked for under proc.
func (c *cli) runSetup(bin, proc string, args ...string) (int, string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	a := c.app("", &out, &errb)
	a.bin, a.proc = bin, proc
	code := a.run(append([]string{"setup"}, args...))
	return code, strings.Join(strings.Fields(out.String()+errb.String()), " ")
}

func TestSetup(t *testing.T) {
	all := []string{"claude", "codex", "pi", "node", "herdr"}
	without := func(name string) []string {
		var out []string
		for _, t := range all {
			if t != name {
				out = append(out, t)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		tools   []string
		args    []string
		env     map[string]string
		code    int
		want    []string
		notWant []string
	}{
		{name: "every harness", tools: all, want: []string{
			"OK link linked {home}/.local/bin/routr → {bin}",
			"OK path {home}/.local/bin is on PATH",
			"OK config wrote {xdg}/routr/config.toml",
			"OK herdr reachable at /tmp/fake-herdr.sock: herdr 0.9.5, protocol 22",
			"OK claude found at {path}/claude → to add the Claude Code plugin, run: claude plugin marketplace add {src} && claude plugin install routr@routr",
			"OK codex found at {path}/codex → to add the routr skill, run: mkdir -p {home}/.codex/skills && ln -s {src}/skills/routr {home}/.codex/skills/routr",
			"OK pi found at {path}/pi → to add the Pi extension, run: pi install {src}/pi",
			"OK next routr is set up → from a herdr pane in a repo, run routr doctor",
		}, notWant: []string{"WARN", "FAIL", "OK plugin"}},
		{name: "no claude", tools: without("claude"), want: []string{"WARN claude claude is not on PATH: routr cannot start claude workers", "OK codex found"}, notWant: []string{"claude plugin"}},
		{name: "no codex", tools: without("codex"), want: []string{"WARN codex codex is not on PATH"}, notWant: []string{"ln -s"}},
		{name: "no pi", tools: without("pi"), want: []string{"WARN pi pi is not on PATH"}, notWant: []string{"pi install", "node"}},
		{name: "pi without node", tools: without("node"), want: []string{"WARN node node is not on PATH: the Pi extension needs Node 22.19 or newer"}},
		{name: "no harness", tools: []string{"herdr"}, want: []string{"WARN harnesses found none of claude, codex, pi"}},
		{name: "dry run", tools: all, args: []string{"--dry-run"}, want: []string{
			"OK link would link {home}/.local/bin/routr → {bin}", "OK config would write {xdg}/routr/config.toml",
			"OK next dry run: nothing was changed"}},
		{name: "bin dir off PATH", tools: all, args: []string{"--bin-dir", "{home}/bin"}, want: []string{
			"OK link linked {home}/bin/routr → {bin}", "WARN path {home}/bin is not on PATH"}},
		{name: "herdr outside a pane", tools: all, env: map[string]string{"HERDR_SOCKET_PATH": ""},
			want: []string{"OK herdr found at {path}/herdr; HERDR_SOCKET_PATH is unset"}},
		{name: "no herdr", tools: without("herdr"), env: map[string]string{"HERDR_SOCKET_PATH": ""},
			want: []string{"WARN herdr herdr is not on PATH: routr starts every worker in a herdr tab → install herdr " + herdr.MinVersion + " or later"}},
		{name: "plugin root", tools: all, env: map[string]string{"HERDR_PLUGIN_ROOT": "{c}/plugin"},
			want:    []string{"OK source {c}/plugin ($HERDR_PLUGIN_ROOT)", "WARN pi found at {path}/pi, but {c}/plugin has no pi/ to add the Pi extension from"},
			notWant: []string{"OK plugin", "FAIL plugin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, bin, src := setupCLI(t, tc.tools...)
			expand := strings.NewReplacer("{home}", c.env["HOME"], "{bin}", bin, "{src}", src, "{path}", filepath.Join(c.dir, "path"),
				"{xdg}", c.env["XDG_CONFIG_HOME"], "{c}", c.dir).Replace
			for k, v := range tc.env {
				c.env[k] = expand(v)
			}
			var args []string
			for _, a := range tc.args {
				args = append(args, expand(a))
			}
			code, out := c.runSetup(bin, "", args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, expand(w)) {
					t.Errorf("setup lacks %q:\n%s", expand(w), out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("setup has %q:\n%s", w, out)
				}
			}
			if tc.name == "dry run" {
				for _, p := range []string{filepath.Join(c.env["HOME"], ".local"), userConfig(func(k string) string { return c.env[k] })} {
					if _, err := os.Lstat(p); !os.IsNotExist(err) {
						t.Errorf("a dry run made %s (%v)", p, err)
					}
				}
			}
		})
	}
}

// A plugin install keeps the binary in bin/ under the tree that holds the
// front ends; run outside herdr's plugin env, setup still finds that tree.
func TestSetupFindsTheTreeAboveBin(t *testing.T) {
	c, _, src := setupCLI(t, "claude", "codex", "pi", "node", "herdr")
	bin := filepath.Join(src, "bin", "routr")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("routr"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := c.runSetup(bin, "")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if want := "OK source " + src + " (the tree around this binary)"; !strings.Contains(out, want) {
		t.Errorf("setup lacks %q:\n%s", want, out)
	}
	if want := "pi install " + src + "/pi"; !strings.Contains(out, want) {
		t.Errorf("setup lacks %q:\n%s", want, out)
	}
}

// pluginCLI is setupCLI with a herdr plugin install of routr at version
// ver, as herdr builds one: the tree under herdr's plugins directory, with
// the binary in bin/. It returns the binary and the install.
func pluginCLI(t *testing.T, c *cli, ver string) (bin, root string) {
	t.Helper()
	root = filepath.Join(c.env["XDG_CONFIG_HOME"], "herdr", "plugins", "github", "routr-"+ver)
	for _, d := range []string{"bin", "frontend", "pi", filepath.Join("skills", "routr")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin = filepath.Join(root, "bin", "routr")
	if err := os.WriteFile(bin, []byte("routr"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.env["HERDR_PLUGIN_ROOT"] = root
	return bin, root
}

// herdr builds each plugin update in a new directory and removes the old
// one, so setup links the install to a path of routr's own and adds the
// front ends from that; an update repoints it.
func TestSetupLinksAPluginInstall(t *testing.T) {
	c, _, _ := setupCLI(t, "claude", "codex", "pi", "node", "herdr")
	link := filepath.Join(c.env["HOME"], ".local", "share", "routr", "plugin")
	codex := filepath.Join(c.env["HOME"], ".codex", "skills", "routr")
	binA, a := pluginCLI(t, c, "aaaa")
	code, out := c.runSetup(binA, "")
	for _, want := range []string{
		"OK plugin linked " + link + " → " + a + ": the front ends are added from it, and plugin updates repoint it",
		"claude plugin marketplace add " + link + " && claude plugin install routr@routr",
		"ln -s " + link + "/skills/routr " + codex,
		"pi install " + link + "/pi",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("exit %d; setup lacks %q:\n%s", code, want, out)
		}
	}
	if got, _ := os.Readlink(link); got != a {
		t.Errorf("link = %q, want %q", got, a)
	}

	// The skill linked as setup says is reported as linked; linked straight
	// into the install, it would break on the next update.
	os.MkdirAll(filepath.Dir(codex), 0o755)
	os.Symlink(link+"/skills/routr", codex)
	if _, out := c.runSetup(binA, ""); !strings.Contains(out, "OK codex found at "+filepath.Join(c.dir, "path", "codex")+"; the routr skill is linked into") {
		t.Errorf("codex linked through the link:\n%s", out)
	}
	os.Remove(codex)
	os.Symlink(a+"/skills/routr", codex)
	if _, out := c.runSetup(binA, ""); !strings.Contains(out, "WARN codex found at "+filepath.Join(c.dir, "path", "codex")+", but "+codex+" links to "+a+"/skills/routr, in a herdr install of routr that its next update removes → to link the path that follows updates, run: ln -sfn "+link+"/skills/routr "+codex) {
		t.Errorf("codex linked into the install:\n%s", out)
	}

	// An update: setup from the new install repoints the link, and a skill
	// linked into the removed one is reported as gone.
	binB, b := pluginCLI(t, c, "bbbb")
	if code, out := c.runSetup(binB, ""); code != 0 || !strings.Contains(out, "OK plugin repointed "+link+" from "+a+" to "+b) {
		t.Errorf("update: exit %d\n%s", code, out)
	}
	if got, _ := os.Readlink(link); got != b {
		t.Errorf("link = %q after the update, want %q", got, b)
	}
	os.RemoveAll(a)
	if _, out := c.runSetup(binB, ""); !strings.Contains(out, "links to "+a+"/skills/routr, which is gone: herdr removed that install when it updated routr → to link the path that follows updates, run: ln -sfn "+link+"/skills/routr "+codex) {
		t.Errorf("codex link dangling:\n%s", out)
	}
	if _, out := c.runSetup(binB, ""); !strings.Contains(out, "OK plugin "+link+" links to "+b+" already") {
		t.Errorf("second run:\n%s", out)
	}
}

// What is at the link and is not routr's is left alone; that fails setup,
// and the front ends are added from the install itself.
func TestSetupLeavesAPluginLinkAlone(t *testing.T) {
	c, _, _ := setupCLI(t, "pi", "node", "herdr")
	link := filepath.Join(c.env["HOME"], ".local", "share", "routr", "plugin")
	bin, root := pluginCLI(t, c, "aaaa")
	os.MkdirAll(link, 0o755)
	code, out := c.runSetup(bin, "")
	if code != exitError || !strings.Contains(out, "FAIL plugin "+link+" exists and is not a symlink; left alone → move it aside") || !strings.Contains(out, "pi install "+root+"/pi") {
		t.Errorf("a directory: exit %d\n%s", code, out)
	}
	if fi, err := os.Lstat(link); err != nil || !fi.IsDir() {
		t.Errorf("%s was replaced (%v)", link, err)
	}
	mine := filepath.Join(c.dir, "mine")
	os.Remove(link)
	os.Symlink(mine, link)
	code, out = c.runSetup(bin, "")
	if code != exitError || !strings.Contains(out, "FAIL plugin "+link+" links to "+mine+", not to a herdr install of routr; left alone") || !strings.Contains(out, "pi install "+root+"/pi") {
		t.Errorf("a link of the user's: exit %d\n%s", code, out)
	}
	if got, _ := os.Readlink(link); got != mine {
		t.Errorf("link = %q, want %q", got, mine)
	}
}

// A dry run makes no link, but names the commands by it.
func TestSetupDryRunPluginLink(t *testing.T) {
	c, _, _ := setupCLI(t, "pi", "node", "herdr")
	link := filepath.Join(c.env["HOME"], ".local", "share", "routr", "plugin")
	binA, a := pluginCLI(t, c, "aaaa")
	code, out := c.runSetup(binA, "", "--dry-run")
	if code != 0 || !strings.Contains(out, "OK plugin would link "+link+" → "+a) || !strings.Contains(out, "pi install "+link+"/pi") || strings.Contains(out, "WARN pi") {
		t.Errorf("dry run: exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(c.env["HOME"], ".local", "share")); !os.IsNotExist(err) {
		t.Errorf("a dry run made the link (%v)", err)
	}
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(a, link)
	binB, b := pluginCLI(t, c, "bbbb")
	if code, out := c.runSetup(binB, "", "--dry-run"); code != 0 || !strings.Contains(out, "OK plugin would repoint "+link+" from "+a+" to "+b) {
		t.Errorf("dry run repoint: exit %d\n%s", code, out)
	}
	if got, _ := os.Readlink(link); got != a {
		t.Errorf("a dry run repointed the link to %q", got)
	}
	c.env["XDG_DATA_HOME"] = filepath.Join(c.dir, "data")
	if _, out := c.runSetup(binB, "", "--dry-run"); !strings.Contains(out, "would link "+filepath.Join(c.dir, "data", "routr", "plugin")+" → "+b) {
		t.Errorf("XDG_DATA_HOME:\n%s", out)
	}
}

// A second run finds everything in place and changes nothing.
func TestSetupIsIdempotent(t *testing.T) {
	c, bin, _ := setupCLI(t, "claude", "codex", "pi", "node", "herdr")
	link := filepath.Join(c.env["HOME"], ".local", "bin", "routr")
	if code, out := c.runSetup(bin, ""); code != 0 {
		t.Fatalf("first run: exit %d\n%s", code, out)
	}
	cfg := userConfig(func(k string) string { return c.env[k] })
	os.WriteFile(cfg, []byte("# mine\n"), 0o600)
	code, out := c.runSetup(bin, "")
	if code != 0 || !strings.Contains(out, "OK link "+link+" links to "+bin+" already") || !strings.Contains(out, "OK config "+cfg+" exists; left alone") {
		t.Errorf("second run: exit %d\n%s", code, out)
	}
	if got, _ := os.Readlink(link); got != bin {
		t.Errorf("link = %q", got)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "# mine\n" {
		t.Errorf("config rewritten: %q", b)
	}
	// A link to another routr is repointed; one already ours is left.
	other := filepath.Join(c.dir, "other-routr")
	os.WriteFile(other, nil, 0o755)
	os.Remove(link)
	os.Symlink(other, link)
	if code, out := c.runSetup(bin, ""); code != 0 || !strings.Contains(out, "repointed "+link+" from "+other+" to "+bin) {
		t.Errorf("repoint: exit %d\n%s", code, out)
	}
	if got, _ := os.Readlink(link); got != bin {
		t.Errorf("link = %q after repointing", got)
	}
	// The codex skill, once linked, is reported as linked.
	skills := filepath.Join(c.env["HOME"], ".codex", "skills")
	os.MkdirAll(skills, 0o755)
	os.Symlink(filepath.Join(filepath.Dir(bin), "skills", "routr"), filepath.Join(skills, "routr"))
	if _, out := c.runSetup(bin, ""); !strings.Contains(out, "the routr skill is linked into "+skills) {
		t.Errorf("codex linked:\n%s", out)
	}
}

// A file that is not a symlink is never replaced; that fails setup. The
// binary itself in the bin dir (install.sh's case) is fine.
func TestSetupLeavesAFileAlone(t *testing.T) {
	c, bin, src := setupCLI(t, "claude", "herdr")
	dir := filepath.Join(c.env["HOME"], ".local", "bin")
	os.MkdirAll(dir, 0o755)
	dest := filepath.Join(dir, "routr")
	os.WriteFile(dest, []byte("someone else's"), 0o755)
	code, out := c.runSetup(bin, "")
	if code != exitError || !strings.Contains(out, "FAIL link "+dest+" exists and is not a symlink; left alone → move it aside") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(dest); string(b) != "someone else's" {
		t.Errorf("%s was replaced", dest)
	}
	if code, out := c.runSetup(bin, "", "--bin-dir", src); code != 0 || !strings.Contains(out, "OK link "+bin+" is this binary") {
		t.Errorf("binary in the bin dir: exit %d\n%s", code, out)
	}
}

// An older herdr fails doctor but only warns in setup: what setup puts in
// place is there all the same.
func TestSetupWarnsOfAnOldHerdr(t *testing.T) {
	c, bin, _ := setupCLI(t, "claude", "herdr")
	c.server = herdr.Server{Version: "0.8.2", Protocol: 20}
	if code, out := c.runSetup(bin, ""); code != 0 || !strings.Contains(out, "WARN herdr herdr 0.8.2 at /tmp/fake-herdr.sock speaks protocol 20") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// Watchers on a replaced binary are found through /proc; one with a worker
// in flight is left running.
func TestStaleWatchers(t *testing.T) {
	c, bin, _ := setupCLI(t, "claude", "herdr")
	proc := filepath.Join(c.dir, "proc")
	idle := filepath.Join(c.dir, "idle")
	os.MkdirAll(idle, 0o755)
	c.seed(registry.Worker{Harness: "claude", Brief: "x", State: registry.Running})
	fake := func(pid, exe, cmdline, cwd, state string) {
		d := filepath.Join(proc, pid)
		os.MkdirAll(d, 0o755)
		os.Symlink(exe, filepath.Join(d, "exe"))
		os.Symlink(cwd, filepath.Join(d, "cwd"))
		os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.ReplaceAll(cmdline, " ", "\x00")+"\x00"), 0o644)
		os.WriteFile(filepath.Join(d, "environ"), []byte("HOME=/h\x00ROUTR_STATE_DIR="+state+"\x00"), 0o644)
	}
	fake("101", bin+" (deleted)", "routr watch --foreground", c.dir, c.p.Dir)                      // busy
	fake("102", bin+" (deleted)", "routr watch --foreground", idle, filepath.Join(idle, ".routr")) // idle
	fake("103", bin, "routr watch --foreground", idle, "")                                         // current binary
	fake("104", bin+" (deleted)", "routr list", idle, "")                                          // not a watcher
	os.MkdirAll(filepath.Join(proc, "self"), 0o755)

	got := staleWatchers(proc, bin)
	if len(got) != 2 || got[0].pid != 101 || !got[0].busy || got[1].pid != 102 || got[1].busy || got[1].dir != idle {
		t.Fatalf("stale watchers = %+v", got)
	}
	code, out := c.runSetup(bin, proc, "--dry-run")
	for _, want := range []string{
		"WARN watcher the watcher for " + c.dir + " (pid 101) runs the old routr and has workers in flight; left running → it switches once it idles out, or stop it when they finish: kill 101",
		"OK watcher would restart the watcher for " + idle + " (pid 102)",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("exit %d; setup lacks %q:\n%s", code, want, out)
		}
	}
}

// Under a herdr plugin install, the next step to a newer routr is herdr's.
func TestUpdateHintForAPluginInstall(t *testing.T) {
	c := newCLI(t)
	for _, tc := range []struct{ src, want string }{
		{filepath.Join(c.env["XDG_CONFIG_HOME"], "herdr", "plugins", "github", "routr-0a1b"), "run herdr plugin update routr"},
		{"/src/routr", "run /src/routr/install.sh"},
		{"", "rebuild routr from its source"},
	} {
		c.build = buildInfo{Src: tc.src}
		var out, errb bytes.Buffer
		if got := c.app("", &out, &errb).updateHint(); got != tc.want {
			t.Errorf("src %q: hint %q, want %q", tc.src, got, tc.want)
		}
	}
}
