// Package watch is `routr watch`: the one process per repo that drives
// workers between CLI calls.
//
// CLI commands only write the registry; the watcher reads it every tick and
// does the herdr work through a worker.Driver: it starts queued workers whose
// dependencies, gate and directory lock allow it, sends them their brief,
// reads their phase, picks up `routr done`, delivers follow-ups (resuming
// closed sessions), and follows review loops. It keeps no state of its own
// beyond what it has seen of each pane's phase, so a restarted watcher picks
// up where the last one stopped. See docs/architecture.md, "The watcher".
package watch

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
	"github.com/bcherb2/herdr-routr/internal/jev"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// Defaults.
const (
	DefaultSettle   = time.Second
	DefaultInterval = 500 * time.Millisecond
	DefaultIdleExit = 10 * time.Minute
	// staleIdle bounds how long an idle phase that predates a prompt is
	// distrusted when herdr's sequence number cannot tell (see observe).
	staleIdle = 15 * time.Second
)

// Watcher drives the workers of one registry.
type Watcher struct {
	Paths    registry.Paths
	Dial     func(socket string) worker.Driver                // the driver for a worker's herdr socket
	Env      func() (map[string]string, error)                // config [env], exported in the tabs of workers queued without their own (registry.Worker.Env); nil = none
	Bin      string                                           // the routr binary, exported as ROUTR_BIN
	Build    string                                           // its revision, for the start line in the log; "" = not said
	Now      func() time.Time                                 // default time.Now
	Sleep    func(ctx context.Context, d time.Duration) error // between ticks; default a timer
	Settle   time.Duration                                    // a phase must hold this long to count (default 1s)
	Interval time.Duration                                    // between ticks (default 500ms)
	IdleExit time.Duration                                    // Run returns after this long with nothing to do (default 10m)
	GiveUp   time.Duration                                    // herdr out of reach this long: its workers are given up (default 10m)
	Log      *log.Logger                                      // default: discard
	Jev      Decider                                          // judge steps and questions (answer = jev) ask it; nil = a Jev client with OPENROUTER_API_KEY (config [env] first)
	// Questions is config [questions], read each time Jev decides a
	// worker's question; nil = the built-in defaults.
	Questions func() (config.Questions, error)

	once    sync.Once
	store   *registry.Store
	mu      sync.Mutex // guards what follows: starts run concurrently
	seen    map[int]*sighting
	jobs    map[int]job         // run steps' commands, judges' and questions' Jev calls this watcher started, by worker id
	picks   map[pick]jev.Answer // Jev's answers to questions in this run, kept for the same question asked again
	drivers map[string]worker.Driver
	denied  error                // herdr refused this process a connection (a sandbox); Run stops
	down    map[string]time.Time // herdr socket → when it was first found out of reach, until it answers

	lastUnreachable string            // the last herdr connection error logged
	tabEnv          map[string]string // config [env], read each tick (loadEnv): the env of records without their own (envOf)
	lastEnvErr      string            // the last config error logged
}

// errStale aborts a registry change when a CLI command moved the worker
// first (cancel, close); the watcher then leaves it alone.
var errStale = errors.New("worker changed under the watcher")

// ErrDenied is returned by Run when herdr refused this process a
// connection, as a sandbox does.
var ErrDenied = errors.New("the watcher may not connect to herdr from here")

func (w *Watcher) deniedErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.denied
}

