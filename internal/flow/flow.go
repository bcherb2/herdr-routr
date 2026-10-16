// Package flow reads flow files and expands a flow into registry workers.
//
// A flow is a named, parameterized chain of steps (skills/routr/flows.md). Flows are
// TOML: the built-ins (builtin.toml, in the binary), then the user's
// $XDG_CONFIG_HOME/routr/flows/*.toml, then the repo's .routr/flows/*.toml;
// a later flow replaces an earlier one of the same name. A flow file given
// for one start (routr flow start --file) is parsed alone. Parsing is strict:
// unknown keys, refs to later or unrelated steps, loops without a cap and
// refs a step cannot have are errors, so a typo fails when the flow is
// loaded, not halfway through a chain.
//
// Flow.Start turns a flow into pending registry workers. Briefs stay
// templates until the watcher starts each worker; RefsFor builds the values
// it renders them with (Render).
package flow

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

//go:embed builtin.toml
var builtin []byte

// Flow is a named chain of steps.
type Flow struct {
	Name        string   `toml:"-"`
	Source      string   `toml:"-"`          // "built-in" or the file it was read from
	Text        string   `toml:"-" json:"-"` // as written: its table and steps, or the whole file when they can't be cut out
	Description string   `toml:"description"`
	Params      []string `toml:"params"` // required at start: routr flow start NAME --task … --param k=v
	Steps       []Step   `toml:"step"`
}

// Step is one worker, or one per matched file for a foreach step.
type Step struct {
	ID            string   `toml:"id"`
	Role          string   `toml:"role"`  // "" = task
	Brief         string   `toml:"brief"` // template over params, upstream steps and item
	After         []string `toml:"after"` // earlier step ids; after a foreach step = after every copy
	Gate          string   `toml:"gate"`  // "" or "you"
	Output        string   `toml:"output"`
	Interactive   bool     `toml:"interactive"`
	Harness       string   `toml:"harness"` // pins; "" leaves the choice to the role
	Model         string   `toml:"model"`
	Models        []string `toml:"models"` // a models step: one worker per model, in the harness's own ids
	Effort        string   `toml:"effort"`
	Answer        string   `toml:"answer"`  // who answers its questions: human, agent or jev; flow start --answer wins; "" = config
	Env           Env      `toml:"env"`     // exported in its tab or command, over config [env]; flow start --env wins
	Args          []string `toml:"args"`    // extra harness args, after config's
	Verdict       bool     `toml:"verdict"` // the brief asks for pass/fail; implied by on_fail
	OnFail        *OnFail  `toml:"on_fail"`
	Write         *bool    `toml:"write"`          // false: it takes no directory lock (it writes only into {{work}}); nil = the role's
	Foreach       string   `toml:"foreach"`        // glob over params, chain and work, relative to the flow's cwd
	MaxItems      *int     `toml:"max_items"`      // a foreach step: more matches need you; nil = DefaultMaxItems
	Parallel      *int     `toml:"parallel"`       // a foreach step: at most this many copies run at once; nil = no limit
	AllowEmpty    bool     `toml:"allow_empty"`    // a foreach step: no match is done with no copies, not a stop for you
	Worktree      bool     `toml:"worktree"`       // its own git worktree per worker, from HEAD when the flow started
	Run           string   `toml:"run"`            // a run step: a shell command instead of a brief; its exit status is the verdict
	Expect        string   `toml:"expect"`         // a run step: "pass" (default, exit 0) or "fail" (exit non-zero)
	Timeout       string   `toml:"timeout"`        // a run step: a Go duration; "" = registry.DefaultRunTimeout
	Judge         string   `toml:"judge"`          // "jev": a judge step; the brief is Jev's instructions, the after steps' results its state
	Pick          bool     `toml:"pick"`           // a judge step: pick one upstream worker instead of pass/fail
	Pass          string   `toml:"pass"`           // a verdict judge: what pass means, for Jev
	Fail          string   `toml:"fail"`           // a verdict judge: what fail means
	MinConfidence *float64 `toml:"min_confidence"` // a judge step: below it the choice needs you; nil = registry.DefaultMinConfidence
	Read          []string `toml:"read"`           // a judge step: more files for the state, templates like the brief
}

