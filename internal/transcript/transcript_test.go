package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Synthetic records shaped like real session files (field names only).
var (
	claudeLines = []string{
		`{"type":"permission-mode"}`,
		`{"type":"user","message":{"role":"user","content":"Fix the bug"}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[{"type":"thinking","thinking":"…"},{"type":"tool_use","name":"Edit","input":{"file_path":"calc.py"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[{"type":"text","text":"Fixed it."}]}}`,
		`not json`,
	}
	codexLines = []string{
		`{"type":"session_meta","payload":{"id":"x"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-6-astra","effort":"low","approval_policy":"on-request"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the bug"}]}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"python3 t.py"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","output":"ok"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Both "},{"type":"output_text","text":"passed."}]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Both passed."}}`,
	}
	// Shaped like Pi 0.84 session files (~/.pi/agent/sessions).
	piLines = []string{
		`{"type":"session","version":3,"id":"01a0dc72","cwd":"/tmp/w"}`,
		`{"type":"model_change","id":"515d489f","provider":"deepseek","modelId":"deepseek-v4-flash"}`,
		`{"type":"thinking_level_change","id":"6ba01eca","thinkingLevel":"low"}`,
		`{"type":"message","id":"1","message":{"role":"user","content":[{"type":"text","text":"Fix the bug"}]}}`,
		`{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"thinking","thinking":"…"},{"type":"text","text":"Looking."},{"type":"toolCall","id":"c1","name":"edit","arguments":{"path":"calc.py"}}],"model":"deepseek-v4-flash","stopReason":"toolUse"}}`,
		`{"type":"message","id":"3","message":{"role":"toolResult","toolCallId":"c1","toolName":"edit","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"message","id":"4","message":{"role":"assistant","content":[{"type":"text","text":"Fixed it."}],"model":"deepseek-v4-flash","stopReason":"stop"}}`,
	}
)

func parseAll(p Parser, lines []string) []Message {
	var out []Message
	for _, l := range lines {
		out = append(out, p([]byte(l))...)
	}
	return out
}

