// Package config loads routr's configuration and resolves roles.
//
// Three layers merge key by key, later ones winning: the built-in roles, the
// user config ($XDG_CONFIG_HOME/routr/config.toml), and the repo's
// .routr/config.toml. A role names what a worker is for (review, research
// …); Resolve turns a role plus `routr start` flags into a harness, model,
// effort, permission mode and who answers its questions, checking that the
// harness is installed and logged in. See skills/routr/cli.md, "Roles and config".
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/bcherb2/herdr-routr/internal/registry"
)

//go:embed builtin.toml
var builtin string

// Example is a user config with every key commented out and explained:
// what `routr config init` writes and `routr config example` prints.
//
//go:embed example.toml
var Example string

// Config is the merged configuration.
type Config struct {
	Permissions string             `toml:"permissions"` // default for workers: default, auto or bypass; "" = unset
	Env         map[string]string  `toml:"env"`         // exported in every worker tab (and to routr's own checks)
	Harness     map[string]Harness `toml:"harness"`
	Roles       map[string]Role    `toml:"roles"`
	Registry    Registry           `toml:"registry"`
	Questions   Questions          `toml:"questions"`
	Main        Main               `toml:"main"`
	Fallback    []Fallback         `toml:"fallback"` // [[fallback]] rules, first match wins; a file that sets any replaces the rules before it

	// StartMCPServers lets workers start the user's MCP servers and
	// claude.ai connectors; false (built in) starts them with none.
	StartMCPServers bool `toml:"start_mcp_servers"`
}

// Questions is who answers a worker's question (a permission prompt, a
// menu), and what agent and jev decide with. builtin.toml sets the defaults:
// human, human, 0.8.
type Questions struct {
	Answer        string   `toml:"answer"`         // human, agent or jev (registry.Answerers); --answer and a flow step's answer win
	Fallback      string   `toml:"fallback"`       // who answers when jev cannot decide: human or agent
	Context       []string `toml:"context"`        // project docs for agent and jev, relative to the repo root; unset = the first of .design/SPEC.md, SPEC.md, README.md
	MinConfidence float64  `toml:"min_confidence"` // jev answers at or above it; above 0, at most 1
}

// Registry is how much the registry keeps of finished workers.
type Registry struct {
	KeepDays int `toml:"keep_days"` // drop them this many days after they last changed; 0 = 14
	Max      int `toml:"max"`       // keep at most this many workers, finished ones going first; 0 = 200
}

// Harness is per-harness defaults.
type Harness struct {
	Model  string   `toml:"model"`
	Effort string   `toml:"effort"`
	Args   []string `toml:"args"` // extra harness argv
}

// Role is what a kind of worker gets by default.
type Role struct {
	Prefer      []string          `toml:"prefer"` // harnesses in order; the first ready and not avoided wins
	Avoid       string            `toml:"avoid"`  // "caller", or a harness name
	Models      map[string]string `toml:"models"` // harness → model for this role
	Effort      string            `toml:"effort"`
	Efforts     map[string]string `toml:"efforts"` // harness → effort for this role, over Effort
	Write       *bool             `toml:"write"`   // takes the directory lock; unset = true
	Verdict     bool              `toml:"verdict"`
	Interactive bool              `toml:"interactive"`
	Gate        string            `toml:"gate"`
	Output      string            `toml:"output"`
	Preamble    string            `toml:"preamble"`
}

// Writes reports whether the role edits the tree (and so takes the lock).
func (r Role) Writes() bool { return r.Write == nil || *r.Write }

// UserPath is $XDG_CONFIG_HOME/routr/config.toml (~/.config/routr/…).
func UserPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "routr", "config.toml")
}

// Load merges the built-in roles, then each existing file in paths (user
// first, then repo). A missing file is skipped; an unknown key is an error
// that names the file and key.
func Load(paths ...string) (Config, error) {
	merged, err := layer("built-in roles", builtin)
	if err != nil {
		return Config{}, err
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Config{}, err
		}
		m, err := layer(p, string(b))
		if err != nil {
			return Config{}, err
		}
		merge(merged, m)
	}
	var c Config
	if err := remarshal(merged, &c); err != nil {
		return Config{}, err
	}
	c.Env = expandEnv(c.Env)
	return c, c.validate()
}

