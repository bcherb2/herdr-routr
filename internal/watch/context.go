package watch

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bcherb2/herdr-routr/internal/menu"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// A question's context (skills/routr/cli.md, "What a question is decided with") is
// what an answer by the main agent or by Jev is decided with: the worker,
// its question and the tool call it is about, its brief, and the project's
// docs. BuildContext reads the worker's record, its brief.md and the docs,
// never herdr, so `routr show ID --context` and the watcher build the same
// one. The main agent gets the docs as paths with sizes (Text, or the
// QuestionContext as JSON): it can read them itself, and pasting them into
// every wake costs context. Jev opens no files, so it gets their text,
// shared out within jevBudget as a judge's files are (JevState).
//
// The docs are config [questions] context, relative to the repo root (the
// parent of .routr), so they come from the main checkout even when the
// worker runs in its own worktree or a subdirectory: the main checkout has
// what the user wrote, while the worktree's copy is the worker's to edit,
// and a worker must not write the rules its own questions are decided by.

// defaultContext are the docs when config [questions] context is unset: the
// first of them that exists.
var defaultContext = []string{".design/SPEC.md", "SPEC.md", "README.md"}

// jevBrief bounds a worker's brief in a question's context: its head and
// tail, with a line saying how much was cut between them.
var jevBrief = 8000

// QuestionContext is what worker Worker's question is decided with, as the
// main agent gets it (routr show ID --context --json).
type QuestionContext struct {
	Worker      int                `json:"worker"`
	Label       string             `json:"label"`
	State       registry.State     `json:"state"`
	Role        string             `json:"role"`
	Chain       string             `json:"chain,omitempty"`
	Step        string             `json:"step,omitempty"`
	Cwd         string             `json:"cwd"`
	Answerer    string             `json:"answerer"`              // who answers its questions: human, agent or jev
	Question    string             `json:"question,omitempty"`    // as on screen, its options numbered; "" when it asks nothing
	Options     []string           `json:"options,omitempty"`     // routr answer ID N picks the Nth
	Fingerprint string             `json:"fingerprint,omitempty"` // for routr answer --expect
	Lasting     []int              `json:"lasting,omitempty"`     // options (from 1) that save a rule or last past this call: only the user may pick them (menu.Lasting)
	Pending     *registry.ToolCall `json:"pending,omitempty"`     // the tool call the question is about
	Brief       string             `json:"brief"`                 // the prompt as sent (brief.md), cut to jevBrief
	BriefPath   string             `json:"brief_path,omitempty"`  // brief.md, all of it; "" before it is sent
	Root        string             `json:"root"`                  // the repo root the docs are listed relative to
	Docs        []Doc              `json:"docs"`
	Missing     []string           `json:"missing,omitempty"` // listed in [questions] context, as listed, but no file there
	Answer      []string           `json:"answer,omitempty"`  // who answers its question and how, a line each (WhoAnswers); none when it asks nothing
}

// Doc is a project doc, as the main agent gets it.
type Doc struct {
	Path string `json:"path"` // absolute
	Size int64  `json:"size"` // in bytes
}

// QuestionState is a question's context as Jev gets it (answer = "jev").
type QuestionState struct {
	Worker   int                `json:"worker"`
	Role     string             `json:"role"`
	Chain    string             `json:"chain,omitempty"`
	Step     string             `json:"step,omitempty"`
	Cwd      string             `json:"cwd"`
	Brief    string             `json:"brief"`
	Question string             `json:"question"`
	Options  []string           `json:"options,omitempty"` // "1. Yes", numbered as routr answer takes them
	Pending  *registry.ToolCall `json:"pending,omitempty"`
	Docs     []File             `json:"docs,omitempty"`    // relative to the repo root, cut to fit
	Missing  []string           `json:"missing,omitempty"` // listed in [questions] context, but no file there
}

