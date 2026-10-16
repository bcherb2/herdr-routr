package main

import (
	"flag"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// DefaultTimeout keeps a blocking command under Claude Code's 10-minute
// shell cap.
const DefaultTimeout = 9 * time.Minute

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type startFlags struct {
	role, harness, model, effort, permissions string
	answer                                    string
	title, cwd, output, gate, chain, step, in string
	socket, caller, run, expect               string
	interactive, wait                         bool
	timeout, runTimeout                       time.Duration
	after, params, env, args                  multi
	set                                       map[string]bool // the flags given
}

func (a *app) start(args []string) error {
	fs, c := a.flags("start")
	var f startFlags
	fs.StringVar(&f.role, "role", "", "role (default task; routr roles lists them)")
	fs.StringVar(&f.title, "title", "", "short name for its tab, e.g. 'fix login race' (default: from the brief's first line)")
	fs.StringVar(&f.harness, "harness", "", "claude, codex or pi")
	fs.StringVar(&f.model, "model", "", "model: "+modelHint()+" (default: from the role and config; routr roles)")
	fs.StringVar(&f.effort, "effort", "", "low, medium or high; claude also xhigh, max; pi also off, minimal, xhigh, max")
	fs.StringVar(&f.permissions, "permissions", "", "default, auto or bypass (default: config, else the caller's mode: bypass from Pi, else default)")
	fs.StringVar(&f.answer, "answer", "", "who answers its questions: human, agent or jev (default: config [questions] answer, else human); pass it only when the user asks")
	fs.Var(&f.env, "env", "`K=V` exported in its tab or command, over config [env]; kept for its life, resumes included (repeatable)")
	fs.Var(&f.args, "arg", "extra harness `arg`, after config's (repeatable; --arg --chrome)")
	fs.StringVar(&f.cwd, "cwd", "", "working directory (default: here)")
	fs.StringVar(&f.output, "output", "", "file the worker writes (default: the role's)")
	fs.BoolVar(&f.interactive, "interactive", false, "you talk to it; it ends only on routr done")
	fs.Var(&f.after, "after", "start after worker `ID` is done; - reads the id from stdin (repeatable)")
	fs.StringVar(&f.gate, "gate", "", "you: wait for routr approve")
	fs.StringVar(&f.chain, "chain", "", "chain name")
	fs.StringVar(&f.step, "step", "", "step id within the chain, for {{step.output}} refs")
	fs.Var(&f.params, "param", "`k=v` for {{k}} in the brief (repeatable)")
	fs.StringVar(&f.in, "in", "", "input file (= --param in=F)")
	fs.BoolVar(&f.wait, "wait", false, "wait for it (see routr wait)")
	fs.DurationVar(&f.timeout, "timeout", DefaultTimeout, "with --wait: give up waiting after this long")
	fs.StringVar(&f.socket, "socket", "", "herdr socket (default $HERDR_SOCKET_PATH)")
	fs.StringVar(&f.caller, "caller", "", "the calling agent's harness (default: from its environment)")
	fs.StringVar(&f.run, "run", "", "run this shell `command` instead of a harness; its exit status is the verdict")
	fs.StringVar(&f.expect, "expect", "", "with --run: pass (the command must exit 0; default) or fail (it must not)")
	fs.DurationVar(&f.runTimeout, "run-timeout", registry.DefaultRunTimeout, "with --run: stop the command after this long, a fail")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	var w registry.Worker
	switch {
	case !f.set["run"] && (f.set["expect"] || f.set["run-timeout"]):
		return usagef("--expect and --run-timeout go with --run")
	case f.set["run"] && len(pos) != 0:
		return usagef("give a brief or --run COMMAND, not both")
	case f.set["run"]:
		w, err = a.newRunWorker(p, f)
	case len(pos) != 1:
		return usagef("give one brief, or - to read it from stdin")
	default:
		w, err = a.newWorker(p, f, pos[0])
	}
	if err != nil {
		return err
	}
	if err := a.canWatch(p, w.Socket); err != nil {
		return err
	}
	if err := p.Init(); err != nil {
		return err
	}
	store := p.Store()
	if err := joinChain(store, &w); err != nil {
		return err
	}
	w, err = store.Create(w)
	if err != nil {
		return err
	}
	a.ensureWatcher(p)
	fmt.Fprintf(a.stderr, "routr: queued worker %s (%s)%s\n", w.Label(), w.Tag(), chainNote(w))
	if f.wait {
		return a.waitFor(p, target{id: w.ID}, f.timeout, c.json)
	}
	if c.json {
		return a.printJSON(a.view(p, w))
	}
	fmt.Fprintln(a.stdout, w.ID)
	return nil
}

func chainNote(w registry.Worker) string {
	if w.Chain == "" {
		return ""
	}
	return " in chain " + w.Chain
}

// newWorker resolves flags, role and config into a pending worker.
func (a *app) newWorker(p registry.Paths, f startFlags, brief string) (registry.Worker, error) {
	fromStdin := false
	if brief == "-" {
		s, err := a.readAll()
		if err != nil {
			return registry.Worker{}, err
		}
		brief, fromStdin = s, true
	}
	after, err := a.afterIDs(f.after, fromStdin)
	if err != nil {
		return registry.Worker{}, err
	}
	params, err := parseParams(f.params, f.in)
	if err != nil {
		return registry.Worker{}, err
	}
	if err := checkRefs(brief, params, after); err != nil {
		return registry.Worker{}, err
	}
	title := registry.Title(f.title)
	if f.title != "" && title == "" {
		return registry.Worker{}, usagef("--title %q: use letters or digits", f.title)
	}
	if title == "" {
		title = briefTitle(brief)
	}
	if f.gate != "" && f.gate != "you" {
		return registry.Worker{}, usagef("--gate %q: the only gate is you", f.gate)
	}
	if err := checkAnswer(f.answer); err != nil {
		return registry.Worker{}, err
	}
	env, err := parseEnv(f.env)
	if err != nil {
		return registry.Worker{}, err
	}
	socket := f.socket
	if socket == "" {
		socket = a.getenv(herdr.SocketEnv)
	}
	if socket == "" {
		return registry.Worker{}, fmt.Errorf("workers run in herdr: run routr inside a herdr pane (%s is unset) or pass --socket PATH", herdr.SocketEnv)
	}
	if f.chain != "" {
		if err := checkChainName(f.chain); err != nil {
			return registry.Worker{}, err
		}
	}
	cwd, err := a.workDir(f.cwd)
	if err != nil {
		return registry.Worker{}, err
	}
	cfg, err := a.applyConfig(p)
	if err != nil {
		return registry.Worker{}, err
	}
	caller := f.caller
	if caller == "" {
		caller = config.DetectCaller(a.getenv)
	}
	res, err := cfg.Resolve(a.ctx, config.Request{
		Role: f.role, Harness: f.harness, Model: f.model, Effort: f.effort, Permissions: f.permissions, Answer: f.answer, Caller: caller,
	}, withShellPath(a.ready))
	if err != nil {
		return registry.Worker{}, err
	}
	if err := registry.CheckArgs(res.Harness, f.args); err != nil {
		return registry.Worker{}, usagef("--arg: %v", err)
	}
	output := f.output
	if output == "" {
		output = res.Output
	}
	gate := f.gate
	if gate == "" {
		gate = res.Gate
	}
	return registry.Worker{
		Chain: f.chain, Step: f.step, Role: res.Role, Title: title,
		Harness: res.Harness, Model: res.Model, Effort: res.Effort, Permissions: res.Permissions, Answerer: res.Answer,
		Args: append(res.Args, f.args...), MCP: cfg.StartMCPServers, Env: workerEnv(cfg.Env, env), Cwd: cwd, Write: res.Write, Tree: registry.Tree(cwd),
		Brief: brief, Params: params, Output: output, Interactive: f.interactive || res.Interactive,
		Preamble: res.Preamble, WantVerdict: res.Verdict,
		Socket: socket, Workspace: a.getenv("HERDR_WORKSPACE_ID"), Caller: a.callerRecord(caller),
		After: after, Gate: gate, State: registry.Pending,
	}, nil
}

// newRunWorker is a pending run step: --run's command, which needs no
// harness, so the flags that pick or brief one are refused.
func (a *app) newRunWorker(p registry.Paths, f startFlags) (registry.Worker, error) {
	for _, n := range []string{"role", "harness", "model", "effort", "permissions", "answer", "output", "interactive", "arg"} {
		if f.set[n] {
			return registry.Worker{}, usagef("--%s: a --run step runs a command, not a harness", n)
		}
	}
	switch {
	case strings.TrimSpace(f.run) == "":
		return registry.Worker{}, usagef("--run: give the command")
	case f.expect != "" && f.expect != "pass" && f.expect != "fail":
		return registry.Worker{}, usagef("--expect %q: use pass or fail", f.expect)
	case f.runTimeout <= 0:
		return registry.Worker{}, usagef("--run-timeout %s: use a duration above 0", f.runTimeout)
	case f.gate != "" && f.gate != "you":
		return registry.Worker{}, usagef("--gate %q: the only gate is you", f.gate)
	}
	after, err := a.afterIDs(f.after, false)
	if err != nil {
		return registry.Worker{}, err
	}
	params, err := parseParams(f.params, f.in)
	if err != nil {
		return registry.Worker{}, err
	}
	if err := checkRefs(f.run, params, after); err != nil {
		return registry.Worker{}, err
	}
	title := registry.Title(f.title)
	if f.title != "" && title == "" {
		return registry.Worker{}, usagef("--title %q: use letters or digits", f.title)
	}
	if title == "" {
		title = briefTitle(f.run)
	}
	if f.chain != "" {
		if err := checkChainName(f.chain); err != nil {
			return registry.Worker{}, err
		}
	}
	env, err := parseEnv(f.env)
	if err != nil {
		return registry.Worker{}, err
	}
	cwd, err := a.workDir(f.cwd)
	if err != nil {
		return registry.Worker{}, err
	}
	cfg, err := a.loadConfig(p)
	if err != nil {
		return registry.Worker{}, err
	}
	caller := f.caller
	if caller == "" {
		caller = config.DetectCaller(a.getenv)
	}
	return registry.Worker{
		Chain: f.chain, Step: f.step, Role: "run", Title: title,
		Run: f.run, Expect: registry.Verdict(f.expect), Timeout: f.runTimeout, Env: workerEnv(cfg.Env, env),
		Cwd: cwd, Write: true, Tree: registry.Tree(cwd), Params: params,
		Socket: first(f.socket, a.getenv(herdr.SocketEnv)), Workspace: a.getenv("HERDR_WORKSPACE_ID"), Caller: a.callerRecord(caller),
		After: after, Gate: f.gate, State: registry.Pending,
	}, nil
}

// checkAnswer rejects an --answer that names nobody who can answer.
func checkAnswer(answer string) error {
	if answer != "" && !slices.Contains(registry.Answerers, answer) {
		return usagef("--answer %q: use human, agent or jev", answer)
	}
	return nil
}

// parseEnv parses --env K=V values, expanding a leading ~/ in V as config
// [env] does; nil when there are none.
func parseEnv(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, usagef("--env %q: use K=V", kv)
		}
		env[k] = v
	}
	if err := registry.CheckEnv(env); err != nil {
		return nil, usagef("--env: %v", err)
	}
	return config.ExpandHome(env), nil
}

