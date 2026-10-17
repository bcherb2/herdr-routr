// Command routr starts and controls coding-agent harnesses (Claude Code,
// Codex, Pi) as workers in herdr tabs. It is written for agents: each
// command is short, writes the per-repo registry and returns; the watcher
// (`routr watch`, started on demand) does the herdr work. See skills/routr/cli.md
// and docs/architecture.md.
//
//	routr start [flags] BRIEF|-        queue a worker; prints its id
//	routr wait ID|CHAIN|- [--timeout]  block until it finishes or needs you
//	routr result ID [--path]           its final message, output and verdict
//	routr send ID TEXT|-               a follow-up turn (resumes a closed session)
//	routr done [--output F] [--verdict pass|fail] MESSAGE   run by the worker
//	routr list [--all]                 workers and their state
//	routr show ID / hide ID            its pane beside yours / back to its tab
//	routr show ID --context            what its question is decided with
//	routr close ID                     close its tab; the session stays resumable
//	routr approve ID|CHAIN             open a gate
//	routr answer ID N [--expect FP]    pick option N of a blocked worker's menu
//	routr cancel ID|CHAIN              stop it
//	routr flow list|show NAME|start NAME   named flows (skills/routr/flows.md)
//	routr roles                        roles as resolved from config
//	routr watch [--foreground]         the watcher
//	routr doctor / version             check the setup / the build
//	routr hook --log FILE EVENT        Claude hook sink
//
// Exit codes: 0 ok · 1 error · 2 usage · 3 still running · 4 needs you ·
// 5 failed or cancelled.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/hook"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/watch"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// Exit codes.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitRunning  = 3
	exitNeedsYou = 4
	exitFailed   = 5
)

// exitCode ends a command with a code after it printed its own output.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit %d", int(c)) }

// usageError is a bad command line; it exits 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// app is one routr invocation, with its dependencies injectable for tests.
type app struct {
	ctx    context.Context
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	cwd    string
	bin    string    // this binary, for ROUTR_BIN and the detached watcher
	build  buildInfo // what this binary was built from

	dial  func(socket string) worker.Driver
	reach func(ctx context.Context, socket string) error // can this process connect to herdr?
	spawn func(p registry.Paths) error                   // start the watcher unless one runs
	ready config.ReadyFunc
	now   func() time.Time
	poll  time.Duration // how often wait re-reads the registry
}

func main() {
	a := newApp()
	os.Exit(a.run(os.Args[1:]))
}

func newApp() *app {
	cwd, _ := os.Getwd()
	bin, err := os.Executable()
	if err == nil {
		bin, _ = filepath.EvalSymlinks(bin)
	}
	a := &app{
		ctx: context.Background(), stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		getenv: os.Getenv, cwd: cwd, bin: bin, build: readBuild(),
		dial: func(socket string) worker.Driver { return worker.NewHerdr(herdr.New(socket)) },
		reach: func(ctx context.Context, socket string) error {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return herdr.New(socket).Reachable(ctx)
		},
		ready: config.Ready,
		now:   time.Now,
		poll:  250 * time.Millisecond,
	}
	a.spawn = func(p registry.Paths) error { return watch.Spawn(p, a.bin) }
	return a
}

type command struct {
	run     func(a *app, args []string) error
	usage   string
	summary string // what it does and what its arguments are; shown by help
}