// BuildContext is the context worker wk's question is decided with. files
// is config [questions] context, relative to p.Root(): nil means the first
// of defaultContext that exists, an empty list no docs. A listed path with
// no file there (or a directory) is left out and named in Missing.
func BuildContext(p registry.Paths, files []string, wk registry.Worker) QuestionContext {
	c := QuestionContext{Worker: wk.ID, Label: wk.Label(), State: wk.State, Role: wk.Role, Chain: wk.Chain, Step: wk.Step,
		Cwd: wk.Cwd, Answerer: cmp.Or(wk.Answerer, "human"), Question: wk.Question, Options: wk.Options,
		Fingerprint: wk.Fingerprint, Pending: wk.Pending, Root: p.Root(), Docs: []Doc{}}
	for i, o := range wk.Options {
		if menu.Lasting(o) {
			c.Lasting = append(c.Lasting, i+1)
		}
	}
	brief, sent := wk.Brief, filepath.Join(p.RunDir(wk.ID), registry.BriefFile)
	if b, err := os.ReadFile(sent); err == nil {
		brief, c.BriefPath = string(b), sent
	}
	c.Brief = textBlob(strings.TrimSpace(brief)).cut(jevBrief)
	if files == nil {
		for _, f := range defaultContext {
			if d, ok := doc(c.Root, f); ok {
				c.Docs = append(c.Docs, d)
				break
			}
		}
	}
	for _, f := range files {
		d, ok := doc(c.Root, f)
		switch {
		case !ok && !slices.Contains(c.Missing, f):
			c.Missing = append(c.Missing, f)
		case ok && !slices.Contains(c.Docs, d):
			c.Docs = append(c.Docs, d)
		}
	}
	if wk.Question != "" {
		c.Answer = c.WhoAnswers(wk, c.ReadFirst())
	}
	return c
}

// WhoAnswers tells the main agent who answers worker wk's question
// (registry.Worker.Answering), and how, a line each: routr wait prints
// them, and front ends put them in their wake (Answer, from routr show ID
// --context --json). agent: the main agent, after reading readFirst
// (ReadFirst, or the command that says what to read); it may answer alone
// only what the docs and brief settle and can be undone, never an option
// that saves a rule. jev: whoever Jev handed it to, after Jev's note on
// what it made of it. Without a menu there is no option to press, so the
// user answers it in its tab, whoever was set.
func (c QuestionContext) WhoAnswers(wk registry.Worker, readFirst string) []string {
	var lines []string
	if wk.Answerer == "jev" && wk.Note != "" {
		lines = append(lines, wk.Note)
	}
	if wk.Answering() == "agent" && wk.Fingerprint != "" {
		lines = append(lines, "You answer this question, not the user.")
		if readFirst != "" {
			lines = append(lines, strings.Split(readFirst, "\n")...)
		}
		lines = append(lines, "Answer it yourself only if what it asks about is allowed by the docs, within its brief, and can be undone "+
			"(deleting files, pushing, sending or installing cannot), and the docs and brief settle which option is right.")
		if l := c.LastingNote(); l != "" {
			lines = append(lines, l)
		}
		return append(lines,
			fmt.Sprintf("To answer, run: routr answer %d N --by agent --expect %s, then tell the user in one line what you chose and why.", wk.ID, wk.Fingerprint),
			fmt.Sprintf("Otherwise ask the user: say what it asks and why you did not decide. They answer in its tab (routr show %d), or tell you an option N: "+
				"then run routr answer %d N --by human --expect %s", wk.ID, wk.ID, wk.Fingerprint))
	}
	lines = append(lines, "The user answers this question, not you: do not choose an option yourself, even an obvious one.")
	if wk.Fingerprint == "" {
		return append(lines, fmt.Sprintf("Tell the user what it asks. They answer in its tab: routr show %d", wk.ID))
	}
	return append(lines,
		fmt.Sprintf("Tell the user what it asks. They answer in its tab (routr show %d), or tell you an option.", wk.ID),
		fmt.Sprintf("If they tell you option N, run: routr answer %d N --by human --expect %s", wk.ID, wk.Fingerprint))
}

// doc is the file rel names in root, if there is one.
func doc(root, rel string) (Doc, bool) {
	path := filepath.Join(root, rel)
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return Doc{}, false
	}
	return Doc{Path: path, Size: fi.Size()}, true
}

