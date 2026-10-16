package watch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/hook"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// Recorded sessions (testdata), cut at the moment the watcher records a
// question: a transcript record or hook run after it is not there yet.
//   - claude-every-hook: Claude Code 2.1.281, default permissions, with
//     every hook logged: AskUserQuestion; an Edit, with a Write made while
//     its prompt was up, waiting behind it; the Write; a Bash; the turn's end.
//   - claude-two-bash: 2.1.287, a Haiku worker of a live run
//     (TestLivePendingClaude), two Bash prompts in a row.
//   - claude-write-then-done: 2.1.287 (recorded with no hook), a Write and
//     routr done made together, the Write asking.
//   - codex-escalated: Codex 0.156.1 (0.160 writes the same records),
//     approvals on-request, read-only sandbox: an exec that asks to leave
//     the sandbox.
//   - claude-rm-unwritten: 2.1.287, a Haiku worker asked about rm -rf
//     build with no assistant record in its transcript yet (the
//     attachments, which hold the user's own setup, left out). Its hook
//     record comes from another run of the same command, moved into this
//     run's turn (as long after its prompt) and session.
func TestPendingCall(t *testing.T) {
	const askPrompt = "2026-09-23T19:38:25.8Z"
	tests := []struct {
		name                string
		harness             string
		transcript, hooks   string // testdata files; "" = none
		at, prompted        string // the block's moment; when the turn's prompt went ("" = long before)
		wantTool, wantShown string // "" = no pending call
		wantInput           string // how its input starts, if it matters
	}{
		{"AskUserQuestion", "claude", "claude-every-hook.jsonl", "claude-every-hook.hooks.jsonl", "2026-09-23T19:38:28.6Z", askPrompt,
			"AskUserQuestion", "AskUserQuestion: Should the test file be named test_calc.py or calc_test.py?", ""},
		// The hook's tool_input has its own key order: the input tells
		// which one it came from.
		{"an Edit, a Write behind it", "claude", "claude-every-hook.jsonl", "claude-every-hook.hooks.jsonl", "2026-09-23T19:38:32.0Z", askPrompt,
			"Edit", "Edit: /tmp/routr-exp-work/calc.py", `{"file_path":`},
		{"an Edit, a Write behind it, no hook", "claude", "claude-every-hook.jsonl", "", "2026-09-23T19:38:32.0Z", askPrompt,
			"Edit", "Edit: /tmp/routr-exp-work/calc.py", `{"replace_all":false,`},
		{"the Write once the Edit ran", "claude", "claude-every-hook.jsonl", "claude-every-hook.hooks.jsonl", "2026-09-23T19:38:33.1Z", askPrompt,
			"Write", "Write: /tmp/routr-exp-work/test_calc.py", ""},
		{"the Write once the Edit ran, no hook", "claude", "claude-every-hook.jsonl", "", "2026-09-23T19:38:33.1Z", askPrompt,
			"Write", "Write: /tmp/routr-exp-work/test_calc.py", ""},
		{"Bash", "claude", "claude-every-hook.jsonl", "claude-every-hook.hooks.jsonl", "2026-09-23T19:38:35.5Z", askPrompt,
			"Bash", "Bash: python3 test_calc.py", ""},
		// The last hook record is the answered Bash's: no call is in flight.
		{"turn over", "claude", "claude-every-hook.jsonl", "claude-every-hook.hooks.jsonl", "2026-09-23T19:40:00Z", askPrompt, "", "", ""},
		{"the second Bash", "claude", "claude-two-bash.jsonl", "claude-two-bash.hooks.jsonl", "2026-10-02T07:24:19Z", "2026-10-02T07:24:12Z",
			"Bash", "Bash: echo two > pending-two.txt", ""},
		// Its hook has not run (or the user's settings turn hooks off): the
		// first Bash's record is older than its result.
		{"the second Bash, its hook not run", "claude", "claude-two-bash.jsonl", "claude-two-bash.hooks.jsonl", "2026-10-02T07:24:17.95Z", "2026-10-02T07:24:12Z",
			"Bash", "Bash: echo two > pending-two.txt", ""},
		{"a Write, routr done behind it", "claude", "claude-write-then-done.jsonl", "", "2026-10-02T07:04:41Z", "",
			"Write", "Write: /tmp/TestLiveAnswerExpect835000401/002/ask.txt", ""},
		{"no transcript, the hook's record", "claude", "", "claude-every-hook.hooks.jsonl", "2026-09-23T19:38:28.6Z", askPrompt,
			"AskUserQuestion", "AskUserQuestion: Should the test file be named test_calc.py or calc_test.py?", ""},
		{"no transcript, a record from before the turn", "claude", "", "claude-every-hook.hooks.jsonl", "2026-09-23T19:38:28.6Z", "2026-09-23T19:38:28Z", "", "", ""},
		// Claude asks before it writes the call: the hook's record, newer
		// than the turn's prompt, names it all the same.
		{"rm -rf asked before Claude wrote the call", "claude", "claude-rm-unwritten.jsonl", "claude-rm-unwritten.hooks.jsonl", "2026-10-02T09:17:56Z", "2026-10-02T09:17:52Z",
			"Bash", "Bash: rm -rf build", `{"command":"rm -rf build",`},
		{"rm -rf asked before Claude wrote the call, no hook", "claude", "claude-rm-unwritten.jsonl", "", "2026-10-02T09:17:56Z", "2026-10-02T09:17:52Z", "", "", ""},
		{"rm -rf asked before Claude wrote the call, a record from before the turn", "claude", "claude-rm-unwritten.jsonl", "claude-rm-unwritten.hooks.jsonl",
			"2026-10-02T09:17:56Z", "2026-10-02T09:17:55Z", "", "", ""},
		{"Codex asks to leave the sandbox", "codex", "codex-escalated.jsonl", "", "2026-09-23T19:35:27Z", "",
			"exec", `exec: printf 'from calc import add\n\nassert add(2, 3) == 5\nassert add(0, -4) == -4\n' > test_calc.py`, ""},
		{"Codex between calls", "codex", "codex-escalated.jsonl", "", "2026-09-23T19:35:24Z", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			w := &Watcher{Paths: registry.Paths{Dir: filepath.Join(dir, ".routr")}, Log: log.New(testLog{t}, "", 0)}
			at := stamp(t, tt.at)
			wk := registry.Worker{ID: 3, Role: "task", Harness: tt.harness}
			if tt.prompted != "" {
				wk.Prompted = stamp(t, tt.prompted)
			}
			if tt.transcript != "" {
				wk.Transcript = filepath.Join(dir, tt.transcript)
				cut(t, tt.transcript, wk.Transcript, func(line []byte) time.Time {
					var r struct{ Timestamp time.Time }
					json.Unmarshal(line, &r)
					return r.Timestamp
				}, at)
			}
			if tt.hooks != "" {
				cut(t, tt.hooks, w.Paths.HookLog(wk.ID), func(line []byte) time.Time {
					var r hook.Record
					json.Unmarshal(line, &r)
					return r.Time()
				}, at)
			}
			got := w.pending(wk, worker.Status{})
			switch {
			case tt.wantTool == "" && got != nil:
				t.Errorf("pending = %+v; want none", got)
			case tt.wantTool == "":
			case got == nil:
				t.Errorf("no pending call; want %s", tt.wantShown)
			case got.Tool != tt.wantTool || got.Summary != tt.wantShown || !strings.HasPrefix(got.Input, tt.wantInput) ||
				tt.harness == "claude" && !json.Valid([]byte(got.Input)):
				t.Errorf("pending = %+v; want %s, %q, input %s…", got, tt.wantTool, tt.wantShown, tt.wantInput)
			}
		})
	}
}

