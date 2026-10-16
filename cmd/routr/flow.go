package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// flow runs `routr flow list|show|start`.
func (a *app) flow(args []string) error {
	if len(args) == 0 {
		return usagef("give list, show NAME or start NAME")
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "-h", "--help":
		return flag.ErrHelp
	case "list":
		return a.flowList(rest)
	case "show":
		return a.flowShow(rest)
	case "start":
		return a.flowStart(rest)
	default:
		return usagef("unknown flow command %q: use list, show or start", sub)
	}
}

func (a *app) loadFlows(c *common) (registry.Paths, flow.Set, error) {
	p, err := a.paths(c)
	if err != nil {
		return p, nil, err
	}
	set, err := flow.Load(p.Root())
	return p, set, err
}

// pickFlow finds the flow a command names. With --file (a path, or - for
// stdin) it comes from that file alone, for this command only: a scratch
// flow, which may share a name with a built-in without replacing it; the
// name may be left out when the file defines one flow. data is the file.
func (a *app) pickFlow(c *common, pos []string, file string) (p registry.Paths, f flow.Flow, data []byte, err error) {
	if len(pos) > 1 || (file == "" && len(pos) != 1) {
		return p, f, nil, usagef("give one flow name (routr flow list), or --file FILE")
	}
	if file == "" {
		p, set, err := a.loadFlows(c)
		if err != nil {
			return p, f, nil, err
		}
		f, ok := set[pos[0]]
		if !ok {
			return p, f, nil, fmt.Errorf("no flow %q; flows: %s", pos[0], strings.Join(set.Names(), ", "))
		}
		return p, f, nil, nil
	}
	if p, err = a.paths(c); err != nil {
		return p, f, nil, err
	}
	src := file
	if file == "-" {
		src = "stdin"
		data, err = io.ReadAll(io.LimitReader(a.stdin, 1<<20))
	} else {
		if !filepath.IsAbs(file) {
			src = filepath.Join(a.cwd, file)
		}
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return p, f, nil, fmt.Errorf("--file: %w", err)
	}
	set, err := flow.Parse(data, src)
	if err != nil {
		return p, f, nil, err
	}
	names := set.Names()
	switch {
	case len(names) == 0:
		return p, f, nil, fmt.Errorf("%s defines no flow: start it with [flow.NAME]", src)
	case len(pos) == 1:
		f, ok := set[pos[0]]
		if !ok {
			return p, f, nil, fmt.Errorf("no flow %q in %s; it defines %s", pos[0], src, strings.Join(names, ", "))
		}
		return p, f, data, nil
	case len(names) > 1:
		return p, f, nil, usagef("%s defines %s: give one name", src, strings.Join(names, ", "))
	}
	return p, set[names[0]], data, nil
}

func (a *app) flowList(args []string) error {
	fs, c := a.flags("flow list")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	_, set, err := a.loadFlows(c)
	if err != nil {
		return err
	}
	if c.json {
		type row struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Params      []string `json:"params"`
			Source      string   `json:"source"`
		}
		var rows []row
		for _, n := range set.Names() {
			f := set[n]
			rows = append(rows, row{n, f.Description, f.Params, f.Source})
		}
		return a.printJSON(rows)
	}
	for _, n := range set.Names() {
		fmt.Fprintln(a.stdout, set[n].Describe())
	}
	return nil
}

func (a *app) flowShow(args []string) error {
	fs, c := a.flags("flow show")
	file := fs.String("file", "", "show a flow from this file (- reads stdin) instead of a named one")
	asTOML := fs.Bool("toml", false, "print the flow as written, to copy into a --file flow")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	_, f, _, err := a.pickFlow(c, pos, *file)
	if err != nil {
		return err
	}
	if *asTOML {
		fmt.Fprint(a.stdout, f.Text)
		return nil
	}
	if c.json {
		return a.printJSON(f)
	}
	fmt.Fprint(a.stdout, f.Detail())
	return nil
}