// Env is a step's env: a table of strings. The TOML decoder would read an
// array, env = ["K=V"], as an empty table, so it is checked here.
type Env map[string]string

// UnmarshalTOML takes a table of strings only.
func (e *Env) UnmarshalTOML(v any) error {
	t, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("env: use a table, env = { K = \"V\" }, not %T", v)
	}
	*e = Env{}
	for k, x := range t {
		s, ok := x.(string)
		if !ok {
			return fmt.Errorf("env: %s: use a string, %s = \"V\"", k, k)
		}
		(*e)[k] = s
	}
	return nil
}

// DefaultMaxItems caps how many workers a foreach step starts when it does
// not set max_items.
const DefaultMaxItems = 12

// Runtime reports whether s is a foreach step expanded by the watcher, once
// the steps it comes after are done, rather than when the flow starts.
func (s Step) Runtime() bool { return s.Foreach != "" && len(s.After) > 0 }

// fanout reports whether s is a fan-out step: one worker per matched file
// (foreach) or per listed model (models).
func (s Step) fanout() bool { return s.Foreach != "" || s.Models != nil }

func (s Step) maxItems() int {
	if s.MaxItems == nil {
		return DefaultMaxItems
	}
	return *s.MaxItems
}

func (s Step) parallel() int {
	if s.Parallel == nil {
		return 0
	}
	return *s.Parallel
}

// timeout is a run step's timeout; Parse has checked it.
func (s Step) timeout() time.Duration {
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || s.Timeout == "" {
		return registry.DefaultRunTimeout
	}
	return d
}

func (s Step) minConfidence() float64 {
	if s.MinConfidence == nil {
		return registry.DefaultMinConfidence
	}
	return *s.MinConfidence
}

// OnFail sends a failing verdict back to an upstream step, at most Max times.
type OnFail struct {
	Resume string `toml:"resume"`
	Brief  string `toml:"brief"`
	Max    int    `toml:"max"`
}

// Set maps flow names to flows.
type Set map[string]Flow

// Names returns the flow names, sorted.
func (s Set) Names() []string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Load returns the built-in flows, overridden by name by the user's flows
// and then by the repo's under root (skipped when root is "").
func Load(root string) (Set, error) {
	dirs := []string{userDir()}
	if root != "" {
		dirs = append(dirs, filepath.Join(root, ".routr", "flows"))
	}
	return load(dirs...)
}

func userDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "routr", "flows")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "routr", "flows")
}

// load layers each dir's *.toml over the built-ins. Within one dir, two
// files defining the same flow are an error: neither can win.
func load(dirs ...string) (Set, error) {
	set, err := Parse(builtin, "built-in")
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("flows: %w", err)
		}
		layer := Set{}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("flows: %w", err)
			}
			more, err := Parse(b, path)
			if err != nil {
				return nil, err
			}
			for n, f := range more {
				if prev, dup := layer[n]; dup {
					return nil, fmt.Errorf("flows %s: flow %s is also defined in %s", path, n, prev.Source)
				}
				layer[n] = f
			}
		}
		for n, f := range layer {
			set[n] = f
		}
	}
	return set, nil
}

