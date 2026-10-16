// Package route decides which harness, model and effort handle a prompt.
//
// Only harnesses that are installed and logged in are offered. Harness and
// model are asked as one combined choice ("claude_code:sonnet") so the router
// can never pair a model with a harness that doesn't serve it; effort is a
// second question in the same Jev call. Jev returns no prose, so the rationale
// is synthesized from its probabilities.
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bcherb2/herdr-routr/internal/jev"
)

// Harness is one routable coding agent.
type Harness struct {
	Name   string                              // routing id, e.g. "claude_code"
	Kind   string                              // herdr `agent start --kind`
	Models map[string]string                   // model → description shown to Jev
	Args   func(model, effort string) []string // harness argv after `--`; "" leaves the harness default
	Ready  func(ctx context.Context) error     // nil when installed and logged in
}

// Efforts are the reasoning-effort levels offered to Jev.
var Efforts = map[string]string{
	"low":    "simple, well-specified task; answer fast",
	"medium": "normal feature work or bug fix",
	"high":   "architecture, security, subtle debugging, or unclear cause",
}

// Defaults returns the harnesses verified to run under herdr.
func Defaults() []Harness {
	return []Harness{
		{
			Name: "claude_code", Kind: "claude",
			Models: map[string]string{
				"haiku":  "fast, cheap; trivial questions and one-line fixes",
				"sonnet": "strong general coder; default for most coding work",
				"opus":   "deepest reasoning; architecture and hard debugging",
			},
			Args: func(m, e string) []string {
				return flags("--model", m, "--effort", e)
			},
			Ready: claudeLoggedIn,
		},
		{
			Name: "codex_cli", Kind: "codex",
			Models: map[string]string{
				"gpt-6-astra": "OpenAI frontier coding model; strong at scripts and critical review",
			},
			Args: func(m, e string) []string {
				if e != "" {
					e = fmt.Sprintf("model_reasoning_effort=%q", e)
				}
				return flags("-m", m, "-c", e)
			},
			Ready: codexLoggedIn,
		},
		{
			// Pi's models depend on the providers it is set up with, so the
			// router offers its configured default; --model takes
			// "provider/id" when a caller pins one.
			Name: "pi", Kind: "pi",
			Models: map[string]string{
				PiDefaultModel: "Pi with its configured default provider and model; open-weight and third-party models",
			},
			Args: func(m, e string) []string {
				if m == PiDefaultModel {
					m = ""
				}
				return flags("--model", m, "--thinking", e)
			},
			Ready: piReady,
		},
		// Gemini is omitted: its auth can need GOOGLE_CLOUD_PROJECT and herdr
		// did not recognize its pane; effort would also need a settings.json override.
	}
}

// PiDefaultModel is the model name that leaves Pi on its configured default.
const PiDefaultModel = "default"

// flags returns name/value pairs, leaving out those with an empty value (the
// harness default).
func flags(kv ...string) []string {
	var out []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			out = append(out, kv[i], kv[i+1])
		}
	}
	return out
}

// ResumeArgs is the harness argv (after `--`) that reopens session, the
// harness's own session id, keeping args (model, effort) applied. Claude and
// Codex use the commands herdr itself restores agents with (agent_resume.rs):
// `claude --resume ID` and `codex resume ID`; Pi takes `--session ID|PATH`.
// All three take the model and effort flags after them.
func ResumeArgs(kind, session string, args []string) ([]string, error) {
	if session == "" || strings.HasPrefix(session, "-") || strings.ContainsFunc(session, unicode.IsSpace) {
		return nil, fmt.Errorf("route: invalid session id %q", session)
	}
	switch kind {
	case "claude":
		return append([]string{"--resume", session}, args...), nil
	case "codex":
		return append([]string{"resume", session}, args...), nil
	case "pi":
		return append([]string{"--session", session}, args...), nil
	}
	return nil, fmt.Errorf("route: cannot resume %q sessions", kind)
}

