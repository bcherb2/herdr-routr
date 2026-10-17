package config

import (
	"strings"
	"testing"
)

const rules = `
[[fallback]]
from = ["*glm-5.3-flash", "*deepseek-v4-flash"]
to = ["pi:openrouter/deepseek/deepseek-v4.1-flash", "pi:openrouter/anthropic/claude-haiku-5.5"]

[[fallback]]
from = ["*opus*"]
to = ["codex:gpt-6.1", "pi:openrouter/moonshotai/kimi-3"]
`

func TestFallbackFor(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", rules))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		model string
		rule  int
	}{
		{"pi:zai/glm-5.3-flash", 1},
		{"pi:deepseek/deepseek-v4-flash", 1},
		{"claude:opus", 2},
		{"pi:openrouter/anthropic/claude-OPUS-5.5", 2}, // * spans / and :, and case is ignored
		{"pi:deepseek/deepseek-v4-pro", 0},             // no rule: it fails
		{"pi:zai/glm-5.3-flash-x", 0},                  // a glob matches the whole name
	} {
		n, r := c.FallbackFor(k.model)
		if n != k.rule {
			t.Errorf("FallbackFor(%q) = rule %d; want %d", k.model, n, k.rule)
		}
		if n > 0 && r.To[0] != c.Fallback[n-1].To[0] {
			t.Errorf("FallbackFor(%q) returned another rule's list: %v", k.model, r.To)
		}
	}
	c, _ = Load()
	if n, _ := c.FallbackFor("claude:opus"); n != 0 {
		t.Errorf("no [[fallback]]: rule %d; want none", n)
	}
}

func TestFallbackFirstRuleWins(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", rules+"\n[[fallback]]\nfrom = [\"*\"]\nto = [\"pi:openrouter/anthropic/claude-haiku-5.5\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := c.FallbackFor("claude:opus"); n != 2 {
		t.Errorf("claude:opus: rule %d; want 2, before the catch-all", n)
	}
	if n, _ := c.FallbackFor("codex:gpt-6.1"); n != 3 {
		t.Errorf("codex:gpt-6.1: rule %d; want the catch-all, 3", n)
	}
}

// A file that sets [[fallback]] replaces the rules of the one before, as any list does.
func TestFallbackRepoReplacesUser(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "user.toml", rules), write(t, dir, "repo.toml", "[[fallback]]\nfrom = [\"claude:*\"]\nto = [\"codex:gpt-6.1\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Fallback) != 1 || c.Fallback[0].From[0] != "claude:*" {
		t.Errorf("Fallback = %+v", c.Fallback)
	}
}

func TestFallbackRejects(t *testing.T) {
	for _, k := range []struct{ file, want string }{
		{"[[fallback]]\nto = [\"pi:x/y\"]\n", "fallback 1: from is empty: name the models it is for, as harness:model globs"},
		{"[[fallback]]\nfrom = [\"*opus*\", \"\"]\nto = [\"pi:x/y\"]\n", `fallback 1: from "": an empty glob`},
		{"[[fallback]]\nfrom = [\"*opus*\"]\n", "fallback 1: to is empty: name the models to move to, as harness:model"},
		{"[[fallback]]\nfrom = [\"*\"]\nto = [\"gpt-6.1\"]\n", `fallback 1: to "gpt-6.1": use harness:model (harnesses: claude, codex, pi)`},
		{"[[fallback]]\nfrom = [\"*\"]\nto = [\"gemini:pro\"]\n", `fallback 1: to "gemini:pro": use harness:model (harnesses: claude, codex, pi)`},
		{"[[fallback]]\nfrom = [\"*\"]\nto = [\"pi:\"]\n", `fallback 1: to "pi:": use harness:model`},
		{"[[fallback]]\nfrom = [\"a\"]\nto = [\"pi:x\"]\n[[fallback]]\nfrom = [\"b\"]\nto = [\"pi:x\", \"pi:x\"]\n", `fallback 2: to "pi:x" twice`},
	} {
		_, err := Load(write(t, t.TempDir(), "c.toml", k.file))
		if err == nil || !strings.Contains(err.Error(), k.want) {
			t.Errorf("%q: err = %v; want %q", k.file, err, k.want)
		}
	}
}

func TestFallbackShadowed(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", `
[[fallback]]
from = ["*opus*"]
to = ["codex:gpt-6.1"]
[[fallback]]
from = ["*OPUS*"]
to = ["pi:x/y"]
[[fallback]]
from = ["*"]
to = ["pi:x/y"]
[[fallback]]
from = ["claude:*"]
to = ["pi:x/y"]
`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.ShadowedFallbacks()
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("ShadowedFallbacks() = %v; want [2 4]", got)
	}
}

func TestGlob(t *testing.T) {
	for _, k := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "pi:a/b", true},
		{"*opus*", "claude:opus", true},
		{"claude:*", "claude:opus", true},
		{"claude:*", "pi:claude/x", false},
		{"pi:?/x", "pi:a/x", true},
		{"pi:?/x", "pi:ab/x", false},
		{"pi:a.b", "pi:axb", false}, // only * and ? are special
		{"PI:A", "pi:a", true},
	} {
		if got := glob(k.pattern, k.s); got != k.want {
			t.Errorf("glob(%q, %q) = %v", k.pattern, k.s, got)
		}
	}
}