var commands = map[string]command{
	"start":    {(*app).start, "routr start [--role R] [--title T] [--harness H] [--model M] [--effort E] [--permissions P] [--answer A] [--cwd D] [--output F] [--interactive] [--after ID|-]… [--gate you] [--chain C] [--step S] [--param k=v]… [--in F] [--wait] [--timeout T] BRIEF|- | --run CMD [--expect pass|fail] [--run-timeout 10m]", "Queue a worker: a harness in a new herdr tab, working on BRIEF (the text, or - to read it from stdin); or with --run, a shell command whose exit status is its verdict. Prints its id; --wait also waits for it. routr roles lists the roles."},
	"wait":     {(*app).wait, "routr wait ID|CHAIN|- [--timeout 9m]", "Wait for worker ID, or every worker of chain CHAIN (- reads the id from stdin), and print its final message. Exit 3: still running; run it again."},
	"result":   {(*app).result, "routr result ID [--path]", "Print worker ID's final message, then output: and verdict: lines. --path prints only the path of the file holding it."},
	"send":     {(*app).send, "routr send ID TEXT|- [--wait] [--timeout 9m]", "Send worker ID a follow-up, TEXT (- reads stdin), in the same session; a closed tab is reopened."},
	"done":     {(*app).done, "routr done [--output F] [--verdict pass|fail] [--id N] MESSAGE|-", "Run by a worker, not by you: record that it finished, with MESSAGE (- reads stdin), the file it wrote and a verdict."},
	"list":     {(*app).list, "routr list [--all] [--since 1h] [--final]", "List the workers that are active, have a tab, or finished in the last hour: id, title, role, harness, state, output."},
	"show":     {(*app).show, "routr show ID [--focus] | show ID --context", "Put worker ID's tab beside your herdr pane, so the user can watch it or talk to it. --context instead prints what its question is decided with: the question, the tool call it is about, its brief, and the project docs to read."},
	"hide":     {(*app).hide, "routr hide ID", "Put worker ID's pane back into its own tab (undoes show)."},
	"close":    {(*app).closeCmd, "routr close ID", "Close worker ID's tab, which stops a worker still at work. Its session stays: routr send resumes it."},
	"approve":  {(*app).approve, "routr approve ID|CHAIN", "Open the gate of worker ID, or of chain CHAIN, so it starts; or accept a review loop that hit its cap. Only when the user says so."},
	"answer":   {(*app).answer, "routr answer ID N [--expect FP] [--by human|agent|jev]", "Choose option N, numbered as on screen, of the question blocked worker ID is asking: the user's choice (--by human), or yours (--by agent) when routr says you answer it. --expect FP presses nothing if the question changed; --by agent or jev needs it, and never picks an option that saves a rule."},
	"retry":    {(*app).retry, "routr retry ID|CHAIN", "Start failed worker ID, or every failed worker of chain CHAIN, again in its place: the same id, chain and step, so the workers after it wait on it as before. One that never started gets its brief again; one that ran is rerun in its session."},
	"cancel":   {(*app).cancel, "routr cancel ID|CHAIN", "Stop worker ID, or every worker of chain CHAIN, and close its tab. Workers queued after it then can't start."},
	"flow":     {(*app).flow, "routr flow list | show NAME [--toml] | start NAME [--task T] [--in F] [--param k=v]… [--harness H [--model M]] [--effort E] [--permissions P] [--answer A] [--env K=V]… [--cwd D] [--chain C] [--wait] | show|start [NAME] --file F|-", "Named multi-step flows: list names them, show NAME gives its steps and params (--toml: as written), start NAME runs it. --file runs a flow from a file or stdin for that start only."},
	"roles":    {(*app).roles, "routr roles", "Show each role and the harness, model and effort it picks now."},
	"main":     {(*app).mainAgent, "routr main", "Show how the main agent (the session that starts workers, never a worker) keeps its context small: the [main] config section with any ROUTR_MAIN_* override. Its front end reads it with --json."},
	"fallback": {(*app).fallback, "routr fallback HARNESS:MODEL [--error MSG]", "Show the [[fallback]] rule for a model: the models its work moves to, in order, when it fails in a way waiting won't fix. --error says whether MSG, the API error it failed with, is one. A front end asks this when its model fails."},
	"config":   {(*app).configCmd, "routr config [init|example]", "List the config files routr reads (yours, then the repo's) and whether each exists. init writes yours, with every key commented out and explained, when you have none; example prints that template."},
	"watch":    {(*app).watchCmd, "routr watch [--foreground]", "Start the repo's watcher, which drives the workers. Other commands start it when needed."},
	"doctor":   {(*app).doctor, "routr doctor [--socket S]", "Check what routr needs (herdr, harnesses, config, registry, whether this binary is current), with the next step for each problem. Exit 1 if a check fails."},
	"version":  {(*app).version, "routr version", "Print the git revision this binary was built from, whether it had uncommitted changes, and the checkout install.sh built it from."},
}

