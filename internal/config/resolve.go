package config

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/route"
)

// Request is what `routr start` knows: the role and any flags. Empty fields
// come from the role and config.
type Request struct {
	Role        string
	Harness     string
	Model       string
	Effort      string
	Permissions string
	Answer      string // --answer, else a flow step's answer; "" = config [questions] answer
	Caller      string // harness of the calling agent; "" = unknown
}

// Resolved is a role applied to a request.
type Resolved struct {
	Role        string
	Harness     string
	Model       string // "" = the harness default
	Effort      string // "" = the harness default
	Permissions string
	Answer      string // who answers its questions: human, agent or jev
	Args        []string
	Write       bool
	Verdict     bool
	Interactive bool
	Gate        string
	Output      string
	Preamble    string
}

// ReadyFunc reports why harness cannot run workers now, or nil.
type ReadyFunc func(ctx context.Context, harness string) error

// ErrAuto is returned for --harness auto / --model auto, which need Jev.
var ErrAuto = errors.New("--harness auto and --model auto need Jev routing, which is not built yet: name a harness, or leave it to the role")

// Resolve applies role req.Role. The harness is req.Harness if given, else
// the first of the role's prefer list (default: the caller's harness, then
// claude, codex, pi) that is not avoided and passes ready.
func (c Config) Resolve(ctx context.Context, req Request, ready ReadyFunc) (Resolved, error) {
	name := req.Role
	if name == "" {
		name = "task"
	}
	role, ok := c.Roles[name]
	if !ok {
		return Resolved{}, fmt.Errorf("unknown role %q; roles: %s (add one under [roles.%s] in config.toml)", name, strings.Join(c.RoleNames(), ", "), name)
	}
	if req.Harness == "auto" || req.Model == "auto" {
		return Resolved{}, ErrAuto
	}
	h, err := c.pickHarness(ctx, name, role, req, ready)
	if err != nil {
		return Resolved{}, err
	}
	hc := c.Harness[h]
	r := Resolved{
		Role:        name,
		Harness:     h,
		Model:       first(req.Model, role.Models[h], hc.Model),
		Effort:      first(req.Effort, role.Efforts[h], role.Effort, hc.Effort),
		Permissions: first(req.Permissions, c.PermissionsFor(req.Caller)),
		Answer:      first(req.Answer, c.Questions.Answer, "human"),
		Args:        slices.Clone(hc.Args),
		Write:       role.Writes(),
		Verdict:     role.Verdict,
		Interactive: role.Interactive,
		Gate:        role.Gate,
		Output:      role.Output,
		Preamble:    role.Preamble,
	}
	if err := checkEffort(h, r.Effort); err != nil {
		return Resolved{}, fmt.Errorf("effort: %w", err)
	}
	if !slices.Contains(Permissions, r.Permissions) {
		return Resolved{}, fmt.Errorf("--permissions %q: use one of %s", r.Permissions, strings.Join(Permissions, ", "))
	}
	if !slices.Contains(registry.Answerers, r.Answer) {
		return Resolved{}, fmt.Errorf("--answer %q: use one of %s", r.Answer, strings.Join(registry.Answerers, ", "))
	}
	return r, nil
}

func (c Config) pickHarness(ctx context.Context, name string, role Role, req Request, ready ReadyFunc) (string, error) {
	if req.Harness != "" {
		if !slices.Contains(Harnesses, req.Harness) {
			return "", fmt.Errorf("--harness %q: use one of %s", req.Harness, strings.Join(Harnesses, ", "))
		}
		if err := ready(ctx, req.Harness); err != nil {
			return "", fmt.Errorf("%s cannot run workers: %w", req.Harness, err)
		}
		return req.Harness, nil
	}
	var why []string
	for _, h := range Candidates(role, req.Caller) {
		if avoided(role, h, req.Caller) {
			why = append(why, h+": avoided (the caller's harness)")
			continue
		}
		err := ready(ctx, h)
		if err == nil {
			return h, nil
		}
		why = append(why, fmt.Sprintf("%s: %v", h, err))
	}
	return "", fmt.Errorf("no harness can take role %s (%s); install or log in to one, or pass --harness", name, strings.Join(why, "; "))
}

// Candidates is the order harnesses are tried for role: its prefer list, or
// the caller's harness then claude, codex, pi.
func Candidates(role Role, caller string) []string {
	if len(role.Prefer) > 0 {
		return role.Prefer
	}
	out := []string{}
	if caller != "" {
		out = append(out, caller)
	}
	for _, h := range Harnesses {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func avoided(role Role, h, caller string) bool {
	switch role.Avoid {
	case "":
		return false
	case "caller":
		return caller != "" && h == caller
	}
	return h == role.Avoid
}

// PermissionsFor is the permission mode of a worker that caller starts
// without --permissions: config's permissions, else the caller's own mode
// where routr knows it (CallerPermissions), else default.
func (c Config) PermissionsFor(caller string) string {
	return first(c.Permissions, CallerPermissions(caller), "default")
}

// CallerPermissions is the permission mode caller runs in, or "" when routr
// can't tell. Pi has no permission prompts (its docs/security.md: it "does
// not ask for approval before every tool call"), so its workers run as it
// does: bypass. Claude Code's and Codex's mode can't be seen from outside;
// the skill asks the model to pass its own.
func CallerPermissions(caller string) string {
	if caller == "pi" {
		return "bypass"
	}
	return ""
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// DetectCaller names the harness of the agent that ran routr, from its
// environment; "" when unknown. Every harness's variables pass on to what it
// starts, and CLAUDECODE is the one most often inherited (a Pi or Codex
// started from a shell in Claude Code keeps it), so Pi's and Codex's own
// variables are asked first.
func DetectCaller(getenv func(string) string) string {
	switch {
	case getenv("PI_SESSION_ID") != "" || getenv("PI_CODING_AGENT") != "":
		return "pi"
	case getenv("CODEX_THREAD_ID") != "" || getenv("CODEX_SANDBOX") != "":
		return "codex"
	case getenv("CLAUDECODE") == "1":
		return "claude"
	}
	return ""
}

// CallerSession is the calling agent's own session id, from the variable
// its harness sets: PI_SESSION_ID, CLAUDE_CODE_SESSION_ID or CODEX_THREAD_ID.
func CallerSession(harness string, getenv func(string) string) string {
	switch harness {
	case "pi":
		return getenv("PI_SESSION_ID")
	case "claude":
		return getenv("CLAUDE_CODE_SESSION_ID")
	case "codex":
		return getenv("CODEX_THREAD_ID")
	}
	return ""
}

// Ready checks a harness with internal/route's readiness checks (installed
// and logged in). A harness route doesn't know yet is ready when its binary
// is on PATH.
func Ready(ctx context.Context, harness string) error {
	for _, h := range route.Defaults() {
		if h.Kind == harness && h.Ready != nil {
			return h.Ready(ctx)
		}
	}
	if _, err := exec.LookPath(harness); err != nil {
		return fmt.Errorf("%s not found on PATH", harness)
	}
	return nil
}
