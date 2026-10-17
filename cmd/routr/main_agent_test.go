package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/config"
)

// userTOML writes the test's user config.
func (c *cli) userTOML(text string) {
	c.t.Helper()
	p := filepath.Join(c.env["XDG_CONFIG_HOME"], "routr", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func TestMainPrintsTheMainAgentSettings(t *testing.T) {
	c := newCLI(t)
	var got config.MainAgent
	if err := json.Unmarshal([]byte(c.mustRun("", "main", "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if want := (config.MainAgent{Context: "truncate", At: 150_000, KeepTurns: 3, KeepTokens: 40_000, StripReasoning: true}); got != want {
		t.Errorf("main --json = %+v; want %+v", got, want)
	}

	c.userTOML("[main]\ncontext = \"summarize\"\nkeep_turns = -1\n")
	c.env["ROUTR_MAIN_STRIP_REASONING"] = "0"
	out := c.mustRun("", "main")
	for _, want := range []string{"context          summarize\n", "at               150000 tokens\n", "keep_turns       no limit\n", "keep_tokens      40000 tokens\n", "strip_reasoning  off\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("main lacks %q:\n%s", want, out)
		}
	}
}

func TestMainReportsABadSetting(t *testing.T) {
	c := newCLI(t)
	c.env["ROUTR_MAIN_AT"] = "20000"
	code, _, errOut := c.run("", "main", "--json")
	if code != exitError || !strings.Contains(errOut, "main.keep_tokens (40000) is not below main.at (20000)") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// Outside a git repo, as for every command, the working directory's
// .routr/config.toml lays over the user config.
func TestMainOutsideARepo(t *testing.T) {
	c := newCLI(t)
	delete(c.env, "ROUTR_STATE_DIR") // c.dir is no git repo
	c.userTOML("[main]\nat = 90000\nkeep_turns = 5\n")
	if err := os.MkdirAll(filepath.Join(c.dir, ".routr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, ".routr", "config.toml"), []byte("[main]\nat = 77777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got config.MainAgent
	if err := json.Unmarshal([]byte(c.mustRun("", "main", "--json")), &got); err != nil || got.At != 77_777 || got.KeepTurns != 5 {
		t.Errorf("main --json = %+v, %v", got, err)
	}
}

// Keys that don't fit together leave other commands alone: only routr main
// and doctor's main line reject them, and an override fixes them for a run.
func TestMainKeysThatDontFitBlockNothingElse(t *testing.T) {
	c := newCLI(t)
	c.userTOML("[main]\nat = 30000\n")
	c.mustRun("", "roles")
	code, _, errOut := c.run("", "main")
	if code != exitError || !strings.Contains(errOut, "main.keep_tokens (40000) is not below main.at (30000)") {
		t.Errorf("main: exit %d: %s", code, errOut)
	}
	c.env["ROUTR_MAIN_KEEP_TOKENS"] = "10000"
	c.mustRun("", "main")
}

func TestDoctorReportsTheMainAgent(t *testing.T) {
	c := newCLI(t)
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	want := "OK main truncate at 150000 tokens, keeping the last 3 turns up to 40000 tokens; earlier turns' reasoning stripped"
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, want) {
		t.Errorf("doctor lacks %q:\n%s", want, out)
	}
	c.env["ROUTR_MAIN_CONTEXT"] = "summarize"
	want = "OK main summarize at 150000 tokens, by Pi's summary, which keeps what its compaction.keepRecentTokens says; earlier turns' reasoning stripped"
	if out := squeeze(c.mustRun("", "doctor")); !strings.Contains(out, want) {
		t.Errorf("doctor lacks %q:\n%s", want, out)
	}
	c.env["ROUTR_MAIN_CONTEXT"] = "compact"
	_, out, _ := c.run("", "doctor")
	if want := `FAIL main main.context = "compact": use one of truncate, summarize, off → fix [main] in`; !strings.Contains(squeeze(out), want) {
		t.Errorf("doctor lacks %q:\n%s", want, out)
	}
	// A file whose keys don't fit: config loads, and the main line says what to fix.
	delete(c.env, "ROUTR_MAIN_CONTEXT")
	c.userTOML("[main]\nat = 30000\n")
	_, out, _ = c.run("", "doctor")
	for _, want := range []string{"OK config loaded", "FAIL main main.keep_tokens (40000) is not below main.at (30000)"} {
		if !strings.Contains(squeeze(out), want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}
