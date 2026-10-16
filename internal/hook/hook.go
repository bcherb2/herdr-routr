// Package hook wires Claude Code's per-launch hooks back to routr.
//
// Claude accepts `--settings '<json>'` per launch, so routr registers hooks
// without touching the user's config: a Claude
// worker's PermissionRequest hook says which tool call a prompt is about.
// Each hook runs `routr hook --log FILE EVENT`, which appends {ts, event,
// payload} to FILE; routr reads the file incrementally.
// Hooks run in the harness pane's environment, so everything they need is in
// the command string.
package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// Hook events routr registers.
const (
	PermissionRequest = "PermissionRequest"
	Stop              = "Stop"
)

// Hooks is the settings "hooks" value that sends each event's payloads to
// `self hook --log logPath EVENT`, for a --settings that says more.
func Hooks(self, logPath string, events ...string) map[string]any {
	hooks := map[string]any{}
	for _, event := range events {
		cmd := fmt.Sprintf("%s hook --log %s %s", shellQuote(self), shellQuote(logPath), event)
		hooks[event] = []map[string]any{{"matcher": "", "hooks": []map[string]string{{"type": "command", "command": cmd}}}}
	}
	return hooks
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Record is one line of the hook log.
type Record struct {
	TS      float64         `json:"ts"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// Time is when the hook ran (TS, Unix seconds), to the microsecond: a
// float64 holds no more.
func (r Record) Time() time.Time {
	sec, frac := math.Modf(r.TS)
	return time.Unix(int64(sec), int64(math.Round(frac*1e6))*1e3)
}

// Sink implements `routr hook --log FILE EVENT`: it appends the payload read
// from stdin. It makes no decision, so the harness shows its normal prompt.
func Sink(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	logPath := fs.String("log", "", "file to append hook records to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath == "" || fs.NArg() != 1 {
		return errors.New("usage: hook --log FILE EVENT")
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return err
	}
	if !json.Valid(payload) {
		payload = []byte("null")
	}
	// Without HTML escaping, the payload stays as the harness wrote it: a
	// command's > and & are not turned into \u003e and \u0026.
	var rec bytes.Buffer
	enc := json.NewEncoder(&rec)
	enc.SetEscapeHTML(false)
	enc.Encode(Record{TS: float64(time.Now().UnixNano()) / 1e9, Event: fs.Arg(0), Payload: payload})
	f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(rec.Bytes())
	return err
}

// Log reads a hook log incrementally. It is not safe for concurrent use.
type Log struct {
	path    string
	off     int64
	partial []byte
}

// NewLog returns a reader for path, which need not exist yet.
func NewLog(path string) *Log { return &Log{path: path} }

// Path is the log file.
func (l *Log) Path() string { return l.path }

// Next returns the complete records appended since the previous call. A
// missing file yields no records; a trailing partial line is kept for later.
func (l *Log) Next() ([]Record, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(l.off, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 64*1024)
	var out []Record
	for {
		chunk, err := r.ReadBytes('\n')
		l.off += int64(len(chunk))
		l.partial = append(l.partial, chunk...)
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		var rec Record
		if json.Unmarshal(l.partial, &rec) == nil {
			out = append(out, rec)
		}
		l.partial = l.partial[:0]
	}
}

// Latest is the last complete record of event in the hook log at path; ok
// is false when there is none, or no file.
func Latest(path, event string) (rec Record, ok bool, err error) {
	recs, err := NewLog(path).Next()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Event == event {
			return recs[i], true, err
		}
	}
	return Record{}, false, err
}

// Option is one AskUserQuestion choice.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question is one AskUserQuestion question.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect"`
}

// Permission is a PermissionRequest payload: the tool Claude wants to run and
// its input, with AskUserQuestion's questions decoded.
type Permission struct {
	Tool      string
	Input     json.RawMessage
	Questions []Question
}

// ParsePermission decodes a PermissionRequest payload.
func ParsePermission(raw json.RawMessage) (Permission, bool) {
	var p struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolName == "" {
		return Permission{}, false
	}
	perm := Permission{Tool: p.ToolName, Input: p.ToolInput}
	if p.ToolName == "AskUserQuestion" {
		var in struct {
			Questions []Question `json:"questions"`
		}
		if json.Unmarshal(p.ToolInput, &in) == nil {
			perm.Questions = in.Questions
		}
	}
	return perm, true
}
