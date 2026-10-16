//go:build integration

package herdr_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/herdrtest"
)

// Exercises the real herdr binary with plain shell panes (no AI harness).
func TestRealServerPaneLifecycle(t *testing.T) {
	c := herdrtest.StartServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()

	root, err := c.CreateWorkspace(ctx, herdr.WorkspaceOptions{Label: "it", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if root.PaneID == "" || root.Cwd != cwd {
		t.Fatalf("root pane = %+v", root)
	}

	side, err := c.SplitPane(ctx, herdr.SplitOptions{Target: root.PaneID, Direction: "right", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if side.PaneID == "" || side.PaneID == root.PaneID {
		t.Fatalf("split pane = %+v", side)
	}
	if err := c.RenamePane(ctx, side.PaneID, "claude_code · sonnet · low"); err != nil {
		t.Fatal(err)
	}
	if p, err := c.GetPane(ctx, side.PaneID); err != nil || p.Label != "claude_code · sonnet · low" {
		t.Fatalf("pane=%+v err=%v", p, err)
	}

	marker := fmt.Sprintf("ROUTR_IT_%d", time.Now().UnixNano())
	if err := c.SendText(ctx, side.PaneID, "echo "+marker+"_$((6*7))"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendKeys(ctx, side.PaneID, "enter"); err != nil {
		t.Fatal(err)
	}
	want := marker + "_42" // only the command's output, not its echoed input, contains 42
	var text string
	for range 50 {
		if text, err = c.ReadPane(ctx, side.PaneID, herdr.Recent, 20); err == nil && strings.Contains(text, want) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(text, want) {
		t.Fatalf("pane output never showed %s; last read (err=%v):\n%s", want, err, text)
	}
}

func TestRealServerErrors(t *testing.T) {
	c := herdrtest.StartServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := c.GetAgent(ctx, "no-such-agent"); !herdr.IsCode(err, "agent_not_found") {
		t.Fatalf("want agent_not_found, got %v", err)
	}
}

// A pane moved between tabs keeps its process: the stage beside the chat and
// the named background tabs routr hides harnesses in.
func TestRealServerMovePaneBetweenTabs(t *testing.T) {
	c := herdrtest.StartServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := t.TempDir()

	chat, err := c.CreateWorkspace(ctx, herdr.WorkspaceOptions{Label: "it", Cwd: cwd, Focus: true})
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.CreateTab(ctx, herdr.TabOptions{Workspace: chat.WorkspaceID, Label: "plan · claude", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if h.TabID == chat.TabID || h.Focused {
		t.Fatalf("background tab pane = %+v", h)
	}
	if tab, err := c.GetTab(ctx, h.TabID); err != nil || tab.Label != "plan · claude" || tab.Focused {
		t.Fatalf("tab=%+v err=%v", tab, err)
	}
	// A shell variable only this process knows proves the process survives moves.
	marker := fmt.Sprintf("it%d", time.Now().UnixNano())
	run := func(pane, cmd string) {
		t.Helper()
		if err := c.SendText(ctx, pane, cmd); err != nil {
			t.Fatal(err)
		}
		if err := c.SendKeys(ctx, pane, "enter"); err != nil {
			t.Fatal(err)
		}
	}
	run(h.PaneID, "M="+marker)

	shown, err := c.MovePane(ctx, h.PaneID, herdr.ToTab(chat.TabID, chat.PaneID, "right"), false)
	if err != nil {
		t.Fatal(err)
	}
	if shown.TabID != chat.TabID {
		t.Fatalf("shown pane = %+v", shown)
	}
	if _, err := c.GetTab(ctx, h.TabID); err == nil {
		t.Error("the emptied background tab should be closed")
	}
	if chatNow, err := c.GetPane(ctx, chat.PaneID); err != nil || !chatNow.Focused {
		t.Errorf("chat lost focus: %+v %v", chatNow, err)
	}

	hidden, err := c.MovePane(ctx, shown.PaneID, herdr.ToNewTab(chat.WorkspaceID, "plan · claude"), false)
	if err != nil {
		t.Fatal(err)
	}
	if tab, err := c.GetTab(ctx, hidden.TabID); err != nil || tab.Label != "plan · claude" || tab.Focused || hidden.TabID == chat.TabID {
		t.Fatalf("hidden tab=%+v err=%v", tab, err)
	}
	t.Logf("pane ids: created %s, shown %s, hidden %s", h.PaneID, shown.PaneID, hidden.PaneID)

	run(hidden.PaneID, `echo "got-$M-$((6*7))"`)
	want := "got-" + marker + "-42"
	var text string
	for range 50 {
		if text, err = c.ReadPane(ctx, hidden.PaneID, herdr.Recent, 20); err == nil && strings.Contains(text, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("moved pane lost its shell state; last read (err=%v):\n%s", err, text)
}