func TestParsers(t *testing.T) {
	tests := []struct {
		name  string
		p     Parser
		lines []string
		want  []Message
	}{
		{"claude", Claude, claudeLines, []Message{
			{Kind: User, Text: "Fix the bug"},
			{Kind: Tool, Model: "claude-sonnet-5", Tool: "Edit", Text: `{"file_path":"calc.py"}`},
			{Kind: ToolResult},
			{Kind: Text, Model: "claude-sonnet-5", Text: "Fixed it."},
		}},
		{"codex", Codex, codexLines, []Message{
			{Kind: Meta, Model: "gpt-6-astra", Effort: "low"},
			{Kind: User, Text: "Fix the bug"},
			{Kind: Tool, Tool: "exec", Text: "python3 t.py"},
			{Kind: ToolResult},
			{Kind: Text, Text: "Both passed."},
			{Kind: TurnEnd, Text: "Both passed."},
		}},
		{"pi", Pi, piLines, []Message{
			{Kind: Meta, Model: "deepseek-v4-flash"},
			{Kind: Meta, Effort: "low"},
			{Kind: User, Text: "Fix the bug"},
			{Kind: Text, Model: "deepseek-v4-flash", Text: "Looking."},
			{Kind: Tool, Model: "deepseek-v4-flash", Tool: "edit", Text: `{"path":"calc.py"}`, ID: "c1"},
			{Kind: ToolResult, ID: "c1"},
			{Kind: Text, Model: "deepseek-v4-flash", Text: "Fixed it."},
			{Kind: TurnEnd, Text: "Fixed it."},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAll(tt.p, tt.lines)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d messages %+v, want %d", len(got), got, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %+v want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestForKind(t *testing.T) {
	for _, k := range []string{"claude", "codex", "pi"} {
		if _, err := ForKind(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	if _, err := ForKind("gemini"); err == nil {
		t.Error("gemini has no parser yet")
	}
}

func TestLocateEnvClaudeAndCodex(t *testing.T) {
	cfg := t.TempDir()
	env := map[string]string{"CLAUDE_CONFIG_DIR": cfg}
	p, err := LocateEnv("claude", "abc", "/tmp/routr-exp.work", func(k string) string { return env[k] })
	if err != nil || p != filepath.Join(cfg, "projects", "-tmp-routr-exp-work", "abc.jsonl") {
		t.Errorf("claude path = %q err=%v", p, err)
	}

	codexHome := t.TempDir()
	env["CODEX_HOME"] = codexHome
	day := filepath.Join(codexHome, "sessions", "2026", "09", "23")
	os.MkdirAll(day, 0o755)
	want := filepath.Join(day, "rollout-2026-09-23T15-34-54-01a0-abc.jsonl")
	os.WriteFile(want, nil, 0o644)
	if p, err := LocateEnv("codex", "01a0-abc", "", func(k string) string { return env[k] }); err != nil || p != want {
		t.Errorf("codex path = %q err=%v", p, err)
	}
	if _, err := LocateEnv("codex", "missing", "", func(k string) string { return env[k] }); err == nil {
		t.Error("want not-found error")
	}
}

func TestLocateEnvPi(t *testing.T) {
	agent := t.TempDir()
	env := func(k string) string {
		if k == "PI_CODING_AGENT_DIR" {
			return agent
		}
		return ""
	}
	dir := filepath.Join(agent, "sessions", "--tmp-routr-driver.work--")
	os.MkdirAll(dir, 0o755)
	want := filepath.Join(dir, "2026-09-26T06-41-43-848Z_01a0dc72-c3a8.jsonl")
	os.WriteFile(want, nil, 0o644)
	if p, err := LocateEnv("pi", "01a0dc72-c3a8", "/tmp/routr-driver.work", env); err != nil || p != want {
		t.Errorf("pi path = %q err=%v", p, err)
	}
	if p, err := LocateEnv("pi", want, "/elsewhere", env); err != nil || p != want {
		t.Errorf("a session path must be returned as is: %q err=%v", p, err)
	}
	if _, err := LocateEnv("pi", "missing", "/tmp/routr-driver.work", env); err == nil {
		t.Error("want not-found error")
	}
	claude := func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "/cfg"
		}
		return ""
	}
	if p, _ := LocateEnv("claude", "abc", "/w", claude); p != "/cfg/projects/-w/abc.jsonl" {
		t.Errorf("claude path = %q, want it under the given CLAUDE_CONFIG_DIR", p)
	}
}

func TestLastReply(t *testing.T) {
	tests := []struct {
		name  string
		p     Parser
		lines []string
		want  string
	}{
		// Claude records no turn end: the reply is what follows the last tool
		// call, not the narration before it (a "Now I'll…" in final.md).
		{"claude: after the last tool call of the last prompt", Claude, append(claudeLines,
			`{"type":"user","message":{"content":"and tests?"}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Now I'll add them."}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Now I'll run them."},{"type":"tool_use","name":"Bash","input":{}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Tests pass."}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Added 3 tests in test_calc.py."}]}}`,
		), "Tests pass.\n\nAdded 3 tests in test_calc.py."},
		{"claude: a turn that ended on a tool call keeps its prose", Claude, append(claudeLines,
			`{"type":"user","message":{"content":"and tests?"}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Done; recording it."},{"type":"tool_use","name":"Bash","input":{}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		), "Done; recording it."},
		{"codex task_complete wins", Codex, codexLines, "Both passed."},
		{"pi stop message wins over earlier prose", Pi, piLines, "Fixed it."},
		{"pi turn still running", Pi, piLines[:5], "Looking."},
		{"no assistant turn yet", Claude, claudeLines[:2], ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			var b []byte
			for _, l := range tt.lines {
				b = append(b, l+"\n"...)
			}
			os.WriteFile(path, b, 0o600)
			got, err := LastReply(path, tt.p)
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := LastReply(filepath.Join(t.TempDir(), "missing.jsonl"), Claude); err == nil {
		t.Error("want an error for a missing file")
	}
}

// Unresolved: the calls since the last prompt that have no result, oldest
// first, matched to results by id; real sessions are in
// internal/watch/testdata (TestPendingCall).
func TestUnresolved(t *testing.T) {
	const (
		prompt = `{"type":"user","timestamp":"2026-10-02T07:00:00Z","message":{"content":"go"}}`
		edit   = `{"type":"assistant","timestamp":"2026-10-02T07:00:01Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/w/a.go"}}]}}`
		bash   = `{"type":"assistant","timestamp":"2026-10-02T07:00:02Z","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test"}}]}}`
		bashOK = `{"type":"user","timestamp":"2026-10-02T07:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}}`
		editOK = `{"type":"user","timestamp":"2026-10-02T07:00:04Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	)
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, "2026-10-02T07:00:0"+s+"Z"); return t }
	tests := []struct {
		name    string
		p       Parser
		lines   []string
		want    []string // tools in flight
		settled time.Time
	}{
		{"none yet", Claude, []string{prompt}, nil, at("0")},
		{"two in flight, oldest first", Claude, []string{prompt, edit, bash}, []string{"Edit", "Bash"}, at("0")},
		{"a later call's result first", Claude, []string{prompt, edit, bash, bashOK}, []string{"Edit"}, at("3")},
		{"all answered", Claude, []string{prompt, edit, bash, bashOK, editOK}, nil, at("4")},
		{"a new prompt drops what was in flight", Claude, []string{prompt, edit, prompt}, nil, at("0")},
		{"codex, by call_id", Codex, []string{
			`{"timestamp":"2026-10-02T07:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"x"}}`,
			`{"timestamp":"2026-10-02T07:00:02Z","type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":[\"ls\"]}"}}`,
			`{"timestamp":"2026-10-02T07:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}}`,
		}, []string{"shell"}, at("3")},
		{"codex turn over", Codex, codexLines, nil, time.Time{}},
		{"pi, by toolCallId", Pi, piLines[:5], []string{"edit"}, time.Time{}},
		{"pi, answered", Pi, piLines[:6], nil, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			os.WriteFile(path, []byte(strings.Join(tt.lines, "\n")+"\n"), 0o600)
			calls, settled, err := Unresolved(path, tt.p)
			var tools []string
			for _, c := range calls {
				tools = append(tools, c.Tool)
			}
			if err != nil || !slices.Equal(tools, tt.want) || !settled.Equal(tt.settled) {
				t.Errorf("got %q, settled %v, %v; want %q, %v", tools, settled, err, tt.want, tt.settled)
			}
		})
	}
	if _, _, err := Unresolved(filepath.Join(t.TempDir(), "missing.jsonl"), Claude); err == nil {
		t.Error("want an error for a missing file")
	}
}

