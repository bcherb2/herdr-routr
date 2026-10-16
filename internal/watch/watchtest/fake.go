// Package watchtest is a scriptable worker.Driver for testing the watcher
// and the CLI without herdr or harnesses.
package watchtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bcherb2/herdr-routr/internal/menu"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// Pane is one fake tab. Tests read it through Fake's methods.
type Pane struct {
	ID          string
	Spec        worker.Spec
	Phase       worker.Phase
	Seq         uint64
	Session     string
	Reply       string
	Failed      string // an API error ended its last turn: LastReply returns it as a *worker.TurnError
	Question    string
	Options     []string
	Fingerprint string // the menu's: Fingerprint(Question, Options)
	Prompts     []string
	Answers     []int
	Pressed     []Press // how each of Answers was asked for
	Closed      bool
	Shown       string            // pane it was shown beside; "" when hidden
	Env         map[string]string // Handle.Env of the last Poll or LastReply
}

// Press is an answer as Answer was asked for it.
type Press struct {
	Option int
	Expect string
	Once   bool
}

// Fake implements worker.Driver. A started pane is idle; a prompt or an
// answer makes it working until the test says otherwise.
type Fake struct {
	mu       sync.Mutex
	panes    []*Pane
	StartErr error // returned by the next Start, then cleared
	TabErr   error // returned by the next Start after it opened the tab (the harness failed in it), then cleared; the handle names the session the harness was to have, as the real driver's does
	PollErr  error // returned by every Poll while set
	ReadErr  error // returned by every LastReply while set (a transcript it can't read)
}

var _ worker.Driver = (*Fake)(nil)

