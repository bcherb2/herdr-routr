package watch

import (
	"context"
	"time"

	"github.com/bcherb2/herdr-routr/internal/worker"
)

// sighting is what the watcher has seen of one worker's pane. herdr's status
// flaps for a moment between phases, so a phase counts only once it has held for Settle;
// and after a prompt, an idle phase counts only once the turn has visibly
// started, so the idle from before the prompt is not taken for its end.
type sighting struct {
	phase worker.Phase
	seq   uint64
	fp    string // the menu's fingerprint, Asking: another question with the same seq settles anew
	since time.Time

	awaiting  bool      // a prompt was sent and its turn not yet seen to start
	seqKnown  bool      // promptSeq is the sequence number from before the prompt
	promptSeq uint64    //
	promptAt  time.Time //

	asked bool // the current Asking phase was recorded as a question

	unread time.Time // when its last reply first failed to read in this phase
}

// observe records a Poll and returns the phase if it has settled.
func (w *Watcher) observe(id int, st worker.Status) (worker.Phase, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	s := w.seen[id]
	if s == nil {
		s = &sighting{phase: st.Phase, seq: st.Seq, fp: st.Fingerprint, since: now}
		w.seen[id] = s
	}
	if st.Phase != s.phase || st.Seq != s.seq || st.Fingerprint != s.fp {
		s.phase, s.seq, s.fp, s.since, s.asked, s.unread = st.Phase, st.Seq, st.Fingerprint, now, false, time.Time{}
	}
	if s.awaiting {
		started := st.Phase == worker.Working || st.Phase == worker.Asking ||
			(s.seqKnown && st.Seq != s.promptSeq) || now.Sub(s.promptAt) >= staleIdle
		if !started {
			return st.Phase, false
		}
		s.awaiting = false
	}
	return st.Phase, now.Sub(s.since) >= w.Settle
}

// beforePrompt notes that a prompt is about to go to worker id. With no
// earlier sighting of its pane it polls once first, so that a turn that ends
// before the next poll still shows as a new sequence number.
func (w *Watcher) beforePrompt(ctx context.Context, d worker.Driver, h worker.Handle, id int) {
	w.mu.Lock()
	_, known := w.seen[id]
	w.mu.Unlock()
	if !known {
		if st, err := d.Poll(ctx, h); err == nil {
			w.observe(id, st)
		}
	}
	w.prompted(id)
}

// prompted notes that a prompt was just sent to worker id.
func (w *Watcher) prompted(id int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	s := w.seen[id]
	if s == nil {
		w.seen[id] = &sighting{since: now, awaiting: true, promptAt: now}
		return
	}
	s.since, s.awaiting, s.promptAt, s.unread = now, true, now, time.Time{}
	s.seqKnown, s.promptSeq = true, s.seq
}

// asked notes that worker id's current question was recorded.
func (w *Watcher) asked(id int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.seen[id]; s != nil {
		s.asked = true
	}
}

// answered restarts the settle time of a recorded question that a CLI
// command (`routr answer`) has since moved the worker on from. herdr goes on
// reporting the old block for a moment after the keys land, and two prompts
// in a row keep one sequence number, so only a question that is
// still on screen a settle time after the answer is a new one.
func (w *Watcher) answered(id int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.seen[id]; s != nil && s.asked {
		s.since, s.asked = w.now(), false
	}
}

// settledIdle reports whether worker id was last seen idle for Settle, with
// no prompt outstanding: ready for a follow-up.
func (w *Watcher) settledIdle(id int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.seen[id]
	return s != nil && !s.awaiting && s.phase == worker.Idle && w.now().Sub(s.since) >= w.Settle
}

// unreadSince notes that worker id's last reply could not be read and
// returns when that began in its current phase; first reports whether this
// is the first failure.
func (w *Watcher) unreadSince(id int) (since time.Time, first bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	s := w.seen[id]
	if s == nil {
		return now, true
	}
	if s.unread.IsZero() {
		s.unread = now
		return now, true
	}
	return s.unread, false
}

// forget drops what was seen of worker id's pane (it is gone).
func (w *Watcher) forget(id int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, id)
}
