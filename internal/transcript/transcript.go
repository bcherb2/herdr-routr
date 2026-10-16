// Package transcript turns harness session files into one message stream.
//
// Claude Code, Codex and Pi all append JSONL as a session runs, and every
// assistant record names the model. herdr's agent
// records carry the session id (agent_session.value) that names the file;
// for Pi it is the file's path.
package transcript

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Kind classifies a normalized message.
type Kind string

const (
	User       Kind = "user"        // prompt text
	Text       Kind = "text"        // assistant prose
	Tool       Kind = "tool"        // assistant tool call
	ToolResult Kind = "tool_result" // result fed back to the model
	Meta       Kind = "meta"        // turn settings (model, effort)
	TurnEnd    Kind = "turn_end"    // harness-reported end of turn, Text = final message
	Failed     Kind = "failed"      // an API error ended the turn instead of a reply, Text = the error
)

// Message is one normalized transcript entry.
type Message struct {
	Kind   Kind
	Model  string
	Effort string
	Tool   string
	Text   string    // prose, tool input JSON, or final message
	ID     string    // Tool: the call's id; ToolResult: the id of the call it answers
	Time   time.Time // when the harness wrote the record; zero if it doesn't say
}

// Parser converts one JSONL line into zero or more messages.
type Parser func(line []byte) []Message

// ForKind returns the parser for a herdr agent kind.
func ForKind(kind string) (Parser, error) {
	switch kind {
	case "claude":
		return Claude, nil
	case "codex":
		return Codex, nil
	case "pi":
		return Pi, nil
	}
	return nil, fmt.Errorf("transcript: no parser for %q", kind)
}

type claudeRecord struct {
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp"`
	IsAPIError bool   `json:"isApiErrorMessage"` // a synthetic message: the API call failed
	Message    struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
}

// Claude parses ~/.claude/projects/<dir>/<session>.jsonl records.
func Claude(line []byte) []Message {
	var r claudeRecord
	if json.Unmarshal(line, &r) != nil {
		return nil
	}
	at := stamp(r.Timestamp)
	switch r.Type {
	case "user":
		var s string
		if json.Unmarshal(r.Message.Content, &s) == nil {
			return []Message{{Kind: User, Text: s, Time: at}}
		}
		var blocks []claudeBlock
		json.Unmarshal(r.Message.Content, &blocks)
		var out []Message
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				out = append(out, Message{Kind: ToolResult, ID: b.ToolUseID, Time: at})
			case "text":
				out = append(out, Message{Kind: User, Text: b.Text, Time: at})
			}
		}
		return out
	case "assistant":
		var blocks []claudeBlock
		json.Unmarshal(r.Message.Content, &blocks)
		if r.IsAPIError {
			var text []string
			for _, b := range blocks {
				text = append(text, b.Text)
			}
			return []Message{{Kind: Failed, Text: strings.TrimSpace(strings.Join(text, "\n")), Time: at}}
		}
		var out []Message
		for _, b := range blocks {
			switch b.Type {
			case "text":
				out = append(out, Message{Kind: Text, Model: r.Message.Model, Text: b.Text, Time: at})
			case "tool_use":
				out = append(out, Message{Kind: Tool, Model: r.Message.Model, Tool: b.Name, Text: string(b.Input), ID: b.ID, Time: at})
			}
		}
		return out
	}
	return nil
}

