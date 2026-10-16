package watch

import (
	"path/filepath"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// flipEnv makes config [env] say CLAUDE_CONFIG_DIR=dir from now on: the
// user switching accounts globally.
func (r *rig) flipEnv(dir string) {
	r.w.Env = func() (map[string]string, error) { return map[string]string{"CLAUDE_CONFIG_DIR": dir}, nil }
}

func TestEnvIsFrozenAtStartAndOnResume(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/work"}})
	r.flipEnv("/other") // config [env] changed after it was queued
	r.tick(1)
	r.state(id, registry.Running)
	if got := r.fake.Panes(id)[0].Spec.Env; got["CLAUDE_CONFIG_DIR"] != "/work" || got["ROUTR_ID"] != "1" {
		t.Errorf("start env = %v, want its own CLAUDE_CONFIG_DIR and routr's", got)
	}
	r.fake.Kill(id)
	r.settle()
	r.state(id, registry.Closed)
	r.s.Update(id, func(w *registry.Worker) { w.Inbox = append(w.Inbox, "why?") })
	r.tick(1)
	r.state(id, registry.Running)
	panes := r.fake.Panes(id)
	if len(panes) != 2 || panes[1].Spec.Session != "sess-1" {
		t.Fatalf("no resume: %+v", panes)
	}
	if got := panes[1].Spec.Env["CLAUDE_CONFIG_DIR"]; got != "/work" {
		t.Errorf("resume env CLAUDE_CONFIG_DIR = %q, want the one it was queued with", got)
	}
	r.settle()
	// Its transcript is looked for in its own store, not config's.
	if got := r.fake.Panes(id)[1].Env["CLAUDE_CONFIG_DIR"]; got != "/work" {
		t.Errorf("Poll env CLAUDE_CONFIG_DIR = %q, want /work", got)
	}
	if got := r.w.handle(r.get(id)).Env["CLAUDE_CONFIG_DIR"]; got != "/work" {
		t.Errorf("handle env CLAUDE_CONFIG_DIR = %q, want /work", got)
	}
}

func TestFrozenEmptyEnvIgnoresConfig(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go", Env: map[string]string{}})
	r.tick(1)
	if got, ok := r.fake.Panes(id)[0].Spec.Env["CLAUDE_CONFIG_DIR"]; ok {
		t.Errorf("a frozen empty env got config's CLAUDE_CONFIG_DIR=%q", got)
	}
}

func TestLegacyRecordWithoutEnvReadsConfig(t *testing.T) {
	r := newRig(t)
	id := r.add(registry.Worker{Brief: "go"}) // no env: a record from before it was frozen
	r.flipEnv("/now")
	r.tick(1)
	r.state(id, registry.Running)
	if got := r.fake.Panes(id)[0].Spec.Env["CLAUDE_CONFIG_DIR"]; got != "/now" {
		t.Errorf("start env CLAUDE_CONFIG_DIR = %q, want config's at spawn", got)
	}
	r.settle()
	if got := r.fake.Panes(id)[0].Env["CLAUDE_CONFIG_DIR"]; got != "/now" {
		t.Errorf("Poll env CLAUDE_CONFIG_DIR = %q, want config's", got)
	}
}

func TestRunStepGetsItsOwnEnv(t *testing.T) {
	r := newRig(t)
	id := r.addRun(registry.Worker{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/mine", "EXTRA": "x"}},
		`echo "$CLAUDE_CONFIG_DIR $EXTRA $ROUTR_ID"`)
	r.await(id, registry.Done)
	if got := r.read(filepath.Join(r.p.RunDir(id), "run.log")); got != "/mine x 1\n" {
		t.Errorf("run.log = %q, want its own env and routr's", got)
	}
}

// start_mcp_servers is frozen on the record: its start and its resume both
// get it, whatever config says by then.
func TestMCPIsFrozenAtStartAndOnResume(t *testing.T) {
	r := newRig(t)
	with := r.add(registry.Worker{Brief: "go", MCP: true})
	without := r.add(registry.Worker{Brief: "go"})
	r.tick(2)
	r.state(with, registry.Running)
	if !r.fake.Panes(with)[0].Spec.MCP || r.fake.Panes(without)[0].Spec.MCP {
		t.Errorf("start MCP = %v, %v; want true, false", r.fake.Panes(with)[0].Spec.MCP, r.fake.Panes(without)[0].Spec.MCP)
	}
	r.fake.Kill(with)
	r.settle()
	r.s.Update(with, func(w *registry.Worker) { w.Inbox = append(w.Inbox, "why?") })
	r.tick(1)
	if panes := r.fake.Panes(with); len(panes) != 2 || !panes[1].Spec.MCP {
		t.Errorf("resume: %+v; want a second pane with MCP", panes)
	}
}
