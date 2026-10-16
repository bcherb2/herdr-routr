// Package registry is the per-repo, durable record of routr's workers.
//
// Every routr command is a short-lived process, and the watcher (`routr
// watch`) is restartable, so all state lives here: a JSON file under the
// repo's .routr/state directory, written under an flock and replaced
// atomically. See skills/routr/cli.md for the layout and docs/architecture.md
// for the state machine.
package registry

import "time"

// State is a worker's place in the lifecycle (docs/architecture.md,
// "Worker states").
type State string

const (
	Pending   State = "pending"    // queued; the watcher has not looked at it yet
	Waiting   State = "waiting"    // waiting on After, the directory lock, or its gate
	Gated     State = "gated"      // everything else is met; waits for `routr approve`
	Starting  State = "starting"   // the watcher is opening its tab and starting the harness
	Running   State = "running"    // brief sent; working or idle between your turns
	Blocked   State = "blocked"    // a settled question or permission menu on screen
	MaybeDone State = "maybe_done" // idle without `routr done`; see docs/architecture.md, "The watcher"
	Done      State = "done"       // `routr done` ran (or its output appeared); tab may still be open
	Closed    State = "closed"     // tab closed; the harness session is kept and resumable
	Failed    State = "failed"     // the harness exited or could not start
	NeedsYou  State = "needs_you"  // a loop hit its cap, or the chain cannot go on alone
	Cancelled State = "cancelled"  // `routr cancel`
)

// Terminal reports whether the worker will not move again on its own.
func (s State) Terminal() bool {
	switch s {
	case Done, Closed, Failed, NeedsYou, Cancelled:
		return true
	}
	return false
}

// Verdict is what a judging step concluded.
type Verdict string

const (
	NoVerdict Verdict = ""
	Pass      Verdict = "pass"
	Fail      Verdict = "fail"
)

// OnFail sends a failing verdict back to an earlier worker (a review loop).
type OnFail struct {
	Resume int    `json:"resume"` // worker id to send Brief to; it resumes its session
	Brief  string `json:"brief"`  // template, rendered when sent
	Max    int    `json:"max"`    // rounds allowed; required (> 0)
}

// DefaultRunTimeout is how long a run step's command may take when it sets
// no timeout.
const DefaultRunTimeout = 10 * time.Minute

// Fanout is a foreach step that comes after other steps: its files exist
// only once those are done. Until then it is one worker; when its After are
// done the watcher expands it into one copy per matched file, adds the
// copies to its After and it waits on them, so that the steps after it
// wait for every copy. It is done when they are.
//
// With AllowEmpty, a glob that matches nothing makes it done with no
// copies. A foreach step without after that matched nothing when the flow
// started is such a worker too, queued done.
type Fanout struct {
	Glob       string `json:"glob"`                  // template over params, relative to Cwd
	Output     string `json:"output,omitempty"`      // each copy's Output, a template over params and item; "" = the worker's Output
	MaxItems   int    `json:"max_items"`             // more matches than this need you
	AllowEmpty bool   `json:"allow_empty,omitempty"` // no match: done with no copies, instead of needing you
	Items      []int  `json:"items,omitempty"`       // the copies, once expanded
}

// Empty reports whether w is a foreach step that matched no files and is
// done with no copies (Fanout.AllowEmpty).
func (w Worker) Empty() bool {
	return w.Fanout != nil && len(w.Fanout.Items) == 0 && w.Succeeded()
}

// Judge is a judge step: the watcher asks Jev instead of a harness, with
// the results of the workers it comes after as the state, and records
// Jev's choice: a verdict, or (Pick) the winner among those workers, whose
// output becomes its own. Its Brief is Jev's instructions. It never opens a
// tab.
type Judge struct {
	Pick          bool     `json:"pick,omitempty"`           // pick one upstream worker instead of pass/fail
	Pass          string   `json:"pass,omitempty"`           // what pass means, for Jev; "" = a default
	Fail          string   `json:"fail,omitempty"`           // what fail means
	MinConfidence float64  `json:"min_confidence,omitempty"` // below it the choice needs you; 0 = DefaultMinConfidence
	Read          []string `json:"read,omitempty"`           // more files for the state: templates, rendered when it starts
	From          []int    `json:"from,omitempty"`           // the workers it judges: its After when queued (and a verdict, from round 2, its OnFail.Resume)
	Winner        int      `json:"winner,omitempty"`         // a pick: the worker it picked
	Accept        bool     `json:"accept,omitempty"`         // routr approve: take Jev's last choice as it is, or ask again if it had none
}

