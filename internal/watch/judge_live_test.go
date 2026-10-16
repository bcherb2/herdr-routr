package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/jev"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

// TestLiveJev asks the real Jev twice, a verdict and a pick on tiny states,
// through the watcher's own client: ROUTR_LIVE_JEV=1 and
// OPENROUTER_API_KEY. Each call costs about $0.00002.
func TestLiveJev(t *testing.T) {
	if os.Getenv("ROUTR_LIVE_JEV") != "1" || os.Getenv(JevKeyEnv) == "" {
		t.Skip("set ROUTR_LIVE_JEV=1 and " + JevKeyEnv + " to ask the real Jev")
	}
	r := newRig(t)
	t.Cleanup(func() { r.reap(r.w) })
	r.w.Jev = &jev.Client{APIKey: os.Getenv(JevKeyEnv), Retries: -1} // no retries: every try is a paid call
	a := r.add(registry.Worker{Chain: "c", Step: "review", Brief: "r"})
	b := r.add(registry.Worker{Chain: "c", Step: "fast", Brief: "f"})
	c := r.add(registry.Worker{Chain: "c", Step: "slow", Brief: "s"})
	ok := r.addJudge(registry.Worker{Chain: "c", Step: "ship", After: []int{a}, Brief: "Does the review leave anything that must be fixed before shipping?"},
		registry.Judge{Pass: "ship as is: no confirmed finding", Fail: "at least one confirmed finding must be fixed first"})
	best := r.addJudge(registry.Worker{Chain: "c", Step: "best", After: []int{b, c}, Brief: "Which implementation of sum(xs) is correct?"},
		registry.Judge{Pick: true})
	r.tick(1)
	files := map[string]string{
		"REVIEW.md": "Finding 1 (confirmed, high): SQL injection in login(): the username is concatenated into the query. Must fix.",
		"fast.py":   "def sum(xs):\n    return xs[0]  # only the first element\n",
		"slow.py":   "def sum(xs):\n    t = 0\n    for x in xs:\n        t += x\n    return t\n",
	}
	for name, text := range files {
		os.WriteFile(filepath.Join(r.cwd, name), []byte(text), 0o644)
	}
	for id, out := range map[int]string{a: "REVIEW.md", b: "fast.py", c: "slow.py"} {
		registry.WriteDone(r.p.RunDir(id), registry.DoneRecord{Output: filepath.Join(r.cwd, out), Message: "wrote " + out})
		r.fake.Finish(id, "")
	}
	r.settle()
	for _, id := range []int{ok, best} {
		var w registry.Worker
		r.until(func() bool {
			r.tick(1)
			w = r.get(id)
			return w.State != registry.Running && w.State != registry.Pending && w.State != registry.Starting && w.State != registry.Waiting
		})
		t.Logf("%s: %s, verdict %q, output %q, note %q\nfinal.md:\n%s\njev.json:\n%s", w.Label(), w.State, w.Verdict, w.Output, w.Note,
			r.read(r.final(id)), r.read(filepath.Join(r.p.RunDir(id), registry.JevFile)))
		if w.State == registry.NeedsYou && w.Finished.IsZero() && w.Summary == "" {
			t.Errorf("%s: Jev did not answer: %s", w.Label(), w.Note)
		}
	}
}
