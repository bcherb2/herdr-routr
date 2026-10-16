package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeServer answers each connection with handle(request) lines.
type fakeServer struct {
	t    *testing.T
	sock string
	reqs chan request
}

func newFakeServer(t *testing.T, handle func(req request, w *json.Encoder)) *fakeServer {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	fs := &fakeServer{t: t, sock: sock, reqs: make(chan request, 16)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req request
				if err := json.Unmarshal(line, &req); err != nil {
					t.Errorf("bad request line %q: %v", line, err)
					return
				}
				fs.reqs <- req
				handle(req, json.NewEncoder(conn))
			}()
		}
	}()
	return fs
}

func (fs *fakeServer) lastParams() map[string]any {
	fs.t.Helper()
	select {
	case req := <-fs.reqs:
		b, _ := json.Marshal(req.Params)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["_method"] = req.Method
		return m
	case <-time.After(time.Second):
		fs.t.Fatal("no request received")
		return nil
	}
}

func ok(w *json.Encoder, id string, result any) { w.Encode(map[string]any{"id": id, "result": result}) }

func TestStartAgentEncodesParamsAndDecodesAgent(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		ok(w, req.ID, map[string]any{"type": "agent_info", "agent": map[string]any{
			"name": "cc", "agent": "claude", "pane_id": "w1:p1", "agent_status": "idle",
			"agent_session": map[string]any{"agent": "claude", "kind": "id", "value": "abc-123"},
		}})
	})
	c := New(fs.sock)
	a, err := c.StartAgent(context.Background(), "cc", "claude", "w1:p1", []string{"--model", "sonnet"}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := fs.lastParams()
	if p["_method"] != "agent.start" || p["kind"] != "claude" || p["pane_id"] != "w1:p1" || p["timeout_ms"] != 30000.0 {
		t.Errorf("unexpected params: %v", p)
	}
	if args := p["args"].([]any); len(args) != 2 || args[1] != "sonnet" {
		t.Errorf("args = %v", args)
	}
	if a.AgentStatus != Idle || a.Session == nil || a.Session.Value != "abc-123" {
		t.Errorf("agent = %+v", a)
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		w.Encode(map[string]any{"id": req.ID, "error": map[string]string{
			"code": "agent_not_ready", "message": "agent cc is blocked during startup"}})
	})
	_, err := New(fs.sock).StartAgent(context.Background(), "cc", "claude", "w1:p1", nil, 0)
	if !IsCode(err, "agent_not_ready") {
		t.Fatalf("want agent_not_ready APIError, got %v", err)
	}
	if p := fs.lastParams(); p["args"] == nil {
		t.Error("nil args must be sent as an empty array")
	}
}

func TestReadPaneUsesSchemaSourceNames(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		ok(w, req.ID, map[string]any{"type": "pane_read", "read": map[string]any{"text": "hello\n"}})
	})
	text, err := New(fs.sock).ReadPane(context.Background(), "w1:p1", RecentUnwrapped, 40)
	if err != nil || text != "hello\n" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	p := fs.lastParams()
	if p["source"] != "recent_unwrapped" || p["lines"] != 40.0 || p["format"] != "text" {
		t.Errorf("params = %v", p)
	}
}

func TestPromptWaitPassesDeadlineAndUntil(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		ok(w, req.ID, map[string]any{"agent": map[string]any{"name": "cc", "agent_status": "done"}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := New(fs.sock).Prompt(ctx, "cc", "hi", true, Blocked, Done)
	if err != nil || a.AgentStatus != Done {
		t.Fatalf("agent=%+v err=%v", a, err)
	}
	wait := fs.lastParams()["wait"].(map[string]any)
	if ms := wait["timeout_ms"].(float64); ms <= 0 || ms > 10000 {
		t.Errorf("timeout_ms = %v", ms)
	}
	if u := wait["until"].([]any); len(u) != 2 || u[0] != "blocked" {
		t.Errorf("until = %v", u)
	}
}

func TestCallHonoursContextCancellation(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) { time.Sleep(5 * time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(fs.sock).WaitAgent(ctx, "cc")
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v; want prompt cancellation", err, time.Since(start))
	}
}

func TestOpenPluginPane(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		ok(w, req.ID, map[string]any{"type": "plugin_pane_opened", "plugin_pane": map[string]any{
			"pane": map[string]any{"pane_id": "w1:p9", "cwd": "/w"}}})
	})
	p, err := New(fs.sock).OpenPluginPane(context.Background(), PluginPaneOptions{
		PluginID: "routr", Entrypoint: "chat", Placement: "split", Target: "w1:p1", Cwd: "/w", Focus: true})
	if err != nil || p.PaneID != "w1:p9" {
		t.Fatalf("pane=%+v err=%v", p, err)
	}
	got := fs.lastParams()
	if got["_method"] != "plugin.pane.open" || got["plugin_id"] != "routr" || got["target_pane_id"] != "w1:p1" || got["focus"] != true {
		t.Errorf("params = %v", got)
	}
	if _, set := got["direction"]; set {
		t.Error("empty direction must be omitted")
	}
}