// Parse decodes and validates a flows file; src names it in errors.
func Parse(data []byte, src string) (Set, error) {
	var file struct {
		Flow map[string]Flow `toml:"flow"`
	}
	md, err := toml.Decode(string(data), &file)
	if err != nil {
		return nil, fmt.Errorf("flows %s: %w", src, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		var bad error
		for _, k := range und {
			bad = errors.Join(bad, unknownKey(k))
		}
		return nil, fmt.Errorf("flows %s: %w", src, bad)
	}
	set := Set{}
	for n, f := range file.Flow {
		f.Name, f.Source = n, src
		f.Text = textOf(string(data), f)
		if err := f.validate(); err != nil {
			return nil, fmt.Errorf("flows %s: %w", src, err)
		}
		set[n] = f
	}
	return set, nil
}

// textOf cuts f's own lines out of the file it was parsed from: from its
// [flow.NAME] header to the first header of another table, without the
// comments and blank lines before that one. When that isn't f as parsed (a
// quoted name, dotted keys, its steps split by another flow's), it is the
// whole file.
func textOf(data string, f Flow) string {
	lines := strings.SplitAfter(data, "\n")
	start, end := -1, len(lines)
	var quote string // the multi-line string delimiter a line is inside, or ""
	for i, l := range lines {
		if t := strings.TrimSpace(l); quote == "" && strings.HasPrefix(t, "[") {
			path := strings.Trim(strings.TrimSpace(strings.SplitN(t, "#", 2)[0]), "[] \t")
			switch {
			case path == "flow."+f.Name:
				start = i
			case start >= 0 && !strings.HasPrefix(path, "flow."+f.Name+"."):
				end = i
			}
			if end < len(lines) {
				break
			}
		}
		quote = multiline(l, quote)
	}
	if start < 0 {
		return data
	}
	for end > start+1 {
		if t := strings.TrimSpace(lines[end-1]); t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		end--
	}
	text := strings.Join(lines[start:end], "")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	var file struct {
		Flow map[string]Flow `toml:"flow"`
	}
	if _, err := toml.Decode(text, &file); err != nil || len(file.Flow) != 1 {
		return data
	}
	g, ok := file.Flow[f.Name]
	if !ok || g.Description != f.Description || !reflect.DeepEqual(g.Params, f.Params) || !reflect.DeepEqual(g.Steps, f.Steps) {
		return data
	}
	return text
}

// multiline reports which multi-line string delimiter, three double or three
// single quotes, is still open after line l, given the one open before it.
func multiline(l, quote string) string {
	for {
		if quote != "" {
			i := strings.Index(l, quote)
			if i < 0 {
				return quote
			}
			l, quote = l[i+3:], ""
			continue
		}
		i, j := strings.Index(l, `"""`), strings.Index(l, "'''")
		switch {
		case i < 0 && j < 0:
			return ""
		case j < 0 || (i >= 0 && i < j):
			l, quote = l[i+3:], `"""`
		default:
			l, quote = l[j+3:], "'''"
		}
	}
}

func unknownKey(k toml.Key) error {
	if len(k) >= 3 && k[0] == "flow" {
		return fmt.Errorf("flow %s: unknown key %s", k[1], strings.Join(k[2:], "."))
	}
	return fmt.Errorf("unknown key %s", k)
}

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// reserved step ids: they are the heads of non-step refs.
var reserved = map[string]bool{"prev": true, "item": true}

var efforts = map[string]bool{"": true, "low": true, "medium": true, "high": true}

// checker validates one flow. up[i] holds the steps step i runs after,
// directly or through others; only those can be referred to, since only
// they are sure to have finished when step i starts.
type checker struct {
	f      Flow
	params map[string]bool
	index  map[string]int
	up     []map[string]bool
}

func (f Flow) validate() error {
	if !validName.MatchString(f.Name) {
		return fmt.Errorf("flow name %q: use letters, digits, - and _", f.Name)
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("flow %s has no steps", f.Name)
	}
	c := checker{f: f, params: map[string]bool{}, index: map[string]int{}, up: make([]map[string]bool, len(f.Steps))}
	for _, p := range f.Params {
		switch {
		case !validName.MatchString(p):
			return fmt.Errorf("flow %s: param %q: use letters, digits, - and _", f.Name, p)
		case builtinRefs[p]:
			return fmt.Errorf("flow %s: param %q is reserved: every template has {{%s}}", f.Name, p, p)
		case c.params[p]:
			return fmt.Errorf("flow %s: duplicate param %q", f.Name, p)
		}
		c.params[p] = true
	}
	for i, s := range f.Steps {
		switch _, dup := c.index[s.ID]; {
		case !validName.MatchString(s.ID):
			return fmt.Errorf("flow %s step %d: id %q: use letters, digits, - and _", f.Name, i+1, s.ID)
		case reserved[s.ID]:
			return fmt.Errorf("flow %s step %d: id %q is reserved", f.Name, i+1, s.ID)
		case dup:
			return fmt.Errorf("flow %s step %d: duplicate step id %q", f.Name, i+1, s.ID)
		}
		c.index[s.ID] = i
	}
	for i, s := range f.Steps {
		if err := c.step(i); err != nil {
			return fmt.Errorf("flow %s step %s: %w", f.Name, s.ID, err)
		}
	}
	return nil
}

func (c *checker) step(i int) error {
	s := c.f.Steps[i]
	if s.Judge != "" {
		if err := c.judgeKeys(i); err != nil {
			return err
		}
	} else if s.Pick || s.Pass != "" || s.Fail != "" || s.MinConfidence != nil || s.Read != nil {
		return errors.New("pick, pass, fail, min_confidence and read: only a judge step has them")
	}
	if s.Run != "" {
		if err := c.runKeys(s); err != nil {
			return err
		}
	} else if s.Expect != "" || s.Timeout != "" {
		return errors.New("expect and timeout: only a run step has them")
	}
	switch {
	case s.Run == "" && strings.TrimSpace(s.Brief) == "":
		return errors.New("empty brief")
	case s.Gate != "" && s.Gate != "you":
		return fmt.Errorf("gate %q: the only gate is \"you\"", s.Gate)
	case !efforts[s.Effort]:
		return fmt.Errorf("effort %q: use low, medium or high", s.Effort)
	case s.Answer != "" && !slices.Contains(registry.Answerers, s.Answer):
		return fmt.Errorf("answer %q: use human, agent or jev", s.Answer)
	case s.Output != "" && !filepath.IsLocal(s.Output):
		return fmt.Errorf("output %q must be a relative path inside the directory", s.Output)
	}
	if s.Models != nil {
		switch {
		case s.Foreach != "":
			return errors.New("foreach: a step fans out over files or models, not both")
		case s.Model != "":
			return errors.New("model: a models step takes each copy's model from models, not model")
		case s.Harness == "":
			return errors.New(`models: pin the harness too (harness = "pi", "claude" or "codex"): the ids are the harness's own`)
		case s.Gate != "":
			return errors.New("gate: a models step cannot gate its copies; gate a run step before it")
		case len(s.Models) == 0:
			return errors.New("models: list at least one model")
		}
		seen := map[string]bool{}
		for _, m := range s.Models {
			switch {
			case m == "":
				return errors.New("models: an empty model id")
			case seen[m]:
				return fmt.Errorf("models: %q is listed twice", m)
			}
			seen[m] = true
		}
	}
	if err := registry.CheckEnv(s.Env); err != nil {
		return fmt.Errorf("env: %w", err)
	}
	if err := registry.CheckArgs(s.Harness, s.Args); err != nil {
		return fmt.Errorf("args: %w", err)
	}
	c.up[i] = map[string]bool{}
	for _, a := range s.After {
		j, err := c.earlier(i, a)
		if err != nil {
			return fmt.Errorf("after: %w", err)
		}
		c.up[i][a] = true
		for u := range c.up[j] {
			c.up[i][u] = true
		}
	}
	item := "" // a fan-out step's copies may use item refs: a file's or a model's
	switch {
	case s.Foreach != "":
		item = "file"
	case s.Models != nil:
		item = "model"
	}
	if s.MaxItems != nil && s.Foreach == "" {
		return errors.New("max_items: only a foreach step has it")
	}
	if s.Parallel != nil && !s.fanout() {
		return errors.New("parallel: only a fan-out step (foreach or models) has it")
	}
	for _, n := range []struct {
		key string
		v   *int
	}{{"max_items", s.MaxItems}, {"parallel", s.Parallel}} {
		if n.v != nil && *n.v <= 0 {
			return fmt.Errorf("%s: must be > 0", n.key)
		}
	}
	if s.AllowEmpty && s.Foreach == "" {
		return errors.New("allow_empty: only a foreach step has it")
	}
	if s.Foreach != "" {
		if err := c.refs(s.Foreach, scope{glob: true}); err != nil {
			return fmt.Errorf("foreach: %w", err)
		}
		if _, err := filepath.Match(s.Foreach, ""); err != nil {
			return fmt.Errorf("foreach %q: %w", s.Foreach, err)
		}
	}
	if err := c.refs(s.Output, scope{item: item}); err != nil {
		return fmt.Errorf("output: %w", err)
	}
	sc := scope{steps: c.up[i], item: item}
	if len(s.After) > 0 {
		sc.prev = s.After[0]
	}
	if err := c.refs(s.Brief, sc); err != nil {
		return fmt.Errorf("brief: %w", err)
	}
	if err := c.refs(s.Run, sc); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	for _, r := range s.Read {
		if err := c.refs(r, sc); err != nil {
			return fmt.Errorf("read %q: %w", r, err)
		}
	}
	if s.OnFail != nil {
		return c.onFail(i, sc)
	}
	return nil
}

// runKeys checks the keys of a run step: it runs a command, so the keys
// that pick or brief a harness, and foreach, are wrong on it.
func (c *checker) runKeys(s Step) error {
	for _, k := range []struct {
		key string
		set bool
	}{
		{"brief", s.Brief != ""}, {"role", s.Role != ""}, {"harness", s.Harness != ""}, {"model", s.Model != ""}, {"models", s.Models != nil},
		{"effort", s.Effort != ""}, {"interactive", s.Interactive}, {"output", s.Output != ""}, {"verdict", s.Verdict},
		{"foreach", s.Foreach != ""}, {"max_items", s.MaxItems != nil}, {"parallel", s.Parallel != nil}, {"allow_empty", s.AllowEmpty},
		{"worktree", s.Worktree}, {"answer", s.Answer != ""}, {"args", s.Args != nil}, {"write", s.Write != nil},
	} {
		if k.set {
			return fmt.Errorf("%s: a run step runs a command, so it has no %s (its output is its log, its verdict its exit status)", k.key, k.key)
		}
	}
	switch {
	case strings.TrimSpace(s.Run) == "":
		return errors.New("empty run")
	case s.Expect != "" && s.Expect != "pass" && s.Expect != "fail":
		return fmt.Errorf("expect %q: use pass or fail", s.Expect)
	}
	if s.Timeout != "" {
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("timeout %q: use a duration above 0 such as 90s or 10m", s.Timeout)
		}
	}
	return nil
}