// Checkout is a worker's own git worktree (a flow step with worktree =
// true), so that writers run side by side without touching the main tree
// or each other. The flow records where it starts from when it starts;
// the watcher names it when it admits the worker (Path, Branch, and the
// worker's Cwd and Tree with them) and makes it when the worker starts.
// It is kept after the worker finishes, for you to merge; retention
// removes it when it drops the chain (checkout.go).
type Checkout struct {
	Base   string `json:"base"`             // the commit it starts from: HEAD where the flow started, when it started
	Repo   string `json:"repo"`             // the top of the checkout the flow started in; git worktree add runs there
	Sub    string `json:"sub,omitempty"`    // the flow's cwd relative to Repo; the worker's cwd is the same place in its worktree
	Path   string `json:"path,omitempty"`   // the worktree, absolute: <.routr>/trees/<chain>/<step>-<id>
	Branch string `json:"branch,omitempty"` // its new branch: routr/<chain>/<step>-<id>
	Made   bool   `json:"made,omitempty"`   // the worktree exists
}

// DefaultMinConfidence is the confidence below which a judge's choice
// needs you, when it sets none.
const DefaultMinConfidence = 0.6

// Live is the worker's current herdr pane. Nil when it has none.
type Live struct {
	Socket string `json:"socket"`
	Pane   string `json:"pane"`
	Tab    string `json:"tab,omitempty"`
	Agent  string `json:"agent"` // herdr agent name
}

// Caller is the agent that queued a worker, so a front end (the Pi
// extension) can tell which workers its session started.
type Caller struct {
	Harness string `json:"harness,omitempty"` // claude, codex, pi; "" = unknown
	Session string `json:"session,omitempty"` // its own session id, when its environment says
}

// ToolCall is the tool call a blocked worker's question is about, as the
// harness recorded it: what "Do you want to proceed?" means.
type ToolCall struct {
	Tool    string `json:"tool"`            // as the harness names it: Bash, Write, mcp__…__…, Codex's exec
	Summary string `json:"summary"`         // one line, the tool and its command, path or URL: "Bash: rm -rf build"
	Input   string `json:"input,omitempty"` // its input as recorded (JSON for Claude, the script for Codex's exec); a long one is cut
}

// Answerers are who may answer a worker's question: the user (human), the
// main agent (agent) or Jev (jev). A worker's Answerer is one (config
// [questions] answer, a flow step's answer, --answer), and so is who
// answered it last (AnsweredBy, routr answer --by).
var Answerers = []string{"human", "agent", "jev"}