// examples are shown by `routr CMD --help`.
var examples = map[string][]string{
	"start": {
		"routr start --role review --title 'red-team plan' --output REDTEAM.md 'Red-team PLAN.md: …'",
		"routr start --harness pi --model openrouter/deepseek/deepseek-v4-flash --effort off --wait 'Summarize the diff'",
		"routr start --role implement 'Fix the flaky test' | routr start --after - --run 'go test ./...'",
	},
	"wait":   {"routr wait --timeout 9m 3", "routr start 'Fix the flaky test' | routr wait -"},
	"result": {"routr result 3", "grep -n TODO \"$(routr result --path 3)\""},
	"send":   {"routr send 3 'Why does point 2 hold?'", "routr send --wait --timeout 9m 3 'Fix point 2'"},
	"done":   {"routr done --output REDTEAM.md --verdict fail 'Two blocking issues, see REDTEAM.md'"},
	"list":   {"routr list --all", "routr list --json --final"},
	"answer": {"routr answer 3 2 --by human --expect 3f9a1c0e5b7d2a64   # the user chose 2; the fingerprint from the [routr] message or routr list --json",
		"routr answer 3 1 --by agent --expect 3f9a1c0e5b7d2a64   # you decided it (answer = \"agent\")"},
	"retry":    {"routr retry 3", "routr retry plan-to-impl-1"},
	"cancel":   {"routr cancel 3", "routr cancel plan-to-impl-1"},
	"flow":     {"routr flow list", "routr flow start plan-to-impl --task 'add retries to fetch' --wait", "routr flow show deep-research --toml > wide.toml   # edit, then: routr flow start --file wide.toml --task '…'"},
	"main":     {"routr main", "routr main --json", "ROUTR_MAIN_AT=-1 routr main   # what one run with no threshold of ours would apply"},
	"fallback": {"routr fallback claude:opus", "routr fallback pi:deepseek/deepseek-v4-pro --error '402: Insufficient Balance' --json"},
	"config":   {"routr config", "routr config init", "routr config example > my-config.toml"},
	"doctor":   {"routr doctor", "routr doctor --json"},
	"version":  {"routr version", "routr version --json"},
}

func (a *app) run(args []string) int {
	if len(args) >= 2 && args[0] == "help" {
		if _, ok := commands[args[1]]; !ok {
			fmt.Fprintf(a.stderr, "routr: unknown command %q; run routr help\n", args[1])
			return exitUsage
		}
		return a.run(append(slices.Clone(args[1:]), "--help")) // routr help flow start = routr flow start --help
	}
	if len(args) == 0 {
		a.help(a.stderr)
		return exitUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.help(a.stdout)
		return exitOK
	}
	name, rest := args[0], args[1:]
	if name == "hook" {
		if err := hook.Sink(rest, a.stdin); err != nil {
			fmt.Fprintln(a.stderr, "routr hook:", err)
		}
		return exitOK // never fail the harness because of the hook
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(a.stderr, "routr: unknown command %q; run routr help\n", name)
		return exitUsage
	}
	err := cmd.run(a, rest)
	var code exitCode
	var usage usageError
	var help helpError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &help):
		a.commandHelp(name, cmd, help.fs)
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		a.commandHelp(name, cmd, nil)
		return exitOK
	case errors.As(err, &usage):
		fmt.Fprintf(a.stderr, "routr %s: %s\nusage: %s\n", name, usage.msg, cmd.usage)
		return exitUsage
	}
	fmt.Fprintf(a.stderr, "routr %s: %v\n", name, err)
	return exitError
}

func (a *app) help(w io.Writer) {
	fmt.Fprintln(w, "routr starts coding-agent harnesses as workers in herdr tabs. Commands:")
	for _, name := range commandOrder {
		fmt.Fprintf(w, "  %s\n      %s\n", commands[name].usage, commands[name].summary)
	}
	fmt.Fprintln(w, "Every command takes --json and --root DIR. Exit codes: 0 ok, 1 error, 2 usage, 3 still running, 4 needs you, 5 failed or cancelled.")
	fmt.Fprintln(w, "routr help CMD (or routr CMD --help) describes each flag, with examples.")
}

// commandOrder is the order help lists the commands in.
var commandOrder = []string{"start", "wait", "result", "send", "done", "list", "show", "hide", "close", "approve", "answer", "retry", "cancel", "flow", "roles", "main", "fallback", "config", "watch", "doctor", "version"}

// commandHelp is `routr CMD --help`: the usage line, each of fs's flags
// with what it does and its default, and examples.
func (a *app) commandHelp(name string, cmd command, fs *flag.FlagSet) {
	fmt.Fprintf(a.stdout, "usage: %s\n\n%s\n", cmd.usage, cmd.summary)
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	first := true
	if fs == nil {
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
	}
	fs.VisitAll(func(f *flag.Flag) {
		if first {
			fmt.Fprintf(tw, "\n%s flags:\n", fs.Name())
			first = false
		}
		arg, text := flag.UnquoteUsage(f)
		if def := f.DefValue; def != "" && def != "false" && def != "0" {
			text += " (default " + def + ")"
		}
		fmt.Fprintf(tw, "  --%s\t%s\n", strings.TrimSpace(f.Name+" "+arg), text)
	})
	tw.Flush()
	if ex := examples[name]; len(ex) > 0 {
		fmt.Fprintln(a.stdout, "\nexamples:")
		for _, e := range ex {
			fmt.Fprintln(a.stdout, "  "+e)
		}
	}
}

