package watch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// writeIn writes text to rel in dir, making its directories.
func writeIn(t *testing.T, dir, rel, text string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The docs are config [questions] context, in its order, or unset the
// first of .design/SPEC.md, SPEC.md and README.md; context = [] is none. A
// listed path with no file is named, not an error.
func TestContextChoosesTheDocs(t *testing.T) {
	for _, tt := range []struct {
		name    string
		have    []string // files in the repo
		files   []string // [questions] context; nil = unset
		docs    []string
		missing []string
	}{
		{"unset: .design/SPEC.md first", []string{"README.md", "SPEC.md", ".design/SPEC.md"}, nil, []string{".design/SPEC.md"}, nil},
		{"unset: then SPEC.md", []string{"README.md", "SPEC.md"}, nil, []string{"SPEC.md"}, nil},
		{"unset: then README.md", []string{"README.md"}, nil, []string{"README.md"}, nil},
		{"unset, and none there", []string{"docs/SPEC.md"}, nil, nil, nil},
		{"a list, in its order", []string{"README.md", "SPEC.md", "docs/design.md"}, []string{"docs/design.md", "README.md"}, []string{"docs/design.md", "README.md"}, nil},
		{"a missing one is named", []string{"SPEC.md"}, []string{"SPEC.md", "docs/gone.md"}, []string{"SPEC.md"}, []string{"docs/gone.md"}},
		{"context = []: none", []string{"SPEC.md", "README.md"}, []string{}, nil, nil},
		{"a directory is no doc", []string{"docs/a.md"}, []string{"docs"}, nil, []string{"docs"}},
		{"named twice, once", []string{"SPEC.md"}, []string{"SPEC.md", "./SPEC.md", "gone.md", "gone.md"}, []string{"SPEC.md"}, []string{"gone.md"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := registry.Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
			for _, f := range tt.have {
				writeIn(t, p.Root(), f, "text of "+f)
			}
			c := BuildContext(p, tt.files, registry.Worker{ID: 1, Role: "task", State: registry.Blocked})
			var docs []string
			for _, d := range c.Docs {
				rel, _ := filepath.Rel(p.Root(), d.Path)
				docs = append(docs, rel)
				if !filepath.IsAbs(d.Path) || d.Size != int64(len("text of "+rel)) {
					t.Errorf("doc %+v", d)
				}
			}
			if !slices.Equal(docs, tt.docs) || !slices.Equal(c.Missing, tt.missing) {
				t.Errorf("docs %v, missing %v; want %v, %v", docs, c.Missing, tt.docs, tt.missing)
			}
			st := c.JevState()
			for i, f := range st.Docs {
				if f.Path != docs[i] || f.Content != "text of "+docs[i] {
					t.Errorf("Jev's doc %d = %+v", i, f)
				}
			}
			if !slices.Equal(st.Missing, tt.missing) {
				t.Errorf("Jev's missing = %v", st.Missing)
			}
		})
	}
}

// Jev gets the docs' text within jevBudget, shared out as a judge's files
// are: a short one whole, long ones cut alike to their head and tail, with
// a line saying how many bytes were cut, on rune boundaries.
func TestContextDocsFitJevsBudget(t *testing.T) {
	defer func(n int) { jevBudget = n }(jevBudget)
	jevBudget = 1000
	long := func(n int) string { return "HEAD" + strings.Repeat("é", (n-8)/2) + "TAIL" }
	for _, tt := range []struct {
		name  string
		sizes []int // of the docs, in [questions] context order
		share []int // what fair gives each
	}{
		{"under budget: whole", []int{100, 300}, []int{100, 300}},
		{"several over budget: cut alike", []int{2000, 3000, 2000}, []int{333, 333, 334}},
		{"one small, one huge", []int{100, 5000}, []int{100, 900}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := registry.Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
			var files, texts []string
			for i, n := range tt.sizes {
				text := strings.Repeat("s", n)
				if n > 500 {
					text = long(n)
				}
				files, texts = append(files, fmt.Sprintf("doc%d.md", i)), append(texts, text)
				writeIn(t, p.Root(), files[i], text)
			}
			st := BuildContext(p, files, registry.Worker{ID: 1}).JevState()
			total := 0
			for i, f := range st.Docs {
				mark := markerOf(f.Content)
				kept := len(f.Content) - len(mark)
				total += kept
				switch {
				case !utf8.ValidString(f.Content):
					t.Errorf("doc %d is not UTF-8", i)
				case len(texts[i]) <= tt.share[i]:
					if f.Content != texts[i] {
						t.Errorf("doc %d was cut: %d of %d bytes", i, len(f.Content), len(texts[i]))
					}
				case kept > tt.share[i] || kept < tt.share[i]-3 || !strings.HasPrefix(f.Content, "HEAD") || !strings.HasSuffix(f.Content, "TAIL"):
					t.Errorf("doc %d keeps %d bytes, its share is %d: %.40q…", i, kept, tt.share[i], f.Content)
				case mark != fmt.Sprintf("\n[routr: %d bytes cut here]\n", len(texts[i])-kept):
					t.Errorf("doc %d: marker %q, %d of %d bytes kept", i, mark, kept, len(texts[i]))
				}
			}
			if total > jevBudget {
				t.Errorf("the docs are %d bytes, over the budget", total)
			}
		})
	}
}

