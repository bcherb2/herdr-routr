package registry

import (
	"slices"
	"time"
)

// Follow-ups (`routr send`, a review loop's revision) move a worker through
// these methods, so that Inbox, Turn, Replies and Rest change together
// wherever the CLI or the watcher touches them.

// Queue adds a follow-up to w's inbox. It returns the turn it will be
// answered in (turns/N.md).
func (w *Worker) Queue(text string) int {
	w.Inbox = append(slices.Clone(w.Inbox), text)
	return w.Turn + len(w.Inbox)
}

// Deliver moves the next follow-up out of the inbox: it is the turn in
// flight, sent at now. w works on it and, once it has replied, returns to
// the state it rested in (a finished worker stays finished) or carries on.
func (w *Worker) Deliver(now time.Time) {
	w.Inbox = slices.Delete(slices.Clone(w.Inbox), 0, 1)
	w.Turn++
	w.Rest = rest(*w)
	if w.TurnError != "" || w.State == Failed {
		w.Error, w.TurnError = "", "" // it takes up the work an API error, or an exit, stopped
	}
	w.State, w.Prompted, w.Note = Running, now, ""
	w.ClearQuestion()
}

// rest is the state a worker returns to once a follow-up is answered: a
// finished worker stays finished; one that was working, or that an API
// error or an exit stopped mid-work, carries on.
func rest(w Worker) State {
	switch {
	case w.State == Running || w.State == MaybeDone || w.State == Failed || (w.State == NeedsYou && w.TurnError != ""):
		return ""
	case w.State == Closed && !w.Finished.IsZero():
		return Done
	case w.State == Closed:
		return ""
	}
	return w.State
}

// Replied settles the turn in flight: its reply was written.
func (w *Worker) Replied() {
	w.Replies = w.Turn
	if w.Rest != "" {
		w.State, w.Rest = w.Rest, ""
	}
}

// DropPane records that w has no pane any more (closed, cancelled, exited or
// gone). A turn in flight gets no reply now: why is written as its reply
// (turns/N.md) and the turn is settled, so neither the worker nor a `routr
// wait` waits on it for good and the next follow-up can go. The error is
// that of writing the reply; the change to w is made regardless.
func (w *Worker) DropPane(p Paths, why string) error {
	w.Live = nil
	if w.Turn <= w.Replies {
		return nil
	}
	err := WriteFile(p.TurnFile(w.ID, w.Turn), []byte("("+why+")\n"))
	w.Replied()
	return err
}
