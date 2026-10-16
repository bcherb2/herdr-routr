// Package worker drives one harness in one herdr tab: open the tab, start
// the harness (new or resumed), answer startup dialogs, send text, report
// status, read the final reply, answer a menu, show, hide and close.
//
// It holds no state between calls beyond the Handle, so the watcher can be
// restarted and pick workers up again from the registry.
package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/bcherb2/herdr-routr/internal/transcript"
)

// TurnError is LastReply's error when an API error ended the worker's last
// turn (the harness gave up on a request): its Message is the harness's.
type TurnError = transcript.TurnError

// Spec is what Start needs.
type Spec struct {
	ID          int
	Label       string   // tab label, e.g. "3 review · codex"
	Harness     string   // herdr agent kind: claude, codex, pi
	Model       string   // "" = the harness default
	Effort      string   // "" = the harness default
	Permissions string   // default, auto or bypass
	Args        []string // extra harness arguments ([harness.X] args in config), after routr's own
	MCP         bool     // start the user's MCP servers and connectors (config start_mcp_servers); false = none
	Cwd         string
	Dirs        []string          // directories outside Cwd it writes (its chain's work directory): the harness may need them allowed
	Session     string            // resume this harness session instead of starting fresh
	Env         map[string]string // exported in the tab's shell before the harness starts
	Workspace   string            // herdr workspace for the tab; "" = a workspace labeled "routr"
	// HookLog is where Claude's PermissionRequest hook appends each prompt's
	// tool call (`$ROUTR_BIN hook --log HookLog`); "" = no hook.
	HookLog string
}

// Handle identifies a started worker. It is stored in the registry (Live,
// Session, Transcript), so it must survive a watcher restart.
type Handle struct {
	Socket     string
	Pane       string
	Tab        string
	Agent      string
	Harness    string
	Session    string // may be empty until the harness reports it; Poll fills it in
	Transcript string
	Cwd        string
	// Env is the tab's environment (Spec.Env), where Poll and LastReply look
	// for the harness's own directories (CODEX_HOME, CLAUDE_CONFIG_DIR, …)
	// to find its transcript. Not stored: the watcher fills it from config.
	Env map[string]string
}

// Phase is the driver's raw reading of a worker. The watcher applies
// settling and turns phases into registry states.
type Phase string

const (
	Working Phase = "working"
	Idle    Phase = "idle"    // herdr idle or done: finished a turn, or waiting on you
	Asking  Phase = "blocked" // a question or permission menu is on screen
	Exited  Phase = "exited"  // the harness is gone but the pane is still there
	Gone    Phase = "gone"    // the pane is gone (closed by you or by routr)
)

// Status is one Poll.
type Status struct {
	Phase   Phase
	Seq     uint64 // herdr's state-change sequence; changes whenever the status does
	Session string // harness session id, once known
	// Transcript is the session file, once known. Codex creates its file
	// only after launch, so Start may not have it.
	Transcript  string
	Question    string // Asking: the question and numbered options as shown
	Options     []string
	Fingerprint string // Asking on a menu: names its dialog (Answer's expect)
}

// Driver is implemented over herdr by NewHerdr (herdr.go) and faked in tests.
type Driver interface {
	Start(ctx context.Context, s Spec) (Handle, error)
	Prompt(ctx context.Context, h Handle, text string) error
	Poll(ctx context.Context, h Handle) (Status, error)
	// LastReply is the final message of the last turn; a *TurnError if an
	// API error ended that turn instead.
	LastReply(ctx context.Context, h Handle) (string, error)
	// Answer picks option (1-based, as shown on screen) of the menu on
	// screen and waits for the question to go; it returns the option's
	// text. With expect, it presses nothing unless the menu's fingerprint
	// is expect (a *ChangedError). With once, it presses nothing on an
	// option that decides more than this call (menu.Lasting; a
	// *LastingError): an answer the user did not give. ErrStillAsking: the
	// keys went, but the question stayed.
	Answer(ctx context.Context, h Handle, option int, expect string, once bool) (string, error)
	Show(ctx context.Context, h Handle, besidePane string, focus bool) error
	Hide(ctx context.Context, h Handle) error
	Close(ctx context.Context, h Handle) error
}

// ChangedError is Answer's error when the menu on screen is not the one
// expected: another question took its place, or (Fingerprint "") no menu is
// on screen any more. No option was chosen: no key went, or only arrows,
// which move the highlight.
type ChangedError struct {
	Question    string // on screen now, with its numbered options
	Options     []string
	Fingerprint string
}

func (e *ChangedError) Error() string {
	if e.Fingerprint == "" {
		return "worker: no menu on screen now; no option was chosen"
	}
	return "worker: another question is on screen now (fingerprint " + e.Fingerprint + "); no option was chosen"
}

// LastingError is Answer's error, with once, when option Option of the menu
// on screen saves a rule or allows something past the call its question is
// about (menu.Lasting). Nothing was pressed.
type LastingError struct {
	Option  int      // 1-based
	Options []string // the menu on screen
}

func (e *LastingError) Error() string {
	return fmt.Sprintf("worker: option %d (%s) decides more than this call; nothing was pressed", e.Option, e.Options[e.Option-1])
}

// ErrStillAsking is Answer's error when it pressed the keys but the question
// was still on screen a while later.
var ErrStillAsking = errors.New("the question is still on screen")