func (f *Fake) pane(h worker.Handle) (*Pane, error) {
	for _, p := range f.panes {
		if p.ID == h.Pane {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no pane %q", h.Pane)
}

// Start opens a pane; a Spec with a Session resumes it.
func (f *Fake) Start(_ context.Context, s worker.Spec) (worker.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.StartErr; err != nil {
		f.StartErr = nil
		return worker.Handle{}, err
	}
	p := &Pane{ID: fmt.Sprintf("p%d", len(f.panes)+1), Spec: s, Phase: worker.Idle, Seq: 1, Session: s.Session}
	if p.Session == "" {
		p.Session = fmt.Sprintf("sess-%d", s.ID)
	}
	f.panes = append(f.panes, p)
	h := worker.Handle{Socket: "fake.sock", Pane: p.ID, Tab: "t" + p.ID, Agent: "a" + p.ID,
		Harness: s.Harness, Session: p.Session, Transcript: "/tmp/" + p.Session + ".jsonl", Cwd: s.Cwd}
	if err := f.TabErr; err != nil {
		f.TabErr = nil
		p.Session = "" // no harness confirmed it: Poll reports none
		return worker.Handle{Socket: h.Socket, Pane: h.Pane, Tab: h.Tab, Agent: h.Agent, Harness: h.Harness, Session: h.Session, Cwd: h.Cwd}, err
	}
	return h, nil
}

// Prompt records text and marks the pane working.
func (f *Fake) Prompt(_ context.Context, h worker.Handle, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.pane(h)
	if err != nil {
		return err
	}
	if p.Closed {
		return errors.New("pane is gone")
	}
	p.Prompts = append(p.Prompts, text)
	p.Phase, p.Seq, p.Failed = worker.Working, p.Seq+1, ""
	return nil
}

// Poll reports the pane's phase; a closed pane is Gone.
func (f *Fake) Poll(_ context.Context, h worker.Handle) (worker.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PollErr != nil {
		return worker.Status{}, f.PollErr
	}
	p, err := f.pane(h)
	if err != nil {
		return worker.Status{Phase: worker.Gone}, nil
	}
	p.Env = h.Env
	if p.Closed {
		return worker.Status{Phase: worker.Gone, Seq: p.Seq}, nil
	}
	return worker.Status{Phase: p.Phase, Seq: p.Seq, Session: p.Session, Question: p.Question, Options: slices.Clone(p.Options), Fingerprint: p.Fingerprint}, nil
}

// LastReply is the pane's Reply.
func (f *Fake) LastReply(_ context.Context, h worker.Handle) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.pane(h)
	if err != nil {
		return "", err
	}
	p.Env = h.Env
	if f.ReadErr != nil {
		return "", f.ReadErr
	}
	if p.Failed != "" {
		return p.Reply, &worker.TurnError{Message: p.Failed}
	}
	return p.Reply, nil
}

// Answer records the option and marks the pane working. As the herdr
// driver, it refuses a fingerprint other than expect (nothing recorded), an
// option the menu lacks, and with once an option that lasts (menu.Lasting).
func (f *Fake) Answer(_ context.Context, h worker.Handle, option int, expect string, once bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.pane(h)
	if err != nil {
		return "", err
	}
	if expect != "" && expect != p.Fingerprint {
		return "", &worker.ChangedError{Question: p.Question, Options: slices.Clone(p.Options), Fingerprint: p.Fingerprint}
	}
	if option < 1 || option > len(p.Options) {
		return "", fmt.Errorf("fake: no option %d; pick 1-%d", option, len(p.Options))
	}
	if once && menu.Lasting(p.Options[option-1]) {
		return "", &worker.LastingError{Option: option, Options: slices.Clone(p.Options)}
	}
	text := p.Options[option-1]
	p.Answers, p.Pressed = append(p.Answers, option), append(p.Pressed, Press{option, expect, once})
	p.Phase, p.Seq, p.Question, p.Options, p.Fingerprint = worker.Working, p.Seq+1, "", nil, ""
	return text, nil
}

// Show records the pane it was shown beside.
func (f *Fake) Show(_ context.Context, h worker.Handle, beside string, _ bool) error {
	return f.edit(h, func(p *Pane) { p.Shown = beside })
}

// Hide clears Shown.
func (f *Fake) Hide(_ context.Context, h worker.Handle) error {
	return f.edit(h, func(p *Pane) { p.Shown = "" })
}

// Close closes the pane.
func (f *Fake) Close(_ context.Context, h worker.Handle) error {
	return f.edit(h, func(p *Pane) { p.Closed = true })
}

func (f *Fake) edit(h worker.Handle, fn func(*Pane)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.pane(h)
	if err != nil {
		return err
	}
	fn(p)
	return nil
}

// Test controls, by worker id: they act on the worker's latest pane.

func (f *Fake) latest(id int) *Pane {
	for i := len(f.panes) - 1; i >= 0; i-- {
		if f.panes[i].Spec.ID == id {
			return f.panes[i]
		}
	}
	return nil
}

func (f *Fake) set(id int, fn func(*Pane)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.latest(id); p != nil {
		fn(p)
		p.Seq++
	}
}

// Finish makes worker id's pane idle with reply as its last message.
func (f *Fake) Finish(id int, reply string) {
	f.set(id, func(p *Pane) { p.Phase, p.Reply, p.Failed = worker.Idle, reply, "" })
}

// Fail ends worker id's turn with an API error instead of a reply.
func (f *Fake) Fail(id int, msg string) {
	f.set(id, func(p *Pane) { p.Phase, p.Failed = worker.Idle, msg })
}

// SetPhase sets worker id's pane phase.
func (f *Fake) SetPhase(id int, ph worker.Phase) { f.set(id, func(p *Pane) { p.Phase = ph }) }

// Ask puts a question on worker id's screen.
func (f *Fake) Ask(id int, q string, options ...string) {
	f.set(id, func(p *Pane) {
		p.Phase, p.Question, p.Options, p.Fingerprint = worker.Asking, q, options, Fingerprint(q, options)
	})
}

// Reask replaces the question on worker id's screen without a new sequence
// number, as herdr does when Claude asks twice in a row.
func (f *Fake) Reask(id int, q string, options ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.latest(id); p != nil {
		p.Phase, p.Question, p.Options, p.Fingerprint = worker.Asking, q, options, Fingerprint(q, options)
	}
}

// Fingerprint is the fake's fingerprint of a menu: its question and options.
func Fingerprint(q string, options []string) string {
	if len(options) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(q + "\n" + strings.Join(options, "\n")))
	return hex.EncodeToString(sum[:8])
}

// Kill closes worker id's pane behind the watcher's back.
func (f *Fake) Kill(id int) { f.set(id, func(p *Pane) { p.Closed = true }) }

// Panes returns copies of worker id's panes, oldest first.
func (f *Fake) Panes(id int) []Pane {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Pane
	for _, p := range f.panes {
		if p.Spec.ID == id {
			c := *p
			c.Prompts, c.Answers, c.Pressed = slices.Clone(p.Prompts), slices.Clone(p.Answers), slices.Clone(p.Pressed)
			out = append(out, c)
		}
	}
	return out
}

// Prompts returns everything sent to worker id, across its panes.
func (f *Fake) Prompts(id int) []string {
	var out []string
	for _, p := range f.Panes(id) {
		out = append(out, p.Prompts...)
	}
	return out
}

// Open counts worker id's panes that are not closed.
func (f *Fake) Open(id int) int {
	n := 0
	for _, p := range f.Panes(id) {
		if !p.Closed {
			n++
		}
	}
	return n
}
