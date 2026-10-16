//go:build live

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdrtest"
	"github.com/bcherb2/herdr-routr/internal/route"
)

// Live tests drive REAL harnesses (they cost money) in an isolated herdr
// server: start, one tiny turn, close, resume the session, one more turn.
//
//	go test -tags live -run TestLive -count=1 -v ./internal/worker/
//
// ROUTR_LIVE_HARNESSES limits the harnesses (e.g. "pi,codex");
// ROUTR_LIVE_WORK is the work directory (default /tmp/routr-live-work,
// reused so each harness records folder trust for it only once).
func TestLive(t *testing.T) {
	cases := []Spec{
		{Harness: "claude", Model: "haiku", Effort: "low", Permissions: PermAuto},
		{Harness: "codex", Effort: "low", Permissions: PermAuto},
		{Harness: "pi", Model: "deepseek/deepseek-v4-flash", Effort: "low"},
	}
	only := os.Getenv("ROUTR_LIVE_HARNESSES")
	work := os.Getenv("ROUTR_LIVE_WORK")
	if work == "" {
		work = "/tmp/routr-live-work"
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := herdrtest.Start(t)
	for i, s := range cases {
		t.Run(s.Harness, func(t *testing.T) {
			if only != "" && !strings.Contains(only, s.Harness) {
				t.Skip("not in ROUTR_LIVE_HARNESSES")
			}
			if err := ready(s.Harness); err != nil {
				t.Skipf("%s not ready: %v", s.Harness, err)
			}
			s.ID, s.Cwd = i+1, work
			s.Label = s.Harness + " live"
			s.Env = map[string]string{"ROUTR_ID": s.Harness}
			d := NewHerdr(srv.Client).(*herdrDriver)
			d.c = keyLogger{d.c, t}
			liveRoundTrip(t, d, s)
		})
	}
}

// keyLogger logs every key the driver sends with the screen it answers, as
// evidence of which startup dialogs were handled.
type keyLogger struct {
	api
	t *testing.T
}

func (k keyLogger) SendAgentKeys(ctx context.Context, target string, keys ...string) error {
	screen, _ := k.ReadAgent(ctx, target, "visible", 0)
	var shown []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			shown = append(shown, l)
		}
	}
	k.t.Logf("keys %v on screen:\n  %s", keys, strings.Join(shown[max(0, len(shown)-14):], "\n  "))
	return k.api.SendAgentKeys(ctx, target, keys...)
}

// TestLiveAskAndExit has Claude (default permissions) ask before writing a
// file, answers the menu with Answer, then exits Claude to see Exited.
func TestLiveAskAndExit(t *testing.T) {
	if err := ready("claude"); err != nil {
		t.Skipf("claude not ready: %v", err)
	}
	work := t.TempDir()
	srv := herdrtest.Start(t)
	d := NewHerdr(srv.Client).(*herdrDriver)
	d.c = keyLogger{d.c, t}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h := liveStart(ctx, t, d, Spec{ID: 9, Harness: "claude", Model: "haiku", Effort: "low", Cwd: work})
	defer d.Close(context.Background(), h)

	if err := d.Prompt(ctx, h, "Create a file named ask.txt containing the word ok, using the Write tool. Then reply done."); err != nil {
		t.Fatal(err)
	}
	st := pollUntil(ctx, t, d, h, func(st Status) bool { return st.Phase == Asking })
	t.Logf("asking:\n%s\noptions %q", st.Question, st.Options)
	if len(st.Options) < 2 || !strings.Contains(st.Question, "ask.txt") {
		t.Errorf("question = %q, options %q", st.Question, st.Options)
	}
	if _, err := d.Answer(ctx, h, 1, "", false); err != nil {
		t.Fatal(err)
	}
	pollUntil(ctx, t, d, h, func(st Status) bool { return st.Phase == Idle })
	if b, err := os.ReadFile(filepath.Join(work, "ask.txt")); err != nil || !strings.Contains(string(b), "ok") {
		t.Errorf("ask.txt = %q, %v", b, err)
	}

	if err := d.Prompt(ctx, h, "/exit"); err != nil {
		t.Logf("prompt /exit: %v", err) // the harness may exit before herdr sees it work
	}
	pollUntil(ctx, t, d, h, func(st Status) bool { return st.Phase == Exited })
}