// judgeKeys checks the keys of a judge step: Jev decides it, so the keys
// that pick or brief a harness, or run a command, are wrong on it.
func (c *checker) judgeKeys(i int) error {
	s := c.f.Steps[i]
	for _, k := range []struct {
		key string
		set bool
	}{
		{"run", s.Run != ""}, {"role", s.Role != ""}, {"harness", s.Harness != ""}, {"model", s.Model != ""}, {"models", s.Models != nil},
		{"effort", s.Effort != ""}, {"interactive", s.Interactive}, {"output", s.Output != ""}, {"verdict", s.Verdict},
		{"foreach", s.Foreach != ""}, {"max_items", s.MaxItems != nil}, {"parallel", s.Parallel != nil}, {"allow_empty", s.AllowEmpty},
		{"worktree", s.Worktree}, {"answer", s.Answer != ""}, {"env", s.Env != nil}, {"args", s.Args != nil},
		{"write", s.Write != nil},
	} {
		if k.set {
			return fmt.Errorf("%s: a judge step asks Jev, so it has no %s", k.key, k.key)
		}
	}
	switch {
	case s.Judge != "jev":
		return fmt.Errorf("judge %q: the only judge is \"jev\"", s.Judge)
	case len(s.After) == 0:
		return errors.New("judge: a judge step needs after: the steps whose results it judges")
	case s.MinConfidence != nil && (*s.MinConfidence <= 0 || *s.MinConfidence > 1):
		return fmt.Errorf("min_confidence %g: use a number above 0 and at most 1", *s.MinConfidence)
	case s.Pick && (s.Pass != "" || s.Fail != ""):
		return errors.New("pass and fail: a pick judge picks one of its after steps' workers; pass and fail describe a verdict")
	case s.Pick && s.OnFail != nil:
		return errors.New("on_fail: a pick judge always passes")
	}
	if s.Pick {
		// Its options are the agent workers of its after; run steps and
		// judges there are context (the check on an attempt).
		n := 0
		for _, a := range s.After {
			if j, ok := c.index[a]; ok {
				switch t := c.f.Steps[j]; {
				case t.Run != "" || t.Judge != "":
				case t.Foreach != "":
					n += 2
				case len(t.Models) > 0:
					n += len(t.Models)
				default:
					n++
				}
			}
		}
		if n < 2 {
			return errors.New("pick: a pick judge needs two agent workers to pick from: two or more after steps that are not run or judge steps, or a foreach step")
		}
	}
	for _, r := range s.Read {
		switch {
		case strings.TrimSpace(r) == "":
			return errors.New("read: an empty path")
		case slices.ContainsFunc(Placeholders(r), func(k string) bool { return strings.HasSuffix(k, ".outputs") }):
			return fmt.Errorf("read %q: one path per entry; a foreach step's outputs are no single file", r)
		case len(Placeholders(r)) == 0 && !filepath.IsLocal(r):
			return fmt.Errorf("read %q must be a relative path inside the directory", r)
		}
	}
	return nil
}

