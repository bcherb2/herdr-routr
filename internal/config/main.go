package config

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Main is the [main] section: how the main agent, the session that starts
// workers, keeps its context small on a long run. A worker is never
// affected. 0 or unset is the default; -1 (Unlimited) is no limit.
type Main struct {
	Context        string `toml:"context"`         // truncate, summarize or off; "" = truncate
	At             int    `toml:"at"`              // compact once the context passes this many tokens after a turn; 0 = 150000
	KeepTurns      int    `toml:"keep_turns"`      // the last turns a compaction keeps as they are; 0 = 3
	KeepTokens     int    `toml:"keep_tokens"`     // at most this many tokens of them; 0 = 40000
	StripReasoning *bool  `toml:"strip_reasoning"` // drop earlier turns' reasoning from each request; unset = true
}

// MainAgent is the [main] section as a front end applies it: defaults
// filled in, ROUTR_MAIN_* applied, checked. `routr main --json` prints it.
type MainAgent struct {
	Context        string `json:"context"`
	At             int    `json:"at"`
	KeepTurns      int    `json:"keep_turns"`
	KeepTokens     int    `json:"keep_tokens"`
	StripReasoning bool   `json:"strip_reasoning"`
}

// Unlimited is the -1 that turns off a [main] limit.
const Unlimited = -1

// Contexts are what [main] context takes.
var Contexts = []string{"truncate", "summarize", "off"}

// MainAgent is c's [main] section with getenv's ROUTR_MAIN_* over it, each
// of which overrides its key for one run.
func (c Config) MainAgent(getenv func(string) string) (MainAgent, error) {
	m := c.Main
	var errs []error
	if v := getenv("ROUTR_MAIN_CONTEXT"); v != "" {
		m.Context = v
	}
	for _, k := range []struct {
		env string
		to  *int
	}{{"ROUTR_MAIN_AT", &m.At}, {"ROUTR_MAIN_KEEP_TURNS", &m.KeepTurns}, {"ROUTR_MAIN_KEEP_TOKENS", &m.KeepTokens}} {
		v := getenv(k.env)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s = %q: use a whole number", k.env, v))
			continue
		}
		*k.to = n
	}
	switch v := getenv("ROUTR_MAIN_STRIP_REASONING"); strings.ToLower(v) {
	case "":
	case "1", "true", "on":
		m.StripReasoning = new(true)
	case "0", "false", "off":
		m.StripReasoning = new(false)
	default:
		errs = append(errs, fmt.Errorf("ROUTR_MAIN_STRIP_REASONING = %q: use 1 or 0", v))
	}
	errs = append(errs, m.validate()...)
	if len(errs) == 0 {
		errs = m.combine()
	}
	if err := errors.Join(errs...); err != nil {
		return MainAgent{}, err
	}
	return m.resolve(), nil
}

func (m Main) resolve() MainAgent {
	return MainAgent{
		Context:        cmp.Or(m.Context, "truncate"),
		At:             cmp.Or(m.At, 150_000),
		KeepTurns:      cmp.Or(m.KeepTurns, 3),
		KeepTokens:     cmp.Or(m.KeepTokens, 40_000),
		StripReasoning: m.StripReasoning == nil || *m.StripReasoning,
	}
}

// validate checks each key alone: a value routr can't take is a config
// error for every command, as in any other section.
func (m Main) validate() []error {
	var errs []error
	if m.Context != "" && !slices.Contains(Contexts, m.Context) {
		errs = append(errs, fmt.Errorf("main.context = %q: use one of %s", m.Context, strings.Join(Contexts, ", ")))
	}
	for _, k := range []struct {
		key, unit string
		v         int
	}{{"at", "tokens", m.At}, {"keep_turns", "turns", m.KeepTurns}, {"keep_tokens", "tokens", m.KeepTokens}} {
		if k.v < Unlimited {
			errs = append(errs, fmt.Errorf("main.%s = %d: use a number of %s, 0 for the default or -1 for no limit", k.key, k.v, k.unit))
		}
	}
	return errs
}

// combine checks the keys together, after ROUTR_MAIN_* applied: only
// MainAgent's readers (routr main, doctor, a front end) need them to fit,
// and an override can make them fit for a run.
func (m Main) combine() []error {
	var errs []error
	r := m.resolve()
	if r.Context == "off" {
		return nil
	}
	if r.KeepTurns == Unlimited && r.KeepTokens == Unlimited {
		errs = append(errs, errors.New("main.keep_turns and main.keep_tokens are both -1: a truncation would keep everything"))
	}
	if r.At != Unlimited && r.KeepTokens != Unlimited && r.KeepTokens >= r.At {
		errs = append(errs, fmt.Errorf("main.keep_tokens (%d) is not below main.at (%d): the context would be over the limit again straight after each cut", r.KeepTokens, r.At))
	}
	return errs
}