// stamp parses a record's RFC 3339 timestamp; zero if it has none.
func stamp(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

type codexRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type      string `json:"type"`
		Role      string `json:"role"`
		Model     string `json:"model"`
		Effort    string `json:"effort"`
		Name      string `json:"name"`
		Input     string `json:"input"`     // a custom_tool_call's
		Arguments string `json:"arguments"` // a function_call's (JSON)
		CallID    string `json:"call_id"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		LastAgentMessage string `json:"last_agent_message"`
		Message          string `json:"message"` // an error event's
	} `json:"payload"`
}

// Codex parses ~/.codex/sessions/YYYY/MM/DD/rollout-*-<session>.jsonl records.
func Codex(line []byte) []Message {
	var r codexRecord
	if json.Unmarshal(line, &r) != nil {
		return nil
	}
	p, at := r.Payload, stamp(r.Timestamp)
	switch {
	case r.Type == "turn_context":
		return []Message{{Kind: Meta, Model: p.Model, Effort: p.Effort, Time: at}}
	case r.Type == "response_item" && p.Type == "message" && (p.Role == "assistant" || p.Role == "user"):
		var sb strings.Builder
		for _, c := range p.Content {
			sb.WriteString(c.Text)
		}
		k := Text
		if p.Role == "user" {
			k = User
		}
		return []Message{{Kind: k, Text: sb.String(), Time: at}}
	case r.Type == "response_item" && (p.Type == "custom_tool_call" || p.Type == "function_call"):
		return []Message{{Kind: Tool, Tool: p.Name, Text: cmp.Or(p.Input, p.Arguments), ID: p.CallID, Time: at}}
	case r.Type == "response_item" && strings.HasSuffix(p.Type, "_call_output"):
		return []Message{{Kind: ToolResult, ID: p.CallID, Time: at}}
	case r.Type == "event_msg" && p.Type == "task_complete":
		return []Message{{Kind: TurnEnd, Text: p.LastAgentMessage, Time: at}}
	case r.Type == "event_msg" && p.Type == "error":
		return []Message{{Kind: Failed, Text: p.Message, Time: at}}
	}
	return nil
}

type piRecord struct {
	Type          string `json:"type"`
	Timestamp     string `json:"timestamp"`
	ModelID       string `json:"modelId"`
	ThinkingLevel string `json:"thinkingLevel"`
	Message       struct {
		Role         string `json:"role"`
		Model        string `json:"model"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		ToolCallID   string `json:"toolCallId"`
		Content      []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			ID        string          `json:"id"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"content"`
	} `json:"message"`
}

// Pi parses ~/.pi/agent/sessions/--<cwd>--/<time>_<session>.jsonl records.
// An assistant message that ends with stopReason "stop" (not "toolUse") is
// the turn's final message, so it is also reported as TurnEnd; one that
// ends with "error" is reported as Failed.
func Pi(line []byte) []Message {
	var r piRecord
	if json.Unmarshal(line, &r) != nil {
		return nil
	}
	switch r.Type {
	case "model_change":
		return []Message{{Kind: Meta, Model: r.ModelID}}
	case "thinking_level_change":
		return []Message{{Kind: Meta, Effort: r.ThinkingLevel}}
	case "message":
	default:
		return nil
	}
	m, at := r.Message, stamp(r.Timestamp)
	var out []Message
	switch m.Role {
	case "user":
		for _, c := range m.Content {
			if c.Type == "text" {
				out = append(out, Message{Kind: User, Text: c.Text, Time: at})
			}
		}
	case "toolResult":
		out = append(out, Message{Kind: ToolResult, ID: m.ToolCallID, Time: at})
	case "assistant":
		var final []string
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				out = append(out, Message{Kind: Text, Model: m.Model, Text: c.Text, Time: at})
				final = append(final, c.Text)
			case "toolCall":
				out = append(out, Message{Kind: Tool, Model: m.Model, Tool: c.Name, Text: string(c.Arguments), ID: c.ID, Time: at})
			}
		}
		switch m.StopReason {
		case "stop":
			out = append(out, Message{Kind: TurnEnd, Text: strings.Join(final, "\n\n"), Time: at})
		case "error":
			out = append(out, Message{Kind: Failed, Text: m.ErrorMessage, Time: at})
		}
	}
	return out
}

var nonPathChar = regexp.MustCompile(`[^A-Za-z0-9-]`)

