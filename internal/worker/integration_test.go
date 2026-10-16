//go:build integration

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/herdrtest"
)

// Exercises tabs, show/hide, close and the no-harness phases against a real
// herdr server with plain shell panes (no AI harness).
func TestRealServerTabsAndPhases(t *testing.T) {
	c := herdrtest.StartServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	d := NewHerdr(c).(*herdrDriver)
	cwd := t.TempDir()

	// The caller's pane, in its own workspace.
	caller, err := c.CreateWorkspace(ctx, herdr.WorkspaceOptions{Label: "main", Cwd: cwd, Focus: true})
	if err != nil {
		t.Fatal(err)
	}

	// No workspace named: a "routr" workspace is created, then reused.
	s := Spec{ID: 1, Label: "1 task · claude", Harness: "claude", Cwd: cwd, Env: map[string]string{"ROUTR_ID": "1"}}
	p1, err := d.place(ctx, s, s.Env)
	if err != nil {
		t.Fatal(err)
	}
	s.Label = "2 task · codex"
	p2, err := d.place(ctx, s, s.Env)
	if err != nil {
		t.Fatal(err)
	}
	if p1.WorkspaceID == caller.WorkspaceID || p2.WorkspaceID != p1.WorkspaceID || p2.TabID == p1.TabID {
		t.Errorf("routr workspace panes: %+v, %+v (caller %s)", p1, p2, caller.WorkspaceID)
	}
	if tab, err := c.GetTab(ctx, p1.TabID); err != nil || tab.Label != "1 task · claude" {
		t.Errorf("first tab = %+v, %v", tab, err)
	}
	// A named workspace that is gone falls back to it too.
	s.Workspace = "w99"
	if p, err := d.place(ctx, s, nil); err != nil || p.WorkspaceID != p1.WorkspaceID {
		t.Errorf("fallback pane = %+v, %v", p, err)
	}

	// A worker tab in the caller's workspace, with its env exported.
	s.Workspace, s.Label = caller.WorkspaceID, "3 review · pi"
	pane, err := d.place(ctx, s, map[string]string{"ROUTR_ID": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RenamePane(ctx, pane.PaneID, s.Label); err != nil {
		t.Fatal(err)
	}
	if err := d.waitShell(ctx, pane.PaneID); err != nil {
		t.Fatal(err)
	}
	h := Handle{Pane: pane.PaneID, Harness: "pi", Cwd: cwd}
	if err := c.SendText(ctx, pane.PaneID, "echo id=$ROUTR_ID"); err != nil {
		t.Fatal(err)
	}
	c.SendKeys(ctx, pane.PaneID, "enter")
	waitFor(t, func() bool {
		text, _ := c.ReadPane(ctx, pane.PaneID, herdr.Recent, 10)
		return strings.Contains(text, "id=3")
	})

	// A shell with no harness is Exited; a running command is not.
	if st, err := d.Poll(ctx, h); err != nil || st.Phase != Exited {
		t.Errorf("idle shell: %+v, %v", st, err)
	}
	c.SendText(ctx, pane.PaneID, "sleep 30")
	c.SendKeys(ctx, pane.PaneID, "enter")
	waitFor(t, func() bool { st, _ := d.Poll(ctx, h); return st.Phase == Working })
	c.SendKeys(ctx, pane.PaneID, "ctrl+c")

	// Show beside the caller, hide back into a tab named by the pane label.
	if err := d.Show(ctx, h, caller.PaneID, false); err != nil {
		t.Fatal(err)
	}
	shown, _ := c.GetPane(ctx, pane.PaneID)
	if shown.TabID != caller.TabID {
		t.Errorf("shown pane is in tab %s, want the caller's %s", shown.TabID, caller.TabID)
	}
	if err := d.Hide(ctx, h); err != nil {
		t.Fatal(err)
	}
	hidden, _ := c.GetPane(ctx, pane.PaneID)
	tab, _ := c.GetTab(ctx, hidden.TabID)
	if hidden.TabID == caller.TabID || tab.Label != "3 review · pi" || tab.PaneCount != 1 {
		t.Errorf("hidden pane %+v in tab %+v", hidden, tab)
	}
	if err := d.Show(ctx, Handle{Pane: p1.PaneID}, caller.PaneID, false); err == nil {
		t.Error("showing a pane from another workspace must fail without focus")
	}

	// Close: the pane is Gone, and closing again is fine.
	if err := d.Close(ctx, h); err != nil {
		t.Fatal(err)
	}
	if st, err := d.Poll(ctx, h); err != nil || st.Phase != Gone {
		t.Errorf("after close: %+v, %v", st, err)
	}
	if err := d.Close(ctx, h); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