// pollUntil polls every 500 ms until ok holds for a status.
func pollUntil(ctx context.Context, t *testing.T, d Driver, h Handle, ok func(Status) bool) Status {
	t.Helper()
	var phases []Phase
	for {
		st, err := d.Poll(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(phases); n == 0 || phases[n-1] != st.Phase {
			phases = append(phases, st.Phase)
		}
		if ok(st) {
			t.Logf("phases %v", phases)
			return st
		}
		select {
		case <-ctx.Done():
			t.Fatalf("condition never held; phases %v", phases)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ready(kind string) error {
	for _, h := range route.Defaults() {
		if h.Kind == kind {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return h.Ready(ctx)
		}
	}
	return nil
}

func liveRoundTrip(t *testing.T, d Driver, s Spec) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	h := liveStart(ctx, t, d, s)
	liveTurn(ctx, t, d, &h, "Reply with just the word pong and nothing else. Do not use any tools.")
	reply, err := d.LastReply(ctx, h)
	t.Logf("reply: %q (transcript %s)", reply, h.Transcript)
	if err != nil || !strings.Contains(strings.ToLower(reply), "pong") {
		t.Fatalf("LastReply = %q, %v", reply, err)
	}
	if h.Session == "" || h.Transcript == "" {
		t.Fatalf("no session or transcript after a turn: %+v", h)
	}
	if err := d.Close(ctx, h); err != nil {
		t.Fatal(err)
	}
	if st, err := d.Poll(ctx, h); err != nil || st.Phase != Gone {
		t.Fatalf("after Close: %+v, %v", st, err)
	}

	s.Session = h.Session
	r := liveStart(ctx, t, d, s)
	defer d.Close(context.Background(), r)
	liveTurn(ctx, t, d, &r, "What single word did you reply with before? Answer with just that word.")
	reply, err = d.LastReply(ctx, r)
	t.Logf("resumed reply: %q (session %s → %s)", reply, h.Session, r.Session)
	if err != nil || !strings.Contains(strings.ToLower(reply), "pong") {
		t.Fatalf("resumed LastReply = %q, %v", reply, err)
	}
	if r.Session != h.Session {
		t.Errorf("session changed on resume: %s → %s", h.Session, r.Session)
	}
}

func liveStart(ctx context.Context, t *testing.T, d Driver, s Spec) Handle {
	t.Helper()
	start := time.Now()
	h, err := d.Start(ctx, s)
	t.Logf("Start(session=%q) in %s: %+v", s.Session, time.Since(start).Round(time.Millisecond), h)
	if err != nil {
		if h.Pane != "" {
			d.Close(context.Background(), h)
		}
		t.Fatal(err)
	}
	if s.Harness != "codex" && h.Transcript == "" {
		t.Errorf("no transcript known at Start")
	}
	return h
}

// liveTurn prompts and polls until the worker has been idle for 1.5 s,
// recording the session and transcript as a watcher would.
func liveTurn(ctx context.Context, t *testing.T, d Driver, h *Handle, text string) {
	t.Helper()
	start := time.Now()
	if err := d.Prompt(ctx, *h, text); err != nil {
		t.Fatal(err)
	}
	t.Logf("Prompt returned after %s", time.Since(start).Round(time.Millisecond))
	idle, phases := 0, []Phase{}
	for idle < 3 {
		st, err := d.Poll(ctx, *h)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(phases); n == 0 || phases[n-1] != st.Phase {
			phases = append(phases, st.Phase)
		}
		switch st.Phase {
		case Idle:
			idle++
		case Asking:
			t.Fatalf("worker asks:\n%s", st.Question)
		case Exited, Gone:
			t.Fatalf("worker %s", st.Phase)
		default:
			idle = 0
		}
		if st.Session != "" {
			h.Session = st.Session
		}
		if st.Transcript != "" {
			h.Transcript = st.Transcript
		}
		select {
		case <-ctx.Done():
			t.Fatalf("turn never settled; phases %v", phases)
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Logf("turn settled after %s; phases %v; transcript %s", time.Since(start).Round(time.Millisecond), phases, filepath.Base(h.Transcript))
}