// Worker is one harness session that routr started (or queued).
type Worker struct {
	ID    int    `json:"id"`
	Chain string `json:"chain,omitempty"` // shared by the workers of one flow or pipe
	Step  string `json:"step,omitempty"`  // step id within the chain, for {{step.x}} refs
	Role  string `json:"role"`
	Title string `json:"title,omitempty"` // short name for its tab (see Title); "" = the role

	// Resolved at `routr start`, from flags, the role and config.
	Harness     string   `json:"harness"` // herdr agent kind: claude, codex, pi
	Model       string   `json:"model,omitempty"`
	Effort      string   `json:"effort,omitempty"`
	Permissions string   `json:"permissions"`        // default, auto or bypass
	Answerer    string   `json:"answerer,omitempty"` // who answers its questions: human, agent or jev (Answerers); "" = human
	Args        []string `json:"args,omitempty"`
	MCP         bool     `json:"mcp,omitempty"` // starts the user's MCP servers and connectors (config start_mcp_servers); false = none
	// Env is what its tab and its run step get exported, besides routr's
	// own ROUTR_*: config [env], then a flow step's env, then --env, frozen
	// when it is queued and kept for its life, resumes included. nil (no
	// key) is a record from before it was frozen, which reads config [env]
	// at each spawn; {} is frozen and empty. Values may be secrets:
	// registry.json is 0600 and `list --json` prints the keys only.
	Env      map[string]string `json:"env"`
	Cwd      string            `json:"cwd"`
	Write    bool              `json:"write"` // takes the directory lock
	WritePin *bool             `json:"-"`     // a flow step's write key, for its Creator: nil leaves Write to the role

	Brief       string            `json:"brief"`            // template, rendered when the worker starts
	Params      map[string]string `json:"params,omitempty"` // {{task}}, {{in}} …
	Output      string            `json:"output,omitempty"` // file the worker is expected to write
	Interactive bool              `json:"interactive,omitempty"`
	Preamble    string            `json:"preamble,omitempty"`     // the role's preamble, sent before the brief
	WantVerdict bool              `json:"want_verdict,omitempty"` // the brief asks for --verdict pass|fail

	// Where it runs, recorded at `routr start`.
	Socket    string    `json:"socket,omitempty"`    // herdr server socket (the caller's)
	Workspace string    `json:"workspace,omitempty"` // herdr workspace for the tab; "" = the driver's default
	Tree      string    `json:"tree,omitempty"`      // worktree top of Cwd: the directory lock's key
	Checkout  *Checkout `json:"checkout,omitempty"`  // its own git worktree (worktree = true): Cwd lies in it
	TreeOf    int       `json:"tree_of,omitempty"`   // a run step: it runs in this worker's worktree (a pick judge: its winner's), its Cwd and Tree set when it is admitted
	Caller    *Caller   `json:"caller,omitempty"`    // the agent that ran `routr start`

	After    []int   `json:"after,omitempty"`     // workers that must be done first
	Gate     string  `json:"gate,omitempty"`      // "you": waits for `routr approve`
	GateOpen bool    `json:"gate_open,omitempty"` //
	OnFail   *OnFail `json:"on_fail,omitempty"`
	Round    int     `json:"round,omitempty"` // review-loop round, from 1

	Fanout   *Fanout `json:"fanout,omitempty"`   // a foreach step the watcher expands; it never opens a tab
	Parallel int     `json:"parallel,omitempty"` // a foreach copy: at most this many copies of its step run at once; 0 = no limit

	// A run step runs a shell command instead of a harness; its exit
	// status is its verdict. It never opens a tab.
	Run     string        `json:"run,omitempty"`     // the command, a template rendered when it starts; run with sh -c in Cwd
	Expect  Verdict       `json:"expect,omitempty"`  // pass ("" too): it must exit 0; fail: it must exit non-zero
	Timeout time.Duration `json:"timeout,omitempty"` // then it is stopped, a fail; 0 = DefaultRunTimeout
	Pid     int           `json:"pid,omitempty"`     // its process group while it runs, so cancel and a later watcher can stop it
	PidAt   uint64        `json:"pid_at,omitempty"`  // when Pid started (Linux clock ticks after boot): tells it from a later process given its pid

	Judge *Judge `json:"judge,omitempty"` // a judge step: the watcher asks Jev; it never opens a tab

	State    State    `json:"state"`
	Verdict  Verdict  `json:"verdict,omitempty"`
	GoOn     bool     `json:"go_on,omitempty"`    // routr approve past a fail that halts (Halts); its verdict stays fail
	Question string   `json:"question,omitempty"` // when blocked: the screen's question and options
	Error    string   `json:"error,omitempty"`
	Note     string   `json:"note,omitempty"`     // why it waits, or how it got here: for `routr wait` and `list`
	Rest     State    `json:"rest,omitempty"`     // the state a follow-up returns to once replied (done, needs_you, failed)
	Inbox    []string `json:"inbox,omitempty"`    // follow-ups from `routr send`, not yet delivered
	Summary  string   `json:"summary,omitempty"`  // the one-line message of `routr done`
	Turn     int      `json:"turn,omitempty"`     // follow-ups delivered; the reply to turn N goes to turns/N.md
	Replies  int      `json:"replies,omitempty"`  // follow-up replies written; Turn > Replies = a turn in flight
	RunFrom  int      `json:"run_from,omitempty"` // the turns up to it are an earlier run's (NewRun): their replies are not this run's result; none (a record from before routr retry) = every reply is this run's
	Runs     int      `json:"runs,omitempty"`     // its current run, from 1 (Store.Create; NewRun counts up): its final message is final-<Runs>.md (Paths.FinalPath); none (a record from before runs were numbered) = final.md

	// Revisions are the turns that are a review loop's on_fail rounds
	// (Queue from the watcher), not `routr send` follow-ups: the Nth is the
	// revision for round N.
	Revisions []int `json:"revisions,omitempty"`

	// The menu it is blocked on: its options (`routr answer ID N` picks the
	// Nth), its fingerprint (`routr answer --expect`) and herdr's
	// state_change_seq when the watcher recorded it, and the tool call the
	// question is about, when it is about one; ClearQuestion drops them with
	// Question. HandedTo is who answers it when Jev, its Answerer, did not:
	// the fallback, human or agent; "" while Jev decides. Note then says
	// why, with Jev's pick and confidence. Answer and AnsweredBy are its
	// last answer: the option as on screen ("2. No") and who chose it
	// (human, agent or jev).
	Options     []string  `json:"options,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	BlockSeq    uint64    `json:"block_seq,omitempty"`
	Pending     *ToolCall `json:"pending,omitempty"`
	HandedTo    string    `json:"handed_to,omitempty"`
	Answer      string    `json:"answer,omitempty"`
	AnsweredBy  string    `json:"answered_by,omitempty"`

	// A turn the harness ended with an API error instead of a reply. The
	// watcher retries a transient one (TurnError is set, Retry is when the
	// next try goes) and then gives up (needs_you, TurnError still set).
	TurnError string    `json:"turn_error,omitempty"`
	Retries   int       `json:"retries,omitempty"` // automatic retries since its last good turn
	Retry     time.Time `json:"retry,omitzero"`

	Session    string `json:"session,omitempty"`    // the harness's own session id (resume key)
	Transcript string `json:"transcript,omitempty"` // its transcript file, for live tailing
	Live       *Live  `json:"live,omitempty"`

	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Started  time.Time `json:"started,omitzero"`
	Idle     time.Time `json:"idle,omitzero"`     // when it went maybe_done
	Prompted time.Time `json:"prompted,omitzero"` // last brief or follow-up sent; outputs must be newer to count
	Finished time.Time `json:"finished,omitzero"` // when it reached done; zero while it has work to do
}
