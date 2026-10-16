package watch

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// A turn that an API error ended (a login refresh racing another Claude
// process, an overloaded server) often goes through a minute later, so the
// watcher tries it again a couple of times before it needs you.
const (
	retryDelay = time.Minute
	maxRetries = 2
	retryText  = "Your last turn ended in an API error. Carry on with the task where you left off."
)

// lasting are signs of an API error that waiting won't fix, unless the
// harness itself says to try again.
var lasting = regexp.MustCompile(`(?i)/login|log ?in again|sign in|authenticat|unauthori[sz]ed|api key|expired|credit balance|billing|usage limit|hit your limit|quota|/model|may not exist|not have access`)

func transient(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "try again") || !lasting.MatchString(msg)
}

// turnFailed handles a settled idle whose turn ended in the harness's API
// error: it schedules a retry, sends one that is due, or gives up.
func (w *Watcher) turnFailed(ctx context.Context, wk registry.Worker, d worker.Driver, h worker.Handle, msg string) {
	switch wk.State {
	case registry.Running, registry.Blocked, registry.MaybeDone:
	default:
		return
	}
	now := w.now()
	switch {
	case wk.Retry.IsZero() && wk.Retries < maxRetries && transient(msg):
		at := now.Add(retryDelay)
		w.set(wk, func(x *registry.Worker) {
			x.State, x.TurnError, x.Retry = registry.Running, msg, at
			x.ClearQuestion()
			x.Note = fmt.Sprintf("its turn ended in an API error; retrying at %s (%d of %d): %s", at.Format("15:04:05"), wk.Retries+1, maxRetries, msg)
		})
		w.Log.Printf("%s: API error, retrying at %s: %s", wk.Label(), at.Format("15:04:05"), firstLine(msg))
	case !wk.Retry.IsZero() && now.Before(wk.Retry):
	case !wk.Retry.IsZero():
		w.beforePrompt(ctx, d, h, wk.ID)
		if err := d.Prompt(ctx, h, retryText); err != nil {
			w.Log.Printf("%s: retry: %v", wk.Label(), err)
			w.giveUp(wk, msg)
			return
		}
		w.set(wk, func(x *registry.Worker) {
			x.Retries, x.Retry = x.Retries+1, time0
			x.Note = fmt.Sprintf("retried after an API error (%d of %d)", x.Retries, maxRetries)
		})
		w.Log.Printf("%s: retried after an API error (%d of %d)", wk.Label(), wk.Retries+1, maxRetries)
	default:
		w.giveUp(wk, msg)
	}
}

// giveUp records a failed turn that won't be retried. A follow-up to a
// finished worker gets the error as its reply and the worker stays
// finished; any other worker needs you, with its tab left open.
func (w *Watcher) giveUp(wk registry.Worker, msg string) {
	how := fmt.Sprintf("routr send %d 'carry on' tries again; routr show %d shows its tab", wk.ID, wk.ID)
	msg = strings.TrimRight(msg, ". ")
	inFlight := wk.Turn > wk.Replies
	if inFlight {
		w.writeRun(wk, w.Paths.TurnFile(wk.ID, wk.Turn), fmt.Sprintf("The harness's turn ended in an API error: %s\n", msg))
	}
	w.set(wk, func(x *registry.Worker) {
		x.Retry, x.TurnError = time0, msg
		x.ClearQuestion()
		if inFlight {
			x.Replied()
		}
		if x.State.Terminal() {
			x.Note = fmt.Sprintf("its last follow-up ended in an API error: %s. %s", msg, how)
			return
		}
		x.State, x.Note = registry.NeedsYou, ""
		x.Error = fmt.Sprintf("its turn ended in an API error: %s. %s", msg, how)
	})
	w.Log.Printf("%s: API error, needs you: %s", wk.Label(), firstLine(msg))
}
