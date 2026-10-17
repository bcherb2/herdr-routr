package config

import (
	"regexp"
	"strings"
	"testing"
)

// Every setting in the example, uncommented, loads; and a value it calls the
// default is the one routr uses without it.
func TestExampleLoadsAndShowsTheDefaults(t *testing.T) {
	if !strings.Contains(Example, "#[main]\n") {
		t.Fatal("Example is not the commented template")
	}
	on := regexp.MustCompile(`(?m)^#([^ #\n])`).ReplaceAllString(Example, "$1")
	c, err := Load(write(t, t.TempDir(), "c.toml", on))
	if err != nil {
		t.Fatalf("the uncommented example does not load: %v\n%s", err, on)
	}
	def, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.MainAgent(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	dm, _ := def.MainAgent(noEnv)
	if m != dm {
		t.Errorf("[main] in the example %+v; the defaults %+v", m, dm)
	}
	if c.Registry.KeepDays != 14 || c.Registry.Max != 200 {
		t.Errorf("[registry] in the example %+v; the defaults are 14 days and 200 workers", c.Registry)
	}
	q, dq := c.Questions, def.Questions
	if q.Answer != dq.Answer || q.Fallback != dq.Fallback || q.MinConfidence != dq.MinConfidence {
		t.Errorf("[questions] in the example %+v; the defaults %+v", q, dq)
	}
	if c.StartMCPServers != def.StartMCPServers {
		t.Errorf("start_mcp_servers in the example: %v", c.StartMCPServers)
	}
	if len(c.Fallback) != 2 {
		t.Errorf("the example's [[fallback]] rules: %+v", c.Fallback)
	}
	// commented, it is no config at all
	c, err = Load(write(t, t.TempDir(), "c.toml", Example))
	if err != nil || len(c.Fallback) != 0 || c.Env["CLAUDE_CONFIG_DIR"] != "" || c.Permissions != "" {
		t.Errorf("the commented example sets something: %+v, %v", c, err)
	}
}