func TestClosePaneAndStateChangeSeq(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		switch req.Method {
		case "pane.close":
			ok(w, req.ID, map[string]string{"type": "ok"})
		default:
			ok(w, req.ID, map[string]any{"agent": map[string]any{"name": "cc", "agent_status": "idle", "state_change_seq": 17}})
		}
	})
	c := New(fs.sock)
	if err := c.ClosePane(context.Background(), "w1:p4"); err != nil {
		t.Fatal(err)
	}
	if p := fs.lastParams(); p["_method"] != "pane.close" || p["pane_id"] != "w1:p4" {
		t.Errorf("params = %v", p)
	}
	a, err := c.GetAgent(context.Background(), "cc")
	if err != nil || a.StateChangeSeq != 17 {
		t.Errorf("agent=%+v err=%v", a, err)
	}
}

func TestListsAndProcessInfo(t *testing.T) {
	fs := newFakeServer(t, func(req request, w *json.Encoder) {
		switch req.Method {
		case "workspace.list":
			ok(w, req.ID, map[string]any{"type": "workspace_list", "workspaces": []any{
				map[string]any{"workspace_id": "w1", "label": "main", "focused": true},
				map[string]any{"workspace_id": "w2", "label": "routr"}}})
		case "pane.list":
			ok(w, req.ID, map[string]any{"type": "pane_list", "panes": []any{
				map[string]any{"pane_id": "w2:p1", "tab_id": "w2:t1", "workspace_id": "w2"}}})
		case "pane.process_info":
			ok(w, req.ID, map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": "w2:p1", "shell_pid": 42, "foreground_process_group_id": 42}})
		}
	})
	c := New(fs.sock)
	ctx := context.Background()
	ws, err := c.ListWorkspaces(ctx)
	if err != nil || len(ws) != 2 || ws[1].Label != "routr" || !ws[0].Focused {
		t.Fatalf("workspaces=%+v err=%v", ws, err)
	}
	fs.lastParams()
	panes, err := c.ListPanes(ctx, "w2")
	if err != nil || len(panes) != 1 || panes[0].TabID != "w2:t1" {
		t.Fatalf("panes=%+v err=%v", panes, err)
	}
	if p := fs.lastParams(); p["workspace_id"] != "w2" {
		t.Errorf("params = %v", p)
	}
	info, err := c.PaneProcessInfo(ctx, "w2:p1")
	if err != nil || !info.AtShell() {
		t.Errorf("process info=%+v err=%v", info, err)
	}
	if (ProcessInfo{ShellPID: 42, ForegroundPG: 50}).AtShell() || (ProcessInfo{}).AtShell() {
		t.Error("AtShell must be false while a command runs or when unknown")
	}
}

func TestReachableUnreachableDenied(t *testing.T) {
	fs := newFakeServer(t, func(request, *json.Encoder) {})
	ctx := context.Background()
	if err := New(fs.sock).Reachable(ctx); err != nil {
		t.Fatalf("reachable: %v", err)
	}
	_, err := New(filepath.Join(t.TempDir(), "gone.sock")).ListWorkspaces(ctx)
	if !Unreachable(err) || Denied(err) {
		t.Errorf("no server: unreachable %v, denied %v (%v)", Unreachable(err), Denied(err), err)
	}
	if Unreachable(&APIError{Code: "pane_not_found"}) {
		t.Error("an API error is not unreachable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root may connect to any socket")
	}
	// A socket this process may not write is refused as Codex's sandbox
	// refuses one outside its workspace (connect: operation not permitted).
	if err := os.Chmod(fs.sock, 0); err != nil {
		t.Fatal(err)
	}
	if err := New(fs.sock).Reachable(ctx); !Unreachable(err) || !Denied(err) {
		t.Errorf("no permission: unreachable %v, denied %v (%v)", Unreachable(err), Denied(err), err)
	}
	if _, err := New(fs.sock).ListWorkspaces(ctx); !Denied(err) {
		t.Errorf("a call: %v", err)
	}
}

// A server that accepts a connection but never answers must not hang the
// caller (the watcher's tick) for good.
func TestACallWithoutADeadlineIsBounded(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	fs := newFakeServer(t, func(request, *json.Encoder) { <-stuck })
	c := New(fs.sock)
	c.timeout = 50 * time.Millisecond
	begin := time.Now()
	if _, err := c.GetAgent(context.Background(), "w1:p1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("GetAgent = %v, want a deadline error", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
	// A wait the caller bounds is not cut short by the per-call timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin = time.Now()
	c.WaitAgent(ctx, "w1:p1")
	if d := time.Since(begin); d < 250*time.Millisecond {
		t.Errorf("a wait bounded by its context ended after %s", d)
	}
}
