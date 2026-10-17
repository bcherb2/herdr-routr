package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcherb2/herdr-routr/internal/config"
)

func TestConfigInitWritesTheExampleOnce(t *testing.T) {
	c := newCLI(t)
	user := filepath.Join(c.env["XDG_CONFIG_HOME"], "routr", "config.toml")
	if out := c.mustRun("", "config", "init"); out != "routr: wrote "+user+": every key commented out; uncomment one to change it\n" {
		t.Errorf("config init: %q", out)
	}
	b, err := os.ReadFile(user)
	if err != nil || string(b) != config.Example {
		t.Fatalf("%s: %v\n%s", user, err, b)
	}
	if fi, _ := os.Stat(user); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	c.mustRun("", "doctor") // the commented file is a valid config

	// never over a file that exists, edited or not
	if err := os.WriteFile(user, []byte("permissions = \"auto\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Path    string `json:"path"`
		Created bool   `json:"created"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("", "config", "init", "--json")), &got); err != nil || got.Path != user || got.Created {
		t.Errorf("config init --json = %+v, %v", got, err)
	}
	if b, _ := os.ReadFile(user); string(b) != "permissions = \"auto\"\n" {
		t.Errorf("init overwrote %s: %q", user, b)
	}
	if out := c.mustRun("", "config", "init"); !strings.Contains(out, user+" exists; left alone") {
		t.Errorf("config init: %q", out)
	}
}

func TestConfigListsItsFiles(t *testing.T) {
	c := newCLI(t)
	user := filepath.Join(c.env["XDG_CONFIG_HOME"], "routr", "config.toml")
	out := c.mustRun("", "config")
	for _, want := range []string{"user  " + user + "  (none; routr config init writes a commented one)\n", "repo  "} {
		if !strings.Contains(out, want) {
			t.Errorf("config lacks %q:\n%s", want, out)
		}
	}
	c.mustRun("", "config", "init")
	var got struct {
		User struct {
			Path   string `json:"path"`
			Exists bool   `json:"exists"`
		} `json:"user"`
		Repo struct {
			Path   string `json:"path"`
			Exists bool   `json:"exists"`
		} `json:"repo"`
	}
	if err := json.Unmarshal([]byte(c.mustRun("", "config", "--json")), &got); err != nil || got.User.Path != user || !got.User.Exists || got.Repo.Exists || !strings.HasSuffix(got.Repo.Path, filepath.Join(".routr", "config.toml")) {
		t.Errorf("config --json = %+v, %v", got, err)
	}
	if out := c.mustRun("", "config", "example"); out != config.Example {
		t.Errorf("config example printed something else:\n%s", out)
	}
	if code, _, errOut := c.run("", "config", "edit"); code != exitUsage || !strings.Contains(errOut, "init, example") {
		t.Errorf("config edit: exit %d: %s", code, errOut)
	}
}
