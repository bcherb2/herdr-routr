package watch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// DefaultGiveUp is how long a herdr server may be out of reach before the
// watcher gives up on its workers.
const DefaultGiveUp = 10 * time.Minute

// outOfReach reports whether a read of a pane failed because herdr is down,
// refuses this process, or does not answer (a herdr call's deadline).
func outOfReach(err error) bool {
	return herdr.Unreachable(err) || errors.Is(err, context.DeadlineExceeded)
}

// unreachable notes a failure to connect to the herdr server at socket. A
// connection this process is not allowed to make (a sandbox) ends the
// watcher: none of its work can succeed from here, and while it holds the
// repo's watcher lock no watcher started elsewhere can take over.
func (w *Watcher) unreachable(socket string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.down[socket]; !ok {
		w.down[socket] = w.now()
	}
	if msg := err.Error(); msg != w.lastUnreachable { // retried every tick
		w.lastUnreachable = msg
		w.Log.Printf("cannot reach herdr: %v", err)
	}
	if herdr.Denied(err) {
		w.denied = err
	}
}

// reachable notes that the herdr server at socket answered.
func (w *Watcher) reachable(socket string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.down[socket]; ok {
		delete(w.down, socket)
		w.lastUnreachable = ""
		w.Log.Printf("herdr at %s answers again", socket)
	}
}

// gaveUp reports whether the herdr server at socket has been out of reach
// for GiveUp. Its live workers are then closed (lost), and its queued ones
// no longer keep the watcher running: the next command that needs a
// watcher starts one, which tries again.
func (w *Watcher) gaveUp(socket string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	since, ok := w.down[socket]
	return ok && w.now().Sub(since) >= w.GiveUp
}

// lost closes a live worker whose herdr server has been out of reach for
// GiveUp: its tab cannot be read or closed, and its session stays
// resumable.
func (w *Watcher) lost(wk registry.Worker) {
	why := fmt.Sprintf("herdr at %s has not answered for %s", wk.Socket, w.GiveUp)
	w.dropPane(wk, why+" before it replied", func(x *registry.Worker) {
		if !x.State.Terminal() {
			x.State = registry.Closed
			x.ClearQuestion()
			x.Note = fmt.Sprintf("%s; its tab is lost. routr send %d … resumes the session once herdr runs", why, wk.ID)
		}
	})
	w.Log.Printf("%s: %s; closed", wk.Label(), why)
}
