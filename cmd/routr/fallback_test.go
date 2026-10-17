package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const fallbackRules = `
[[fallback]]
from = ["*deepseek-v4-pro"]
to = ["pi:openrouter/deepseek/deepseek-v4.1-flash", "pi:openrouter/anthropic/claude-haiku-5.5"]
`

const balance = `402: {"message":"Insufficient Balance (request_id: 88fc6ff6)","type":"unknown_error"}`

func TestFallbackJSON(t *testing.T) {
	c := newCLI(t)
	c.userTOML(fallbackRules)
	var got fallbackOut
	if err := json.Unmarshal([]byte(c.mustRun("", "fallback", "pi:deepseek/deepseek-v4-pro", "--error", balance, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "pi:deepseek/deepseek-v4-pro" || got.Rule != 1 || len(got.To) != 2 || got.To[1] != "pi:openrouter/anthropic/claude-haiku-5.5" || got.Lasting == nil || !*got.Lasting {
		t.Errorf("fallback --json = %+v", got)
	}
	// an error that may pass (Pi and the watcher retry it): no fallback
	_ = json.Unmarshal([]byte(c.mustRun("", "fallback", "pi:deepseek/deepseek-v4-pro", "--error", "Overloaded", "--json")), &got)
	if got.Lasting == nil || *got.Lasting {
		t.Errorf("Overloaded: lasting = %v", got.Lasting)
	}
	// no rule: an empty list, and no error, so a caller tells "no rule" from "routr failed"
	got = fallbackOut{}
	if err := json.Unmarshal([]byte(c.mustRun("", "fallback", "claude:opus", "--json")), &got); err != nil || got.Rule != 0 || got.To == nil || len(got.To) != 0 || got.Lasting != nil {
		t.Errorf("no rule: %+v, %v", got, err)
	}
}

func TestFallbackText(t *testing.T) {
	c := newCLI(t)
	c.userTOML(fallbackRules)
	out := c.mustRun("", "fallback", "pi:deepseek/deepseek-v4-pro", "--error", balance)
	for _, want := range []string{
		"error: waiting won't fix it; move to the next model\n",
		"pi:deepseek/deepseek-v4-pro: rule 1 (from *deepseek-v4-pro)\n",
		"  1  pi:openrouter/deepseek/deepseek-v4.1-flash\n",
		"  2  pi:openrouter/anthropic/claude-haiku-5.5\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback lacks %q:\n%s", want, out)
		}
	}
	out = c.mustRun("", "fallback", "claude:opus", "--error", "Request timed out.")
	for _, want := range []string{"error: it may pass; it is retried, not moved\n", "claude:opus: no [[fallback]] rule matches; a failure that waiting won't fix stops it\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback lacks %q:\n%s", want, out)
		}
	}
	if code, _, errOut := c.run("", "fallback"); code != exitUsage || !strings.Contains(errOut, "harness:model") {
		t.Errorf("no model: exit %d: %s", code, errOut)
	}
	if code, _, errOut := c.run("", "fallback", "opus"); code != exitUsage || !strings.Contains(errOut, `"opus": use harness:model`) {
		t.Errorf("bare model: exit %d: %s", code, errOut)
	}
}

func TestDoctorReportsFallback(t *testing.T) {
	c := newCLI(t)
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, "OK fallback no [[fallback]] rules: a model that runs out of credit or quota stops") {
		t.Errorf("doctor:\n%s", out)
	}
	c.userTOML(fallbackRules + "[[fallback]]\nfrom = [\"*\"]\nto = [\"pi:x/y\"]\n[[fallback]]\nfrom = [\"claude:*\"]\nto = [\"pi:x/y\"]\n")
	_, out, _ := c.run("", "doctor")
	if want := "WARN fallback 3 rules; rule 3 never matches: an earlier rule catches all it would → reorder [[fallback]] in"; !strings.Contains(squeeze(out), want) {
		t.Errorf("doctor lacks %q:\n%s", want, out)
	}
}