// cut copies the testdata file name to path, without the lines after at.
func cut(t *testing.T, name, path string, when func([]byte) time.Time, at time.Time) {
	t.Helper()
	in, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var b strings.Builder
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if !when(sc.Bytes()).After(at) {
			b.WriteString(sc.Text() + "\n")
		}
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stamp(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// A long input is cut on a rune boundary, with how much went; the summary
// is made from all of it.
func TestToolCallCutsLongInput(t *testing.T) {
	content := strings.Repeat("é", 2500) // 5000 bytes
	in := `{"file_path":"/w/big.txt","content":"` + content + `"}`
	c := toolCall("Write", in)
	if c.Summary != "Write: /w/big.txt" || !strings.HasPrefix(c.Input, `{"file_path":"/w/big.txt","content":"éé`) {
		t.Errorf("call = %+v", c)
	}
	head, tail, ok := strings.Cut(c.Input, "… [routr: ")
	if !ok || len(head) > pendingInputMax || len(head) < pendingInputMax-1 || tail != fmt.Sprintf("%d bytes cut]", len(in)-len(head)) || !strings.HasSuffix(head, "é") {
		t.Errorf("input cut to %d bytes, then %q", len(head), tail)
	}
	if c := toolCall("Bash", `{"command":"ls"}`); c.Input != `{"command":"ls"}` {
		t.Errorf("a short input changed: %q", c.Input)
	}
}
