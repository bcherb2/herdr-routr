package route

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/jev"
)

type fakeJev struct {
	answers map[string]jev.Answer
	err     error
	asked   map[string]jev.Choice
	state   any
}

func (f *fakeJev) Decide(_ context.Context, state any, qs map[string]jev.Choice) (jev.Decision, error) {
	f.asked, f.state = qs, state
	return jev.Decision{ID: "gen-dec-1", Answers: f.answers}, f.err
}

func ready(b bool) func(context.Context) error {
	return func(context.Context) error {
		if b {
			return nil
		}
		return errors.New("not logged in")
	}
}

func harnesses(claudeReady, codexReady bool) []Harness {
	hs := Defaults()[:2]
	hs[0].Ready, hs[1].Ready = ready(claudeReady), ready(codexReady)
	return hs
}

func TestHarnessArgsLeaveDefaultsOut(t *testing.T) {
	byKind := map[string]Harness{}
	for _, h := range Defaults() {
		byKind[h.Kind] = h
	}
	tests := []struct {
		kind, model, effort, want string
	}{
		{"claude", "haiku", "low", "--model haiku --effort low"},
		{"claude", "", "", ""},
		{"codex", "", "high", `-c model_reasoning_effort="high"`},
		{"codex", "gpt-6-astra", "", "-m gpt-6-astra"},
		{"pi", "deepseek/deepseek-v4-flash", "low", "--model deepseek/deepseek-v4-flash --thinking low"},
		{"pi", PiDefaultModel, "medium", "--thinking medium"},
	}
	for _, tt := range tests {
		if got := strings.Join(byKind[tt.kind].Args(tt.model, tt.effort), " "); got != tt.want {
			t.Errorf("%s(%q, %q) = %q, want %q", tt.kind, tt.model, tt.effort, got, tt.want)
		}
	}
}

func TestResumeArgs(t *testing.T) {
	model := []string{"--model", "haiku"}
	tests := []struct {
		kind, session string
		want          string // joined argv, "" for an error
	}{
		{"claude", "4f1c-9a", "--resume 4f1c-9a --model haiku"},
		{"codex", "019a-77", "resume 019a-77 --model haiku"},
		{"pi", "/home/u/.pi/agent/sessions/--w--/t_01a0.jsonl", "--session /home/u/.pi/agent/sessions/--w--/t_01a0.jsonl --model haiku"},
		{"gemini", "x", ""},
		{"claude", "", ""},
		{"claude", "--dangerously-skip-permissions", ""},
		{"codex", "a b", ""},
	}
	for _, tt := range tests {
		got, err := ResumeArgs(tt.kind, tt.session, model)
		if tt.want == "" {
			if err == nil {
				t.Errorf("%s %q: want error, got %v", tt.kind, tt.session, got)
			}
			continue
		}
		if err != nil || strings.Join(got, " ") != tt.want {
			t.Errorf("%s %q = %v, %v; want %s", tt.kind, tt.session, got, err, tt.want)
		}
	}
}

func ans(choice string, p map[string]float64) jev.Answer {
	return jev.Answer{Choice: choice, Probabilities: p, Confidence: p[choice]}
}