// piReady checks Pi's default provider without refreshing credentials, so the
// check has no side effects.
func piReady(ctx context.Context) error {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	var settings struct {
		Provider string `json:"defaultProvider"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err == nil {
		err = json.Unmarshal(b, &settings)
	}
	if err != nil || settings.Provider == "" {
		return fmt.Errorf("no default provider in pi settings (%v)", err)
	}
	out, err := run(ctx, "pi", "auth", "check", "--no-refresh", "--json", "--provider", settings.Provider)
	if err != nil {
		return err
	}
	for line := range strings.Lines(string(out)) { // warnings share the output
		var st struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(line), &st) == nil && st.Status != "" {
			if st.Status != "ready" {
				return fmt.Errorf("%s: %s", settings.Provider, st.Status)
			}
			return nil
		}
	}
	return errors.New("pi auth check gave no status")
}

func claudeLoggedIn(ctx context.Context) error {
	out, err := run(ctx, "claude", "auth", "status")
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err == nil && (json.Unmarshal(out, &st) != nil || !st.LoggedIn) {
		err = errors.New("not logged in")
	}
	return err
}

func codexLoggedIn(ctx context.Context) error {
	out, err := run(ctx, "codex", "login", "status")
	if err == nil && !strings.Contains(string(out), "Logged in") {
		err = errors.New("not logged in")
	}
	return err
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%s not found on PATH", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Decider is the Jev call; *jev.Client satisfies it.
type Decider interface {
	Decide(ctx context.Context, state any, questions map[string]jev.Choice) (jev.Decision, error)
}

// Router routes prompts across Harnesses.
type Router struct {
	Jev       Decider
	Harnesses []Harness
}

// Decision is the routing outcome.
type Decision struct {
	Harness   string   `json:"harness"`
	Kind      string   `json:"kind"`
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Args      []string `json:"agent_args"`
	Rationale string   `json:"rationale"`
	JevID     string   `json:"jev_id,omitempty"`
	Cost      float64  `json:"cost"`
}

// Label is the harness · model · effort tag shown in the master chat.
func (d Decision) Label() string { return d.Harness + " · " + d.Model + " · " + d.Effort }

// Available returns the harnesses whose Ready check passes, checked
// concurrently, and why each of the others is unavailable.
func (r Router) Available(ctx context.Context) ([]Harness, error) {
	errs := make([]error, len(r.Harnesses))
	var wg sync.WaitGroup
	for i, h := range r.Harnesses {
		if h.Ready != nil {
			wg.Go(func() { errs[i] = h.Ready(ctx) })
		}
	}
	wg.Wait()
	var out []Harness
	var why []error
	for i, h := range r.Harnesses {
		if errs[i] == nil {
			out = append(out, h)
		} else {
			why = append(why, fmt.Errorf("%s: %w", h.Name, errs[i]))
		}
	}
	return out, errors.Join(why...)
}

// Pin constrains routing; empty fields are left to Jev. Harness matches a
// harness's Name or Kind ("codex_cli" or "codex").
type Pin struct{ Harness, Model, Effort string }

func (p Pin) String() string {
	var s []string
	for _, kv := range [][2]string{{"harness", p.Harness}, {"model", p.Model}, {"effort", p.Effort}} {
		if kv[1] != "" {
			s = append(s, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(s, ", ")
}

// Route picks harness, model and effort for prompt under the user's rules.
func (r Router) Route(ctx context.Context, prompt, rules string) (Decision, error) {
	return r.RouteWith(ctx, prompt, rules, Pin{})
}

// RouteWith is Route with parts of the decision fixed by pin. Jev is asked
// only about what is left open, and not at all when pin fixes everything.
func (r Router) RouteWith(ctx context.Context, prompt, rules string, pin Pin) (Decision, error) {
	if _, ok := Efforts[pin.Effort]; pin.Effort != "" && !ok {
		return Decision{}, fmt.Errorf("route: unknown effort %q", pin.Effort)
	}
	avail, unready := r.Available(ctx)
	if len(avail) == 0 {
		if unready == nil {
			return Decision{}, errors.New("route: no harnesses configured")
		}
		return Decision{}, fmt.Errorf("route: no installed and logged-in harness (%s)", strings.ReplaceAll(unready.Error(), "\n", "; "))
	}
	byName := map[string]Harness{}
	combos := map[string]string{}
	for _, h := range avail {
		if pin.Harness != "" && pin.Harness != h.Name && pin.Harness != h.Kind {
			continue
		}
		byName[h.Name] = h
		for m, desc := range h.Models {
			if pin.Model == "" || pin.Model == m {
				combos[h.Name+":"+m] = fmt.Sprintf("%s running %s — %s", strings.ReplaceAll(h.Name, "_", " "), m, desc)
			}
		}
	}
	if len(combos) == 0 {
		return Decision{}, fmt.Errorf("route: no ready harness matches %s", pin)
	}
	questions := map[string]jev.Choice{}
	if pin.Effort == "" {
		questions["effort"] = jev.Choice{Instructions: "How much reasoning effort does this request need?", Options: Efforts}
	}
	if len(combos) > 1 {
		questions["harness_model"] = jev.Choice{
			Instructions: "Which harness and model should handle this request?", Options: combos}
	}
	var d jev.Decision
	if len(questions) > 0 {
		state := map[string]string{
			"task":        "Route a coding request to the best available coding agent harness.",
			"user_prompt": prompt,
			"user_rules":  rules,
		}
		var err error
		if d, err = r.Jev.Decide(ctx, state, questions); err != nil {
			return Decision{}, fmt.Errorf("route: %w", err)
		}
	}

	pick := onlyKey(combos)
	if a, ok := d.Answers["harness_model"]; ok {
		pick = a.Choice
	}
	name, model, _ := strings.Cut(pick, ":")
	h, ok := byName[name]
	if !ok || combos[pick] == "" {
		return Decision{}, fmt.Errorf("route: jev chose unknown option %q", pick)
	}
	effort := pin.Effort
	if effort == "" {
		effort = d.Answers["effort"].Choice
	}
	if _, ok := Efforts[effort]; !ok {
		return Decision{}, fmt.Errorf("route: jev chose unknown effort %q", effort)
	}
	why := Rationale(d.Answers)
	if p := pin.String(); p != "" {
		why = strings.TrimPrefix(why+"; pinned: "+p, "; ")
	}
	return Decision{
		Harness: name, Kind: h.Kind, Model: model, Effort: effort,
		Args: h.Args(model, effort), Rationale: why,
		JevID: d.ID, Cost: d.Usage.Cost,
	}, nil
}

// Rationale renders Jev's answers as "q: choice (p, conf; next: alternatives)".
func Rationale(answers map[string]jev.Answer) string {
	ids := make([]string, 0, len(answers))
	for id := range answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		a := answers[id]
		type kv struct {
			k string
			v float64
		}
		var ranked []kv
		for k, v := range a.Probabilities {
			if k != a.Choice && v > 0 {
				ranked = append(ranked, kv{k, v})
			}
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].v > ranked[j].v })
		alts := "no close alternative"
		if len(ranked) > 0 {
			var s []string
			for _, r := range ranked[:min(2, len(ranked))] {
				s = append(s, fmt.Sprintf("%s %.2f", r.k, r.v))
			}
			alts = strings.Join(s, ", ")
		}
		parts = append(parts, fmt.Sprintf("%s: %s (p=%.2f, conf=%.2f; next: %s)",
			id, a.Choice, a.Probabilities[a.Choice], a.Confidence, alts))
	}
	return strings.Join(parts, "; ")
}

func onlyKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}