func (w *Watcher) init() {
	w.once.Do(func() {
		w.store = w.Paths.Store()
		w.store.Now = w.now
		w.seen = map[int]*sighting{}
		w.jobs = map[int]job{}
		w.picks = map[pick]jev.Answer{}
		w.drivers = map[string]worker.Driver{}
		w.down = map[string]time.Time{}
		if w.Log == nil {
			w.Log = log.New(io.Discard, "", 0)
		}
		if w.Settle == 0 {
			w.Settle = DefaultSettle
		}
		if w.Interval == 0 {
			w.Interval = DefaultInterval
		}
		if w.GiveUp == 0 {
			w.GiveUp = DefaultGiveUp
		}
		if w.IdleExit == 0 {
			w.IdleExit = DefaultIdleExit
		}
	})
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) sleep(ctx context.Context, d time.Duration) error {
	if w.Sleep != nil {
		return w.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run ticks until ctx ends or nothing has been pending, running or blocked
// for IdleExit. Errors in a tick are logged, not fatal: the next tick retries.
func (w *Watcher) Run(ctx context.Context) error {
	w.init()
	if w.Build != "" {
		w.Log.Printf("watching %s (routr %s)", w.Paths.Dir, w.Build)
	} else {
		w.Log.Printf("watching %s", w.Paths.Dir)
	}
	lastBusy := w.now()
	for {
		busy, err := w.Tick(ctx)
		if err != nil {
			w.Log.Printf("tick: %v", err)
		}
		if err := w.deniedErr(); err != nil {
			w.Log.Printf("exiting: this process may not connect to herdr (%v), as in a sandbox; a watcher started outside it takes over", err)
			return ErrDenied
		}
		if busy || err != nil {
			lastBusy = w.now()
		} else if w.now().Sub(lastBusy) >= w.IdleExit {
			w.Log.Printf("nothing to do for %s; exiting", w.IdleExit)
			return nil
		}
		if err := w.sleep(ctx, w.Interval); err != nil {
			return err
		}
	}
}

// Tick is one pass: record ended commands, start what can start, read live
// workers, deliver follow-ups, close the tabs of ended chains. busy reports whether any
// worker can still move without you.
func (w *Watcher) Tick(ctx context.Context) (busy bool, err error) {
	w.init()
	w.loadEnv()
	steps := []func(context.Context) error{w.recoverStarting, w.tendRuns, w.startQueued, w.pollLive, w.deliverInbox, w.closeEndedChains}
	var errs []error
	for _, step := range steps {
		if err := step(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	all, err := w.store.List()
	if err != nil {
		return false, errors.Join(append(errs, err)...)
	}
	for _, wk := range all {
		if busyWorker(wk) && (wk.Tabless() || !w.gaveUp(wk.Socket)) { // a command or Jev needs no herdr
			busy = true
		}
	}
	return busy, errors.Join(errs...)
}

// busyWorker reports whether wk can move on its own. Waiting and gated
// workers move only when another worker (itself busy) or you act, and every
// CLI command that acts starts the watcher.
func busyWorker(wk registry.Worker) bool {
	switch wk.State {
	case registry.Pending, registry.Starting, registry.Running, registry.Blocked:
		return true
	}
	return wk.TurnsPending() && wk.State != registry.Cancelled
}

func byID(all []registry.Worker) map[int]registry.Worker {
	m := make(map[int]registry.Worker, len(all))
	for _, wk := range all {
		m[wk.ID] = wk
	}
	return m
}

// driver is the driver for wk's herdr server, one per socket.
func (w *Watcher) driver(wk registry.Worker) worker.Driver {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.drivers[wk.Socket]
	if !ok {
		d = w.Dial(wk.Socket)
		w.drivers[wk.Socket] = d
	}
	return d
}

func (w *Watcher) runDir(wk registry.Worker) string { return w.Paths.RunDir(wk.ID) }

// writeRun writes a file in wk's run dir, logging failures: the registry
// change it belongs to still goes ahead.
func (w *Watcher) writeRun(wk registry.Worker, path, text string) {
	if err := registry.WriteFile(path, []byte(text)); err != nil {
		w.Log.Printf("%s: write %s: %v", wk.Label(), path, err)
	}
}

// loadEnv reads config [env] once a tick for envOf.
func (w *Watcher) loadEnv() {
	if w.Env == nil {
		return
	}
	env, err := w.Env()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if msg := err.Error(); msg != w.lastEnvErr {
			w.lastEnvErr = msg
			w.Log.Printf("config: %v", err)
		}
		return
	}
	w.tabEnv, w.lastEnvErr = env, ""
}

// handle is Handle with the environment of wk's tab, so that the driver
// finds a harness's transcript where it writes it (CODEX_HOME in config
// [env], say).
func (w *Watcher) handle(wk registry.Worker) worker.Handle {
	h := Handle(wk)
	h.Env = w.envOf(wk)
	return h
}

// envOf is the env wk's tab or command gets besides routr's own: its own,
// frozen when it was queued, or for a record from before that, config [env]
// as of this tick.
func (w *Watcher) envOf(wk registry.Worker) map[string]string {
	if wk.Env != nil {
		return wk.Env
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tabEnv
}

// Handle is the driver's handle for a worker, from its registry record.
func Handle(wk registry.Worker) worker.Handle {
	h := worker.Handle{Socket: wk.Socket, Harness: wk.Harness, Session: wk.Session, Transcript: wk.Transcript, Cwd: wk.Cwd}
	if wk.Live != nil {
		h.Socket, h.Pane, h.Tab, h.Agent = wk.Live.Socket, wk.Live.Pane, wk.Live.Tab, wk.Live.Agent
	}
	return h
}

// record stores what a handle says about where a worker runs.
func record(wk *registry.Worker, h worker.Handle) {
	recordPane(wk, h)
	if h.Session != "" {
		wk.Session = h.Session
	}
	if h.Transcript != "" {
		wk.Transcript = h.Transcript
	}
}

// recordPane stores a handle's pane only, not its session: that of a failed
// Start may never have been created.
func recordPane(wk *registry.Worker, h worker.Handle) {
	wk.Live = &registry.Live{Socket: h.Socket, Pane: h.Pane, Tab: h.Tab, Agent: h.Agent}
}