func TestRouteBuildsLaunchArgs(t *testing.T) {
	tests := []struct {
		name, pick, effort string
		wantKind           string
		wantArgs           []string
	}{
		{"claude", "claude_code:sonnet", "medium", "claude", []string{"--model", "sonnet", "--effort", "medium"}},
		{"codex", "codex_cli:gpt-6-astra", "high", "codex", []string{"-m", "gpt-6-astra", "-c", `model_reasoning_effort="high"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeJev{answers: map[string]jev.Answer{
				"harness_model": ans(tt.pick, map[string]float64{tt.pick: 0.9}),
				"effort":        ans(tt.effort, map[string]float64{tt.effort: 1}),
			}}
			d, err := Router{Jev: f, Harnesses: harnesses(true, true)}.Route(context.Background(), "p", "rules")
			if err != nil {
				t.Fatal(err)
			}
			if d.Kind != tt.wantKind || strings.Join(d.Args, " ") != strings.Join(tt.wantArgs, " ") || d.JevID != "gen-dec-1" {
				t.Errorf("decision = %+v", d)
			}
			if d.Label() != strings.Replace(tt.pick, ":", " · ", 1)+" · "+tt.effort {
				t.Errorf("label = %q", d.Label())
			}
		})
	}
}

func TestRouteWithNoReadyHarnessSaysWhy(t *testing.T) {
	_, err := Router{Jev: &fakeJev{}, Harnesses: harnesses(false, false)}.Route(context.Background(), "p", "")
	if err == nil || !strings.Contains(err.Error(), "claude_code: not logged in") ||
		!strings.Contains(err.Error(), "codex_cli: not logged in") {
		t.Fatalf("err = %v", err)
	}
}

func TestRouteOffersOnlyReadyHarnesses(t *testing.T) {
	f := &fakeJev{answers: map[string]jev.Answer{
		"harness_model": ans("claude_code:haiku", map[string]float64{"claude_code:haiku": 1}),
		"effort":        ans("low", map[string]float64{"low": 1}),
	}}
	if _, err := (Router{Jev: f, Harnesses: harnesses(true, false)}).Route(context.Background(), "p", "r"); err != nil {
		t.Fatal(err)
	}
	for opt := range f.asked["harness_model"].Options {
		if !strings.HasPrefix(opt, "claude_code:") {
			t.Errorf("offered unavailable option %q", opt)
		}
	}
	if st := f.state.(map[string]string); st["user_rules"] != "r" || st["user_prompt"] != "p" {
		t.Errorf("state = %v", st)
	}
}

func TestRouteSingleComboSkipsHarnessQuestion(t *testing.T) {
	f := &fakeJev{answers: map[string]jev.Answer{"effort": ans("low", map[string]float64{"low": 1})}}
	d, err := Router{Jev: f, Harnesses: harnesses(false, true)}.Route(context.Background(), "p", "")
	if err != nil || d.Model != "gpt-6-astra" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if _, asked := f.asked["harness_model"]; asked {
		t.Error("a single option must not be sent to Jev (it requires >= 2)")
	}
}

func TestRouteErrors(t *testing.T) {
	bad := &fakeJev{answers: map[string]jev.Answer{
		"harness_model": ans("gemini_cli:pro", nil), "effort": ans("low", nil)}}
	if _, err := (Router{Jev: bad, Harnesses: harnesses(true, true)}).Route(context.Background(), "p", ""); err == nil {
		t.Error("want error for an option we never offered")
	}
	if _, err := (Router{Jev: &fakeJev{}, Harnesses: harnesses(false, false)}).Route(context.Background(), "p", ""); err == nil {
		t.Error("want error when no harness is ready")
	}
	boom := errors.New("down")
	if _, err := (Router{Jev: &fakeJev{err: boom}, Harnesses: harnesses(true, true)}).Route(context.Background(), "p", ""); !errors.Is(err, boom) {
		t.Errorf("want wrapped jev error, got %v", err)
	}
}

func TestRationale(t *testing.T) {
	got := Rationale(map[string]jev.Answer{
		"harness_model": {Choice: "a", Probabilities: map[string]float64{"a": 0.7, "b": 0.25, "c": 0.05, "d": 0}, Confidence: 0.6},
		"effort":        {Choice: "low", Probabilities: map[string]float64{"low": 1}, Confidence: 1},
	})
	want := "effort: low (p=1.00, conf=1.00; next: no close alternative); harness_model: a (p=0.70, conf=0.60; next: b 0.25, c 0.05)"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRouteWithPin(t *testing.T) {
	f := &fakeJev{answers: map[string]jev.Answer{"effort": ans("medium", map[string]float64{"medium": 1})}}
	r := Router{Jev: f, Harnesses: harnesses(true, true)}
	d, err := r.RouteWith(context.Background(), "p", "", Pin{Harness: "codex"})
	if err != nil || d.Harness != "codex_cli" || d.Effort != "medium" || !strings.HasSuffix(d.Rationale, "pinned: harness=codex") {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if _, asked := f.asked["harness_model"]; asked {
		t.Error("a pinned harness with one model must not be asked about")
	}

	f.asked = nil
	d, err = r.RouteWith(context.Background(), "p", "", Pin{Harness: "claude_code", Model: "opus", Effort: "high"})
	if err != nil || d.Model != "opus" || d.Effort != "high" || f.asked != nil || d.Rationale != "pinned: harness=claude_code, model=opus, effort=high" {
		t.Fatalf("fully pinned: d=%+v asked=%v err=%v", d, f.asked, err)
	}

	f.answers = map[string]jev.Answer{
		"harness_model": ans("claude_code:haiku", map[string]float64{"claude_code:haiku": 1}),
	}
	if d, err := r.RouteWith(context.Background(), "p", "", Pin{Harness: "claude", Effort: "low"}); err != nil || d.Model != "haiku" {
		t.Fatalf("model left open: d=%+v err=%v", d, err)
	}
	if _, asked := f.asked["effort"]; asked {
		t.Error("a pinned effort must not be asked about")
	}
	for _, pin := range []Pin{{Harness: "gemini"}, {Harness: "codex", Model: "opus"}, {Effort: "max"}} {
		if _, err := r.RouteWith(context.Background(), "p", "", pin); err == nil {
			t.Errorf("pin %+v: want error", pin)
		}
	}
}
