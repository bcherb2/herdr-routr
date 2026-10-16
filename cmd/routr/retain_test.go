package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// seedAt creates w as if it last changed at `at`.
func (c *cli) seedAt(at time.Time, w registry.Worker) registry.Worker {
	c.t.Helper()
	s := c.p.Store()
	s.Now = func() time.Time { return at }
	if w.Cwd == "" {
		w.Cwd = c.dir
	}
	w, err := s.Create(w)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := registry.WriteFile(c.p.FinalPath(w), []byte("final\n")); err != nil {
		c.t.Fatal(err)
	}
	return w
}

func TestRetentionFollowsTheConfig(t *testing.T) {
	for _, k := range []struct {
		name, config string
		dropped      bool
	}{
		{"the default keeps 14 days", "", false},
		{"keep_days", "[registry]\nkeep_days = 1\n", true},
		{"a config it cannot read keeps everything", "[registry]\nkeep_days = 1\nbogus = 1\n", false},
	} {
		t.Run(k.name, func(t *testing.T) {
			c := newCLI(t)
			if k.config != "" {
				os.MkdirAll(c.p.Dir, 0o755)
				os.WriteFile(c.p.Config(), []byte(k.config), 0o644)
			}
			old := c.seedAt(time.Now().Add(-48*time.Hour), registry.Worker{Role: "research", State: registry.Done})
			c.seed(registry.Worker{State: registry.Pending})
			c.mustRun("", "cancel", "2") // any write applies retention
			_, err := c.p.Store().Get(old.ID)
			_, dirErr := os.Stat(c.p.RunDir(old.ID))
			if dropped := errors.Is(err, registry.ErrNotFound); dropped != k.dropped || (dirErr == nil) == dropped {
				t.Errorf("worker %d: %v, run dir: %v; want dropped %v", old.ID, err, dirErr, k.dropped)
			}
			if out := c.mustRun("", "list", "--all"); strings.Contains(out, "research") == k.dropped {
				t.Errorf("list --all:\n%s", out)
			}
		})
	}
}

// `routr send` to a closed worker old enough to drop, racing a watcher
// write that applies retention: each is one transaction under the
// registry's flock, so either the follow-up is queued and keeps the worker
// (and its run dir), or the worker was dropped first and send says there
// is no such worker. A follow-up is never queued on a dropped worker.
func TestSendRacingRetentionNeverLosesAFollowUp(t *testing.T) {
	outcomes := map[bool]int{}
	for i := range 40 {
		c := newCLI(t)
		long := time.Now().Add(-30 * 24 * time.Hour)
		closed := c.seedAt(long, registry.Worker{State: registry.Closed, Finished: long, Session: "s-1"})
		busy := c.seedAt(long, registry.Worker{State: registry.Running})
		watcher := c.p.Store()

		var wg sync.WaitGroup
		var code int
		var errOut string
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			code, _, errOut = c.run("", "send", "1", "one more thing")
		}()
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(i) * 100 * time.Microsecond) // stagger it across send's steps
			if _, err := watcher.Update(busy.ID, func(w *registry.Worker) { w.Note = "tick" }); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()

		w, err := c.p.Store().Get(closed.ID)
		_, dirErr := os.Stat(filepath.Join(c.p.Dir, "runs", "1"))
		queued := code == 0
		outcomes[queued]++
		switch {
		case queued && (err != nil || len(w.Inbox) != 1 || dirErr != nil):
			t.Fatalf("send queued a follow-up, then: worker %+v, %v; run dir %v", w, err, dirErr)
		case !queued && (!strings.Contains(errOut, "no worker 1") || !errors.Is(err, registry.ErrNotFound) || dirErr == nil):
			t.Fatalf("send exit %d (%s); worker: %v; run dir: %v", code, errOut, err, dirErr)
		}
	}
	t.Logf("queued / dropped first: %d / %d", outcomes[true], outcomes[false])
}