// The brief is the prompt as sent (brief.md), else the worker's brief as
// queued; a long one is cut to its head and tail, for the agent and Jev
// alike.
func TestContextCutsTheBrief(t *testing.T) {
	defer func(n int) { jevBrief = n }(jevBrief)
	jevBrief = 100
	for _, tt := range []struct {
		name    string
		sent    string // brief.md; "" = none
		brief   string
		want    string
		hasPath bool
	}{
		{"as sent", "Fix the flaky test.\n---\nYou are routr worker 1.\n", "{{task}}", "Fix the flaky test.\n---\nYou are routr worker 1.", true},
		{"a long one is cut", "A" + strings.Repeat("a", 998) + "Z", "{{task}}", "A" + strings.Repeat("a", 49) + "\n[routr: 900 bytes cut here]\n" + strings.Repeat("a", 49) + "Z", true},
		{"not sent yet: as queued", "", "Fix {{task}}", "Fix {{task}}", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := registry.Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
			if tt.sent != "" {
				writeIn(t, p.RunDir(1), registry.BriefFile, tt.sent)
			}
			c := BuildContext(p, []string{}, registry.Worker{ID: 1, Brief: tt.brief})
			if c.Brief != tt.want || c.JevState().Brief != tt.want {
				t.Errorf("brief = %q, want %q", c.Brief, tt.want)
			}
			if want := filepath.Join(p.RunDir(1), registry.BriefFile); (c.BriefPath == want) != tt.hasPath {
				t.Errorf("brief path = %q", c.BriefPath)
			}
		})
	}
}

// The repo root is the main checkout's, found from a subdirectory or a
// worker's own worktree (registry.Find, as routr show does): the docs are
// what the user wrote, not the worktree's copy, which the worker may have
// edited.
func TestContextDocsComeFromTheMainCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	writeIn(t, repo, "SPEC.md", "what the user wrote\n")
	writeIn(t, repo, "sub/a.go", "package sub\n")
	git("add", ".")
	git("commit", "-q", "-m", "first")
	tree := filepath.Join(repo, ".routr", "trees", "c", "implement-2")
	git("worktree", "add", "-q", "-b", "routr/c/implement-2", tree)
	writeIn(t, tree, "SPEC.md", "what the worker made of it\n")
	noEnv := func(string) string { return "" }
	for _, cwd := range []string{repo, filepath.Join(repo, "sub"), tree, filepath.Join(tree, "sub")} {
		p, err := registry.Find(cwd, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		wk := registry.Worker{ID: 2, Cwd: cwd, Tree: tree, Checkout: &registry.Checkout{Repo: repo, Path: tree, Made: true}}
		c := BuildContext(p, nil, wk)
		st := c.JevState()
		if c.Root != repo || len(c.Docs) != 1 || c.Docs[0].Path != filepath.Join(repo, "SPEC.md") ||
			st.Docs[0].Path != "SPEC.md" || st.Docs[0].Content != "what the user wrote\n" {
			t.Errorf("from %s: root %s, docs %+v, Jev's %+v", cwd, c.Root, c.Docs, st.Docs)
		}
	}
}

