package flow

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	r := Refs{
		Params: map[string]string{"task": "add OAuth", "in": "DECISIONS.md"},
		Steps: map[string]StepRef{
			"plan":   {Output: "PLAN.md", Final: ".routr/runs/1/final.md"},
			"review": {Output: "REDTEAM.md", Verdict: "fail"},
			"dig":    {Outputs: []string{"research/a.md", "research/b.md"}},
		},
		Item: &Item{Path: "briefs/a.md", Name: "a.md"},
	}
	got, err := Render("{{task}} from {{ in }}: {{plan.output}} {{plan.final}} {{review.verdict}} {{dig.outputs}} {{item.name}}", r)
	if err != nil {
		t.Fatal(err)
	}
	want := "add OAuth from DECISIONS.md: PLAN.md .routr/runs/1/final.md fail research/a.md research/b.md a.md"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestRenderReportsEveryBadRef(t *testing.T) {
	_, err := Render("{{nope}} {{plan.verdict}} {{ghost.output}} {{plan.size}} {{item.path}}",
		Refs{Steps: map[string]StepRef{"plan": {Output: "PLAN.md"}}})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"{{nope}}", "{{plan.verdict}}", "{{ghost.output}}", "{{plan.size}}", "{{item.path}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestRenderChainAndWork(t *testing.T) {
	got, err := Render("{{chain}}: {{work}}/findings/*.md", Refs{Chain: "fix-2", Work: ".routr/work/fix-2", Params: map[string]string{"work": "no"}})
	if err != nil || got != "fix-2: .routr/work/fix-2/findings/*.md" {
		t.Errorf("got %q, %v", got, err)
	}
	_, err = Render("{{chain}} {{work}}", Refs{})
	if err == nil || !strings.Contains(err.Error(), "{{chain}}: no chain is known here") || !strings.Contains(err.Error(), "{{work}}: no work directory is known here") {
		t.Errorf("err = %v", err)
	}
}

func TestRenderShellQuotesAllButParams(t *testing.T) {
	r := Refs{
		Params: map[string]string{"check": "go test ./... && go vet ./..."},
		Steps: map[string]StepRef{
			"plan": {Output: "it's; rm -rf ~.md", Final: ".routr/runs/1/final.md", Verdict: "fail"},
			"dig":  {Outputs: []string{"a b.md", "c.md", "$(x).md"}},
		},
		Chain: "fix-1",
		Work:  "/repo dir/.routr/work/fix-1",
	}
	got, err := RenderShell("{{check}}; cat {{plan.output}} {{plan.final}} {{dig.outputs}} > {{work}}/x # {{chain}} {{plan.verdict}}", r)
	want := `go test ./... && go vet ./...; cat 'it'\''s; rm -rf ~.md' .routr/runs/1/final.md 'a b.md' c.md '$(x).md' > '/repo dir/.routr/work/fix-1'/x # fix-1 fail`
	if err != nil || got != want {
		t.Errorf("got  %s\nwant %s (%v)", got, want, err)
	}
	if _, err := RenderShell("{{nope}}", Refs{}); err == nil {
		t.Error("an unknown ref rendered")
	}
	for in, want := range map[string]string{"": "''", "a=b": "'a=b'", "~x": "'~x'", "x-1.md": "x-1.md", "a\nb": "'a\nb'"} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	got := strings.Join(Placeholders("a {{x}} b {{ y.output }} {{x}}"), ",")
	if got != "x,y.output,x" {
		t.Errorf("got %s", got)
	}
}