// Text is c for the main agent (routr show ID --context): the docs as paths
// with sizes, which it reads before it decides when it answers (agent).
func (c QuestionContext) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: role %s", c.Label, c.Role)
	if c.Chain != "" {
		fmt.Fprintf(&b, ", chain %s", c.Chain)
	}
	if c.Step != "" {
		fmt.Fprintf(&b, ", step %s", c.Step)
	}
	fmt.Fprintf(&b, ", in %s\nWho answers its questions: %s\n\n", c.Cwd, c.Answerer)
	if c.Question == "" {
		fmt.Fprintf(&b, "It asks nothing now (%s).\n", c.State)
	} else {
		fmt.Fprintf(&b, "It asks:\n%s\n", c.Question)
		if c.Fingerprint != "" {
			fmt.Fprintf(&b, "fingerprint: %s\n", c.Fingerprint)
		}
		if l := c.LastingNote(); l != "" {
			b.WriteString(l + "\n")
		}
		if p := c.Pending; p != nil {
			fmt.Fprintf(&b, "It asks about %s\n", p.Summary)
			if p.Input != "" {
				fmt.Fprintf(&b, "input:\n%s\n", p.Input)
			}
		}
	}
	if c.BriefPath != "" {
		fmt.Fprintf(&b, "\nIts brief (%s):\n%s\n\n", c.BriefPath, c.Brief)
	} else {
		fmt.Fprintf(&b, "\nIts brief:\n%s\n\n", c.Brief)
	}
	switch {
	case len(c.Docs) == 0:
		b.WriteString("Project docs: none. Config [questions] context names them.\n")
	case c.Answerer == "agent":
		b.WriteString("Project docs: read them before you decide.\n")
	default:
		b.WriteString("Project docs:\n")
	}
	for _, d := range c.Docs {
		fmt.Fprintf(&b, "%s (%d bytes)\n", d.Path, d.Size)
	}
	if len(c.Missing) > 0 {
		fmt.Fprintf(&b, "Missing: %s, listed in [questions] context but not in %s\n", strings.Join(c.Missing, ", "), c.Root)
	}
	return b.String()
}

// LastingNote is the line that keeps the main agent off the options only
// the user may pick, or "" when there are none.
func (c QuestionContext) LastingNote() string {
	switch len(c.Lasting) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("Do not pick option %d: it saves a rule or lasts past this call, so only the user may.", c.Lasting[0])
	}
	n := make([]string, len(c.Lasting))
	for i, o := range c.Lasting {
		n[i] = strconv.Itoa(o)
	}
	return fmt.Sprintf("Do not pick options %s: they save a rule or last past this call, so only the user may.", strings.Join(n, ", "))
}

// ReadFirst tells the main agent what to read before it decides, a line
// each: the project docs, every time (a model sure of the answer skips
// them, and approves a call they forbid), then the brief as
// sent, which it often wrote itself. "" when there are neither.
func (c QuestionContext) ReadFirst() string {
	var lines []string
	if len(c.Docs) > 0 {
		paths := make([]string, len(c.Docs))
		for i, d := range c.Docs {
			paths[i] = d.Path
		}
		lines = append(lines, "Before you decide, read the project docs, even when the answer looks obvious: "+strings.Join(paths, ", "))
	}
	if c.BriefPath != "" {
		lines = append(lines, "Its brief: "+c.BriefPath)
	}
	return strings.Join(lines, "\n")
}

// JevState is c as Jev gets it: the docs' text in place of their paths and
// sizes, shared out as a judge's files are, a long one cut to its head and
// tail with a line saying how much was cut. They get what is left of
// jevBudget after the brief, the question and the call (at most 8 KB, 4 KB
// and a menu), so that all of it fits Jev's window as a judge's state
// does. No less: a long doc cut further loses more of its middle, where the
// rule that forbids the call may be, and a call costs a tenth of a cent.
func (c QuestionContext) JevState() QuestionState {
	st := QuestionState{Worker: c.Worker, Role: c.Role, Chain: c.Chain, Step: c.Step, Cwd: c.Cwd, Brief: c.Brief,
		Question: c.Question, Pending: c.Pending, Missing: c.Missing}
	for i, o := range c.Options {
		st.Options = append(st.Options, fmt.Sprintf("%d. %s", i+1, o))
	}
	rest := len(c.Brief) + len(c.Question)
	if c.Pending != nil {
		rest += len(c.Pending.Summary) + len(c.Pending.Input)
	}
	blobs := make([]*blob, len(c.Docs))
	for i, d := range c.Docs {
		st.Docs = append(st.Docs, File{Path: relTo(c.Root, d.Path)})
		blobs[i] = readBlob(d.Path)
	}
	for i, n := range fair(blobs, max(jevBudget-rest, 0)) {
		st.Docs[i].Content = blobs[i].cut(n)
	}
	return st
}
