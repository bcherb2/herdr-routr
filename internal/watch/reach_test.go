package watch

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// alwaysDown is a driver whose herdr refuses every connection.
type alwaysDown struct{ worker.Driver }

func (alwaysDown) Start(context.Context, worker.Spec) (worker.Handle, error) {
	return worker.Handle{}, unreachableErr()
}

// The watcher gives up on a dead herdr socket instead of waiting for good,
// which would leave its workers running and queued and the watcher never
// idling out.
func TestAHerdrGoneForGoodIsGivenUp(t *testing.T) {
	r := newRig(t)
	live := r.add(registry.Worker{Brief: "go"})
	r.tick(1)
	r.state(live, registry.Running)

	r.fake.PollErr = unreachableErr()
	r.w = r.watcher()
	r.w.GiveUp = time.Minute
	r.w.Dial = func(string) worker.Driver { return alwaysDown{r.fake} }
	queued := r.add(registry.Worker{Brief: "later"})
	r.tick(2)
	r.state(live, registry.Running)
	r.state(queued, registry.Pending)
	if busy, _ := r.w.Tick(context.Background()); !busy {
		t.Error("gave up at once")
	}

	r.now = r.now.Add(time.Minute)
	busy, _ := r.w.Tick(context.Background())
	w := r.state(live, registry.Closed)
	if w.Live != nil || !strings.Contains(w.Note, "has not answered for 1m0s") || !strings.Contains(w.Note, "routr send 1") {
		t.Errorf("lost worker: %+v", w)
	}
	if busy {
		t.Error("still busy: a queued worker of a dead herdr keeps the watcher running")
	}
	r.state(queued, registry.Pending) // never failed for it: a new watcher tries again

	// herdr is back: a new watcher starts the queued worker.
	r.fake.PollErr = nil
	r.w = r.watcher()
	r.tick(1)
	r.state(queued, registry.Running)
}

func TestTheWatchLogIsCapped(t *testing.T) {
	r := newRig(t)
	if got := LogTo(r.p, &bytes.Buffer{}); got == nil {
		t.Fatal("nil writer")
	} else if _, ok := got.(*bytes.Buffer); !ok {
		t.Error("a stderr that is not the watch log is used as is")
	}
	f, err := os.OpenFile(r.p.WatchLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := LogTo(r.p, f)
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for range maxLog/len(line) + 10 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	fi, _ := os.Stat(r.p.WatchLog())
	old, err := os.Stat(r.p.WatchLog() + ".1")
	if err != nil || fi.Size() > maxLog || old.Size() == 0 || old.Size() > maxLog {
		t.Errorf("watch.log %d bytes, watch.log.1 %v (%v)", fi.Size(), old, err)
	}
}
