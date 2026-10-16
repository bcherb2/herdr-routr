package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestLivePendingClaude blocks a real Claude Haiku worker on two Bash
// prompts in a row, in a throwaway herdr server with an in-process watcher:
// each time `routr list --json` and `routr wait` say which command it asks
// about, from its PermissionRequest hook. It costs a Haiku turn. Run it with
// Claude's own config, as real workers have:
//
//	env -u CLAUDE_CONFIG_DIR ROUTR_LIVE_CLAUDE=1 go test -run TestLivePendingClaude -count=1 -v ./cmd/routr/
func TestLivePendingClaude(t *testing.T) {
	if os.Getenv("ROUTR_LIVE_CLAUDE") != "1" {
		t.Skip("set ROUTR_LIVE_CLAUDE=1 to block a real Claude Haiku worker")
	}
	l := startLive(t, "--harness", "claude", "--model", "haiku", "--effort", "low",
		"Run these two shell commands with the Bash tool, one tool call each, in this order, and nothing else: "+
			"`echo one > pending-one.txt`, then `echo two > pending-two.txt`.")
	w := l.blocked("on the first command", nil)
	l.says(w, "Bash", "echo one > pending-one.txt")
	if code, _, _ := l.run("answer", "1", "1", "--expect", w.Fingerprint); code != 0 {
		t.FailNow()
	}
	if x := l.c.worker(1); x.Pending != nil {
		t.Errorf("answered, but pending is %+v", x.Pending)
	}
	w = l.blocked("on the second command", func(x registry.Worker) bool {
		return x.Pending != nil && !strings.Contains(x.Pending.Summary, "one")
	})
	l.says(w, "Bash", "echo two > pending-two.txt")
	recs, _ := os.ReadFile(l.c.p.HookLog(1))
	t.Logf("transcript: %s\nhooks.jsonl:\n%s", w.Transcript, recs)
	if n := strings.Count(string(recs), `"event":"PermissionRequest"`); n != 2 || !strings.Contains(string(recs), "echo two > pending-two.txt") {
		t.Errorf("its PermissionRequest hook wrote %d records", n)
	}
}

// TestLivePendingCodex blocks a real Codex worker (gpt-6-luna, low effort)
// on an approval and checks that `routr list --json` shows the command,
// from its transcript. In a workspace-write sandbox with approvals
// on-request, as in a repo Codex trusts, a write outside the workspace asks
// to leave the sandbox. The sandbox is set: Codex 0.160 in a folder it
// doesn't trust runs read-only and exits on routr's --add-dir. Nothing is
// approved, so nothing is written.
//
//	ROUTR_LIVE_CODEX=1 go test -run TestLivePendingCodex -count=1 -v ./cmd/routr/
func TestLivePendingCodex(t *testing.T) {
	if os.Getenv("ROUTR_LIVE_CODEX") != "1" {
		t.Skip("set ROUTR_LIVE_CODEX=1 to block a real Codex worker")
	}
	home, _ := os.UserHomeDir()
	cmd := "echo codex > " + filepath.Join(home, ".cache", "routr-pending-codex.txt")
	l := startLive(t, "--harness", "codex", "--model", "gpt-6-luna", "--effort", "low",
		"Run exactly this shell command and nothing else: `"+cmd+"`. It writes outside the workspace, "+
			"so run it outside the sandbox (sandbox_permissions require_escalated) and wait for approval.")
	w := l.blocked("on the command", nil)
	l.says(w, "exec", cmd)
	t.Logf("transcript: %s", w.Transcript)
}

type live struct {
	t   *testing.T
	c   *cli
	run func(args ...string) (int, string, string)
}

// startLive starts worker 1 with default permissions and flags, in a
// throwaway herdr server, with a watcher in this process until the test
// ends (then the worker is cancelled).
func startLive(t *testing.T, flags ...string) *live {
	srv := herdrtest.Start(t)
	bin := filepath.Join(t.TempDir(), "routr")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	c := newCLI(t)
	if err := c.p.Init(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.p.Config(), []byte(`[harness.codex]
args = ["--sandbox", "workspace-write", "--ask-for-approval", "on-request"]
`), 0o644)
	c.env["HERDR_SOCKET_PATH"] = srv.Socket()
	delete(c.env, "HERDR_WORKSPACE_ID") // the throwaway server's own "routr" workspace
	dial := func(socket string) worker.Driver { return worker.NewHerdr(herdr.New(socket)) }
	l := &live{t: t, c: c}
	l.run = func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		a := c.app("", &out, &errb)
		a.dial, a.bin = dial, bin
		code := a.run(args)
		t.Logf("routr %s: exit %d\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
		return code, out.String(), errb.String()
	}
	if code, _, _ := l.run(append([]string{"start", "--permissions", "default"}, flags...)...); code != 0 {
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
		l.run("cancel", "1")
		cancel()
		<-stopped
		logf.Close()
		b, _ := os.ReadFile(c.p.WatchLog())
		t.Logf("watch.log:\n%s", b)
	})
	return l
}

// blocked waits for worker 1 to be blocked on a question, and for ok if
// given.
func (l *live) blocked(what string, ok func(registry.Worker) bool) registry.Worker {
	l.t.Helper()
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(250 * time.Millisecond) {
		got := l.c.worker(1)
		if got.State == registry.Blocked && (ok == nil || ok(got)) {
			return got
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("blocked %s: never; worker %+v", what, got)
		}
	}
}

// says checks that w's pending call, in the registry, in `routr list
// --json` and in `routr wait`, is a tool call whose summary has cmd and
// whose input names the file cmd writes.
func (l *live) says(w registry.Worker, tool, cmd string) {
	l.t.Helper()
	l.t.Logf("blocked: %q, pending %+v", w.Question, w.Pending)
	file := cmd[strings.LastIndex(cmd, " ")+1:]
	if p := w.Pending; p == nil || p.Tool != tool || !strings.Contains(p.Summary, cmd) || !strings.Contains(p.Input, file) {
		l.t.Fatalf("pending = %+v; want %s with %q", p, tool, cmd)
	}
	_, out, _ := l.run("list", "--json")
	var ws []registry.Worker
	if err := json.Unmarshal([]byte(out), &ws); err != nil || len(ws) != 1 || ws[0].Pending == nil || *ws[0].Pending != *w.Pending {
		l.t.Errorf("list --json: %v %+v", err, ws)
	}
	if code, out, _ := l.run("wait", "1"); code != exitNeedsYou || !strings.Contains(out, "It asks about "+w.Pending.Summary) {
		l.t.Errorf("wait: exit %d %q", code, out)
	}
}
