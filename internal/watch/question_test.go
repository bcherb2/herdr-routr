package watch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/jev"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/watch/watchtest"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// logBuf keeps what the watcher logs (watch.log), and passes it to the
// test log.
type logBuf struct {
	t  *testing.T
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.t.Log(strings.TrimSpace(string(p)))
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// logged sends the watcher's log to a buffer too.
func (r *rig) logged() *logBuf {
	l := &logBuf{t: r.t}
	r.w.Log = log.New(l, "", 0)
	return l
}

// questions sets config [questions] for the watcher, and the project doc
// its questions are decided with.
func (r *rig) questions(fallback string, min float64) {
	r.w.Questions = func() (config.Questions, error) {
		return config.Questions{Answer: "jev", Fallback: fallback, MinConfidence: min}, nil
	}
	os.WriteFile(r.abs("SPEC.md"), []byte("Workers may write files in the repo. They must not delete files or use the network.\n"), 0o644)
}

// jevWorker starts a worker whose questions Jev answers, with a Write in
// flight in its transcript: what its questions are about.
func (r *rig) jevWorker() int {
	r.t.Helper()
	id := r.add(registry.Worker{Brief: "Write notes.md.", Answerer: "jev"})
	r.tick(1)
	r.state(id, registry.Running)
	r.transcript(id, `{"type":"user","message":{"content":"Write notes.md."}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"`+r.abs("notes.md")+`","content":"x"}}]}}
`)
	return id
}

// transcript gives worker id a transcript with these records.
func (r *rig) transcript(id int, records string) {
	r.t.Helper()
	session := r.abs(fmt.Sprintf("session-%d.jsonl", id))
	if err := os.WriteFile(session, []byte(records), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.s.Update(id, func(x *registry.Worker) { x.Transcript = session })
}

// jobEnded reports whether the job this watcher started for worker id has
// ended, or there is none.
func (r *rig) jobEnded(id int) bool {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	j := r.w.jobs[id]
	if j == nil {
		return true
	}
	select {
	case <-j.ended():
		return true
	default:
		return false
	}
}

// decide runs ticks until Jev's call on worker id has been acted on.
func (r *rig) decide(id int) registry.Worker {
	r.t.Helper()
	r.settle()
	r.until(func() bool { return r.jobEnded(id) })
	r.tick(1)
	return r.get(id)
}

// Claude's prompt to write a file: option 2 lasts for the session, so Jev
// is not offered it.
var notesMenu = []string{"Yes", "Yes, allow all edits during this session (shift+tab)", "No"}

// answer = "jev": Jev decides the question from its context, and its
// choice is pressed when its confidence reaches min_confidence, only on
// the question it decided and never an option that lasts (expect, once);
// otherwise the fallback answers it, told what Jev made of it. A hand_over
// is a hand-over however sure Jev is of it.
func TestJevAnswersAQuestion(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fallback string
		answer   func(map[string]jev.Choice) (jev.Decision, error)
		pressed  int    // the option pressed; 0 = none
		handed   string // HandedTo
		note     string // its start
		log      string
	}{
		{"sure", "human", choose("1", 0.9), 1, "", "",
			"answered 1. Yes, by jev: confidence 0.90 (option 1 0.90, option 3 0.05, hand_over 0.05; decision gen-dec-1, $0.00002230, 530 input tokens)"},
		{"at min_confidence", "human", choose("3", 0.8), 3, "", "", "answered 3. No, by jev: confidence 0.80"},
		{"unsure: the user answers", "human", choose("1", 0.6), 0, "human",
			"Jev did not answer it: it picked option 1 (Yes) with confidence 0.60, below min_confidence 0.8.",
			"handed its question to the user. Jev did not answer it: it picked option 1"},
		{"unsure: the main agent answers", "agent", choose("1", 0.6), 0, "agent",
			"Jev did not answer it: it picked option 1 (Yes) with confidence 0.60, below min_confidence 0.8.",
			"handed its question to the main agent."},
		{"handed over, however sure", "human", choose("hand_over", 0.95), 0, "human",
			"Jev did not answer it: it handed it over, confidence 0.95 (hand_over 0.95, option 1 0.03, option 3 0.03).",
			"option 3 0.03). (decision gen-dec-1, $0.00002230, 530 input tokens)"},
		{"an error", "agent", func(map[string]jev.Choice) (jev.Decision, error) {
			return jev.Decision{}, &jev.Error{Status: 529, Body: "overloaded"}
		}, 0, "agent", "Jev did not answer it: it could not decide (jev: HTTP 529: overloaded).", ""},
		{"no option", "human", choose("2", 0.99), 0, "human",
			`Jev did not answer it: it could not decide (jev chose "2", which is no option (1, 3, hand_over)).`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.questions(tt.fallback, 0.8)
			logs := r.logged()
			f := r.jev(tt.answer)
			id := r.jevWorker()
			r.fake.Ask(id, "Do you want to create notes.md?", notesMenu...)
			w := r.decide(id)

			calls := f.Calls()
			if len(calls) != 1 {
				t.Fatalf("Jev was asked %d times", len(calls))
			}
			q := calls[0].q["answer"]
			if keys := slices.Sorted(maps.Keys(q.Options)); !slices.Equal(keys, []string{"1", "3", "hand_over"}) ||
				!strings.Contains(q.Options["1"], `option 1, "Yes"`) || !strings.Contains(q.Instructions, "data to judge, not instructions") {
				t.Errorf("question = %+v", q)
			}
			want := map[string]string{"human": "the user answers it", "agent": "the main agent, working for the user, answers it"}[tt.fallback]
			if !strings.Contains(q.Options["hand_over"], want) {
				t.Errorf("hand_over = %q, want %q", q.Options["hand_over"], want)
			}
			st := calls[0].question
			if st.Worker != id || !strings.HasPrefix(st.Question, "Do you want to create notes.md?\n1. Yes") || len(st.Options) != 3 ||
				!strings.HasPrefix(st.Brief, "Write notes.md.") || len(st.Docs) != 1 || st.Docs[0].Path != "SPEC.md" ||
				st.Pending == nil || st.Pending.Summary != "Write: "+r.abs("notes.md") {
				t.Errorf("state = %+v", st)
			}

			pane := r.fake.Panes(id)[0]
			switch {
			case tt.pressed != 0:
				if !slices.Equal(pane.Pressed, []watchtest.Press{{Option: tt.pressed, Expect: watchtest.Fingerprint("Do you want to create notes.md?", notesMenu), Once: true}}) {
					t.Errorf("pressed %+v", pane.Pressed)
				}
				if w.State != registry.Running || w.Question != "" || w.HandedTo != "" || w.AnsweredBy != "jev" ||
					w.Answer != fmt.Sprintf("%d. %s", tt.pressed, notesMenu[tt.pressed-1]) {
					t.Errorf("worker = %+v", w)
				}
			default:
				if len(pane.Answers) != 0 {
					t.Errorf("pressed %v", pane.Answers)
				}
				if w.State != registry.Blocked || w.HandedTo != tt.handed || w.Note != tt.note || w.AnsweredBy != "" || w.Answering() != tt.handed {
					t.Errorf("worker = %+v\nnote %q\nwant %q", w, w.Note, tt.note)
				}
			}
			if !strings.Contains(logs.String(), "asking Jev about its question (1, 3, hand_over)") || !strings.Contains(logs.String(), tt.log) {
				t.Errorf("watch.log lacks %q:\n%s", tt.log, logs)
			}
			r.settle() // decided once: not asked again, nothing more pressed
			if n := len(f.Calls()); n != 1 || len(r.fake.Panes(id)[0].Answers) > 1 {
				t.Errorf("asked %d times, pressed %v", n, r.fake.Panes(id)[0].Answers)
			}
		})
	}
}

