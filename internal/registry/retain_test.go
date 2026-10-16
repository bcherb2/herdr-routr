package registry

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ids(t *testing.T, s *Store) []int {
	t.Helper()
	ws, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, w := range ws {
		out = append(out, w.ID)
	}
	return out
}

// seed creates a worker, sets it up with fn and gives every worker a run
// directory with a final.md.
func seed(t *testing.T, s *Store, p Paths, w Worker, fn func(*Worker)) int {
	t.Helper()
	w.Role = "task"
	c, err := s.Create(w)
	if err != nil {
		t.Fatal(err)
	}
	if fn != nil {
		if _, err := s.Update(c.ID, fn); err != nil {
			t.Fatal(err)
		}
	}
	if p.Dir != "" {
		if err := WriteFile(p.FinalPath(c), []byte("final\n")); err != nil {
			t.Fatal(err)
		}
	}
	return c.ID
}

func TestRetentionDropsOnlyFinishedUnneededOldWorkers(t *testing.T) {
	now := t0
	root := t.TempDir()
	p := Paths{Dir: filepath.Join(root, ".routr")}
	s := p.Store()
	s.Now = clock(&now)
	done := func(w *Worker) { w.State = Done }

	plan := filepath.Join(root, "PLAN.md")
	os.WriteFile(plan, []byte("plan\n"), 0o644)
	old := seed(t, s, p, Worker{State: Pending, Write: true, Tree: "/src/app", Output: plan}, nil)
	if _, h, err := s.Claim(old, func(w *Worker) error { w.State = Starting; return nil }); err != nil || h != old {
		t.Fatalf("claim: %d %v", h, err)
	}
	s.Update(old, done)
	running := seed(t, s, p, Worker{State: Running}, nil)
	queued := seed(t, s, p, Worker{State: Closed}, func(w *Worker) { w.Queue("more?") })
	inFlight := seed(t, s, p, Worker{State: Done}, func(w *Worker) { w.Turn, w.Replies = 2, 1 })
	tabOpen := seed(t, s, p, Worker{State: Done}, func(w *Worker) { w.Live = &Live{Pane: "w1:p2"} })
	upstream := seed(t, s, p, Worker{State: Done}, nil)
	seed(t, s, p, Worker{State: Waiting, After: []int{upstream}}, nil)
	resumed := seed(t, s, p, Worker{State: Done}, nil)
	seed(t, s, p, Worker{State: Running, OnFail: &OnFail{Resume: resumed, Max: 3}}, nil)
	halfChain := seed(t, s, p, Worker{State: Done, Chain: "half-1"}, nil)
	seed(t, s, p, Worker{State: Pending, Chain: "half-1", Step: "b"}, nil)
	endedChain := []int{
		seed(t, s, p, Worker{State: Done, Chain: "ended-1"}, nil),
		seed(t, s, p, Worker{State: Failed, Chain: "ended-1"}, nil),
	}
	ownOutput := seed(t, s, p, Worker{State: Done}, nil)
	inRun := filepath.Join(p.RunDir(ownOutput), "notes.md")
	s.Update(ownOutput, func(w *Worker) { w.Output = inRun })
	os.WriteFile(inRun, []byte("notes\n"), 0o644)
	lateChain := seed(t, s, p, Worker{State: Done, Chain: "late-1"}, nil)
	lateSibling := seed(t, s, p, Worker{State: Done, Chain: "late-1"}, nil)
	usedByFinished := seed(t, s, p, Worker{State: Cancelled}, nil)
	last := seed(t, s, p, Worker{State: Done}, nil)
	if got := ids(t, s); len(got) != last {
		t.Fatalf("retention dropped young workers: %v", got)
	}
	work := []string{"ended-1", "half-1", "late-1", "c" + strconv.Itoa(last), "c" + strconv.Itoa(running)}
	for _, c := range work {
		os.MkdirAll(filepath.Join(p.WorkDir(c), "findings"), 0o755)
	}

	now = now.Add(defaultKeep + time.Hour)
	s.Update(lateSibling, func(w *Worker) { w.Note = "touched" }) // the write that drops the rest
	young := seed(t, s, p, Worker{State: Done, After: []int{usedByFinished}}, nil)
	if young != last+1 {
		t.Errorf("new id %d after dropping %d: ids are not reused", young, last)
	}

	got := ids(t, s)
	gone := []int{old, usedByFinished, ownOutput, last, endedChain[0], endedChain[1]}
	for _, id := range gone {
		if slices.Contains(got, id) {
			t.Errorf("worker %d was kept: %v", id, got)
		}
	}
	for _, id := range []int{running, queued, inFlight, tabOpen, upstream, resumed, halfChain, lateChain, lateSibling, young} {
		if !slices.Contains(got, id) {
			t.Errorf("worker %d was dropped: %v", id, got)
		}
	}

	for _, id := range got {
		if _, err := os.Stat(p.FinalPath(Worker{ID: id, Runs: 1})); err != nil {
			t.Errorf("a kept worker lost its run dir: %v", err)
		}
	}
	for _, id := range []int{old, usedByFinished, last, endedChain[0], endedChain[1]} {
		if _, err := os.Stat(p.RunDir(id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("run dir of dropped worker %d: %v", id, err)
		}
	}
	for i, c := range work {
		_, err := os.Stat(p.WorkDir(c))
		if gone := i == 0 || i == 3; gone != errors.Is(err, os.ErrNotExist) {
			t.Errorf("work dir of chain %s (dropped whole: %v): %v", c, gone, err)
		}
	}
	if b, err := os.ReadFile(inRun); err != nil || string(b) != "notes\n" {
		t.Errorf("a dropped worker's output in its run dir was removed: %v", err)
	}
	if _, err := os.Stat(plan); err != nil {
		t.Errorf("a dropped worker's output was removed: %v", err)
	}
	if b, _ := os.ReadFile(p.Registry()); strings.Contains(string(b), `"locks"`) {
		t.Errorf("a dropped worker's directory lock stayed:\n%s", b)
	}

	again, _ := (&Store{Path: p.Registry()}).Create(Worker{Role: "task", State: Pending})
	if again.ID != young+1 {
		t.Errorf("reopened store gave id %d", again.ID)
	}
}

func TestRetentionMaxDropsLeastRecentlyTouchedFirst(t *testing.T) {
	now := t0
	s := &Store{Now: clock(&now)}
	at := func(d time.Duration, w Worker) int {
		now = t0.Add(d)
		w.Role = "task"
		c, _ := s.Create(w)
		return c.ID
	}
	one := at(time.Hour, Worker{State: Done})
	two := at(0, Worker{State: Done})
	busy := at(0, Worker{State: Running})
	chainA := at(2*time.Hour, Worker{State: Done, Chain: "x-1"})
	chainB := at(30*time.Minute, Worker{State: Done, Chain: "x-1"})
	six := at(3*time.Hour, Worker{State: Closed})

	s.retain = &retention{Retention: Retention{Off: true, Max: 1}}
	s.Update(busy, func(*Worker) {})
	if got := ids(t, s); len(got) != 6 {
		t.Fatalf("Off dropped workers: %v", got)
	}
	s.retain = &retention{Retention: Retention{Max: 3}}
	s.Update(busy, func(*Worker) {})
	got := ids(t, s)
	if !slices.Equal(got, []int{busy, six}) {
		t.Errorf("kept %v; want %d (active) and %d (newest), dropping %d, %d, then chain %d+%d whole",
			got, busy, six, two, one, chainA, chainB)
	}
}

// A command that brings a finished worker back (`routr send` queues a
// follow-up) and a watcher write that would drop it are both one
// transaction under the registry's flock, and retention looks at the
// registry as each transaction leaves it. So whichever commits first wins:
// the follow-up keeps the worker, or the send finds no worker and says so.
// It is never queued on a worker that is then dropped.
func TestRetentionAndSendSerialize(t *testing.T) {
	for _, sendFirst := range []bool{true, false} {
		t.Run("send first "+strconv.FormatBool(sendFirst), func(t *testing.T) {
			now := t0
			p := Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
			setup := &Store{Path: p.Registry(), Now: clock(&now)}
			closed := seed(t, setup, p, Worker{State: Closed, Finished: t0}, nil)
			other := seed(t, setup, p, Worker{State: Running}, nil)
			now = now.Add(defaultKeep + time.Hour)

			cli, watcher := p.Store(), p.Store()
			cli.Now, watcher.Now = clock(&now), clock(&now)
			send := func() error {
				_, err := cli.Modify(closed, func(w *Worker) error { w.Queue("one more thing"); return nil })
				return err
			}
			tick := func() {
				if _, err := watcher.Update(other, func(w *Worker) { w.Note = "tick" }); err != nil {
					t.Fatal(err)
				}
			}
			var sendErr error
			if sendFirst {
				sendErr = send()
				tick()
			} else {
				tick()
				sendErr = send()
			}
			w, getErr := watcher.Get(closed)
			_, dirErr := os.Stat(p.RunDir(closed))
			switch {
			case sendFirst && (sendErr != nil || getErr != nil || len(w.Inbox) != 1 || dirErr != nil):
				t.Errorf("a queued follow-up lost its worker: send %v, get %v, inbox %v, run dir %v", sendErr, getErr, w.Inbox, dirErr)
			case !sendFirst && (!errors.Is(sendErr, ErrNotFound) || !errors.Is(getErr, ErrNotFound) || dirErr == nil):
				t.Errorf("send to a dropped worker: send %v, get %v, run dir %v", sendErr, getErr, dirErr)
			}
		})
	}
}

func TestRemoveRunsStaysInsideRuns(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, "runs")
	keep := filepath.Join(root, "3")
	os.MkdirAll(keep, 0o755)
	os.MkdirAll(filepath.Join(runs, "3"), 0o755)
	removeRuns("runs", []Worker{{ID: 3}}) // relative: nothing
	removeRuns(runs, []Worker{{ID: 0}, {ID: -1}})
	if _, err := os.Stat(filepath.Join(runs, "3")); err != nil {
		t.Fatal(err)
	}
	removeRuns(runs, []Worker{{ID: 3}})
	if _, err := os.Stat(filepath.Join(runs, "3")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("runs/3: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("removed outside runs: %v", err)
	}
	work := filepath.Join(root, "work")
	os.MkdirAll(filepath.Join(work, "c-1"), 0o755)
	removeWork(work, []string{"..", "../work", "", "c-1/.."})
	if _, err := os.Stat(filepath.Join(work, "c-1")); err != nil {
		t.Fatalf("removed outside work: %v", err)
	}
	removeWork(work, []string{"c-1"})
	if _, err := os.Stat(filepath.Join(work, "c-1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("work/c-1: %v", err)
	}
	if !within("/a/runs/3", "/a/runs/3/x.md") || within("/a/runs/3", "/a/runs/30/x.md") || within("/a/runs/3", "/a/PLAN.md") {
		t.Error("within")
	}
}