func (a *app) flowStart(args []string) error {
	fs, c := a.flags("flow start")
	file := fs.String("file", "", "run a flow from this file (- reads stdin), for this start only; kept in the chain's work directory")
	var params multi
	task := fs.String("task", "", "the task (= --param task=T)")
	in := fs.String("in", "", "input file (= --param in=F)")
	fs.Var(&params, "param", "`k=v` for {{k}} in the steps' briefs (repeatable)")
	chain := fs.String("chain", "", "chain name (default: the flow's name and a number)")
	cwd := fs.String("cwd", "", "working directory (default: here)")
	socket := fs.String("socket", "", "herdr socket (default $HERDR_SOCKET_PATH)")
	caller := fs.String("caller", "", "the calling agent's harness (default: from its environment)")
	perms := fs.String("permissions", "", "default, auto or bypass for every step (default: config, else the caller's mode: bypass from Pi, else default)")
	answer := fs.String("answer", "", "who answers every step's questions: human, agent or jev, over a step's own answer (default: the step's, else config [questions] answer, else human); pass it only when the user asks")
	harness := fs.String("harness", "", "claude, codex or pi for every agent step, over the step's own; drops a step's model unless --model; pass it only when the user asks")
	model := fs.String("model", "", "model for every agent step, over the step's own: "+modelHint()+"; needs --harness; pass it only when the user asks")
	effort := fs.String("effort", "", "effort for every agent step, over the step's own; pass it only when the user asks")
	var env multi
	fs.Var(&env, "env", "`K=V` exported in every agent step's tab, over config [env] and the step's env; kept for the worker's life (repeatable)")
	wait := fs.Bool("wait", false, "wait for the chain (see routr wait)")
	timeout := fs.Duration("timeout", DefaultTimeout, "with --wait: give up waiting after this long")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := checkAnswer(*answer); err != nil {
		return err
	}
	if *model != "" && *harness == "" {
		return usagef("--model needs --harness: a model belongs to one harness")
	}
	stepEnv, err := parseEnv(env)
	if err != nil {
		return err
	}
	p, f, data, err := a.pickFlow(c, pos, *file)
	if err != nil {
		return err
	}
	ps, err := parseParams(params, *in)
	if err != nil {
		return err
	}
	if *task != "" {
		if ps == nil {
			ps = map[string]string{}
		}
		ps["task"] = *task
	}
	dir, err := a.workDir(*cwd)
	if err != nil {
		return err
	}
	cr, err := a.creator(p, *socket, *caller)
	if cr != nil {
		cr.permissions, cr.answer = *perms, *answer
		cr.harness, cr.model, cr.effort, cr.env = *harness, *model, *effort, stepEnv
	}
	if err != nil {
		return err
	}
	if err := a.canWatch(p, cr.socket); err != nil {
		return err
	}
	if err := p.Init(); err != nil {
		return err
	}
	if err := f.Check(ps); err != nil {
		return err
	}
	name := *chain
	if name == "" {
		if name, err = cr.store.NewChain(f.Name); err != nil {
			return err
		}
	} else if err := checkChainName(name); err != nil {
		return err
	} else if err := freshChain(cr.store, p, name); err != nil {
		return err
	}
	work := p.WorkDir(name)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	ws, err := f.Start(cr, name, ps, dir, work)
	if err != nil {
		return err
	}
	if data != nil {
		// After Start: a start that fails leaves the work directory empty,
		// so the same --chain can be tried again.
		kept := filepath.Join(work, "flow.toml")
		if err := os.WriteFile(kept, data, 0o644); err != nil {
			fmt.Fprintf(a.stderr, "routr: could not keep the flow file: %v\n", err)
		} else {
			fmt.Fprintf(a.stderr, "routr: the flow file is kept as %s; to keep it for good, move it to .routr/flows/\n", kept)
		}
	}
	a.ensureWatcher(p)
	if i := slices.IndexFunc(ws, func(w registry.Worker) bool { return w.Checkout != nil }); i >= 0 && registry.Dirty(dir) {
		fmt.Fprintf(a.stderr, "routr: the worktrees start from HEAD (%.12s): uncommitted changes here are not in them\n", ws[i].Checkout.Base)
	}
	for _, w := range ws {
		if w.Empty() {
			fmt.Fprintf(a.stderr, "routr: step %s is done: %s\n", w.Step, w.Summary)
			continue
		}
		fan := ""
		if w.Fanout != nil {
			fan = fmt.Sprintf(": one worker per file of %s, once the steps before it are done", w.Fanout.Glob)
		}
		fmt.Fprintf(a.stderr, "routr: queued %s (%s) step %s%s\n", w.Label(), w.Tag(), w.Step, fan)
	}
	if *wait {
		return a.waitFor(p, target{chain: name}, *timeout, c.json)
	}
	if c.json {
		vs := make([]view, 0, len(ws))
		for _, w := range ws {
			vs = append(vs, a.view(p, w))
		}
		return a.printJSON(vs)
	}
	fmt.Fprintln(a.stdout, name)
	return nil
}