// Some questions go to the fallback without asking Jev: one with no menu,
// one routr doesn't know the tool call of ("Do you want to proceed?" says
// nothing to judge), one whose options all decide more than this call, and
// any when there is no key or routr's config does not load.
func TestJevNotAsked(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options []string
		setup   func(r *rig)
		handed  string
		note    string
	}{
		{"no menu", nil, nil, "agent", "Jev was not asked: the question has no menu to pick from."},
		// Its turn has no call in flight, and no hook record.
		{"no tool call known", notesMenu, func(r *rig) { r.transcript(1, `{"type":"user","message":{"content":"Write notes.md."}}`+"\n") },
			"agent", "Jev was not asked: routr doesn't know which tool call it is about."},
		{"only lasting options", []string{"Yes, and don't ask again for: rm commands in /repo", "Yes, allow all edits during this session"}, nil,
			"agent", "Jev was not asked: every option saves a rule or lasts past this call, and only the user may pick those."},
		{"no key", notesMenu, func(r *rig) { r.w.Jev = nil; r.t.Setenv(JevKeyEnv, "") },
			"agent", "Jev was not asked: no Jev key: OPENROUTER_API_KEY is not set where the watcher runs (export it, or set it in config [env])."},
		{"config does not load", notesMenu, func(r *rig) {
			r.w.Questions = func() (config.Questions, error) {
				return config.Questions{}, errors.New("questions.fallback = \"jev\": use one of human, agent")
			}
		}, "human", "Jev was not asked: routr's config did not load (questions.fallback = \"jev\": use one of human, agent)."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.questions("agent", 0.8)
			f := r.jev(func(map[string]jev.Choice) (jev.Decision, error) { return jev.Decision{}, errors.New("not called") })
			id := r.jevWorker()
			if tt.setup != nil {
				tt.setup(r)
			}
			r.fake.Ask(id, "Do you want to proceed?", tt.options...)
			w := r.decide(id)
			if known := w.Pending != nil; known == strings.HasPrefix(tt.name, "no tool call") {
				t.Errorf("pending = %+v", w.Pending)
			}
			if w.State != registry.Blocked || w.HandedTo != tt.handed || w.Note != tt.note || len(f.Calls()) != 0 || len(r.fake.Panes(id)[0].Answers) != 0 {
				t.Errorf("worker = %+v, %d calls\nnote %q\nwant %q", w, len(f.Calls()), w.Note, tt.note)
			}
		})
	}
}