// What a blocked worker's question is decided with, for the main agent
// (the docs as paths with sizes, to read first) and for Jev (their text,
// the options numbered as routr answer takes them).
func TestContextRenderings(t *testing.T) {
	p := registry.Paths{Dir: filepath.Join(t.TempDir(), ".routr")}
	root := p.Root()
	writeIn(t, root, "SPEC.md", "# Spec\nTests run with go test.\n")
	writeIn(t, p.RunDir(6), registry.BriefFile, "Fix the flaky test in ./store.\n")
	blocked := registry.Worker{ID: 6, Chain: "fix-3", Step: "implement", Role: "implement", Harness: "claude", Answerer: "agent",
		Cwd: filepath.Join(root, "store"), State: registry.Blocked,
		Question: "Do you want to proceed?\n1. Yes\n2. No", Options: []string{"Yes", "No"}, Fingerprint: "6a128b4273b95faa",
		Pending: &registry.ToolCall{Tool: "Bash", Summary: "Bash: go test ./...", Input: `{"command":"go test ./..."}`}}
	c := BuildContext(p, []string{"SPEC.md", "docs/gone.md"}, blocked)
	want := `6 implement · claude: role implement, chain fix-3, step implement, in /repo/store
Who answers its questions: agent

It asks:
Do you want to proceed?
1. Yes
2. No
fingerprint: 6a128b4273b95faa
It asks about Bash: go test ./...
input:
{"command":"go test ./..."}

Its brief (/repo/.routr/runs/6/brief.md):
Fix the flaky test in ./store.

Project docs: read them before you decide.
/repo/SPEC.md (31 bytes)
Missing: docs/gone.md, listed in [questions] context but not in /repo
`
	if got := strings.ReplaceAll(c.Text(), root, "/repo"); got != want {
		t.Errorf("Text:\n%s\nwant\n%s", got, want)
	}
	st := c.JevState()
	if st.Worker != 6 || st.Role != "implement" || st.Chain != "fix-3" || st.Step != "implement" || st.Cwd != blocked.Cwd ||
		st.Question != blocked.Question || !slices.Equal(st.Options, []string{"1. Yes", "2. No"}) || st.Pending != blocked.Pending ||
		st.Brief != "Fix the flaky test in ./store." || len(st.Docs) != 1 || st.Docs[0] != (File{"SPEC.md", "# Spec\nTests run with go test.\n"}) ||
		!slices.Equal(st.Missing, []string{"docs/gone.md"}) {
		t.Errorf("JevState = %+v", st)
	}

	// A worker that asks nothing now, in a repo with no docs: the rest.
	quiet := registry.Worker{ID: 7, Role: "task", Harness: "codex", Cwd: root, State: registry.Running, Brief: "Summarize the diff"}
	want = `7 task · codex: role task, in /repo
Who answers its questions: human

It asks nothing now (running).

Its brief:
Summarize the diff

Project docs: none. Config [questions] context names them.
`
	if got := strings.ReplaceAll(BuildContext(p, []string{}, quiet).Text(), root, "/repo"); got != want {
		t.Errorf("Text:\n%s\nwant\n%s", got, want)
	}

	// The user answers: the docs are listed, not to be read before deciding.
	// An option that saves a rule is the user's alone, whoever answers.
	human := blocked
	human.Answerer = ""
	human.Question = "Do you want to proceed?\n1. Yes\n2. Yes, and don't ask again for: go test\n3. No"
	human.Options = []string{"Yes", "Yes, and don't ask again for: go test", "No"}
	c = BuildContext(p, nil, human)
	want = `It asks:
Do you want to proceed?
1. Yes
2. Yes, and don't ask again for: go test
3. No
fingerprint: 6a128b4273b95faa
Do not pick option 2: it saves a rule or lasts past this call, so only the user may.
It asks about Bash: go test ./...
input:
{"command":"go test ./..."}

Its brief (/repo/.routr/runs/6/brief.md):
Fix the flaky test in ./store.

Project docs:
/repo/SPEC.md (31 bytes)
`
	if got := strings.ReplaceAll(c.Text(), root, "/repo"); !strings.HasSuffix(got, want) || !strings.Contains(got, "Who answers its questions: human\n") {
		t.Errorf("Text:\n%s\nwant it to end\n%s", got, want)
	}
	if got := strings.ReplaceAll(c.ReadFirst(), root, "/repo"); !slices.Equal(c.Lasting, []int{2}) ||
		got != "Before you decide, read the project docs, even when the answer looks obvious: /repo/SPEC.md\nIts brief: /repo/.routr/runs/6/brief.md" {
		t.Errorf("lasting %v, read first %q", c.Lasting, got)
	}
	if got := BuildContext(p, []string{}, quiet).ReadFirst(); got != "" {
		t.Errorf("nothing to read: %q", got)
	}
	c.Lasting = []int{2, 3}
	if got := c.LastingNote(); got != "Do not pick options 2, 3: they save a rule or last past this call, so only the user may." {
		t.Errorf("LastingNote = %q", got)
	}
}
