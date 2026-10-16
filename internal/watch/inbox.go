package watch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// deliverInbox sends each worker its next follow-up: when its pane is idle,
// or after resuming its session in a new tab when it has none. One turn is
// in flight per worker; the next goes once the reply is written.
func (w *Watcher) deliverInbox(ctx context.Context) error {
	all, err := w.store.List()
	if err != nil {
		return err
	}
	for _, wk := range all {
		if len(wk.Inbox) == 0 || wk.Turn > wk.Replies {
			continue
		}
		switch wk.State {
		case registry.Running, registry.Done, registry.MaybeDone, registry.Closed, registry.NeedsYou, registry.Failed:
			w.deliver(ctx, wk)
		}
	}
	return nil
}

func (w *Watcher) deliver(ctx context.Context, wk registry.Worker) {
	if wk.Live != nil && !w.settledIdle(wk.ID) {
		return
	}
	if wk.Write {
		_, holder, err := w.store.Claim(wk.ID, func(x *registry.Worker) error {
			if len(x.Inbox) == 0 || x.Turn > x.Replies || x.State == registry.Cancelled {
				return errStale
			}
			return nil
		})
		if err != nil {
			if !errors.Is(err, errStale) {
				w.Log.Printf("%s: %v", wk.Label(), err)
			}
			return
		}
		if holder != wk.ID {
			note := fmt.Sprintf("its follow-up waits for the directory lock on %s (held by worker %d)", wk.Worktree(), holder)
			if wk.Note != note {
				w.set(wk, func(x *registry.Worker) { x.Note = note })
			}
			return
		}
	}
	d, h := w.driver(wk), w.handle(wk)
	if wk.Live == nil {
		var ok bool
		if h, ok = w.resume(ctx, wk); !ok {
			return
		}
		wk.Live = &registry.Live{Socket: h.Socket, Pane: h.Pane}
	}
	text := wk.Inbox[0]
	w.beforePrompt(ctx, d, h, wk.ID)
	if err := d.Prompt(ctx, h, text); err != nil {
		w.Log.Printf("%s: follow-up: %v", wk.Label(), err)
		return
	}
	now := w.now()
	w.set(wk, func(x *registry.Worker) {
		if len(x.Inbox) > 0 && x.Inbox[0] == text {
			x.Deliver(now)
		}
	})
	w.Log.Printf("%s: sent turn %d", wk.Label(), wk.Turn+1)
}

// resume opens a new tab on wk's harness session.
func (w *Watcher) resume(ctx context.Context, wk registry.Worker) (h worker.Handle, ok bool) {
	if wk.Session == "" {
		w.set(wk, func(x *registry.Worker) {
			x.State, x.Inbox = registry.NeedsYou, nil
			x.Error = fmt.Sprintf("cannot send it a follow-up: no harness session was recorded to resume; start a new worker (the dropped text: %q)", firstLine(wk.Inbox[0]))
		})
		return h, false
	}
	spec, err := w.spec(wk)
	if err == nil {
		spec.Session = wk.Session
		h, err = w.driver(wk).Start(ctx, spec)
	}
	if herdr.Unreachable(err) { // the follow-up waits for herdr
		w.unreachable(wk.Socket, err)
		w.discard(ctx, wk, h)
		return h, false
	}
	if err != nil {
		w.Log.Printf("%s: could not resume session %s: %v; dropped follow-up %q", wk.Label(), wk.Session, err, firstLine(wk.Inbox[0]))
		w.set(wk, func(x *registry.Worker) {
			if h.Pane != "" { // the tab opened: keep it to look at, and for routr cancel
				recordPane(x, h)
			}
			x.State, x.Inbox = registry.Failed, nil
			x.Error = fmt.Sprintf("could not resume session %s: %v. The follow-up was dropped: %q", wk.Session, err, firstLine(wk.Inbox[0]))
		})
		return h, false
	}
	w.reachable(wk.Socket)
	if _, ok := w.set(wk, func(x *registry.Worker) { record(x, h) }); !ok {
		w.driver(wk).Close(ctx, h)
		return h, false
	}
	w.forget(wk.ID)
	w.Log.Printf("%s: resumed session %s", wk.Label(), wk.Session)
	return h, true
}

// closeEndedChains closes the tabs a chain kept open once every worker in it
// has ended: a tab kept to look at (a failed resume, an API error that needs
// you). A tab reopened by a follow-up after the chain ended is left to it.
func (w *Watcher) closeEndedChains(ctx context.Context) error {
	all, err := w.store.List()
	if err != nil {
		return err
	}
	ended := map[string]bool{}
	endedAt := map[string]time.Time{}
	for _, wk := range all {
		if wk.Chain == "" {
			continue
		}
		if _, seen := ended[wk.Chain]; !seen {
			ended[wk.Chain] = true
		}
		if !wk.State.Terminal() || wk.TurnsPending() {
			ended[wk.Chain] = false
		}
		at := wk.Finished
		if !wk.Succeeded() {
			at = wk.Updated
		}
		if at.After(endedAt[wk.Chain]) {
			endedAt[wk.Chain] = at
		}
	}
	for _, wk := range all {
		if wk.Live != nil && ended[wk.Chain] && !wk.Prompted.After(endedAt[wk.Chain]) {
			w.closeTab(ctx, wk, w.driver(wk))
		}
	}
	return nil
}