// common are the flags every command takes.
type common struct {
	json bool
	root string
}

func (a *app) flags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := &common{}
	fs.BoolVar(&c.json, "json", false, "print JSON")
	fs.StringVar(&c.root, "root", "", "repo root (default: from the working directory)")
	return fs, c
}

// helpError is -h or --help; it carries the command's flags for the help
// text.
type helpError struct{ fs *flag.FlagSet }

func (helpError) Error() string { return flag.ErrHelp.Error() }
func (helpError) Unwrap() error { return flag.ErrHelp }

// parse parses flags anywhere among the positional arguments; everything
// after "--" is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, arg := range args {
		if arg == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, helpError{fs}
			}
			return nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// paths is the repo's .routr directory: --root, else found from the cwd,
// with the config's retention.
func (a *app) paths(c *common) (registry.Paths, error) {
	var p registry.Paths
	var err error
	if c.root != "" {
		p, err = registry.At(c.root)
	} else {
		p, err = registry.Find(a.cwd, a.getenv)
	}
	if err != nil {
		return p, err
	}
	p.Retention = a.retention(p)
	return p, nil
}

// retention is the config's [registry] section. With a config it cannot
// read, this command keeps every worker rather than drop by the defaults
// what the config may keep longer; the command that needs the config
// reports the error.
func (a *app) retention(p registry.Paths) registry.Retention {
	cfg, err := a.loadConfig(p)
	if err != nil {
		return registry.Retention{Off: true}
	}
	return registry.Retention{Keep: time.Duration(cfg.Registry.KeepDays) * 24 * time.Hour, Max: cfg.Registry.Max}
}

func (a *app) loadConfig(p registry.Paths) (config.Config, error) {
	return config.Load(userConfig(a.getenv), p.Config())
}

// applyConfig loads the config and applies its [env] to this process, so
// readiness checks see e.g. the CLAUDE_CONFIG_DIR workers will get.
func (a *app) applyConfig(p registry.Paths) (config.Config, error) {
	cfg, err := a.loadConfig(p)
	if err != nil {
		return cfg, err
	}
	for k, v := range cfg.Env {
		os.Setenv(k, v)
	}
	return cfg, nil
}

func userConfig(getenv func(string) string) string {
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "routr", "config.toml")
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", "routr", "config.toml")
	}
	return config.UserPath()
}

// callerRecord is the calling agent as recorded on its workers.
func (a *app) callerRecord(harness string) *registry.Caller {
	c := &registry.Caller{Harness: harness, Session: config.CallerSession(harness, a.getenv)}
	if c.Harness == "" && c.Session == "" {
		return nil
	}
	return c
}

// canWatch fails when no watcher runs for p and none started from here
// could reach herdr at socket. Codex runs an agent's commands in a sandbox
// that forbids connecting to the socket; a watcher spawned there cannot
// start anything. Nothing is queued then, so the same command run
// again outside the sandbox makes no duplicate.
func (a *app) canWatch(p registry.Paths, socket string) error {
	if socket == "" || watch.Running(p) {
		return nil
	}
	err := a.reach(a.ctx, socket)
	switch {
	case err == nil:
		return nil
	case herdr.Denied(err):
		return fmt.Errorf("nothing was queued: no routr watcher runs for this repo, and none started from here may connect to herdr (%v). "+
			"This command runs in a sandbox (Codex runs commands in one): run it again outside the sandbox, with escalated permissions", err)
	}
	return fmt.Errorf("nothing was queued: cannot reach herdr (%v). Is its server running?", err)
}

// ensureWatcher starts the repo's watcher if none runs. Failing to is a
// warning: the registry change stands, and the next command tries again.
func (a *app) ensureWatcher(p registry.Paths) {
	if err := a.spawn(p); err != nil {
		fmt.Fprintf(a.stderr, "routr: could not start the watcher: %v. Run: routr watch --foreground\n", err)
	}
}

// readToken reads the first non-empty whitespace-separated token from stdin.
func (a *app) readToken(what string) (string, error) {
	b, err := io.ReadAll(io.LimitReader(a.stdin, 1<<16))
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return "", fmt.Errorf("no %s on stdin: the command before the pipe failed; see its error above", what)
	}
	return f[0], nil
}

func (a *app) readAll() (string, error) {
	b, err := io.ReadAll(a.stdin)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", usagef("stdin is empty")
	}
	return s, nil
}
