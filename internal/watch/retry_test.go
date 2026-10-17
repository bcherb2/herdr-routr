package watch

import (
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

const loginRace = "Could not refresh your login because another Claude Code process is refreshing it (or exited mid-refresh) · Try again in a minute; if it keeps happening, close other Claude Code windows or sign in again with /login"

// An interactive plan worker's first turn can end in Claude's login refresh
// error; unretried, it sits "running" until the caller's wait times out.
func TestAPIErrorIsRetried(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Role: "plan", Interactive: true, Brief: "Plan it.", Output: "PLAN.md"})
	r.tick(1)
	r.fake.Fail(id, loginRace)
	r.settle()
	w := r.state(id, registry.Running)
	if w.TurnError != loginRace || w.Retry.IsZero() || !strings.Contains(w.Note, "retrying at") {
		t.Fatalf("scheduled: %+v", w)
	}
	r.settle() // not due yet
	if n := len(r.fake.Prompts(id)); n != 1 {
		t.Fatalf("%d prompts before the retry is due", n)
	}
	r.now = r.now.Add(retryDelay)
	r.settle()
	if p := r.fake.Prompts(id); len(p) != 2 || p[1] != retryText {
		t.Fatalf("prompts = %q", p)
	}
	if w = r.state(id, registry.Running); w.Retries != 1 || !w.Retry.IsZero() {
		t.Errorf("after the retry: %+v", w)
	}
	r.done(id, "PLAN.md", "", "planned", "")
	r.settle()
	if w = r.state(id, registry.Done); w.TurnError != "" || w.Retries != 0 {
		t.Errorf("done keeps the failure: %+v", w)
	}
}

func TestAPIErrorNeedsYouAfterTheRetries(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	for range maxRetries {
		r.fake.Fail(id, "Overloaded")
		r.settle()
		r.now = r.now.Add(retryDelay)
		r.settle()
	}
	r.fake.Fail(id, "Overloaded")
	r.settle()
	w := r.state(id, registry.NeedsYou)
	if len(r.fake.Prompts(id)) != 1+maxRetries || !strings.Contains(w.Error, "Overloaded. routr send") || !strings.Contains(w.Error, "routr send 1") || w.Live == nil {
		t.Fatalf("gave up: %+v, prompts %q", w, r.fake.Prompts(id))
	}
	r.settle() // stays put
	r.state(id, registry.NeedsYou)
	// You set it going again in its tab.
	r.fake.SetPhase(id, worker.Working)
	r.settle()
	if w = r.state(id, registry.Running); w.Error != "" || w.TurnError != "" {
		t.Errorf("working again: %+v", w)
	}
	r.fake.Finish(id, "done now")
	r.settle()
	if w = r.state(id, registry.MaybeDone); w.Retries != 0 {
		t.Errorf("a good turn keeps the retry count: %+v", w)
	}
}

func TestLastingAPIErrorNeedsYouAtOnce(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.fake.Fail(id, "Invalid API key · Please run /login")
	r.settle()
	r.state(id, registry.NeedsYou)
	if n := len(r.fake.Prompts(id)); n != 1 {
		t.Errorf("%d prompts; a lasting error is not retried", n)
	}
	// Once you have logged in again, `routr send` sets it going: it
	// carries on, and is not put back to needs_you by the reply.
	r.s.Update(id, func(w *registry.Worker) { w.Queue("carry on") })
	r.tick(1)
	if w := r.state(id, registry.Running); w.Error != "" || w.TurnError != "" || w.Rest != "" {
		t.Fatalf("delivering: %+v", w)
	}
	r.fake.Finish(id, "picked it up")
	r.settle()
	if w := r.state(id, registry.MaybeDone); r.read(r.p.TurnFile(id, 1)) != "picked it up" {
		t.Errorf("after the follow-up: %+v", w)
	}
}

func TestFailedFollowUpToADoneWorker(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.done(id, "", "", "found it", "")
	r.settle()
	r.s.Update(id, func(w *registry.Worker) { w.Inbox = []string{"why?"} })
	r.tick(1)
	r.fake.Fail(id, "Invalid API key")
	r.settle()
	w := r.state(id, registry.Done)
	if w.Replies != 1 || !strings.Contains(w.Note, "Invalid API key") {
		t.Errorf("after the failed follow-up: %+v", w)
	}
	if got := r.read(r.p.TurnFile(id, 1)); !strings.Contains(got, "Invalid API key") {
		t.Errorf("turns/1.md = %q", got)
	}
}

func TestTransient(t *testing.T) {
	for msg, want := range map[string]bool{
		loginRace:                               true,
		"Overloaded":                            true,
		"stream disconnected before completion": true,
		"Request timed out.":                    true,
		"Invalid API key · Please run /login":   false,
		"OAuth token has expired. Please obtain a new token or refresh your existing token.": false,
		"You've hit your limit · resets 5pm":                                                 false,
		"Credit balance is too low":                                                          false,
		`402: {"message":"Insufficient Balance (request_id: 88fc6ff6-85d0-45b4-a0bf-47d3768bd908)","type":"unknown_error","param":null,"code":"invalid_request_error"}`: false,
		"402 Payment Required":                    false,
		"Payment required: add funds to continue": false,
		"There's an issue with the selected model (x). It may not exist or you may not have access to it. Run /model to pick a different model.": false,
	} {
		if got := transient(msg); got != want {
			t.Errorf("transient(%q) = %v", msg, got)
		}
	}
}