// A harness that gives up on an API call ends the turn with an error record,
// not a reply (Claude: a synthetic isApiErrorMessage message when
// its login refresh raced another process).
func TestLastReplyTurnError(t *testing.T) {
	const loginErr = "Could not refresh your login because another Claude Code process is refreshing it · Try again in a minute"
	tests := []struct {
		name  string
		p     Parser
		lines []string
		want  string // the error; "" = the turn did not fail
	}{
		{"claude api error", Claude, slices.Concat(claudeLines[:2], []string{
			`{"type":"user","message":{"content":"plan it"}}`,
			`{"type":"assistant","isApiErrorMessage":true,"error":"server_error","message":{"model":"<synthetic>","content":[{"type":"text","text":"` + loginErr + `"}]}}`,
		}), loginErr},
		{"claude: a new prompt after the error clears it", Claude, slices.Concat(claudeLines[:2], []string{
			`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"boom"}]}}`,
			`{"type":"user","message":{"content":"carry on"}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Planned."}]}}`,
		}), ""},
		{"codex error event, then an empty task_complete", Codex, slices.Concat(codexLines[:1], []string{
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}}`,
			`{"type":"event_msg","payload":{"type":"error","message":"stream disconnected before completion"}}`,
			`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}`,
		}), "stream disconnected before completion"},
		{"pi stopReason error", Pi, slices.Concat(piLines[:1], []string{
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`,
			`{"type":"message","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Request timed out."}}`,
		}), "Request timed out."},
		{"pi aborted is not an error", Pi, slices.Concat(piLines[:1], []string{
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`,
			`{"type":"message","message":{"role":"assistant","content":[],"stopReason":"aborted","errorMessage":"Operation aborted"}}`,
		}), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			var b []byte
			for _, l := range tt.lines {
				b = append(b, l+"\n"...)
			}
			os.WriteFile(path, b, 0o600)
			_, err := LastReply(path, tt.p)
			var te *TurnError
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("got %v, want no error", err)
			case tt.want != "" && (!errors.As(err, &te) || te.Message != tt.want):
				t.Errorf("got %v, want a TurnError %q", err, tt.want)
			}
		})
	}
}

func TestToolArg(t *testing.T) {
	for in, want := range map[string]string{
		`{"command":"go test ./...","description":"x"}`:                                                                                           "go test ./...",
		`{"command":["bash","-lc","ls -la"],"workdir":"/w"}`:                                                                                      "bash -lc ls -la",
		`{"file_path":"/w/calc.py","old_string":"a"}`:                                                                                             "/w/calc.py",
		`{"query":"herdr 0.9 release notes"}`:                                                                                                     "herdr 0.9 release notes",
		`{"questions":[{"question":"Name the test file?","header":"Test file","options":[]},{"question":"Run it?"}]}`:                             "Name the test file?",
		`text(await tools.exec_command({cmd:"pwd; rg --files -g 'PLAN.md'", yield_time_ms: 1000}))`:                                               "pwd; rg --files -g 'PLAN.md'",
		"const r = await Promise.allSettled([ tools.exec_command({cmd:\"git status\"}), tools.exec_command({cmd: \"nl -ba \\\"PLAN.md\\\"\"}) ])": `git status ; nl -ba "PLAN.md"`,
		"*** Begin Patch\n*** Update File: calc.py":                                                                                               "apply_patch calc.py",
		// Under on-request, only the command that asks to leave the sandbox asks.
		`text(await tools.exec_command({cmd:"ls"}));\ntext(await tools.exec_command({cmd:"rm -rf build","sandbox_permissions":"require_escalated","justification":"May I?"}));`: "rm -rf build",
		// A patch in a script: its files, after the command before it.
		`text(await tools.exec_command({cmd:"bd where"}));\ntext(await tools.apply_patch("*** Begin Patch\n*** Update File: /w/a.py\n@@\n-x\n+y\n*** Add File: /w/b.py\n+z\n*** End Patch"));`: "bd where ; apply_patch /w/a.py /w/b.py",
	} {
		if got := ToolArg(in); got != want {
			t.Errorf("ToolArg(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Summary("Bash", `{"command":"rm -rf build"}`); got != "Bash: rm -rf build" {
		t.Errorf("Summary = %q", got)
	}
	if got := Summary("mcp__x__ping", ""); got != "mcp__x__ping" {
		t.Errorf("Summary without input = %q", got)
	}
}