// validChain is a chain name: it names its work directory, .routr/work/CHAIN.
var validChain = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// checkChainName rejects chain names routr would read as a worker id, and
// ones that are no plain directory name.
func checkChainName(name string) error {
	if _, err := strconv.Atoi(name); err == nil || name == "-" || strings.TrimSpace(name) == "" {
		return usagef("chain %q: use a name with a letter in it; numbers are worker ids", name)
	}
	if !validChain.MatchString(name) {
		return usagef("chain %q: use letters, digits, ., - and _ (it names the directory .routr/work/%s)", name, name)
	}
	// It also names worktree branches, routr/<chain>/<step>-<id>, which git
	// refuses with these.
	if strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return usagef("chain %q: no \"..\", and no \".\" or \".lock\" at the end (it names git branches)", name)
	}
	return nil
}

// freshChain rejects a --chain name that is in use: a worker has it, or its
// work directory has files. A flow's chain starts with no workers and an
// empty work directory, so it never reads an earlier run's leftovers. (A
// pipe, routr start --chain, joins an existing chain on purpose.)
func freshChain(s *registry.Store, p registry.Paths, name string) error {
	const fresh = "give a new --chain name, or leave --chain out for a fresh one"
	all, err := s.List()
	if err != nil {
		return err
	}
	for _, w := range all {
		if w.ChainRef() == name {
			return fmt.Errorf("chain %s is in use (worker %d is in it); %s", name, w.ID, fresh)
		}
	}
	work := p.WorkDir(name)
	ents, err := os.ReadDir(work)
	switch {
	case err != nil && !os.IsNotExist(err):
		return err
	case len(ents) > 0:
		return fmt.Errorf("chain %s: its work directory %s holds files of an earlier run; %s", name, work, fresh)
	}
	return nil
}

// workDir resolves a --cwd flag against the caller's directory.
func (a *app) workDir(flagCwd string) (string, error) {
	dir := flagCwd
	if dir == "" {
		dir = a.cwd
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.cwd, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("--cwd %s is not a directory", dir)
	}
	return dir, nil
}

// roleCreator is the flow.Creator that fills each worker a flow queues with
// its role's defaults and where it runs, as `routr start` does.
type roleCreator struct {
	a           *app
	store       *registry.Store
	cfg         config.Config
	caller      string
	socket      string
	workspace   string
	ready       config.ReadyFunc
	permissions string            // --permissions for every step; "" = config
	answer      string            // --answer for every step, over a step's own answer; "" = the step's, else config
	harness     string            // --harness for every agent step, over the step's; "" = the step's
	model       string            // --model for every agent step, over the step's; needs harness
	effort      string            // --effort for every agent step, over the step's
	env         map[string]string // --env for every agent step, over config [env] and the step's env
}