// layer parses one file, checking its keys against Config.
func layer(name, text string) (map[string]any, error) {
	var c Config
	md, err := toml.Decode(text, &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if bad := md.Undecoded(); len(bad) > 0 {
		keys := make([]string, len(bad))
		for i, k := range bad {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", name, strings.Join(keys, ", "))
	}
	m := map[string]any{}
	_, err = toml.Decode(text, &m)
	return m, err
}

// merge lays src over dst: tables merge key by key, anything else replaces.
func merge(dst, src map[string]any) {
	for k, v := range src {
		sub, isTable := v.(map[string]any)
		have, hasTable := dst[k].(map[string]any)
		if isTable && hasTable {
			merge(have, sub)
			continue
		}
		dst[k] = v
	}
}

func remarshal(m map[string]any, out *Config) error {
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(m); err != nil {
		return err
	}
	_, err := toml.Decode(sb.String(), out)
	return err
}

// expandEnv expands a leading ~/ and $VAR in [env] values.
func expandEnv(env map[string]string) map[string]string {
	out := ExpandHome(env)
	for k, v := range out {
		out[k] = os.ExpandEnv(v)
	}
	return out
}

// ExpandHome is env with a leading ~/ in its values expanded to the home
// directory: what the shell does not do in --env K=~/dir.
func ExpandHome(env map[string]string) map[string]string {
	home, _ := os.UserHomeDir()
	out := make(map[string]string, len(env))
	for k, v := range env {
		if rest, ok := strings.CutPrefix(v, "~/"); ok && home != "" {
			v = filepath.Join(home, rest)
		}
		out[k] = v
	}
	return out
}

func (c Config) validate() error {
	var errs []error
	if c.Registry.KeepDays < 0 {
		errs = append(errs, fmt.Errorf("registry.keep_days = %d: use a number of days, or 0 for the default", c.Registry.KeepDays))
	}
	if c.Registry.Max < 0 {
		errs = append(errs, fmt.Errorf("registry.max = %d: use a number of workers, or 0 for the default", c.Registry.Max))
	}
	if c.Permissions != "" && !slices.Contains(Permissions, c.Permissions) {
		errs = append(errs, fmt.Errorf("permissions = %q: use one of %s", c.Permissions, strings.Join(Permissions, ", ")))
	}
	errs = append(errs, c.Questions.validate()...)
	errs = append(errs, c.Main.validate()...)
	for i, f := range c.Fallback {
		errs = append(errs, f.validate(i+1)...)
	}
	for h, hc := range c.Harness {
		if err := checkEffort(h, hc.Effort); err != nil {
			errs = append(errs, fmt.Errorf("harness.%s.effort: %w", h, err))
		}
	}
	for name, r := range c.Roles {
		for _, h := range r.Prefer {
			if !slices.Contains(Harnesses, h) {
				errs = append(errs, fmt.Errorf("roles.%s.prefer: unknown harness %q (known: %s)", name, h, strings.Join(Harnesses, ", ")))
			}
		}
		if r.Avoid != "" && r.Avoid != "caller" && !slices.Contains(Harnesses, r.Avoid) {
			errs = append(errs, fmt.Errorf("roles.%s.avoid = %q: use \"caller\" or a harness", name, r.Avoid))
		}
		if r.Effort != "" && !slices.Contains(Efforts, r.Effort) {
			errs = append(errs, fmt.Errorf("roles.%s.effort = %q: use one of %s", name, r.Effort, strings.Join(Efforts, ", ")))
		}
		for h, e := range r.Efforts {
			if err := checkEffort(h, e); err != nil {
				errs = append(errs, fmt.Errorf("roles.%s.efforts.%s: %w", name, h, err))
			}
		}
		if r.Gate != "" && r.Gate != "you" {
			errs = append(errs, fmt.Errorf("roles.%s.gate = %q: the only gate is \"you\"", name, r.Gate))
		}
	}
	return errors.Join(errs...)
}

func (q Questions) validate() []error {
	var errs []error
	if !slices.Contains(registry.Answerers, q.Answer) {
		errs = append(errs, fmt.Errorf("questions.answer = %q: use one of %s", q.Answer, strings.Join(registry.Answerers, ", ")))
	}
	if !slices.Contains(Fallbacks, q.Fallback) {
		errs = append(errs, fmt.Errorf("questions.fallback = %q: use one of %s", q.Fallback, strings.Join(Fallbacks, ", ")))
	}
	if q.MinConfidence <= 0 || q.MinConfidence > 1 {
		errs = append(errs, fmt.Errorf("questions.min_confidence = %g: use a number above 0 and at most 1", q.MinConfidence))
	}
	for _, p := range q.Context {
		if !filepath.IsLocal(p) {
			errs = append(errs, fmt.Errorf("questions.context: %q: use a path relative to the repo root, inside it", p))
		}
	}
	return errs
}

// RoleNames lists the roles, sorted.
func (c Config) RoleNames() []string { return slices.Sorted(maps.Keys(c.Roles)) }

// Known values.
var (
	Harnesses = []string{"claude", "codex", "pi"}
	// HarnessEfforts are the levels each harness's effort flag takes:
	// claude --effort, codex model_reasoning_effort, pi --thinking.
	HarnessEfforts = map[string][]string{
		"claude": {"low", "medium", "high", "xhigh", "max"},
		"codex":  {"low", "medium", "high"},
		"pi":     {"off", "minimal", "low", "medium", "high", "xhigh", "max"},
	}
	// Efforts is every level some harness takes, lowest first.
	Efforts     = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	Permissions = []string{"default", "auto", "bypass"}
	// Fallbacks are who may answer when jev cannot decide.
	Fallbacks = []string{"human", "agent"}
)

// checkEffort reports an effort level harness h doesn't take; "" (the
// harness default) and an unknown harness pass.
func checkEffort(h, effort string) error {
	levels, ok := HarnessEfforts[h]
	if effort == "" || !ok || slices.Contains(levels, effort) {
		return nil
	}
	return fmt.Errorf("%s doesn't take effort %q: use one of %s", h, effort, strings.Join(levels, ", "))
}
