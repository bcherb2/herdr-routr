package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Fallback is one [[fallback]] rule: when a model it is for fails in a way
// waiting won't fix (credit, quota, login), the work moves to the first
// model of To that can take it, then the next. The first rule whose From
// matches wins; a model no rule matches stops and tells the user. Shared by
// the main agent (which can take only its own harness's models) and
// workers.
type Fallback struct {
	From []string `toml:"from"` // globs on harness:model: * any run of characters, / and : too; ? one; case ignored
	To   []string `toml:"to"`   // harness:model, in order
}

// FallbackFor is the first rule for model (harness:model) and its 1-based
// number; 0 when none matches.
func (c Config) FallbackFor(model string) (int, Fallback) {
	for i, f := range c.Fallback {
		if slices.ContainsFunc(f.From, func(p string) bool { return glob(p, model) }) {
			return i + 1, f
		}
	}
	return 0, Fallback{}
}

// ShadowedFallbacks are the 1-based numbers of rules that never match: an
// earlier rule has "*" or each of their globs.
func (c Config) ShadowedFallbacks() []int {
	var out []int
	for i, f := range c.Fallback {
		covered := func(p string) bool {
			for _, e := range c.Fallback[:i] {
				if slices.ContainsFunc(e.From, func(q string) bool { return q == "*" || strings.EqualFold(q, p) }) {
					return true
				}
			}
			return false
		}
		if len(f.From) > 0 && !slices.ContainsFunc(f.From, func(p string) bool { return !covered(p) }) {
			out = append(out, i+1)
		}
	}
	return out
}

func (f Fallback) validate(n int) []error {
	var errs []error
	if len(f.From) == 0 {
		errs = append(errs, fmt.Errorf("fallback %d: from is empty: name the models it is for, as harness:model globs", n))
	}
	for _, p := range f.From {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, fmt.Errorf("fallback %d: from %q: an empty glob", n, p))
		}
	}
	if len(f.To) == 0 {
		errs = append(errs, fmt.Errorf("fallback %d: to is empty: name the models to move to, as harness:model", n))
	}
	for i, m := range f.To {
		h, model, ok := strings.Cut(m, ":")
		if !ok || !slices.Contains(Harnesses, h) || strings.TrimSpace(model) == "" {
			errs = append(errs, fmt.Errorf("fallback %d: to %q: use harness:model (harnesses: %s)", n, m, strings.Join(Harnesses, ", ")))
		} else if slices.Contains(f.To[:i], m) {
			errs = append(errs, fmt.Errorf("fallback %d: to %q twice", n, m))
		}
	}
	return errs
}

// glob matches s whole against pattern: * is any run of characters, ? one,
// the rest literal; case is ignored.
func glob(pattern, s string) bool {
	var re strings.Builder
	re.WriteString("(?is)^")
	for _, r := range pattern {
		switch r {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(s)
}
