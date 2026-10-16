package registry

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)

func clock(at *time.Time) func() time.Time { return func() time.Time { return *at } }

func TestStoreCRUDAndPersistence(t *testing.T) {
	for _, name := range []string{"disk", "memory"} {
		t.Run(name, func(t *testing.T) {
			now := t0
			path := ""
			if name == "disk" {
				path = filepath.Join(t.TempDir(), "state", "registry.json")
			}
			s := &Store{Path: path, Now: clock(&now)}
			a, err := s.Create(Worker{Role: "plan", Harness: "claude", Chain: "c1", State: Pending})
			if err != nil || a.ID != 1 || !a.Created.Equal(t0) || !a.Updated.Equal(t0) {
				t.Fatalf("create = %+v, %v", a, err)
			}
			now = now.Add(time.Minute)
			b, _ := s.Create(Worker{Role: "review", Harness: "codex", Chain: "c1", After: []int{1}, State: Pending})
			if b.ID != 2 {
				t.Fatalf("second id = %d", b.ID)
			}
			s.Create(Worker{Role: "task", State: Pending})
			now = now.Add(time.Minute)
			got, err := s.Update(1, func(w *Worker) { w.Session, w.State = "sess-1", Running })
			if err != nil || got.Session != "sess-1" || !got.Updated.Equal(now) {
				t.Fatalf("update = %+v, %v", got, err)
			}
			if _, err := s.Update(9, func(*Worker) {}); !errors.Is(err, ErrNotFound) {
				t.Errorf("update unknown: %v", err)
			}
			refuse := errors.New("stale")
			if _, err := s.Modify(1, func(w *Worker) error { w.State = Failed; return refuse }); err != refuse {
				t.Errorf("modify error = %v", err)
			}
			if w, _ := s.Get(1); w.State != Running {
				t.Errorf("a refused Modify changed the worker: %s", w.State)
			}
			chain, _ := s.Chain("c1")
			if len(chain) != 2 || chain[0].ID != 1 || chain[1].ID != 2 {
				t.Errorf("chain = %+v", chain)
			}
			all, _ := s.List()
			if len(all) != 3 || all[2].ID != 3 {
				t.Errorf("list = %+v", all)
			}
			if path == "" {
				return
			}
			other := &Store{Path: path}
			if w, err := other.Get(2); err != nil || w.After[0] != 1 {
				t.Errorf("reopened: %+v, %v", w, err)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("registry file: %v %v", fi, err)
			}
		})
	}
}

func TestTxChangesTogetherOrNotAtAll(t *testing.T) {
	now := t0
	s := &Store{Path: filepath.Join(t.TempDir(), "registry.json"), Now: clock(&now)}
	s.Create(Worker{Role: "plan", State: Done})
	s.Create(Worker{Role: "review", State: Running})
	now = now.Add(time.Hour)
	err := s.Tx(func(ws map[int]*Worker) error {
		ws[1].State, ws[1].Inbox = Running, append(ws[1].Inbox, "fix it")
		ws[2].State = Failed
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("want the fn error")
	}
	if w, _ := s.Get(1); w.State != Done || len(w.Inbox) != 0 {
		t.Fatalf("aborted Tx leaked: %+v", w)
	}
	if err := s.Tx(func(ws map[int]*Worker) error { ws[1].State = Running; return nil }); err != nil {
		t.Fatal(err)
	}
	one, _ := s.Get(1)
	two, _ := s.Get(2)
	if one.State != Running || !one.Updated.Equal(now) || !two.Updated.Equal(t0) {
		t.Errorf("Tx stamps only changed workers: %v %v", one.Updated, two.Updated)
	}
}

func TestSpawnAddsAndChangesTogether(t *testing.T) {
	now := t0
	s := &Store{Path: filepath.Join(t.TempDir(), "registry.json"), Now: clock(&now)}
	s.Create(Worker{Step: "dig", State: Waiting, Fanout: &Fanout{Glob: "*.md"}})
	refuse := errors.New("stale")
	if _, err := s.Spawn(1, []Worker{{Step: "dig"}}, func(*Worker, []Worker) error { return refuse }); err != refuse {
		t.Fatalf("refused spawn: %v", err)
	}
	if all, _ := s.List(); len(all) != 1 {
		t.Fatalf("a refused spawn added %d workers", len(all)-1)
	}
	if _, err := s.Spawn(7, nil, func(*Worker, []Worker) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown parent: %v", err)
	}
	now = now.Add(time.Minute)
	got, err := s.Spawn(1, []Worker{{Step: "dig", Output: "a"}, {Step: "dig", Output: "b"}}, func(w *Worker, ws []Worker) error {
		for _, c := range ws {
			w.Fanout.Items = append(w.Fanout.Items, c.ID)
		}
		return nil
	})
	if err != nil || len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 || got[1].Output != "b" || !got[0].Created.Equal(now) {
		t.Fatalf("spawn = %+v, %v", got, err)
	}
	if w, _ := s.Get(1); len(w.Fanout.Items) != 2 || w.Fanout.Items[1] != 3 || !w.Updated.Equal(now) {
		t.Errorf("parent = %+v", w)
	}
	if c, _ := s.Create(Worker{}); c.ID != 4 {
		t.Errorf("next id = %d", c.ID)
	}
}

func TestCorruptRegistryIsSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	s := &Store{Path: path}
	if w, err := s.Create(Worker{Role: "x"}); err != nil || w.ID != 1 {
		t.Fatalf("create after corruption = %+v, %v", w, err)
	}
	if m, _ := filepath.Glob(path + ".corrupt-*"); len(m) != 1 {
		t.Errorf("corrupt file not kept aside: %v", m)
	}
	os.WriteFile(path, []byte(`{"version":99,"workers":[]}`), 0o600)
	if _, err := s.List(); err == nil {
		t.Error("a newer format must be refused, not overwritten")
	}
}

