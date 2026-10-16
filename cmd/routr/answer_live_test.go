package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/herdrtest"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/watch"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// TestLiveAnswerExpect answers a real Claude Haiku permission prompt with
// routr answer --expect, in a throwaway herdr server, with an in-process
// watcher: a stale fingerprint presses nothing and leaves the prompt as it
// was, and so does option 2 by agent (it switches the session to accept
// edits, which only the user may pick); the one the watcher recorded
// answers it by the user (option 2, so the highlight moves before enter),
// and the worker goes on.
// It costs a Haiku turn or two. Run it with Claude's own config, as real
// workers have:
//
//	env -u CLAUDE_CONFIG_DIR ROUTR_LIVE_CLAUDE=1 go test -run TestLiveAnswerExpect -count=1 -v ./cmd/routr/
func TestLiveAnswerExpect(t *testing.T) {
	if os.Getenv("ROUTR_LIVE_CLAUDE") != "1" {
		t.Skip("set ROUTR_LIVE_CLAUDE=1 to answer a real Claude Haiku prompt")
	}
	srv := herdrtest.Start(t)
	bin := filepath.Join(t.TempDir(), "routr")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	c := newCLI(t)
	c.env["HERDR_SOCKET_PATH"] = srv.Socket()
	delete(c.env, "HERDR_WORKSPACE_ID") // the throwaway server's own "routr" workspace
	dial := func(socket string) worker.Driver { return worker.NewHerdr(herdr.New(socket)) }
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		a := c.app("", &out, &errb)
		a.dial, a.bin = dial, bin
		code := a.run(args)
		t.Logf("routr %s: exit %d\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
		return code, out.String(), errb.String()
	}
	readLog := func() string { b, _ := os.ReadFile(c.p.WatchLog()); return string(b) }
	if code, _, _ := run("start", "--harness", "claude", "--model", "haiku", "--effort", "low", "--permissions", "default",
		"--output", "ask.txt", "Create the file ask.txt containing the word ok, with the Write tool. Do nothing else."); code != 0 {
		t.FailNow()
	}

	logf, err := os.OpenFile(c.p.WatchLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	w := &watch.Watcher{Paths: c.p, Dial: dial, Bin: bin, Interval: 250 * time.Millisecond, IdleExit: time.Hour,
		Log: log.New(logf, "", log.LstdFlags)}
	go func() { defer close(stopped); w.Run(ctx) }()
	t.Cleanup(func() {
		run("cancel", "1")
		cancel()
		<-stopped
		logf.Close()
		t.Logf("watch.log:\n%s", readLog())
	})
	until := func(what string, limit time.Duration, ok func(registry.Worker) bool) registry.Worker {
		t.Helper()
		for deadline := time.Now().Add(limit); ; time.Sleep(250 * time.Millisecond) {
			got := c.worker(1)
			if ok(got) {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: never; worker %+v", what, got)
			}
		}
	}

	blocked := until("blocked on the Write prompt", 3*time.Minute, func(x registry.Worker) bool {
		return x.State == registry.Blocked && x.Fingerprint != ""
	})
	t.Logf("blocked: %q, options %q, fingerprint %s, seq %d", blocked.Question, blocked.Options, blocked.Fingerprint, blocked.BlockSeq)
	d, h := dial(srv.Socket()), watch.Handle(blocked)
	if st, err := d.Poll(ctx, h); err != nil || st.Phase != worker.Asking || st.Fingerprint != blocked.Fingerprint {
		t.Fatalf("on screen: %+v, %v; recorded %s", st, err, blocked.Fingerprint)
	}

	for _, tt := range []struct {
		expect string
		code   int
		want   string
	}{
		{"0123456789abcdef", exitNeedsYou, "no option was chosen"},
		{blocked.Fingerprint, exitError, "only the user may pick it"},
	} {
		code, _, errOut := run("answer", "1", "2", "--expect", tt.expect, "--by", "agent")
		if code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Fatalf("--expect %s: exit %d %q", tt.expect, code, errOut)
		}
		time.Sleep(2 * time.Second) // time for anything pressed to show
		if st, err := d.Poll(ctx, h); err != nil || st.Phase != worker.Asking || st.Fingerprint != blocked.Fingerprint {
			t.Fatalf("after the refusal, on screen: %+v, %v", st, err)
		}
		if _, err := os.Stat(filepath.Join(c.dir, "ask.txt")); err == nil {
			t.Fatal("ask.txt was written: the refusal pressed a key")
		}
		if x := c.worker(1); x.State != registry.Blocked || x.AnsweredBy != "" {
			t.Fatalf("after the refusal: %+v", x)
		}
	}

	if code, _, _ := run("answer", "1", "2", "--expect", blocked.Fingerprint, "--by", "human"); code != 0 {
		t.FailNow()
	}
	if x := c.worker(1); x.Answer != "2. "+blocked.Options[1] || x.AnsweredBy != "human" {
		t.Errorf("answer recorded as %q by %q", x.Answer, x.AnsweredBy)
	}
	end := until("going on to the end", 3*time.Minute, func(x registry.Worker) bool {
		return x.State == registry.Done || x.State == registry.MaybeDone
	})
	if b, err := os.ReadFile(filepath.Join(c.dir, "ask.txt")); err != nil || !strings.Contains(string(b), "ok") {
		t.Errorf("ask.txt = %q, %v (worker %s)", b, err, end.State)
	}
	if l := readLog(); !strings.Contains(l, "answer 2 by agent refused: the question") || !strings.Contains(l, "decides more than this call") ||
		!strings.Contains(l, "answered 2. "+blocked.Options[1]+", by human") {
		t.Errorf("watch.log lacks the answers:\n%s", l)
	}
}