// LocateEnv returns the session file for a herdr agent kind, session id and
// cwd, with the harness directories (CLAUDE_CONFIG_DIR, CODEX_HOME,
// PI_CODING_AGENT_DIR, PI_CODING_AGENT_SESSION_DIR) read through getenv, for
// a harness started with its own environment. A Pi session may
// be given as its file path, which is returned as is.
func LocateEnv(kind, sessionID, cwd string, getenv func(string) string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch kind {
	case "claude":
		dir := getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		return filepath.Join(dir, "projects", nonPathChar.ReplaceAllString(cwd, "-"), sessionID+".jsonl"), nil
	case "pi":
		return locatePi(sessionID, cwd, home, getenv)
	case "codex":
		dir := getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		var found string
		err := filepath.WalkDir(filepath.Join(dir, "sessions"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, "-"+sessionID+".jsonl") {
				found = p
				return fs.SkipAll
			}
			return nil
		})
		if found == "" {
			return "", fmt.Errorf("transcript: codex session %s not found: %w", sessionID, errors.Join(err, fs.ErrNotExist))
		}
		return found, nil
	}
	return "", fmt.Errorf("transcript: cannot locate %q sessions", kind)
}

// locatePi finds <sessions>/--<cwd with / as ->--/<time>_<id>.jsonl.
func locatePi(sessionID, cwd, home string, getenv func(string) string) (string, error) {
	if filepath.IsAbs(sessionID) {
		return sessionID, nil
	}
	dir := getenv("PI_CODING_AGENT_SESSION_DIR")
	if dir == "" {
		agent := getenv("PI_CODING_AGENT_DIR")
		if agent == "" {
			agent = filepath.Join(home, ".pi", "agent")
		}
		dir = filepath.Join(agent, "sessions")
	}
	project := "--" + strings.ReplaceAll(strings.Trim(cwd, "/"), "/", "-") + "--"
	matches, _ := filepath.Glob(filepath.Join(dir, project, "*_"+sessionID+".jsonl"))
	if len(matches) == 0 {
		return "", fmt.Errorf("transcript: pi session %s not found: %w", sessionID, fs.ErrNotExist)
	}
	return matches[0], nil
}

// TurnError is LastReply's error when the session's last turn ended in an
// API error (the harness gave up on a request) instead of a reply.
type TurnError struct{ Message string }

func (e *TurnError) Error() string { return "the harness's last turn failed: " + e.Message }

// tailWindow bounds how much of a transcript LastReply and Unresolved read.
const tailWindow = 4 << 20

// eachTail calls fn with the messages of the last tailWindow bytes of a
// session file, in order.
func eachTail(path string, parse Parser, fn func(Message)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	start := max(0, fi.Size()-tailWindow)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	if start > 0 {
		r.ReadBytes('\n') // skip the partial first line
	}
	for {
		line, err := r.ReadBytes('\n')
		for _, m := range parse(line) {
			fn(m)
		}
		if err != nil {
			return nil
		}
	}
}

// LastReply is the final message of the assistant's last turn in a session
// file: the harness-reported one (Codex's task_complete, Pi's stop) if there
// is one, else the prose written after the turn's last tool call or result
// (Claude records no turn end, and its narration between tool calls, "Now
// I'll…", is not the reply), else all the prose since the last user
// message. Only the file's tail is read. If the turn ended in an API
// error, the prose so far comes with a *TurnError.
func LastReply(path string, parse Parser) (string, error) {
	var prose, last []string // since the user message; since the last tool call or result
	final, failed := "", ""
	err := eachTail(path, parse, func(m Message) {
		switch m.Kind {
		case User:
			if strings.TrimSpace(m.Text) != "" {
				prose, last, final, failed = nil, nil, "", ""
			}
		case Text:
			if t := strings.TrimSpace(m.Text); t != "" {
				prose, last, failed = append(prose, t), append(last, t), ""
			}
		case Tool, ToolResult:
			last, failed = nil, ""
		case TurnEnd:
			// Codex may end a failed turn with an empty task_complete.
			if final = strings.TrimSpace(m.Text); final != "" {
				failed = ""
			}
		case Failed:
			if failed = strings.TrimSpace(m.Text); failed == "" {
				failed = "unknown error"
			}
		}
	})
	switch {
	case err != nil:
		return "", err
	case failed != "":
		return strings.Join(prose, "\n\n"), &TurnError{Message: failed}
	case final != "":
		return final, nil
	case len(last) > 0:
		return strings.Join(last, "\n\n"), nil
	}
	return strings.Join(prose, "\n\n"), nil
}