// onFail checks the loop edge. Its brief may also name the failing step.
func (c *checker) onFail(i int, sc scope) error {
	s := c.f.Steps[i]
	o := s.OnFail
	switch {
	case s.Foreach != "":
		return errors.New("on_fail: a foreach step cannot loop (which copy would it resume?)")
	case s.Models != nil:
		return errors.New("on_fail: a models step cannot loop (which copy would it resume?)")
	case o.Max <= 0:
		return errors.New("on_fail: max must be > 0")
	case strings.TrimSpace(o.Brief) == "":
		return errors.New("on_fail: empty brief")
	}
	j, err := c.earlier(i, o.Resume)
	if err != nil {
		return fmt.Errorf("on_fail resume: %w", err)
	}
	switch {
	case !c.up[i][o.Resume]:
		return fmt.Errorf("on_fail resume: step %q is not upstream of this step; add it to after (directly or through another step)", o.Resume)
	case c.f.Steps[j].Foreach != "":
		return fmt.Errorf("on_fail resume: %q is a foreach step", o.Resume)
	case c.f.Steps[j].Models != nil:
		return fmt.Errorf("on_fail resume: %q is a models step", o.Resume)
	case c.f.Steps[j].Run != "":
		return fmt.Errorf("on_fail resume: %q is a run step; it has no session to resume", o.Resume)
	case c.f.Steps[j].Judge != "":
		return fmt.Errorf("on_fail resume: %q is a judge step; it has no session to resume", o.Resume)
	}
	sc.steps = map[string]bool{s.ID: true}
	for u := range c.up[i] {
		sc.steps[u] = true
	}
	if err := c.refs(o.Brief, sc); err != nil {
		return fmt.Errorf("on_fail brief: %w", err)
	}
	return nil
}