// Ids go on above the highest run directory after the registry is set aside
// or lost, so a new worker never writes into an old worker's runs/<id>.
func TestIDsSurviveALostRegistry(t *testing.T) {
	p := Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"3", "12", "chain-x"} {
		os.MkdirAll(filepath.Join(p.Dir, "runs", d), 0o755)
	}
	os.WriteFile(p.Registry(), []byte("{not json"), 0o600)
	if w, err := p.Store().Create(Worker{Role: "x"}); err != nil || w.ID != 13 {
		t.Fatalf("create after corruption = %+v, %v", w, err)
	}
	if m := p.SetAside(); len(m) != 1 || !strings.HasPrefix(m[0], p.Registry()+".corrupt-") {
		t.Errorf("SetAside = %v", m)
	}
	os.Remove(p.Registry())
	os.MkdirAll(p.RunDir(13), 0o755)
	if w, err := p.Store().Create(Worker{Role: "x"}); err != nil || w.ID != 14 {
		t.Fatalf("create after the registry was lost = %+v, %v", w, err)
	}
}

func TestConcurrentWritersGetUniqueIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	var wg sync.WaitGroup
	ids := make(chan int, 40)
	for range 4 {
		s := &Store{Path: path} // one Store per "process"
		wg.Go(func() {
			for range 10 {
				w, err := s.Create(Worker{Role: "x"})
				if err != nil {
					t.Error(err)
					return
				}
				ids <- w.ID
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := map[int]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if all, _ := (&Store{Path: path}).List(); len(all) != 40 {
		t.Errorf("lost writes: %d workers", len(all))
	}
}

// Two `routr flow start` at once picked the same free chain name (it was
// chosen from a List, before the slow role resolution).
func TestNewChainIsUniqueAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	(&Store{Path: path}).Create(Worker{Chain: "plan-redteam-4"}) // named by hand
	var wg sync.WaitGroup
	names := make(chan string, 20)
	for range 4 {
		s := &Store{Path: path}
		wg.Go(func() {
			for range 5 {
				n, err := s.NewChain("plan-redteam")
				if err != nil {
					t.Error(err)
				}
				names <- n
			}
		})
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for n := range names {
		if seen[n] || n == "plan-redteam-4" {
			t.Errorf("chain %s given out twice", n)
		}
		seen[n] = true
	}
	if !seen["plan-redteam-5"] || !seen["plan-redteam-24"] {
		t.Errorf("names: %v", seen)
	}
	if n, _ := (&Store{Path: path}).NewChain("other"); n != "other-1" {
		t.Errorf("another flow: %s", n)
	}
}

func TestDirectoryLock(t *testing.T) {
	now := t0
	s := &Store{Path: filepath.Join(t.TempDir(), "state", "registry.json"), Now: clock(&now)}
	a, _ := s.Create(Worker{Write: true, Tree: "/a", State: Pending})
	b, _ := s.Create(Worker{Write: true, Tree: "/a", State: Pending})
	c, _ := s.Create(Worker{Write: true, Tree: "/b", State: Pending})
	ro, _ := s.Create(Worker{Tree: "/a", State: Pending})
	start := func(w *Worker) error { w.State = Starting; return nil }
	claim := func(id int) int {
		t.Helper()
		_, h, err := s.Claim(id, start)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if h := claim(a.ID); h != a.ID {
		t.Fatalf("a free tree: holder %d", h)
	}
	if h := claim(b.ID); h != a.ID {
		t.Errorf("a held tree went to %d", h)
	}
	if w, _ := s.Get(b.ID); w.State != Pending {
		t.Errorf("a refused claim changed the worker: %s", w.State)
	}
	if h := claim(c.ID); h != c.ID {
		t.Errorf("another worktree locks separately; got %d", h)
	}
	if h := claim(ro.ID); h != ro.ID {
		t.Errorf("a read-only worker takes no lock; got %d", h)
	}
	if m, _ := s.Holders(); len(m) != 2 || m["/a"] != a.ID || m["/b"] != c.ID {
		t.Errorf("holders = %v", m)
	}
	// maybe_done keeps it for a while: the worker may go on working.
	s.Update(a.ID, func(w *Worker) { w.State, w.Idle = MaybeDone, s.now() })
	if h := claim(b.ID); h != a.ID {
		t.Errorf("maybe_done gave up the lock to %d", h)
	}
	// Finishing frees it without a release; a follow-up it then gets does not.
	s.Update(a.ID, func(w *Worker) { w.State = Done })
	if h := claim(b.ID); h != b.ID {
		t.Errorf("a finished holder kept the lock: %d", h)
	}
	s.Update(a.ID, func(w *Worker) { w.Queue("more?") })
	if _, h, _ := s.Claim(a.ID, func(*Worker) error { return nil }); h != b.ID {
		t.Errorf("a follow-up took the lock from a working holder: %d", h)
	}
	// A cancelled holder frees it at once.
	s.Update(b.ID, func(w *Worker) { w.State = Cancelled })
	if _, h, _ := s.Claim(a.ID, func(*Worker) error { return nil }); h != a.ID {
		t.Errorf("after cancel: %d", h)
	}
	if m, _ := s.Holders(); m["/a"] != a.ID {
		t.Errorf("a follow-up holds the lock until it is answered: %v", m)
	}
	if _, _, err := s.Claim(99, start); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
}

func TestDoneRecordIsTakenOnce(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := TakeDone(dir); ok || err != nil {
		t.Fatalf("no done.json: %v %v", ok, err)
	}
	WriteDone(dir, DoneRecord{Output: "/r/PLAN.md", Verdict: Pass, Message: "plan written", At: t0})
	if !HasDone(dir) {
		t.Fatal("HasDone")
	}
	d, ok, err := TakeDone(dir)
	if !ok || err != nil || d.Message != "plan written" || d.Verdict != Pass {
		t.Fatalf("take = %+v %v %v", d, ok, err)
	}
	WriteDone(dir, DoneRecord{Message: "round 2"})
	d, _, _ = TakeDone(dir)
	if HasDone(dir) || d.Message != "round 2" {
		t.Errorf("second take = %+v", d)
	}
	for _, f := range []string{"done.1.json", "done.2.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestFindRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv(StateDirEnv, "")
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repo, "init", "-q")
	run(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	sub := filepath.Join(repo, "a", "b")
	os.MkdirAll(sub, 0o755)
	wt := filepath.Join(t.TempDir(), "wt")
	run(repo, "worktree", "add", "-q", wt)

	repoReal, _ := filepath.EvalSymlinks(repo)
	for _, dir := range []string{repo, sub, wt} {
		p, err := Find(dir, os.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := filepath.EvalSymlinks(p.Root()); got != repoReal {
			t.Errorf("Find(%s).Root() = %s, want %s", dir, p.Root(), repo)
		}
	}
	if got, _ := filepath.EvalSymlinks(Tree(sub)); got != repoReal {
		t.Errorf("Tree(sub) = %s", got)
	}
	if got, want := Tree(wt), wt; got != want {
		if a, _ := filepath.EvalSymlinks(got); a != want {
			if b, _ := filepath.EvalSymlinks(want); a != b {
				t.Errorf("a worktree is its own tree: %s", got)
			}
		}
	}
	outside := t.TempDir()
	if p, _ := Find(outside, os.Getenv); p.Dir != filepath.Join(outside, ".routr") {
		t.Errorf("outside git: %s", p.Dir)
	}
	t.Setenv(StateDirEnv, "/tmp/elsewhere/.routr")
	if p, _ := Find(repo, os.Getenv); p.Dir != "/tmp/elsewhere/.routr" {
		t.Errorf("override: %s", p.Dir)
	}
	rel := func(string) string { return "state/.routr" }
	if p, _ := Find(outside, rel); p.Dir != filepath.Join(outside, "state", ".routr") {
		t.Errorf("a relative override is relative to cwd: %s", p.Dir)
	}
}

func TestInitKeepsTheGitignore(t *testing.T) {
	p := Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(p.Dir, ".gitignore")
	if b, _ := os.ReadFile(gi); string(b) != "/state/\n/runs/\n/work/\n/trees/\n/.gitignore\n" {
		t.Errorf(".gitignore = %q", b)
	}
	p.Init()
	if b, _ := os.ReadFile(gi); string(b) != "/state/\n/runs/\n/work/\n/trees/\n/.gitignore\n" {
		t.Errorf("Init again: .gitignore = %q", b)
	}
	// One an older routr wrote gets the new line; an edit stays.
	os.WriteFile(gi, []byte("/state/\n/runs/\nmine"), 0o644)
	p.Init()
	if b, _ := os.ReadFile(gi); string(b) != "/state/\n/runs/\nmine\n/work/\n/trees/\n/.gitignore\n" {
		t.Errorf("repaired .gitignore = %q", b)
	}
	for _, d := range []string{"state", "runs"} {
		if fi, err := os.Stat(filepath.Join(p.Dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
}

// What routr writes in .routr/ leaves git status clean: the .gitignore
// ignores itself, until the user adds a config.toml.
func TestInitLeavesGitStatusClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	status := func() string {
		t.Helper()
		out, err := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=all").CombinedOutput()
		if err != nil {
			t.Fatalf("git status: %v\n%s", err, out)
		}
		return string(out)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	p := Paths{Dir: filepath.Join(repo, ".routr")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.Dir, "state", "registry.json"), []byte("{}"), 0o644)
	if out := status(); out != "" {
		t.Errorf("git status after Init:\n%s", out)
	}
	os.WriteFile(p.Config(), nil, 0o644)
	if out := status(); out != "?? .routr/config.toml\n" {
		t.Errorf("git status with a config.toml:\n%s", out)
	}
}

func TestWorkerPredicates(t *testing.T) {
	w := Worker{ID: 3, Role: "review", Harness: "codex", Model: "gpt-6-astra"}
	if w.Label() != "3 review · codex" || w.Tag() != "codex · gpt-6-astra" {
		t.Errorf("label %q tag %q", w.Label(), w.Tag())
	}
	if w.Title = "fix-login-race"; w.Label() != "3 fix-login-race · codex" {
		t.Errorf("titled label %q", w.Label())
	}
	if w.ChainRef() != "c3" || (Worker{ID: 3, Chain: "plan-1"}).ChainRef() != "plan-1" {
		t.Error("ChainRef")
	}
	closedAfterDone := Worker{State: Closed, Finished: t0}
	if !closedAfterDone.Succeeded() || (Worker{State: Closed}).Succeeded() || !(Worker{State: Failed}).Stuck() {
		t.Error("Succeeded/Stuck")
	}
	if !(Worker{State: Done, Inbox: []string{"x"}}).Active() || (Worker{State: Done, Turn: 2, Replies: 2}).Active() {
		t.Error("Active")
	}
	idle := Worker{Write: true, State: MaybeDone, Idle: t0}
	if !(Worker{Write: true, State: Blocked}).LocksDir(t0) || !idle.LocksDir(t0.Add(MaybeDoneLock-time.Second)) ||
		idle.LocksDir(t0.Add(MaybeDoneLock)) || (Worker{Write: true, State: MaybeDone}).LocksDir(t0) ||
		(Worker{Write: true, State: Done}).LocksDir(t0) || (Worker{State: Running}).LocksDir(t0) {
		t.Error("LocksDir")
	}
}

// One rule, whatever kind of step gave the verdict: a fail without on_fail
// halts the steps after it, until routr approve; a fail with on_fail goes
// back round its loop instead, and a pass never halts.
func TestHaltsWhateverTheStepKind(t *testing.T) {
	onFail := &OnFail{Resume: 1, Brief: "Again.", Max: 2}
	for _, kind := range []struct {
		name string
		w    Worker
	}{
		{"agent", Worker{Role: "review", Harness: "codex"}},
		{"agent with no verdict ask", Worker{Role: "hunt", Harness: "codex"}},
		{"run", Worker{Role: "run", Run: "go test ./..."}},
		{"judge", Worker{Role: "judge", Judge: &Judge{}}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			for _, tt := range []struct {
				name    string
				verdict Verdict
				onFail  *OnFail
				goOn    bool
				state   State
				want    bool
			}{
				{"fail", Fail, nil, false, Done, true},
				{"fail, closed after done", Fail, nil, false, Closed, true},
				{"fail with on_fail", Fail, onFail, false, Done, false},
				{"fail approved past", Fail, nil, true, Done, false},
				{"fail, not done", Fail, nil, false, NeedsYou, false},
				{"pass", Pass, nil, false, Done, false},
				{"no verdict", "", nil, false, Done, false},
			} {
				w := kind.w
				w.State, w.Finished, w.Verdict, w.OnFail, w.GoOn = tt.state, t0, tt.verdict, tt.onFail, tt.goOn
				if got := w.Halts(); got != tt.want {
					t.Errorf("%s: Halts() = %v, want %v", tt.name, got, tt.want)
				}
			}
		})
	}
}

// A registry written before workers kept their menu as data still loads:
// the question as it was, no options, fingerprint, pending call or answer.
func TestOldRecordsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	os.WriteFile(path, []byte(`{"version":1,"next_id":1,"workers":[{"id":1,"role":"task","harness":"claude",
		"permissions":"default","cwd":"/w","write":false,"brief":"b","state":"blocked","question":"Allow?\n1. Yes\n2. No",
		"live":{"socket":"/s","pane":"w1:p1","agent":"a"},"created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z"}]}`), 0o600)
	s := &Store{Path: path, Now: clock(&t0)}
	w, err := s.Get(1)
	if err != nil || w.State != Blocked || w.Question != "Allow?\n1. Yes\n2. No" || w.Options != nil || w.Fingerprint != "" || w.AnsweredBy != "" {
		t.Fatalf("old record: %+v, %v", w, err)
	}
	if w.Pending != nil {
		t.Errorf("old record has a pending call: %+v", w.Pending)
	}
	call := ToolCall{Tool: "Bash", Summary: "Bash: rm -rf build", Input: `{"command":"rm -rf build"}`}
	w, err = s.Update(1, func(x *Worker) {
		x.Options, x.Fingerprint, x.BlockSeq = []string{"Yes", "No"}, "3f9a1c0e5b7d2a64", 26
		x.Pending = &call
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(1); !slices.Equal(got.Options, w.Options) || got.Fingerprint != "3f9a1c0e5b7d2a64" || got.BlockSeq != 26 ||
		got.Pending == nil || *got.Pending != call {
		t.Errorf("round trip: %+v", got)
	}
	w.ClearQuestion()
	if w.Question != "" || w.Options != nil || w.Fingerprint != "" || w.BlockSeq != 0 || w.Pending != nil {
		t.Errorf("ClearQuestion: %+v", w)
	}
}

func TestTitle(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"BT grace period", "bt-grace-period"},
		{"  --Fix: the (login) race!  ", "fix-the-login-race"},
		{"exactly-twenty-chars", "exactly-twenty-chars"},
		{"Name dispatched workspaces readably", "name-dispatched-wor~"},
		{"name dispatched wo rkspaces", "name-dispatched-wo~"}, // no "-" before the "~"
		{"Überprüfe die Größe", "überprüfe-die-größe"},
		{"!!! ---", ""},
		{"", ""},
	} {
		if got := Title(tt.in); got != tt.want {
			t.Errorf("Title(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if n := len([]rune(Title(tt.in))); n > TitleMax {
			t.Errorf("Title(%q) is %d long", tt.in, n)
		}
	}
}

// Ancestors walks After breadth first, nearest first, each worker once,
// past a cycle and over ids that are gone.
func TestAncestors(t *testing.T) {
	m := map[int]Worker{}
	for _, w := range []Worker{
		{ID: 1},
		{ID: 2, After: []int{1}},
		{ID: 3, After: []int{1, 9}}, // 9 is gone
		{ID: 4, After: []int{3, 2}},
		{ID: 5, After: []int{6}},
		{ID: 6, After: []int{5}},
	} {
		m[w.ID] = w
	}
	ids := func(ws []Worker) []int {
		var out []int
		for _, w := range ws {
			out = append(out, w.ID)
		}
		return out
	}
	for id, want := range map[int][]int{1: nil, 2: {1}, 4: {3, 2, 1}, 5: {6}} {
		if got := ids(Ancestors(m[id], m)); !slices.Equal(got, want) {
			t.Errorf("Ancestors(%d) = %v, want %v", id, got, want)
		}
	}
}

// NewRun counts the run up, takes the replies so far as an earlier run's
// and clears what the run concluded; a run or judge step's output and a
// judge's pick and approval too. It touches no file.
func TestNewRun(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	w := Worker{ID: 1, Runs: 1, State: Done, Turn: 3, Replies: 3, Verdict: Fail, Summary: "2 findings", Finished: t0, GoOn: true, Error: "e", Output: "OUT.md"}
	if err := WriteFile(p.FinalPath(w), []byte("first run\n")); err != nil {
		t.Fatal(err)
	}
	first := p.FinalPath(w)
	w.NewRun()
	if w.Runs != 2 || w.RunFrom != 3 || w.Verdict != "" || w.Summary != "" || !w.Finished.IsZero() || w.GoOn || w.Error != "" || w.Output != "OUT.md" || w.State != Done {
		t.Errorf("after NewRun: %+v", w)
	}
	if b, _ := os.ReadFile(first); string(b) != "first run\n" {
		t.Errorf("run 1's final = %q, want it where it was", b)
	}
	if p.FinalPath(w) == first {
		t.Errorf("run 2 writes run 1's final %s", first)
	}

	j := Worker{ID: 2, Run: "make", Output: "/r/run.log", Verdict: Pass}
	pick := Worker{ID: 3, Judge: &Judge{Pick: true, Winner: 7, Accept: true}, Output: "W.md"}
	for _, x := range []*Worker{&j, &pick} {
		if x.NewRun(); x.Output != "" || x.Verdict != "" || x.Runs != 1 {
			t.Errorf("tabless NewRun: %+v", x)
		}
	}
	if pick.Judge.Winner != 0 || pick.Judge.Accept || !pick.Judge.Pick {
		t.Errorf("judge after NewRun: %+v", pick.Judge)
	}
}

// Each run has a final file of its own, named so it cannot be taken for
// final.md or an archive final.<n>.md of the routr before; a record from
// before runs were numbered (Runs 0) keeps final.md. PrevFinal is the run
// before's, once written.
func TestFinalPathByRun(t *testing.T) {
	p := Paths{Dir: "/r/.routr"}
	for runs, want := range map[int]string{0: "final.md", 1: "final-1.md", 12: "final-12.md"} {
		if got := p.FinalPath(Worker{ID: 4, Runs: runs}); got != filepath.Join(p.RunDir(4), want) {
			t.Errorf("FinalPath at run %d = %s, want %s", runs, got, want)
		}
	}

	p = Paths{Dir: t.TempDir()}
	legacy := Worker{ID: 1}
	if err := WriteFile(p.FinalPath(legacy), []byte("legacy\n")); err != nil {
		t.Fatal(err)
	}
	if got := p.PrevFinal(legacy); got != "" {
		t.Errorf("PrevFinal of a record from before runs = %q", got)
	}
	legacy.NewRun()
	if got := p.PrevFinal(legacy); got != filepath.Join(p.RunDir(1), FinalFile) {
		t.Errorf("PrevFinal after a legacy record's NewRun = %q, want its final.md", got)
	}
	w := Worker{ID: 2, Runs: 1}
	w.NewRun()
	if got := p.PrevFinal(w); got != "" {
		t.Errorf("PrevFinal with run 1 writing none = %q", got)
	}
	if err := WriteFile(p.finalOf(2, 1), []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if got := p.PrevFinal(w); got != p.finalOf(2, 1) {
		t.Errorf("PrevFinal = %q", got)
	}
}

// Current is this run's final and a reply only if its follow-up came in
// this run and the reply is newer than the final: never an earlier run's.
// A record from before runs were numbered reads its final.md as before.
func TestCurrent(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	write := func(path, text string, at time.Time) {
		t.Helper()
		if err := WriteFile(path, []byte(text)); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, at, at)
	}
	w := Worker{ID: 1, Runs: 1, Turn: 1, Replies: 1}
	write(p.TurnFile(1, 1), "reply 1\n", t0)
	write(p.FinalPath(w), "final 1\n", t0.Add(time.Minute))
	round1 := p.FinalPath(w)
	if got := p.Current(w); got != (Result{Final: "final 1\n", FinalPath: round1}) {
		t.Errorf("final newer than the reply: %+v", got)
	}
	write(p.TurnFile(1, 1), "reply 1\n", t0.Add(2*time.Minute))
	if got := p.Current(w); got.Reply != "reply 1\n" || got.Turn != 1 || got.Final != "final 1\n" {
		t.Errorf("reply newer than the final: %+v", got)
	}

	w.NewRun()
	if got := p.Current(w); got != (Result{}) {
		t.Errorf("a new run shows %+v, want nothing of run 1", got)
	}
	w.Turn, w.Replies = 2, 2
	write(p.TurnFile(1, 2), "reply 2\n", t0.Add(3*time.Minute))
	if got := p.Current(w); got != (Result{Reply: "reply 2\n", ReplyPath: p.TurnFile(1, 2), Turn: 2}) || got.Path() != p.TurnFile(1, 2) {
		t.Errorf("run 2's reply: %+v", got)
	}
	write(p.FinalPath(w), "final 2\n", t0.Add(4*time.Minute))
	if got := p.Current(w); got.Final != "final 2\n" || got.FinalPath == round1 || got.Reply != "" || got.Path() != got.FinalPath {
		t.Errorf("run 2's final: %+v", got)
	}
	if b, _ := os.ReadFile(round1); string(b) != "final 1\n" {
		t.Errorf("the path run 1 handed out reads %q", b)
	}

	legacy := Worker{ID: 2, Turn: 1, Replies: 1}
	write(p.TurnFile(2, 1), "old reply\n", t0)
	write(filepath.Join(p.RunDir(2), FinalFile), "old final\n", t0.Add(time.Minute))
	if got := p.Current(legacy); got != (Result{Final: "old final\n", FinalPath: filepath.Join(p.RunDir(2), FinalFile)}) {
		t.Errorf("legacy record: %+v", got)
	}
}

// The format is version 1 again: a record without run_from (the installed
// routr's, from before routr retry) reads as RunFrom 0, every reply its
// run's, and is saved as version 1. A newer file is refused.
func TestFormatVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	os.WriteFile(path, []byte(`{"version":1,"next_id":1,"workers":[{"id":1,"role":"task","harness":"claude","permissions":"default","cwd":"/w","write":false,"brief":"b","state":"running","turn":2,"replies":2,"created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z"}]}`), 0o600)
	s := &Store{Path: path, Now: clock(&t0)}
	if w, err := s.Get(1); err != nil || w.RunFrom != 0 || w.Replies != 2 {
		t.Fatalf("read: %+v, %v", w, err)
	}
	if _, err := s.Update(1, func(w *Worker) { w.Note = "x" }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"version": 1,`) || strings.Contains(string(b), "run_from") {
		t.Errorf("saved:\n%s", b)
	}
	os.WriteFile(path, []byte(`{"version":2,"next_id":0,"workers":[]}`), 0o600)
	if _, err := s.List(); err == nil || !strings.Contains(err.Error(), "format version 2; this routr reads up to 1") {
		t.Errorf("a version 2 file: %v", err)
	}
}