// workerEnv is the env a worker is queued with and keeps: the layers in
// order, later ones winning; {} when they are all empty, never nil (a
// record without its own reads config [env] at each spawn).
func workerEnv(layers ...map[string]string) map[string]string {
	env := map[string]string{}
	for _, l := range layers {
		maps.Copy(env, l)
	}
	return env
}

// templateRef matches a {{…}} reference in a brief.
var templateRef = regexp.MustCompile(`\{\{[^}]*\}\}`)

// briefTitle is the default title: the brief's first line that makes one,
// leaving out {{…}} references, which name files rather than the work.
func briefTitle(brief string) string {
	for line := range strings.Lines(brief) {
		if t := registry.Title(templateRef.ReplaceAllString(line, " ")); t != "" {
			return t
		}
	}
	return ""
}

// afterIDs parses --after values; "-" reads an id from stdin.
func (a *app) afterIDs(vals []string, stdinUsed bool) ([]int, error) {
	var ids []int
	for _, v := range vals {
		if v == "-" {
			if stdinUsed {
				return nil, usagef("stdin can carry the brief or an --after id, not both")
			}
			tok, err := a.readToken("worker id")
			if err != nil {
				return nil, err
			}
			stdinUsed, v = true, tok
		}
		id, err := strconv.Atoi(v)
		if err != nil || id <= 0 {
			return nil, usagef("--after %q is not a worker id", v)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseParams(kvs []string, in string) (map[string]string, error) {
	params := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.Contains(k, ".") {
			return nil, usagef("--param %q: use key=value (no dots in the key)", kv)
		}
		if k == "chain" || k == "work" {
			return nil, usagef("--param %q: {{%s}} is routr's, not a param", kv, k)
		}
		params[k] = v
	}
	if in != "" {
		params["in"] = in
	}
	if len(params) == 0 {
		return nil, nil
	}
	return params, nil
}

// checkRefs rejects refs that can never resolve: an unknown param, or
// {{prev.*}} with nothing before it. Step refs are checked when it starts;
// {{chain}} and {{work}} always resolve.
func checkRefs(brief string, params map[string]string, after []int) error {
	var bad []string
	for _, ref := range flow.Placeholders(brief) {
		head, _, dotted := strings.Cut(ref, ".")
		switch {
		case ref == "chain" || ref == "work":
		case !dotted && params[ref] == "":
			bad = append(bad, fmt.Sprintf("{{%s}} needs --param %s=… (or --in for {{in}})", ref, ref))
		case head == "prev" && len(after) == 0:
			bad = append(bad, fmt.Sprintf("{{%s}} needs --after", ref))
		}
	}
	if len(bad) > 0 {
		return usagef("%s", strings.Join(bad, "; "))
	}
	return nil
}

// joinChain puts w in its upstream's chain, creating one named after the
// first upstream worker if it has none, so {{step.*}} refs and `routr wait
// CHAIN` see the whole pipe.
func joinChain(store *registry.Store, w *registry.Worker) error {
	if len(w.After) == 0 {
		return nil
	}
	var chain string
	for i, id := range w.After {
		up, err := store.Get(id)
		if err != nil {
			return fmt.Errorf("--after %d: no such worker (routr list --all)", id)
		}
		if i == 0 {
			chain = up.Chain
		}
	}
	if w.Chain == "" && chain == "" {
		chain = "c" + strconv.Itoa(w.After[0])
		if _, err := store.Update(w.After[0], func(up *registry.Worker) {
			if up.Chain == "" {
				up.Chain = chain
			}
		}); err != nil {
			return err
		}
	}
	if w.Chain == "" {
		w.Chain = chain
	}
	return nil
}
