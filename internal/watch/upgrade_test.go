package watch

import (
	"slices"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

func (r *rig) judgeInputs(id int) []int {
	r.t.Helper()
	all, err := r.s.List()
	if err != nil {
		r.t.Fatal(err)
	}
	st, _, err := r.w.judgeState(r.get(id), all)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []int
	for _, u := range st.Upstream {
		out = append(out, u.Worker)
	}
	return out
}

// A verdict judge between a loop's target and its reviewer, which an
// earlier routr queued behind the revision with edges added to its After
// (After [3 1 2]), and that reviewer one loop round in: the judge judges
// only what it was queued after (From), now and over further rounds, and
// the reviewer still judges the revision it sent back for.
func TestLegacyJudgeScope(t *testing.T) {
	r := newRig(t)
	for _, w := range []registry.Worker{
		{Step: "draft"},
		{Step: "check", Run: "true", After: []int{1}},
		{Step: "report", After: []int{2}},
		{Step: "judge_report", After: []int{3, 1, 2}, Judge: &registry.Judge{From: []int{3}}},
		{Step: "review", After: []int{4, 1}, Judge: &registry.Judge{From: []int{4}}, OnFail: &registry.OnFail{Resume: 1, Max: 5, Brief: "revise"}, Round: 2},
	} {
		w.Chain, w.State, w.Verdict = "c", registry.Done, registry.Pass
		if _, err := r.s.Create(w); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n <= 2; n++ {
		if n > 0 {
			if err := r.s.Tx(func(m map[int]*registry.Worker) error {
				for _, w := range m {
					w.State, w.Inbox, w.Rest = registry.Done, nil, ""
				}
				m[5].Verdict = registry.Fail
				r.w.loop(m[5], m)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		judge, review := r.judgeInputs(4), r.judgeInputs(5)
		if !slices.Equal(judge, []int{3}) || !slices.Equal(review, []int{1, 4}) {
			w := r.get(4)
			t.Errorf("legacy edges + %d loops: judge inputs %v (After %v From %v), reviewer %v", n, judge, w.After, w.Judge.From, review)
		}
	}
}
