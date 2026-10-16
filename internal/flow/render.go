package flow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// StepRef is what a brief can say about an earlier step.
type StepRef struct {
	Output   string   // its output path
	Final    string   // path of its current run's final message
	Verdict  string   // pass, fail or ""
	Outputs  []string // a foreach step: every copy's output
	Empty    bool     // a foreach step that matched no files (allow_empty): its outputs are none
	Worktree string   // the git worktree of a worktree step (or a pick's winner), absolute
	Branch   string   // its branch
}

// NoOutputs is what {{x.outputs}} says in a brief for a foreach step that
// matched no files. In a shell command it is no word at all.
const NoOutputs = "(none)"

// Item is the current element of a fan-out step: a foreach copy's matched
// file, or a models copy's model.
type Item struct {
	Path  string // as matched by the glob
	Name  string // the file's base name, or the model id made file-safe
	Model string // the model id as listed, a models step copy
}

// Refs are the values a brief template may use.
type Refs struct {
	Params map[string]string  // {{task}}, {{in}}, any --param
	Steps  map[string]StepRef // by step id; "prev" is the upstream worker of a pipe
	Item   *Item              // set inside a foreach step
	Chain  string             // {{chain}}: the chain's name
	Work   string             // {{work}}: the chain's scratch directory (registry.Paths.WorkDir)
}

// Every template has these refs; no param may take their names.
var builtinRefs = map[string]bool{"chain": true, "work": true}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)

// Placeholders lists the refs used in tmpl, in order of appearance.
func Placeholders(tmpl string) []string {
	var out []string
	for _, m := range placeholder.FindAllStringSubmatch(tmpl, -1) {
		out = append(out, m[1])
	}
	return out
}

// Render fills tmpl's placeholders. Every unknown or empty ref is reported,
// joined into one error; the returned text keeps those placeholders as is.
func Render(tmpl string, r Refs) (string, error) {
	return r.render(tmpl, func(s string) string { return s }, NoOutputs)
}

// RenderShell is Render for a shell command (a run step's). Params go in
// as they are: whoever gave them wrote shell (--param check="go test
// ./..."). Every other value, a path or name routr or a worker made, is
// quoted for sh, each of {{x.outputs}} on its own, so that a file name
// cannot inject a command. A foreach step that matched no files has no
// outputs: "" there, no word.
func RenderShell(tmpl string, r Refs) (string, error) {
	return r.render(tmpl, ShellQuote, "")
}

// render fills tmpl; none is what an empty foreach step's outputs read.
func (r Refs) render(tmpl string, quote func(string) string, none string) (string, error) {
	var bad error
	out := placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := placeholder.FindStringSubmatch(m)[1]
		v, err := r.lookup(key, quote, none)
		if err != nil {
			bad = errors.Join(bad, fmt.Errorf("{{%s}}: %w", key, err))
			return m
		}
		return v
	})
	return out, bad
}

// slug is a model id as one file-safe name: openrouter/deepseek/deepseek-v4-flash
// becomes openrouter-deepseek-deepseek-v4-flash.
func slug(m string) string {
	return strings.NewReplacer("/", "-", ":", "-").Replace(m)
}

// shellSafe are words sh reads as themselves.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,-]+$`)

// ShellQuote quotes s as one sh word; a word that needs no quotes is left
// as it is.
func ShellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (r Refs) lookup(key string, quote func(string) string, none string) (string, error) {
	head, field, dotted := strings.Cut(key, ".")
	switch {
	case key == "chain" && r.Chain != "":
		return quote(r.Chain), nil
	case key == "work" && r.Work != "":
		return quote(r.Work), nil
	case key == "chain":
		return "", errors.New("no chain is known here")
	case key == "work":
		return "", errors.New("no work directory is known here")
	case !dotted:
		if v, ok := r.Params[key]; ok {
			return v, nil
		}
		return "", errors.New("no such parameter")
	}
	if head == "item" {
		if r.Item == nil {
			return "", errors.New("only valid in a fan-out step")
		}
		switch field {
		case "path":
			return quote(r.Item.Path), nil
		case "name":
			return quote(r.Item.Name), nil
		case "model":
			return quote(r.Item.Model), nil
		}
		return "", errors.New("item has path, name and model")
	}
	s, ok := r.Steps[head]
	switch {
	case !ok:
		return "", fmt.Errorf("no earlier step %q", head)
	case s.Empty && field == "outputs":
		return none, nil
	}
	var v string
	switch field {
	case "output":
		v = s.Output
	case "final":
		v = s.Final
	case "verdict":
		v = s.Verdict
	case "outputs":
		outs := make([]string, len(s.Outputs))
		for i, o := range s.Outputs {
			outs[i] = quote(o)
		}
		v = strings.Join(outs, " ")
	case "worktree":
		v = s.Worktree
	case "branch":
		v = s.Branch
	default:
		return "", errors.New("a step has output, final, verdict, outputs, worktree and branch")
	}
	if v == "" {
		return "", fmt.Errorf("step %q has no %s", head, field)
	}
	if field == "outputs" {
		return v, nil
	}
	return quote(v), nil
}