// earlier returns the index of step id, which must come before step i.
func (c *checker) earlier(i int, id string) (int, error) {
	j, ok := c.index[id]
	switch {
	case !ok:
		return 0, fmt.Errorf("no step %q", id)
	case j == i:
		return 0, fmt.Errorf("step %q is this step", id)
	case j > i:
		return 0, fmt.Errorf("step %q comes later; a step can only name earlier ones", id)
	}
	return j, nil
}

// scope is what a template may refer to besides params, {{chain}} and
// {{work}}. The zero scope allows those only.
type scope struct {
	steps map[string]bool // step ids
	prev  string          // the step {{prev.*}} means; "" = none
	item  string          // "" = none; "file" = a foreach copy (path, name); "model" = a models copy (model, name)
	glob  bool            // a foreach glob: params, chain and work only
}

// refs checks every placeholder in tmpl, reporting each bad one once.
func (c *checker) refs(tmpl string, sc scope) error {
	var bad error
	seen := map[string]bool{}
	for _, key := range Placeholders(tmpl) {
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := c.ref(key, sc); err != nil {
			bad = errors.Join(bad, fmt.Errorf("{{%s}}: %w", key, err))
		}
	}
	return bad
}

func (c *checker) ref(key string, sc scope) error {
	head, field, dotted := strings.Cut(key, ".")
	if !dotted {
		if c.params[key] || builtinRefs[key] {
			return nil
		}
		return fmt.Errorf("not a param of this flow (params: %s)", c.paramList())
	}
	if head == "item" {
		switch {
		case sc.item == "":
			return errors.New("only valid in a fan-out step (foreach or models)")
		case sc.item == "file" && field != "path" && field != "name":
			return errors.New("item has path and name")
		case sc.item == "model" && field != "model" && field != "name":
			return errors.New("item has model and name")
		}
		return nil
	}
	if sc.steps == nil {
		switch {
		case sc.glob:
			return errors.New("a foreach glob may use params, chain and work only")
		case sc.item != "":
			return errors.New("only params, chain, work and item are known when the flow starts")
		}
		return errors.New("only params, chain and work are known when the flow starts")
	}
	id := head
	if head == "prev" {
		if sc.prev == "" {
			return errors.New("this step has no after")
		}
		id = sc.prev
	}
	j, ok := c.index[id]
	switch {
	case !ok:
		return fmt.Errorf("no step %q", id)
	case !sc.steps[id]:
		return fmt.Errorf("step %q is not upstream of this step; add it to after (directly or through another step)", id)
	}
	fan := c.f.Steps[j].fanout()
	switch field {
	case "outputs":
		if !fan {
			return fmt.Errorf("%q is not a fan-out step; use {{%s.output}}", id, head)
		}
	case "output", "final", "verdict":
		if fan {
			return fmt.Errorf("%q is a fan-out step; use {{%s.outputs}}", id, head)
		}
		if t := c.f.Steps[j]; field == "output" && t.Judge != "" && !t.Pick {
			return fmt.Errorf("%q is a verdict judge, which writes no output; use {{%s.verdict}} or {{%s.final}}", id, head, head)
		}
	case "worktree", "branch":
		return c.checkoutRef(j, head, field)
	default:
		return errors.New("a step has output, final, verdict, outputs, worktree and branch")
	}
	return nil
}

