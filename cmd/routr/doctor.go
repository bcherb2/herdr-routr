package main

import (
	"errors"
	"fmt"
	"os"
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
		add("config", "ok", "loaded "+files+" (each optional)", "")
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

// checkHerdr checks that this process can reach herdr's server. A sandbox
// that denies the connection matters only when no watcher runs, since
// commands then start one from here.
func (a *app) checkHerdr(add func(name, status, detail, fix string), socket string, running bool) {
	if socket == "" {
		add("herdr", "fail", herdr.SocketEnv+" is unset", "run routr inside a herdr pane, or pass --socket PATH")
		return
	}
	err := a.reach(a.ctx, socket)
	switch {
	case err == nil:
		add("herdr", "ok", "reachable at "+socket, "")
	case herdr.Denied(err) && running:
		add("herdr", "warn", fmt.Sprintf("this process may not connect to %s (%v), but the watcher can", socket, err),
			"fine while the watcher runs; if it stops, run routr outside the sandbox")
	case herdr.Denied(err):
		add("herdr", "fail", fmt.Sprintf("this process may not connect to %s (%v)", socket, err),
			"routr runs in a sandbox (Codex runs commands in one): run it outside the sandbox, with escalated permissions")
	default:
		add("herdr", "fail", fmt.Sprintf("cannot reach %s: %v", socket, err), "is herdr's server running? Start herdr, then run routr from one of its panes")
	}
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
