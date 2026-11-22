package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/watch"
)

// check is one line of `routr doctor`.
type check struct {
	Check  string `json:"check"`
	Status string `json:"status"` // ok, warn or fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"` // the next step, for warn and fail
}

// doctor checks what routr needs, one line per check with the next step for
// each problem; it exits 1 if any check fails.
func (a *app) doctor(args []string) error {
	fs, c := a.flags("doctor")
	socket := fs.String("socket", "", "herdr socket (default $HERDR_SOCKET_PATH)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a.toldUnknown = true // doctor reports unknown keys as a check, not on stderr
	var checks []check
	add := func(name, status, detail, fix string) {
		checks = append(checks, check{name, status, detail, fix})
	}
	p, err := a.paths(c)
	if err != nil {
		add("repo", "fail", err.Error(), "run routr in the repo, or pass --root DIR")
		return a.report(c, checks)
	}
	// Probing the watcher and registry creates .routr; doctor leaves a
	// directory routr has not been used in as it found it.
	_, err = os.Stat(p.Dir)
	fresh := errors.Is(err, os.ErrNotExist)
	if fresh {
		add("repo", "ok", fmt.Sprintf("%s (no %s yet; routr start creates it)", p.Root(), p.Dir), "")
	} else {
		add("repo", "ok", fmt.Sprintf("%s (state in %s)", p.Root(), p.Dir), "")
	}

	files := userConfig(a.getenv) + ", " + p.Config()
	cfg, err := a.applyConfig(p) // its [env] reaches the readiness checks
	if err != nil {
		add("config", "fail", err.Error(), "fix the file it names ("+files+")")
	} else {
		if len(cfg.Unknown) > 0 {
			add("config", "warn", "unknown keys, ignored: "+strings.Join(cfg.Unknown, "; "),
				"fix a typo, or remove a key this routr ("+a.build.short()+") doesn't know; a newer routr may")
		} else {
			add("config", "ok", "loaded "+files+" (each optional)", "")
		}
		if m, err := cfg.MainAgent(a.getenv); err != nil {
			add("main", "fail", err.Error(), "fix [main] in "+files+", or the ROUTR_MAIN_* variable it names")
		} else {
			add("main", "ok", mainSummary(m), "")
		}
		if s, warn := fallbackSummary(cfg); warn {
			add("fallback", "warn", s, "reorder [[fallback]] in "+files+": the first rule that matches wins")
		} else {
			add("fallback", "ok", s, "")
		}
	}

	a.checkBuild(add)

	running := !fresh && watch.Running(p)
	a.checkHerdr(add, first(*socket, a.getenv(herdr.SocketEnv)), running)
	if running {
		add("watcher", "ok", "running (log: "+p.WatchLog()+")", "")
	} else {
		add("watcher", "ok", "not running; the next command that queues work starts it", "")
	}

	var ready []string
	for _, h := range config.Harnesses {
		if err := a.ready(a.ctx, h); err != nil {
			add(h, "warn", err.Error(), "install or log in to "+h+" to run workers on it")
			continue
		}
		add(h, "ok", "installed and logged in", "")
		ready = append(ready, h)
	}
	if len(ready) == 0 {
		add("harnesses", "fail", "no harness can run workers", "install and log in to claude, codex or pi")
	}
	a.checkPi(add, p.Root())
	if fresh {
		add("registry", "ok", "none yet", "")
	} else if ws, err := p.Store().List(); err != nil {
		add("registry", "fail", err.Error(), "")
	} else {
		active := 0
		for _, w := range ws {
			if w.Active() {
				active++
			}
		}
		add("registry", "ok", fmt.Sprintf("%d workers, %d active (%s)", len(ws), active, p.Registry()), "")
	}
	for _, aside := range p.SetAside() {
		add("registry", "warn", "an unreadable registry was set aside as "+aside+"; the workers it held are no longer tracked",
			"look at it (their tabs may still be open), then delete it")
	}

	if first(cfg.Env[watch.JevKeyEnv], a.getenv(watch.JevKeyEnv)) != "" {
		add("jev", "ok", watch.JevKeyEnv+" is set: judge steps can ask Jev", "")
	} else {
		add("jev", "warn", watch.JevKeyEnv+" is not set: a judge step (judge = \"jev\") needs you instead of deciding",
			"export it where routr runs, or set it in config [env]")
	}

	caller := config.DetectCaller(a.getenv)
	if caller != "" {
		add("caller", "ok", strings.TrimSpace(caller+" "+config.CallerSession(caller, a.getenv)), "")
	} else {
		add("caller", "ok", "no agent detected (a shell): roles that avoid the caller's harness avoid none", "")
	}
	why := "no flag: each harness's own settings decide what it asks"
	switch {
	case cfg.Permissions != "":
		why = "config permissions"
	case config.CallerPermissions(caller) != "":
		why = "the mode " + caller + ", the caller, runs in"
	}
	add("permissions", "ok", fmt.Sprintf("%s for workers started without --permissions (%s)", cfg.PermissionsFor(caller), why), "")
	return a.report(c, checks)
}

// checkHerdr checks that this process can reach herdr's server and that it
// speaks the protocol routr is written against. A sandbox that denies the
// connection matters only when no watcher runs, since commands then start
// one from here.
func (a *app) checkHerdr(add func(name, status, detail, fix string), socket string, running bool) {
	if socket == "" {
		add("herdr", "fail", herdr.SocketEnv+" is unset", "run routr inside a herdr pane, or pass --socket PATH")
		return
	}
	s, err := a.ping(a.ctx, socket)
	server := fmt.Sprintf("herdr %s at %s speaks protocol %d", s.Version, socket, s.Protocol)
	switch {
	case err == nil && s.Protocol == herdr.Protocol:
		add("herdr", "ok", fmt.Sprintf("reachable at %s: herdr %s, protocol %d", socket, s.Version, s.Protocol), "")
	case err == nil && s.Protocol < herdr.Protocol:
		add("herdr", "fail", fmt.Sprintf("%s; routr needs protocol %d, herdr %s or later", server, herdr.Protocol, herdr.MinVersion),
			"run herdr update --handoff (it tries to move the running server onto the new version; if it cannot, restart herdr)")
	case err == nil:
		add("herdr", "warn", fmt.Sprintf("%s, newer than the %d routr is written against; a command may fail on a changed method", server, herdr.Protocol),
			"update routr: "+a.updateHint())
	case herdr.Denied(err) && running:
		add("herdr", "warn", fmt.Sprintf("this process may not connect to %s (%v), but the watcher can", socket, err),
			"fine while the watcher runs; if it stops, run routr outside the sandbox")
	case herdr.Denied(err):
		add("herdr", "fail", fmt.Sprintf("this process may not connect to %s (%v)", socket, err),
			"routr runs in a sandbox (Codex runs commands in one): run it outside the sandbox, with escalated permissions")
	case !herdr.Unreachable(err):
		add("herdr", "fail", fmt.Sprintf("%s did not answer ping as herdr does: %v", socket, err), "is it herdr's socket? herdr status shows the server's")
	default:
		add("herdr", "fail", fmt.Sprintf("cannot reach %s: %v", socket, err), "is herdr's server running? Start herdr, then run routr from one of its panes")
	}
}

// checkPi warns of Pi settings that make a long main-agent run re-send its
// whole conversation uncached. It reads Pi's files and changes nothing, and
// says nothing where Pi's agent directory doesn't exist.
func (a *app) checkPi(add func(name, status, detail, fix string), root string) {
	dir := piAgentDir(a.getenv)
	if fi, err := os.Stat(dir); dir == "" || err != nil || !fi.IsDir() {
		return
	}
	a.checkPiMemory(add, dir, root)
	switch r := a.getenv("PI_CACHE_RETENTION"); r {
	case "", "long":
		add("pi cache", "ok", "routr's Pi extension asks Anthropic models to keep the main agent's cached conversation for an hour", "")
	default:
		add("pi cache", "warn", fmt.Sprintf("PI_CACHE_RETENTION=%s: Anthropic models keep the cached conversation for 5 minutes; a turn after a longer gap (common overnight) re-sends all of it uncached", r),
			"unset PI_CACHE_RETENTION where you start Pi; routr's extension then asks for an hour (a cache write costs 2x instead of 1.25x)")
	}
}

// checkPiMemory warns when pi-memory, if Pi loads it, adds its recall to
// every request. Pi loads the packages of its agent directory's
// settings.json and the project's; pi-memory reads its own settings from
// ~/.pi/agent/settings.json, whatever PI_CODING_AGENT_DIR says, and the
// project's over it.
func (a *app) checkPiMemory(add func(name, status, detail, fix string), dir, root string) {
	global, project := filepath.Join(dir, "settings.json"), filepath.Join(root, ".pi", "settings.json")
	memGlobal := global
	if home := a.getenv("HOME"); home != "" {
		memGlobal = filepath.Join(home, ".pi", "agent", "settings.json")
	}
	files := map[string]map[string]any{}
	for _, f := range []string{global, project, memGlobal} {
		v, err := readPiSettings(f)
		if err != nil {
			add("pi memory", "warn", err.Error(), "fix the file, then run routr doctor again")
			return
		}
		files[f] = v
	}
	if !hasPiMemory(files[global]) && !hasPiMemory(files[project]) {
		return
	}
	perTurn, inProject := memoryPerTurn(files[memGlobal], files[project])
	fixIn := memGlobal
	if inProject {
		fixIn = project
	}
	if perTurn {
		add("pi memory", "warn", "pi-memory inserts its recall before your latest message on every request, so each message you type re-sends the whole conversation to the model uncached",
			`set "memory": {"perTurnInjection": false} in `+fixIn+" (it then adds its memory once, at session start)")
	} else {
		add("pi memory", "ok", "pi-memory adds its memory once, at session start", "")
	}
}

// piAgentDir is Pi's agent directory: $PI_CODING_AGENT_DIR, else
// ~/.pi/agent; "" without either.
func piAgentDir(getenv func(string) string) string {
	home := getenv("HOME")
	if d := getenv("PI_CODING_AGENT_DIR"); d != "" {
		if rest, ok := strings.CutPrefix(d, "~/"); ok && home != "" {
			return filepath.Join(home, rest)
		}
		if d == "~" && home != "" {
			return home
		}
		return d
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// readPiSettings is a Pi settings.json; none when the file doesn't exist.
func readPiSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return v, nil
}

// hasPiMemory says whether settings' packages list pi-memory, as a source
// string or an object's "source".
func hasPiMemory(settings map[string]any) bool {
	list, _ := settings["packages"].([]any)
	for _, p := range list {
		src, _ := p.(string)
		if o, ok := p.(map[string]any); ok {
			src, _ = o["source"].(string)
		}
		if strings.Contains(src, "pi-memory") {
			return true
		}
	}
	return false
}

// memoryPerTurn is pi-memory's perTurnInjection as pi-memory reads it: the
// global file's "memory" block, then the project's "memory" block (or,
// without one, its "pi-memory" block). Only a boolean counts; unset is on.
// inProject says the project's file decided it.
func memoryPerTurn(global, project map[string]any) (on, inProject bool) {
	on = true
	if v, ok := perTurnInjection(global["memory"]); ok {
		on = v
	}
	block := project["memory"]
	if block == nil {
		block = project["pi-memory"]
	}
	if v, ok := perTurnInjection(block); ok {
		return v, true
	}
	return on, false
}

func perTurnInjection(block any) (v, ok bool) {
	m, _ := block.(map[string]any)
	v, ok = m["perTurnInjection"].(bool)
	return v, ok
}

// report prints the checks, or with --json {ok, checks}, and fails with
// exit 1 if any check failed.
func (a *app) report(c *common, checks []check) error {
	ok := true
	for _, ch := range checks {
		ok = ok && ch.Status != "fail"
	}
	if c.json {
		if err := a.printJSON(struct {
			OK     bool    `json:"ok"`
			Checks []check `json:"checks"`
		}{ok, checks}); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		for _, ch := range checks {
			line := ch.Detail
			if ch.Fix != "" {
				line += " → " + ch.Fix
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", strings.ToUpper(ch.Status), ch.Check, line)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if !ok {
		return exitCode(exitError)
	}
	return nil
}
