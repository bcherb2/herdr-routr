package config

import (
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestMainAgentDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.MainAgent(noEnv)
	want := MainAgent{Context: "truncate", At: 150_000, KeepTurns: 3, KeepTokens: 40_000, StripReasoning: true}
	if err != nil || got != want {
		t.Fatalf("MainAgent() = %+v, %v; want %+v", got, err, want)
	}
}

func TestMainAgentFromFiles(t *testing.T) {
	dir := t.TempDir()
	user := write(t, dir, "user.toml", "[main]\ncontext = \"summarize\"\nat = 200000\nkeep_turns = -1\nstrip_reasoning = false\n")
	repo := write(t, dir, "repo.toml", "[main]\nkeep_tokens = 60000\n")
	c, err := Load(user, repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.MainAgent(noEnv)
	want := MainAgent{Context: "summarize", At: 200_000, KeepTurns: Unlimited, KeepTokens: 60_000, StripReasoning: false}
	if err != nil || got != want {
		t.Fatalf("MainAgent() = %+v, %v; want %+v", got, err, want)
	}
}

func TestMainAgentZeroIsTheDefault(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", "[main]\nat = 0\nkeep_turns = 0\nkeep_tokens = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.MainAgent(noEnv); got.At != 150_000 || got.KeepTurns != 3 || got.KeepTokens != 40_000 {
		t.Fatalf("MainAgent() = %+v", got)
	}
}

func TestMainAgentEnvOverrides(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ROUTR_MAIN_CONTEXT":         "off",
		"ROUTR_MAIN_AT":              "-1",
		"ROUTR_MAIN_KEEP_TURNS":      "5",
		"ROUTR_MAIN_KEEP_TOKENS":     "-1",
		"ROUTR_MAIN_STRIP_REASONING": "0",
	}
	got, err := c.MainAgent(func(k string) string { return env[k] })
	want := MainAgent{Context: "off", At: Unlimited, KeepTurns: 5, KeepTokens: Unlimited, StripReasoning: false}
	if err != nil || got != want {
		t.Fatalf("MainAgent() = %+v, %v; want %+v", got, err, want)
	}
	for _, v := range []string{"1", "true", "on"} {
		got, err := c.MainAgent(func(k string) string { return map[string]string{"ROUTR_MAIN_STRIP_REASONING": v}[k] })
		if err != nil || !got.StripReasoning {
			t.Errorf("ROUTR_MAIN_STRIP_REASONING=%s: %+v, %v", v, got, err)
		}
	}
}

func TestMainAgentRejects(t *testing.T) {
	// A value routr can't take fails Load, as in any section.
	for _, k := range []struct{ file, want string }{
		{"[main]\ncontext = \"compact\"\n", `main.context = "compact": use one of truncate, summarize, off`},
		{"[main]\nat = -2\n", "main.at = -2: use a number of tokens, 0 for the default or -1 for no limit"},
		{"[main]\nkeep_turns = -5\n", "main.keep_turns = -5: use a number of turns, 0 for the default or -1 for no limit"},
		{"[main]\nkeep_tokens = -3\n", "main.keep_tokens = -3: use a number of tokens, 0 for the default or -1 for no limit"},
	} {
		_, err := Load(write(t, t.TempDir(), "c.toml", k.file))
		if err == nil || !strings.Contains(err.Error(), k.want) {
			t.Errorf("%q: err = %v; want %q", k.file, err, k.want)
		}
	}
	// Keys that don't fit together load (nothing else reads them), and MainAgent rejects them.
	for _, k := range []struct{ file, want string }{
		{"[main]\nkeep_turns = -1\nkeep_tokens = -1\n", "main.keep_turns and main.keep_tokens are both -1: a truncation would keep everything"},
		{"[main]\nat = 30000\n", "main.keep_tokens (40000) is not below main.at (30000): the context would be over the limit again straight after each cut"},
		{"[main]\nat = 50000\nkeep_tokens = 50000\n", "main.keep_tokens (50000) is not below main.at (50000)"},
	} {
		c, err := Load(write(t, t.TempDir(), "c.toml", k.file))
		if err != nil {
			t.Fatalf("%q: Load: %v", k.file, err)
		}
		if _, err := c.MainAgent(noEnv); err == nil || !strings.Contains(err.Error(), k.want) {
			t.Errorf("%q: err = %v; want %q", k.file, err, k.want)
		}
	}
}

// An override can make keys fit for one run: the documented use.
func TestMainAgentEnvRepairsAFile(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", "[main]\nat = 30000\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []map[string]string{{"ROUTR_MAIN_KEEP_TOKENS": "10000"}, {"ROUTR_MAIN_AT": "-1"}, {"ROUTR_MAIN_CONTEXT": "off"}} {
		if _, err := c.MainAgent(func(k string) string { return env[k] }); err != nil {
			t.Errorf("%v: %v", env, err)
		}
	}
}

// The repo's config wins per key over the user's, a *bool included.
func TestMainAgentRepoWinsPerKey(t *testing.T) {
	for _, k := range []struct {
		user, repo string
		strip      bool
	}{{"true", "false", false}, {"false", "true", true}} {
		dir := t.TempDir()
		c, err := Load(write(t, dir, "user.toml", "[main]\nat = 90000\nstrip_reasoning = "+k.user+"\n"), write(t, dir, "repo.toml", "[main]\nat = 120000\nstrip_reasoning = "+k.repo+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := c.MainAgent(noEnv); err != nil || got.At != 120_000 || got.StripReasoning != k.strip {
			t.Errorf("user %s, repo %s: %+v, %v", k.user, k.repo, got, err)
		}
	}
}

func TestMainAgentOffChecksNoLimits(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", "[main]\ncontext = \"off\"\nat = 30000\nkeep_turns = -1\nkeep_tokens = -1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.MainAgent(noEnv); err != nil || got.Context != "off" {
		t.Fatalf("MainAgent() = %+v, %v", got, err)
	}
}

func TestMainAgentRejectsBadEnv(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct{ name, value, want string }{
		{"ROUTR_MAIN_AT", "150k", "ROUTR_MAIN_AT = \"150k\": use a whole number"},
		{"ROUTR_MAIN_STRIP_REASONING", "maybe", "ROUTR_MAIN_STRIP_REASONING = \"maybe\": use 1 or 0"},
		{"ROUTR_MAIN_CONTEXT", "compact", `main.context = "compact"`},
		{"ROUTR_MAIN_KEEP_TOKENS", "200000", "main.keep_tokens (200000) is not below main.at (150000)"},
	} {
		_, err := c.MainAgent(func(n string) string { return map[string]string{k.name: k.value}[n] })
		if err == nil || !strings.Contains(err.Error(), k.want) {
			t.Errorf("%s=%s: err = %v; want %q", k.name, k.value, err, k.want)
		}
	}
}
