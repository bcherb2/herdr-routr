package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Payload shapes captured from a real Claude Code session.
const (
	askPayload  = `{"hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Name the test file?","header":"Test file","options":[{"label":"test_calc.py","description":"pytest"},{"label":"calc_test.py","description":"suffix"}],"multiSelect":false}]}}`
	editPayload = `{"hook_event_name":"PermissionRequest","tool_name":"Edit","tool_input":{"file_path":"/w/calc.py","old_string":"a - b","new_string":"a + b"}}`
	stopPayload = `{"hook_event_name":"Stop","last_assistant_message":"Fixed.\nTests pass."}`
)

func sink(t *testing.T, log, event, payload string) {
	t.Helper()
	if err := Sink([]string{"--log", log, event}, strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestSinkAndIncrementalLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.jsonl")
	l := NewLog(path)
	if recs, err := l.Next(); err != nil || len(recs) != 0 {
		t.Fatalf("missing file: %v %v", recs, err)
	}
	sink(t, path, PermissionRequest, askPayload)
	recs, err := l.Next()
	if err != nil || len(recs) != 1 || recs[0].Event != PermissionRequest {
		t.Fatalf("first read: %+v %v", recs, err)
	}
	if recs, _ := l.Next(); len(recs) != 0 {
		t.Errorf("records must not repeat: %+v", recs)
	}

	// A half-written line is held back until it completes.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"ts":1,"event":"Stop","payload":`)
	if recs, _ := l.Next(); len(recs) != 0 {
		t.Errorf("partial line returned: %+v", recs)
	}
	f.WriteString(stopPayload + "}\n")
	f.Close()
	sink(t, path, Stop, "not json")
	recs, _ = l.Next()
	if len(recs) != 2 || string(recs[0].Payload) != stopPayload || string(recs[1].Payload) != "null" {
		t.Errorf("after completion: %+v", recs)
	}
	if err := Sink([]string{Stop}, strings.NewReader("{}")); err == nil {
		t.Error("missing --log must be an error")
	}
}

func TestParsePermission(t *testing.T) {
	p, ok := ParsePermission(json.RawMessage(askPayload))
	if !ok || p.Tool != "AskUserQuestion" || len(p.Questions) != 1 {
		t.Fatalf("ask = %+v %v", p, ok)
	}
	q := p.Questions[0]
	if q.Question != "Name the test file?" || len(q.Options) != 2 || q.Options[1] != (Option{"calc_test.py", "suffix"}) {
		t.Errorf("question = %+v", q)
	}
	e, ok := ParsePermission(json.RawMessage(editPayload))
	if !ok || e.Tool != "Edit" || !strings.Contains(string(e.Input), "a + b") {
		t.Errorf("edit = %+v", e)
	}
	if _, ok := ParsePermission(json.RawMessage(`null`)); ok {
		t.Error("null payload parsed")
	}
}

// A worker's --settings carries only the PermissionRequest hook.
func TestHooks(t *testing.T) {
	b, _ := json.Marshal(Hooks("/opt/routr", "/r/.routr/runs/3/hooks.jsonl", PermissionRequest))
	want := `{"PermissionRequest":[{"hooks":[{"command":"'/opt/routr' hook --log '/r/.routr/runs/3/hooks.jsonl' PermissionRequest","type":"command"}],"matcher":""}]}`
	if string(b) != want {
		t.Errorf("hooks = %s", b)
	}
}

// Latest is the newest record of an event; the payload is kept as Claude
// wrote it (no > for a command's >).
func TestLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.jsonl")
	if _, ok, err := Latest(path, PermissionRequest); ok || err != nil {
		t.Fatalf("no file: %v %v", ok, err)
	}
	sink(t, path, PermissionRequest, editPayload)
	sink(t, path, PermissionRequest, `{"tool_name":"Bash","tool_input":{"command":"echo one > one.txt && cat one.txt"}}`)
	sink(t, path, Stop, stopPayload)
	rec, ok, err := Latest(path, PermissionRequest)
	p, _ := ParsePermission(rec.Payload)
	if !ok || err != nil || p.Tool != "Bash" || string(p.Input) != `{"command":"echo one > one.txt && cat one.txt"}` {
		t.Errorf("latest = %s, %v %v", rec.Payload, ok, err)
	}
	if d := time.Since(rec.Time()); d < 0 || d > time.Minute {
		t.Errorf("record time %v", rec.Time())
	}
	if got := (Record{TS: 1790925854.959}).Time().UTC().Format(time.RFC3339Nano); got != "2026-10-02T07:24:14.959Z" {
		t.Errorf("time = %s", got)
	}
}