// Unresolved is what a session file says is in flight: the tool calls made
// since the last prompt that have no result yet, oldest first, and when it
// last recorded a result or a prompt (zero if its tail has neither). A turn
// that ended has none. A harness runs a turn's calls in order, one at a time
// unless they only read, so the oldest is the one a permission prompt waits
// on; the others (Claude's `routr done` after a Write, say) wait behind it.
// Only the file's tail is read.
func Unresolved(path string, parse Parser) (calls []Message, settled time.Time, err error) {
	err = eachTail(path, parse, func(m Message) {
		switch m.Kind {
		case User:
			if strings.TrimSpace(m.Text) != "" {
				calls, settled = nil, latest(settled, m.Time)
			}
		case Tool:
			calls = append(calls, m)
		case ToolResult:
			// A result without an id answers the oldest call.
			if i := slices.IndexFunc(calls, func(c Message) bool { return c.ID == m.ID || m.ID == "" }); i >= 0 {
				calls = slices.Delete(calls, i, i+1)
			}
			settled = latest(settled, m.Time)
		case TurnEnd, Failed:
			calls = nil
		}
	})
	return calls, settled, err
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Summary is a tool call in one line, its tool and ToolArg: "Bash: rm -rf
// build", "Write: /w/calc.py", Codex's "exec: python3 t.py".
func Summary(tool, input string) string {
	if arg := ToolArg(input); arg != "" {
		return tool + ": " + arg
	}
	return tool
}

// ToolArg is the most telling argument of a tool call's input (the command,
// path, pattern, URL, search or AskUserQuestion's first question), clipped
// to one line, instead of the raw JSON. Codex sends commands as argv arrays;
// non-JSON input is clipped as is, unless it is Codex's exec script or a
// patch (codexArgs). Claude's hook payloads use the same tool_input shape.
func ToolArg(input string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &fields) == nil {
		for _, k := range []string{"command", "cmd", "file_path", "path", "pattern", "url", "query", "description"} {
			var s string
			var argv []string
			switch raw := fields[k]; {
			case raw == nil:
			case json.Unmarshal(raw, &s) == nil:
				return clip(s, 120)
			case json.Unmarshal(raw, &argv) == nil:
				return clip(strings.Join(argv, " "), 120)
			}
		}
		var qs []struct{ Question string }
		if json.Unmarshal(fields["questions"], &qs) == nil && len(qs) > 0 {
			return clip(qs[0].Question, 120)
		}
	}
	if args := codexArgs(input); len(args) > 0 {
		return clip(strings.Join(args, " ; "), 120)
	}
	return clip(input, 120)
}

// codexCall finds, in Codex's exec script, each tools.exec_command({cmd:
// "…", …}) (its cmd, and the rest of its options), and the start and file
// lines of a patch, there or as a patch tool's input.
var codexCall = regexp.MustCompile(`exec_command\(\{\s*cmd:\s*("(?:[^"\\]|\\.)*")([^}]*)|(\*\*\* Begin Patch)|\*\*\* (?:Add|Update|Delete) File: ([^\n"\\]+)`)

// codexArgs are the commands and patches in Codex's exec script, in order:
// "python3 t.py", "apply_patch calc.py test_calc.py". When some commands
// ask to leave the sandbox (sandbox_permissions "require_escalated"), the
// others are left out: under on-request only those ask.
func codexArgs(input string) []string {
	type arg struct {
		text      string
		cmd, asks bool
	}
	var args []arg
	asking := false
	for _, m := range codexCall.FindAllStringSubmatch(input, -1) {
		switch {
		case m[1] != "":
			var cmd string
			if json.Unmarshal([]byte(m[1]), &cmd) == nil {
				asks := strings.Contains(m[2], "require_escalated")
				args, asking = append(args, arg{text: cmd, cmd: true, asks: asks}), asking || asks
			}
		case m[3] != "" || len(args) == 0 || args[len(args)-1].cmd:
			args = append(args, arg{text: "apply_patch"})
			fallthrough
		default:
			if m[4] != "" {
				args[len(args)-1].text += " " + strings.TrimSpace(m[4])
			}
		}
	}
	var out []string
	for _, a := range args {
		if !asking || !a.cmd || a.asks {
			out = append(out, a.text)
		}
	}
	return out
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