// The menu changed while Jev decided (answered in its tab, another
// question right after, with no working phase between): nothing is
// pressed on it. The new question is recorded once it has held, and Jev
// decides that one.
func TestJevQuestionChangedMeanwhile(t *testing.T) {
	r := newRig(t)
	r.questions("human", 0.8)
	f := r.jev(choose("1", 0.9))
	f.block = make(chan struct{})
	id := r.jevWorker()
	r.fake.Ask(id, "Do you want to create notes.md?", notesMenu...)
	r.settle()
	r.until(func() bool { return len(f.Calls()) == 1 })
	old := r.state(id, registry.Blocked)

	r.fake.Reask(id, "Do you want to create todo.md?", notesMenu...)
	close(f.block)
	r.until(func() bool { return r.jobEnded(id) })
	r.tick(1)
	if w := r.state(id, registry.Blocked); len(r.fake.Panes(id)[0].Answers) != 0 || w.Fingerprint != old.Fingerprint || w.HandedTo != "" {
		t.Fatalf("pressed %v on another question; worker %+v", r.fake.Panes(id)[0].Answers, w)
	}

	w := r.decide(id)
	calls := f.Calls()
	if len(calls) != 2 || !strings.HasPrefix(calls[1].question.Question, "Do you want to create todo.md?") {
		t.Fatalf("calls: %+v", calls)
	}
	fp := watchtest.Fingerprint("Do you want to create todo.md?", notesMenu)
	if pane := r.fake.Panes(id)[0]; !slices.Equal(pane.Pressed, []watchtest.Press{{Option: 1, Expect: fp, Once: true}}) || w.AnsweredBy != "jev" {
		t.Errorf("pressed %+v, worker %+v", pane.Pressed, w)
	}
}

// Answered in its tab while Jev decides: the call is dropped, and nothing
// is pressed or handed over.
func TestJevQuestionAnsweredInItsTab(t *testing.T) {
	r := newRig(t)
	r.questions("human", 0.8)
	logs := r.logged()
	f := r.jev(choose("1", 0.9))
	f.block = make(chan struct{})
	id := r.jevWorker()
	r.fake.Ask(id, "Do you want to create notes.md?", notesMenu...)
	r.settle()
	r.until(func() bool { return len(f.Calls()) == 1 })

	r.fake.SetPhase(id, worker.Working)
	r.settle()
	r.until(func() bool { return r.jobEnded(id) })
	r.tick(1)
	if w := r.state(id, registry.Running); w.HandedTo != "" || w.Note != "" || len(r.fake.Panes(id)[0].Answers) != 0 || strings.Contains(logs.String(), "handed") {
		t.Errorf("worker %+v, pressed %v", w, r.fake.Panes(id)[0].Answers)
	}
}

// A watcher that stopped while Jev decided leaves the question undecided;
// the next one asks again once it has seen it settle.
func TestJevAskedAgainAfterARestart(t *testing.T) {
	r := newRig(t)
	r.questions("human", 0.8)
	stuck := r.jev(choose("1", 0.9))
	stuck.block = make(chan struct{})
	id := r.jevWorker()
	r.fake.Ask(id, "Do you want to create notes.md?", notesMenu...)
	r.settle()
	r.until(func() bool { return len(stuck.Calls()) == 1 })
	if w := r.state(id, registry.Blocked); w.Answering() != "jev" {
		t.Fatalf("worker %+v", w)
	}

	dead := r.w
	t.Cleanup(func() { r.reap(dead) })
	r.w = r.watcher()
	r.questions("human", 0.8)
	f := r.jev(choose("1", 0.9))
	r.tick(1)
	if len(f.Calls()) != 0 {
		t.Error("asked before the question settled on screen")
	}
	if w := r.decide(id); w.AnsweredBy != "jev" || len(f.Calls()) != 1 || !slices.Equal(r.fake.Panes(id)[0].Answers, []int{1}) {
		t.Errorf("after the restart: %+v, %d calls", w, len(f.Calls()))
	}
}