func (a *app) creator(p registry.Paths, socket, caller string) (*roleCreator, error) {
	if socket == "" {
		socket = a.getenv(herdr.SocketEnv)
	}
	if socket == "" {
		return nil, fmt.Errorf("workers run in herdr: run routr inside a herdr pane (%s is unset) or pass --socket PATH", herdr.SocketEnv)
	}
	cfg, err := a.applyConfig(p)
	if err != nil {
		return nil, err
	}
	if caller == "" {
		caller = config.DetectCaller(a.getenv)
	}
	return &roleCreator{a: a, store: p.Store(), cfg: cfg, caller: caller, socket: socket,
		workspace: a.getenv("HERDR_WORKSPACE_ID"), ready: memoReady(withShellPath(a.ready))}, nil
}

// Create resolves w's role (keeping the flow's pins, its write among them)
// and adds it. A run
// step or a judge has no role to resolve: it runs a command, or asks Jev.
// Who answers its questions is --answer, else the step's answer (w's
// Answerer), else config: whoever starts the run decides for this run over
// what the flow file says for every run, as --permissions does; so do
// --harness, --model, --effort and --env for an agent step. Its env (and a
// run step's) is frozen here: config [env], the step's env, --env.
func (c *roleCreator) Create(w registry.Worker) (registry.Worker, error) {
	if w.Tabless() {
		if w.Run != "" {
			w.Env = workerEnv(c.cfg.Env, config.ExpandHome(w.Env))
		}
		w.Socket, w.Workspace, w.Tree = c.socket, c.workspace, registry.Tree(w.Cwd)
		w.Caller = c.a.callerRecord(c.caller)
		return c.store.Create(w)
	}
	harness, model, effort := w.Harness, w.Model, cmp.Or(c.effort, w.Effort)
	if c.harness != "" {
		if c.harness != w.Harness {
			model = "" // the step's model is its own harness's; the role's for this one applies
		}
		harness, model = c.harness, cmp.Or(c.model, model)
	}
	res, err := c.cfg.Resolve(c.a.ctx, config.Request{
		Role: w.Role, Harness: harness, Model: model, Effort: effort, Permissions: c.permissions,
		Answer: first(c.answer, w.Answerer), Caller: c.caller,
	}, c.ready)
	if err != nil {
		return registry.Worker{}, err
	}
	if err := registry.CheckArgs(res.Harness, w.Args); err != nil {
		return registry.Worker{}, fmt.Errorf("args: %w", err)
	}
	w.Role, w.Harness, w.Model, w.Effort = res.Role, res.Harness, res.Model, res.Effort
	w.Permissions, w.Answerer, w.Write, w.Preamble = res.Permissions, res.Answer, res.Write, res.Preamble
	if w.WritePin != nil {
		w.Write = *w.WritePin
	}
	w.Args, w.MCP = append(res.Args, w.Args...), c.cfg.StartMCPServers
	w.Env = workerEnv(c.cfg.Env, config.ExpandHome(w.Env), c.env)
	w.WantVerdict = res.Verdict || w.OnFail != nil || strings.Contains(w.Brief, "--verdict")
	w.Interactive = w.Interactive || res.Interactive
	if w.Output == "" && !w.Empty() { // a foreach step that matched nothing writes nothing
		w.Output = res.Output
	}
	if w.Gate == "" {
		w.Gate = res.Gate
	}
	w.Socket, w.Workspace, w.Tree = c.socket, c.workspace, registry.Tree(w.Cwd)
	w.Caller = c.a.callerRecord(c.caller)
	return c.store.Create(w)
}

// Update changes a worker (flow.Start cancels what it created on failure).
func (c *roleCreator) Update(id int, fn func(*registry.Worker)) (registry.Worker, error) {
	return c.store.Update(id, fn)
}

// memoReady checks each harness once per command.
func memoReady(ready config.ReadyFunc) config.ReadyFunc {
	seen := map[string]error{}
	return func(ctx context.Context, h string) error {
		if err, ok := seen[h]; ok {
			return err
		}
		err := ready(ctx, h)
		seen[h] = err
		return err
	}
}