// checkoutRef checks {{head.worktree}} and {{head.branch}}, head being
// step j: a step with worktree = true has them, and so has a pick judge
// over such steps (the winner's).
func (c *checker) checkoutRef(j int, head, field string) error {
	t := c.f.Steps[j]
	switch {
	case t.Worktree && t.Foreach != "":
		return fmt.Errorf("%q is a foreach step: each copy has its own %s", t.ID, field)
	case t.Worktree:
		return nil
	case t.Judge != "" && t.Pick:
		// Every agent step it may pick needs one, or Jev could pick a
		// winner without it; run steps and judges are only context.
		for _, a := range t.After {
			if k, ok := c.index[a]; ok {
				if s := c.f.Steps[k]; s.Run == "" && s.Judge == "" && !s.Worktree {
					return fmt.Errorf("%q may pick %q, which has no worktree = true, so the winner may have no %s", t.ID, a, field)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("%q has no %s: only a step with worktree = true (or a pick judge over such steps) has {{%s.%s}}", t.ID, field, head, field)
}

func (c *checker) paramList() string {
	if len(c.f.Params) == 0 {
		return "none"
	}
	return strings.Join(c.f.Params, ", ")
}

// Describe is one line for `routr flow list`: name, params, steps and
// description.
func (f Flow) Describe() string {
	ids := make([]string, len(f.Steps))
	for i, s := range f.Steps {
		ids[i] = s.ID
	}
	d := f.Name
	if len(f.Params) > 0 {
		d += " (" + strings.Join(f.Params, ", ") + ")"
	}
	d += ": " + strings.Join(ids, " → ")
	if f.Description != "" {
		d += " — " + f.Description
	}
	return d
}

// Detail is the whole flow for `routr flow show`: each step's settings and
// briefs.
func (f Flow) Detail() string {
	var b strings.Builder
	b.WriteString(f.Name)
	if f.Description != "" {
		b.WriteString(": " + f.Description)
	}
	fmt.Fprintf(&b, "\nfrom: %s\n", f.Source)
	if len(f.Params) > 0 {
		fmt.Fprintf(&b, "params: %s\n", strings.Join(f.Params, ", "))
	}
	for _, s := range f.Steps {
		fmt.Fprintf(&b, "\nstep %s: %s\n", s.ID, strings.Join(s.settings(), " · "))
		if s.Run != "" {
			indent(&b, "run", s.Run)
		} else {
			indent(&b, "brief", s.Brief)
		}
		if s.Pass != "" {
			indent(&b, "pass", s.Pass)
		}
		if s.Fail != "" {
			indent(&b, "fail", s.Fail)
		}
		if s.OnFail != nil {
			indent(&b, "on fail", s.OnFail.Brief)
		}
	}
	return b.String()
}

func (s Step) settings() []string {
	role := s.Role
	if role == "" {
		role = "task"
	}
	out := []string{"role " + role}
	if s.Run != "" {
		out = []string{"run"}
	}
	if s.Judge != "" {
		out = []string{"judge " + s.Judge}
	}
	add := func(ok bool, format string, a ...any) {
		if ok {
			out = append(out, fmt.Sprintf(format, a...))
		}
	}
	add(len(s.After) > 0, "after %s", strings.Join(s.After, ", "))
	add(s.Foreach != "" && !s.Runtime(), "foreach %s", s.Foreach)
	add(s.Runtime(), "foreach %s, globbed after %s", s.Foreach, strings.Join(s.After, ", "))
	add(len(s.Models) > 0, "models %d", len(s.Models))
	add(s.MaxItems != nil, "max items %d", s.maxItems())
	add(s.Parallel != nil, "parallel %d", s.parallel())
	add(s.AllowEmpty, "allow empty")
	add(s.Worktree, "worktree")
	add(s.Write != nil, "write %t", s.Write != nil && *s.Write)
	add(s.Gate != "", "gate %s", s.Gate)
	add(s.Interactive, "interactive")
	add(s.Output != "", "output %s", s.Output)
	add(s.Harness != "", "harness %s", s.Harness)
	add(s.Model != "", "model %s", s.Model)
	add(s.Effort != "", "effort %s", s.Effort)
	add(s.Answer != "", "answer %s", s.Answer)
	add(len(s.Env) > 0, "env %s", strings.Join(slices.Sorted(maps.Keys(s.Env)), ", "))
	add(len(s.Args) > 0, "args %s", strings.Join(s.Args, " "))
	add(s.Verdict, "verdict")
	add(s.Expect != "", "expect %s", s.Expect)
	add(s.Timeout != "", "timeout %s", s.Timeout)
	add(s.Pick, "pick")
	add(s.MinConfidence != nil, "min confidence %g", s.minConfidence())
	add(len(s.Read) > 0, "read %s", strings.Join(s.Read, ", "))
	if o := s.OnFail; o != nil {
		add(true, "on fail resume %s, max %d", o.Resume, o.Max)
	}
	return out
}

func indent(b *strings.Builder, label, text string) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	fmt.Fprintf(b, "  %s: %s\n", label, lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(b, "    %s\n", l)
	}
}