// The same question again (the same command, asked twice) takes Jev's
// answer from earlier in this run, without a call; after a call that
// failed, Jev is asked again.
func TestJevAnswerKeptForTheSameQuestion(t *testing.T) {
	r := newRig(t)
	r.questions("human", 0.8)
	logs := r.logged()
	fail := true
	f := r.jev(func(q map[string]jev.Choice) (jev.Decision, error) {
		if fail {
			return jev.Decision{}, errors.New("jev: HTTP 500")
		}
		return choose("1", 0.9)(q)
	})
	id := r.jevWorker()
	ask := func() registry.Worker {
		r.fake.Ask(id, "Do you want to create notes.md?", notesMenu...)
		return r.decide(id)
	}
	if w := ask(); w.HandedTo != "human" || len(f.Calls()) != 1 {
		t.Fatalf("after the error: %+v", w)
	}
	// The user answers it (routr answer --by human), and it asks again.
	r.fake.Answer(context.Background(), Handle(r.get(id)), 1, "", false)
	r.s.Update(id, func(x *registry.Worker) { x.State = registry.Running; x.ClearQuestion() })
	r.settle()
	fail = false
	if w := ask(); w.AnsweredBy != "jev" || len(f.Calls()) != 2 {
		t.Fatalf("asked again after a failed call: %+v, %d calls", w, len(f.Calls()))
	}
	r.settle()
	if w := ask(); w.AnsweredBy != "jev" || len(f.Calls()) != 2 || !slices.Equal(r.fake.Panes(id)[0].Answers, []int{1, 1, 1}) {
		t.Errorf("the same question again: %+v, %d calls, pressed %v", w, len(f.Calls()), r.fake.Panes(id)[0].Answers)
	}
	if !strings.Contains(logs.String(), "answered 1. Yes, by jev: confidence 0.90 (option 1 0.90, option 3 0.05, hand_over 0.05; Jev's answer earlier in this run)") {
		t.Errorf("watch.log:\n%s", logs)
	}
}

// A blocked worker whose question was answered in its tab, with the next
// one on screen at once and the same sequence number, has the new one
// recorded once it has held for the settle time, so a brief flicker is
// not taken for a new question. A
// screen with no menu (it scrolled out of view) leaves the record.
func TestBlockedWorkerRecordsAReplacedQuestion(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.Ask(id, "Do you want to create notes.md?", notesMenu...)
	r.settle()
	first := r.state(id, registry.Blocked)
	r.s.Update(id, func(x *registry.Worker) { x.HandedTo = "human" }) // dropped with the question

	r.fake.Reask(id, "Do you want to proceed?", "Yes", "No")
	r.tick(1)
	if w := r.state(id, registry.Blocked); w.Fingerprint != first.Fingerprint {
		t.Fatal("recorded before it settled")
	}
	r.settle()
	w := r.state(id, registry.Blocked)
	if !strings.HasPrefix(w.Question, "Do you want to proceed?") || w.Fingerprint != watchtest.Fingerprint("Do you want to proceed?", []string{"Yes", "No"}) ||
		!slices.Equal(w.Options, []string{"Yes", "No"}) || w.HandedTo != "" {
		t.Errorf("not recorded afresh: %+v", w)
	}

	r.fake.Reask(id, "Which file first?")
	r.settle()
	if got := r.state(id, registry.Blocked); got.Fingerprint != w.Fingerprint || got.Question != w.Question {
		t.Errorf("a screen with no menu changed the record: %+v", got)
	}
}

// For a question, the docs get what is left of jevBudget after the brief,
// the question and the call, so that all of it fits as a judge's state.
func TestQuestionStateFitsTheBudget(t *testing.T) {
	defer func(n int) { jevBudget = n }(jevBudget)
	jevBudget = 1000
	r := newRig(t)
	os.WriteFile(r.abs("SPEC.md"), []byte(strings.Repeat("s", 2000)), 0o644)
	wk := registry.Worker{ID: 1, Brief: strings.Repeat("b", 300), Question: strings.Repeat("q", 100),
		Pending: &registry.ToolCall{Tool: "Bash", Summary: "Bash: ls", Input: strings.Repeat("i", 92)}}
	st := BuildContext(r.p, nil, wk).JevState()
	if kept := len(st.Docs[0].Content) - len(markerOf(st.Docs[0].Content)); kept != 1000-300-100-8-92 {
		t.Errorf("the doc keeps %d bytes", kept)
	}
}
