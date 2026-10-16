# routr CLI: the reference

What each command does, where state lives, what a worker gets, who answers
its questions, and how roles and config resolve. Read it when `routr help
CMD` isn't enough. How flows are written is in [flows.md](flows.md); worked
examples are in [examples.md](examples.md). What the watcher does each tick,
and why routr is shaped this way, is for people changing routr: the repo's
`docs/architecture.md`.

## Where state lives

- **Root.** The repo is `git rev-parse --path-format=absolute
  --git-common-dir`; if that ends in `/.git`, the root is its parent (so a
  repo's worktrees share one root). Outside git, the root is the working
  directory. `ROUTR_STATE_DIR` overrides the state directory (tests).
- **`<root>/.routr/`:**

  ```
  .gitignore         "/state/\n/runs/\n/work/\n/trees/\n/.gitignore\n"; a line it lacks is added on use
  config.toml        repo overrides of the user config (committed)
  flows/*.toml       repo flows (committed)
  state/registry.json   the registry (+ registry.json.lock, flock), with the
                        directory locks: "locks": {"/src/app": 7}
  state/watch.lock   flock held by the live watcher for its whole life
  state/watch.log    the watcher's log, and routr answer's; capped at 1 MB (the one before is watch.log.1).
                     A watcher's first line names its build: watching DIR (routr <rev12>[+dirty])
  runs/<id>/brief.md    the brief as sent (after templating and the preamble)
  runs/<id>/done.json   written by `routr done`: {"output","verdict","message","at"};
                        the watcher renames it done.<n>.json once read
  runs/<id>/turns/N.md  replies to `routr send` follow-ups
  runs/<id>/final-<n>.md the worker's final message in its run n (`runs`, from 1;
                        `routr retry` and a loop round start the next run)
  runs/<id>/run.log     a run step's command's output, stdout and stderr (the
                        first and last 512 KB)
  runs/<id>/state.json  what a judge step sent Jev; jev.json: its question,
                        options and Jev's decision, or why it has none
  runs/<id>/hooks.jsonl a Claude worker's PermissionRequest hook: one line per
                        prompt, the tool call it is about (`routr hook`)
  work/<chain>/         the chain's scratch directory, {{work}}: made when a
                        flow starts (or a pipe's step first names it)
  trees/<chain>/<step>-<id>/   a worktree step's own git worktree, on branch
                        routr/<chain>/<step>-<id> (flows.md, "Worktrees")
  ```
- **Reading a result.** Which run is current, and which file holds its
  result, is routr's to decide (`Paths.Current`), not the layout's. Read
  it with `routr result ID`, or `routr result --path ID` for the file,
  `final_path` in `--json`, or `{{step.final}}` in a brief; never build a
  `runs/<id>/` path yourself.
- The registry is a JSON store (flock, atomic replace, move a corrupt
  file aside, refuse a newer version), with
  `Worker` records and `next_id`, `version`. A corrupt `registry.json` is
  renamed `registry.json.corrupt-<unix>` and routr carries on with an
  empty registry, rather than stall the watcher and its running workers.
  Ids then go on above the highest `runs/<id>`, so no run directory is
  reused, and `routr list` warns on stderr while the set-aside file
  exists.
- **Format versions.** routr writes version 1 and refuses a newer file. A
  field a record lacks reads as its zero value, so a field added later
  must make its zero mean what routr did before it existed.
- **Retention.** Every registry write also drops the finished workers
  routr no longer needs, in the same transaction, so `registry.json` and
  `runs/` stay bounded. A worker may go once it is terminal, has no
  follow-up queued or in flight, has no tab, and no active worker (or one
  with a tab) names it in `after` or `on_fail.resume`. A chain goes as one:
  only when every worker in it may go, and it counts as old as its newest
  worker. What may go is dropped once it has not changed for `keep_days`
  (default 14); beyond `max` workers (default 200) the least recently
  changed of it goes first (config `[registry]`; a watcher reads it when
  it starts). Its directory lock entry
  goes with it, and its `runs/<id>` is removed after the write (best
  effort, and not one that holds the worker's output file), with the
  chain's `work/<chain>` once the whole chain is gone, and its worktree
  (`trees/…`) with `git worktree remove`, which leaves one with
  uncommitted or untracked changes where it is; its branch is deleted only
  if nothing was committed on it. Harness sessions and output files stay. Ids are never given out again, so a
  command on a dropped id says there is no such worker. A write that
  brings a worker back (`routr send` to a closed one) and a write that
  would drop it are both one transaction: whichever commits first wins, and
  a queued follow-up keeps its worker. A config routr cannot read keeps
  every worker.

## Environment a worker gets

Exported in the worker tab's shell before the harness starts:

| Var | Value |
|---|---|
| `ROUTR_ID` | its worker id |
| `ROUTR_STATE_DIR` | the `.routr` dir, so `routr done` finds the root from anywhere |
| `ROUTR_RUN_DIR` | `.routr/runs/<id>` |
| `ROUTR_BIN` | absolute path of the routr binary |
| `ROUTR_CWD` | its cwd; `routr done` looks there for a relative `--output` it can't find in the shell's cwd |

plus the worker's own env: config `[env]`, then a flow step's `env`, then
`--env K=V` (`routr start`'s, or `flow start`'s for an agent step), later
ones winning. It is read when the worker is queued and
kept in its record for its life, resumes included, so editing `[env]` (say,
flipping `CLAUDE_CONFIG_DIR` to another account) changes only workers queued
after it; to change a queued or resumable worker's, cancel it and start it
again. Its transcript is looked for with the same env (`CODEX_HOME`,
`CLAUDE_CONFIG_DIR`). A run step's command gets the same, on
top of the watcher's own environment, except `flow start --env`: that is for
agent steps only, so a flow's run step keeps config `[env]` and its step's
`env` (`routr start --run --env` does reach its command). The five variables above are routr's:
`--env` and a step's `env` may not set them (a usage error), and a leading
`~/` in a value is expanded, as in `[env]`. Values may be secrets (an API
key): `registry.json` is written `0600`, and `--json` prints `env` as the
names only. The brief tells the worker to finish with
`/abs/path/routr done --output /abs/FILE [--verdict pass|fail] 'SUMMARY'`,
SUMMARY being one line on what it did. The brief spells the binary's
path, not `"$ROUTR_BIN"`, and puts SUMMARY in single quotes, saying to
keep them: Claude asks before any command with a variable or a backtick in
it ("Part of this command (a variable) cannot be checked in advance"),
whatever its rules allow. The same closing text also tells it not to end
its turn while a command it started still runs (routr takes an ended turn
as finished), to run `done` on its own as its last command once its checks
pass, that a failed `done` is not finished, and that it needs no routr skill; a worker in its own
worktree is told that `.routr/` exists only in the main checkout and where
to write reports. A revision and an `on_fail` follow-up repeat these
instructions.
Workers never write the registry; `routr done` only writes `done.json` in
the run dir (a sandboxed worker can write inside the repo), and the
watcher picks it up.

## Commands

Output is written for agents. Exit codes: 0 ok · 1 error · 2 usage ·
3 still running (a `--timeout` ran out) · 4 needs you (blocked, gated,
needs_you, closed before it finished, or a step waits on one that stopped
or failed a check; `answer --expect` found another question) · 5 failed
or cancelled.

| Command | Does |
|---|---|
| `routr start [flags] BRIEF\|-` | queue one worker. Prints its id alone on stdout (so pipes work) unless `--wait`. `-` reads the brief from stdin. With `--run CMD` instead of a brief, a run step (flows.md, "Run steps"). |
| `routr wait ID\|chain\|- [--timeout 9m]` | block until the worker (or every worker of a chain) is terminal, or blocked/gated. For an ID or a chain it also stops when a worker a queued step comes after (transitively) is blocked, gated, needs you, failed or cancelled, went idle without `routr done` (`maybe_done`: exit 4, with its last reply), or is a step that failed with no `on_fail` and was not approved past (exit 4, with its final message), so `… \| routr wait -` ends when the chain can't go on. A queued or unanswered follow-up keeps it waiting, even on a failed or needs_you worker (the follow-up resumes it). Prints the final message (for a chain, each step's one-line summary and the last step's final message), or the reply to the latest follow-up if that is newer; for a blocked worker, its question and who answers it ("Who answers"); for a failed one, its error and then the next step its record calls for (never started: `routr retry ID` with its brief; no session: `routr retry ID`; a session: `routr send ID …`). It goes on waiting while Jev decides a question (`answer = jev`). On timeout: `still running: 3 review · codex (4m12s). Run: routr wait 3`, exit 3. `-` reads an id from stdin. |
| `routr result ID [--path]` | this run's final message (or the newer follow-up reply, or `revision for 4 review (on_fail round 1):` for a review loop's revision), then `output: PATH`, `verdict: pass` and (a worktree step) `worktree: PATH (branch B)` lines. A worker whose state calls for it gets the line `routr wait` would stop with first, then a blank line: failed, with its error and next step (`routr retry ID` or `routr send ID …`); cancelled; needs you; blocked; closed; `maybe done`, with `routr send ID …` to have it go on. What it prints is always the current run's: a new run (`routr retry`; a review loop, for its `resume` worker, its reviewer and the checks it runs again) writes its own `final-<n>.md`, leaving the last run's where it was, and clears the verdict, and a reply counts only if its follow-up came in this run (`run_from`). With none, it says why in `routr wait`'s words (failed, with `routr retry ID` or `routr send ID …`; cancelled; needs you; blocked; gated), a foreach step lists its copies, a `previous run's final message: PATH` line names the run before's, and only a worker still at work says `Run: routr wait ID` (exit 1). `--path` prints only the path of the file holding the result, for `cat` or `grep`: the reply's `turns/N.md` when the reply is the result, else this run's final message. With no result it prints what `routr result` would, with the same exit code; with `--json` it is a usage error |
| `routr send ID TEXT\|-` | queue a follow-up turn; prints the id. If the tab is gone, the watcher resumes the session in a new tab. `--wait` waits for the reply to this turn and prints it. A run or judge step has no session: refused. So is a failed worker with no session to resume (its start failed, or its harness recorded none): the refusal names `routr retry ID`, which starts it again. A follow-up whose session will not resume is dropped: the worker is `failed` with the dropped text, and watch.log says so. |
| `routr done [--output F] [--verdict pass\|fail] [MESSAGE\|-]` | run by the worker (uses `ROUTR_ID`, or `--id`). `--output` must exist; it is stored as an absolute path. |
| `routr list [--all] [--since 1h] [--final]` | workers that are active (not terminal, or a follow-up queued or in flight), have an open tab, or changed within `--since` (default 1h); every worker the registry keeps (see Retention) with `--all`: id, chain/step, role, harness·model·effort, state, age, output, note. A run step shows role `run`, harness `sh` and its summary (`exit 1 after 3.2s`); its label is `5 check · run`. A judge step shows role `judge`, harness `jev` and its summary (`Jev: fail (p 0.83, confidence 0.83)`); its label is `6 ship · jev`. A worker whose question Jev decides is `blocked, Jev decides`. A worktree step's worktree and branch follow the table (`worktrees:`), and are `checkout` in `--json`. `--json` gives `final_path`, not the text of `final` or `reply`, unless `--final` |
| `routr show ID [--focus]` / `routr hide ID` | put the worker's pane beside the caller's pane (`HERDR_PANE_ID`), `--focus` moving focus to it / back into its tab |
| `routr show ID --context` | print what the worker's question is decided with: the question, the tool call it is about, its brief, and the project docs to read, as paths. No pane moves; it reads only the registry and files. `--json` gives the same as data. See "What a question is decided with" |
| `routr close ID` | close the tab; the session stays resumable (`routr send` reopens it). A run or judge step has no tab: refused (`routr cancel` stops its command or call) |
| `routr approve ID\|chain` | open a gate (for a chain: the gated workers). On a review worker at its loop cap (`needs_you`), accept its result as done so the chain goes on. On a foreach step that matched no files or too many (`needs_you`), look for its files again. On a judge step that needs you because Jev was unsure, take Jev's choice; because it had no key or failed, ask again. On any step that failed with no `on_fail`, let the steps after it go on past the fail (`go_on`; its verdict stays fail). |
| `routr answer ID N [--expect FP] [--by human\|agent\|jev]` | pick option N of a blocked worker's menu, numbered as on screen. `--expect FP` presses nothing unless the question on screen is still the one with fingerprint FP. `--by` says who chose (default `human`); `agent` and `jev` need `--expect`, and may not pick an option that saves a rule or lasts past this call. The same holds whoever runs it, the user or an agent. See "Answering a question" |
| `routr retry ID\|chain` | queue a `failed` worker (for a chain: each of its failed workers) again in its place: the same id, chain, step, round and worktree, so the workers after it wait on it as before. One that never started gets its brief afresh; one that ran keeps its session and is rerun in it (a review loop's round gets the revised prompt). What the failed run concluded becomes history: the new run writes its own final message, leaving the failed run's where it was, and its verdict and summary are cleared. A tab its failure left open is closed, and a dropped follow-up stays dropped. Any other state is refused, with what to do instead (`routr send` for a closed or `maybe_done` worker). |
| `routr cancel ID\|chain` | stop: close tabs, kill a run step's command (its process group), mark cancelled, release the lock. Workers after a cancelled ID are not cancelled; it names them. Cancelling a foreach step's worker cancels its copies. |
| `routr flow list` / `flow show NAME [--toml]` / `flow start NAME [--task T] [--in F] [--param k=v]… [--permissions P] [--answer A] [--harness H [--model M]] [--effort E] [--env K=V]… [--chain C] [--wait]` | named flows (flows.md); `start` fills each step's role defaults as `routr start` does and prints the chain id (`NAME-N` unless `--chain`). `--permissions` and `--answer` are for every step; `--answer` wins over a step's own `answer` ("Who answers"). `--harness`, `--model`, `--effort` and `--env` are for every agent step (not run or judge steps), over the step's own: a `--harness` other than a step's drops the step's `model`, which is its own harness's, so the role's model for the new harness applies unless `--model` is given; `--model` needs `--harness` (a usage error otherwise); `--effort` is checked against each step's harness; `--env` lays over config `[env]` and the step's `env` (see "Environment a worker gets"). So `flow start deep-research --harness claude --model haiku` runs every agent step on Haiku without a scratch flow. `show --toml` prints the flow as written. |
| `routr flow show\|start [NAME] --file F\|-` | a scratch flow: one from file F (`-` reads stdin), checked as a flow file is, for this command only. It is not in `flow list` and replaces no flow, even one of its name; NAME may be left out when F defines one flow. `start` keeps F as `flow.toml` in the chain's work directory. |
| `routr roles` | roles as resolved from config: the harness, model (`default model`: the harness's own) and effort each picks now; the permissions a worker started without `--permissions` gets; the roles at low effort; and the models `--model` takes on each harness (from `internal/route`) |
| `routr watch [--foreground]` | the watcher; normally started detached by other commands |
| `routr doctor [--socket S]` | one line per check, with the next step for each problem: the repo and its state directory, config, whether this process can reach herdr (a sandbox that denies it fails only while no watcher runs), the watcher, each harness (installed and logged in; none ready fails), the registry (a set-aside corrupt one warns), whether `OPENROUTER_API_KEY` is set for judge steps (a warning), the caller, the permissions a worker gets without `--permissions`, and the build: a binary built by `install.sh` (which stamps its checkout as `main.srcDir`) warns when that checkout has commits since its revision under `cmd`, `internal`, `go.mod` or `go.sum`, since the skill, the Claude Code plugin and the Pi extension are read live from it and are then newer than the binary; a binary built with uncommitted changes warns too. Exit 1 if a check fails |
| `routr version` | the git revision the binary was built from (`go build` stamps it), `+dirty` if it had uncommitted changes, the commit time, the Go version, and the checkout `install.sh` built it from. `--json`: `{revision, time, modified, go, src}`; `revision` is empty for a binary built outside a git checkout |
| `routr hook --log FILE EVENT` | Claude's hook sink: appends `{ts, event, payload}` (the payload as Claude sent it) to FILE. A Claude worker's PermissionRequest hook runs it, see "Answering a question" |

Every command takes `--json` and `--root DIR`. `routr help` lists every
command with a sentence on what it does and what its arguments are.
`routr CMD --help` (or `routr help CMD`) repeats that sentence, then lists
the command's flags, with what each does and its default, and examples.
Help that was asked for goes to stdout; bare `routr` prints it to stderr
and exits 2, and so does `routr help` with an unknown command. Flags may
come before or
after the positional arguments (`routr wait 3 --timeout 9m`); `--` ends
them. `--json` prints one JSON object or array: the registry record(s)
(`internal/registry/types.go`; `env` is its variables' names, not their values; unset times are omitted, never
`0001-01-01…`; a blocked worker's menu is `options`, `fingerprint` and
`block_seq`, the tool call its question is about `pending`, its last
answer `answer` and `answered_by`, who answers its questions
`answerer`, and who Jev handed its question to `handed_to`, see
"Answering a question" and "Who answers") plus `run_dir`, `final` (the text of this run's final message),
`final_path` (this run's final message, once it exists; `routr result --path`
names the reply's file when the reply is the result), `reply` (the latest follow-up reply, if it
came in this run and is newer than the final) and
`halts` (a step that failed with no `on_fail`: the steps
after it wait for `routr approve`); `list` leaves out `final` and `reply` unless `--final`.
The commands that act on workers (`show`, `hide`, `close`, `approve`,
`answer`, `cancel`) print `{action, message, workers}`: `message` is what
they print without `--json`, `workers` the records they changed, as `list
--json` gives them. `watch` prints `{action, message, root, log}`, and
nothing when it runs in the foreground. Nothing but that one JSON value
goes to stdout; notes go to stderr.
Each record has `caller: {harness, session}`: the agent that ran `routr
start`, its session from `PI_SESSION_ID`, `CLAUDE_CODE_SESSION_ID` or
`CODEX_THREAD_ID`.

**`routr start` flags:** `--role R` (default `task`), `--title T` (a short
name for the worker: lower case, `-` between words, at most 20 characters, a
cut marked `~`; it stands in for the role in its tab and label, `3
fix-login-race · codex`; default: from the brief's first line, leaving out
`{{…}}` refs; flow steps keep their role), `--harness
claude|codex|pi`, `--model M`, `--effort low|medium|high` (claude also `xhigh`, `max`; pi also `off`,
`minimal`, `xhigh`, `max`; a flow file's `effort` takes only low, medium, high),
`--permissions default|auto|bypass`, `--answer human|agent|jev` (who
answers its questions, "Who answers"), `--cwd D`, `--output F`,
`--interactive`, `--after ID|-` (repeatable; `-` reads an id from stdin),
`--gate you`, `--chain C`, `--step S`, `--param k=v`, `--in F` (= `--param
in=F`), `--env K=V` (repeatable: exported in its tab over config `[env]` and
kept for its life, "Environment a worker gets"; two Claude accounts are
`--env CLAUDE_CONFIG_DIR=~/.claude-alt`), `--arg A` (repeatable: an extra
harness argument after config's `args`, `--arg --chrome`; routr's own
session arguments are refused: `--session-id`, `--session`, `--resume`,
`--continue`, `--print`, Claude's and Pi's `-r`, `-c` and `-p`, and Codex's
`resume`), `--wait`, `--timeout T`, `--socket PATH` (default
`$HERDR_SOCKET_PATH`; required), `--caller H`. `--run CMD` queues a run
step instead of a harness: CMD is run with `sh -c`, and its exit status is
the verdict; `--expect pass|fail` (default pass), `--run-timeout D`
(default 10m), `--env`, and no role, harness, model, effort, permissions,
answer, output, interactive or arg. A param a brief uses but
nobody set, or `{{prev.*}}` without `--after`, is a usage error at once.
Passing `auto` as a harness or model is an error (Jev routing is not built).

A pipe reads the upstream id from stdin and joins its chain (a new chain
is created for the first step if none is given), so `{{prev.output}}`,
`{{prev.final}}` work. Refs in a brief:

| Ref | Is |
|---|---|
| `{{task}}`, `{{in}}`, `{{<param>}}` | params |
| `{{<step>.output}}` | that step's output path |
| `{{<step>.final}}` | path of its current run's final message |
| `{{<step>.verdict}}` | pass / fail |
| `{{<step>.outputs}}` | a fan-out step (foreach or models): its copies' outputs, space-separated; `(none)` if it matched no files (`allow_empty`), and no word in a run step's command |
| `{{prev.*}}` | the worker in `--after` (the first, if several) |
| `{{item.path}}`, `{{item.name}}` | inside a foreach step |
| `{{item.model}}`, `{{item.name}}` | inside a models step (a flow's `models` fan-out): the model id as listed, and it file-safe |
| `{{chain}}` | the chain's name; outside a chain `c<ID>`, the name a pipe from the worker gives its chain |
| `{{work}}` | the chain's scratch directory, `.routr/work/<chain>` |
| `{{<step>.worktree}}`, `{{<step>.branch}}` | a step with `worktree = true` (or a pick judge over such steps: the winner's): its git worktree, absolute, and its branch |

In a worker's brief, every path is absolute, its output's too. Claude's
file tools take absolute paths, and a Haiku worker told to write
`.routr/work/…` wrote `~/.routr/work/…` instead, outside the repo, where
even acceptEdits asks first. In a run step's command and a judge's brief
and `read`, a path inside the cwd is relative to it.

In a run step's command, params go in as written (`--param check="go test
./..."` is shell), and every other ref is quoted for `sh` (`'a b.md'`), so a
file name cannot inject a command: write refs bare, not inside quotes.

`chain` and `work` are not params (`--param work=…` is an error), and a
chain name (`--chain`) is letters, digits, `.`, `-` and `_`, since it names
the work directory. `flow start --chain C` refuses a C that a worker
already has or whose work directory has files: a flow starts a new chain.
`routr start --chain C` joins C on purpose.

Unknown refs are an error at start time for a flow, and at render time for
a pipe.

### Answering a question

A blocked worker's record has the question as the screen shows it
(`question`, with its numbered options) and, when it is a menu, the menu as
data: `options` (option N is the Nth), `fingerprint`, and `block_seq`,
herdr's `state_change_seq` when the watcher recorded it.

When the question is about a tool call, `pending` says which:
`{tool, summary, input}`. `tool` is the harness's name for it (`Bash`,
`Write`, `mcp__…`, Codex's `exec`). `summary` is one line, the tool and
its command, path, URL or search (`Bash: rm -rf build`, `Write:
/abs/path/file.go`), the command cut at 120 characters; for
AskUserQuestion it is the first question. `input` is the tool's input as
the harness recorded it (JSON for Claude, the script for Codex's `exec`),
cut at 4 KB with `… [routr: N bytes cut]`: a command or an Edit fits whole,
a long Write's content does not. The screen says only "Do you want to
proceed?"; this is what it means. `routr wait` prints the summary under the
question ("It asks about Bash: rm -rf build"). It is set when the watcher
records the question and dropped with it.

Where it comes from:

- **Claude:** its PermissionRequest hook. Every Claude worker's
  `--settings` registers one, in every mode, that runs `routr hook --log
  runs/<id>/hooks.jsonl PermissionRequest`; it decides nothing, so Claude
  shows its prompt as usual. Its `tool_name` and `tool_input` are the call
  the prompt is for (a subagent's too, by Claude's hooks docs; not seen
  live). Claude Code 2.1.287 sends no `tool_use_id` with it. The record
  counts if it is newer than the prompt that began the turn and than any
  tool result in the transcript since; else it was answered, and the
  transcript decides. It counts also when the transcript has no call
  without a result: the hook runs before Claude draws the prompt, and
  Claude may write the call only later (an `rm -rf` can ask with no
  assistant record in the transcript yet).
- **Otherwise, and for Codex and Pi:** the transcript's oldest tool call
  since the last prompt that has no result yet. A turn runs its calls in
  order, so that is the one waiting: a Haiku worker made a Write
  and `routr done` together, and the Write asked while `routr done` waited
  behind it. Codex's `exec` script can run several commands: the summary
  lists them, only those that ask to leave the sandbox
  (`require_escalated`) when some do, and a patch as `apply_patch FILE…`.
- **None** when no call is in flight: a question that is not about a tool
  call (a menu of the harness's own). Pi asks no permissions.

`routr answer ID N` reads the screen and picks option N, numbered as
shown. N must be one of them, and a numbered menu must start at 1 on screen
(option 1 scrolled away: nothing is pressed). The arrow keys only move the
highlight; enter goes once the screen shows it on option N, in the same
menu (a key can land late). Then it waits up to about 5 s for the
question to leave the screen; herdr goes on saying blocked for a moment
after the keys land. If the question stays, it says so and exits 1. The
worker keeps its last answer, `answer` (`"2. No"`) and `answered_by`
(`--by human|agent|jev`, default `human`), and watch.log gets a line for
each answer and each refusal. Run by an agent or from a plain shell, the
flags work the same: neither is required for the user's answer. Requiring
them of an agent gated nothing, since a model that answers a question it
should not would type `--by human` too.

An answer `--by agent` or `--by jev` may not pick an option that decides
more than the call the question is about. Some save a rule: Claude's "Yes,
and don't ask again for … commands in /path" and "Yes, and always allow
access to /dir from this project" write the repo's
`.claude/settings.local.json`, and Codex's "Yes, and don't ask again for
commands that start with …" saves a rule of its own. Others allow or
block something for the rest of the session: "Yes, and switch to accept
edits … for this session", Codex's "Allow for this session", "No, and
block … from now on". Every "Yes, and …" of Claude and Codex is one of
them. routr reads option N on screen before any key goes; if it is one of
these, it presses nothing, exits 1 and names the options that may be
picked, and watch.log gets a line. The words it goes by are in
`internal/menu` (`Lasting`). `--by human` may pick one: the user chose
it.

With `--expect FP` it presses nothing unless the menu on screen has
fingerprint FP. Use it whenever the choice was made away from the screen
(by an agent, by Jev, or by you from a picker): the menu may have changed
since. Another question exits 4 and prints it, with its fingerprint; no
menu exits 1. Without `--expect` it answers whatever menu is on screen, as
you would in its tab. `--by agent` and `--by jev` require it: without
it, a usage error (exit 2), before anything is read or pressed, says to
pass the worker's fingerprint from the `[routr]` message, `routr wait` or
`routr list --json`. A question with no menu has no fingerprint, and it
is the user's to answer anyway ("Who answers"). `--by human` works
without it.

The fingerprint is a hash of the dialog: from the rule Claude draws across
the pane above it (without one, from the question) down to the menu's last
line, leaving out whitespace, box edges, rules and the selection marker. So
a redraw of the same dialog keeps it: the highlight moving, the blinking ●
of the pending tool call above the dialog, the footer that changes with the
highlight, a resize that wraps the lines anew. Another question, command or
list of options changes it. Claude asked about two `echo`
commands in a row with the same question and options, and only the command
told them apart. `block_seq` is kept beside the fingerprint, not in it. It
held still while a prompt waited, but herdr's status can flap while a
dialog is up, and two prompts in a row have kept one sequence number,
so it cannot tell one question from another.

### Who answers

Each worker has `answerer`: who answers its questions, `human`, `agent` or
`jev`. routr decides it when the worker is queued, so it is the same
whichever agent started the worker: `--answer` on `routr start` or `routr
flow start`, else the flow step's `answer`, else config `[questions]
answer`, else `human`. On `flow start`, `--answer` is for every step and
wins over a step's own `answer`: whoever starts the run decides for this
run, over what the flow file says for every run, as `--permissions` does.
A bad `--answer` is a usage error. A run or judge step asks nothing and has no
`answerer`.

- **`human`** (the default): the user answers, never the main agent.
  `routr wait` prints the question and what it asks about, then:

  ```
  The user answers this question, not you: do not choose an option yourself, even an obvious one.
  Tell the user what it asks. They answer in its tab (routr show 6), or tell you an option.
  If they tell you option N, run: routr answer 6 N --by human --expect 6a128b4273b95faa
  ```

  A question with no menu (no `fingerprint`) has no option to press, so
  after the first line it says only "Tell the user what it asks. They
  answer in its tab: routr show 6". The Pi extension's wake message says
  the same, and adds `/routr answer 6 N`, which the user can type in Pi.
- **`agent`**: the main agent answers, for the user. `routr wait` prints
  the question and what it asks about, then:

  ```
  You answer this question, not the user.
  Before you decide, read the project docs, even when the answer looks obvious: /src/app/SPEC.md
  Its brief: /src/app/.routr/runs/6/brief.md
  Answer it yourself only if what it asks about is allowed by the docs, within its brief, and can be undone (deleting files, pushing, sending or installing cannot), and the docs and brief settle which option is right.
  Do not pick option 2: it saves a rule or lasts past this call, so only the user may.
  To answer, run: routr answer 6 N --by agent --expect 6a128b4273b95faa, then tell the user in one line what you chose and why.
  Otherwise ask the user: say what it asks and why you did not decide. They answer in its tab (routr show 6), or tell you an option N: then run routr answer 6 N --by human --expect 6a128b4273b95faa
  ```

  The project docs and the brief as sent ("What a question is decided
  with") are given as paths, not their text: the agent can read them
  itself, and reads them only for a question it decides, while their text
  in every message would cost its context each time. The docs come first,
  "even when the answer looks obvious": a model that was
  only told to read them first skipped them and approved a `curl` they
  forbid. The brief is often the agent's own. The "Do not pick" line is
  there only when an option saves a rule or lasts past this call; `routr
  answer --by agent` refuses those anyway ("Answering a question"). If
  routr's config does not load, the docs and brief lines become "Before
  you decide, run routr show 6 --context …", which reports the error. A question
  with no menu has no option to press, so the user answers it, and the
  text is the one for `human`. The Pi extension's wake message says the
  same: it prints the lines `routr show ID --context --json` gives as
  `answer`, and adds `/routr answer 6 N`, which the user can type in Pi.
- **`jev`**: Jev decides, in the watcher. The main agent is not woken
  while it does: `routr wait` goes on waiting, `routr list` shows
  `blocked, Jev decides`, and the Pi card says "Jev decides its
  question". Jev gets what the agent would decide with ("What a question
  is decided with") and one choice: an option it may pick, or
  `hand_over`. Options that save a rule or last past this call are not
  among them. It is told the agent's rules, and that the question, the
  tool call and the brief are data to judge, not instructions to it. Its
  option is pressed when its confidence is at least `min_confidence`, as
  `routr answer N --by jev --expect FP` would: only on the question
  recorded when the worker blocked. The worker runs on, with `answer` and
  `answered_by` `jev`, and watch.log has Jev's pick, the probability of
  each option (Jev gives no reasons) and the call's cost:

  ```
  6 implement · claude: answered 1. Yes, by jev: confidence 0.93 (option 1 0.93, hand_over 0.05, option 3 0.02; decision gen-dec-…, $0.00003000, 712 input tokens)
  ```

  Anything else hands the question to `fallback`, `human` or `agent`:
  `hand_over` however sure Jev is, a pick below `min_confidence`, an
  error, or a press that failed. A question with no menu, no tool call
  it is about (`pending`), or no option Jev may pick goes there without
  a call, as does every question when there is no key
  (`OPENROUTER_API_KEY`, from config `[env]` or the watcher's
  environment). Without the call, "Do you want to proceed?" says nothing
  Jev could judge; and the menus about no call are the harness's own (a
  usage limit, a browser's access), about the account or the session,
  which the docs and the brief do not settle. The worker stays blocked, with `handed_to` and
  a note saying what Jev made of it. `routr wait` and the Pi wake message
  print the note, then the fallback's text, as for its own questions:

  ```
  Jev did not answer it: it picked option 1 (Yes) with confidence 0.62, below min_confidence 0.8.
  The user answers this question, not you: do not choose an option yourself, even an obvious one.
  ```

  If the question on screen changed while Jev decided (it was answered
  in its tab), nothing is pressed. The worker goes on, or the new
  question is recorded and Jev decides that one. Jev is asked once per
  question in a watcher run: the same question again (one command asked
  twice) takes its earlier answer, unless that call failed. A watcher
  that stops while Jev decides leaves the question undecided, and the
  next one asks again. `fallback` and `min_confidence` are read from
  config each time Jev decides.

### What a question is decided with

`agent` and `jev` decide from the same facts. routr gathers them from the
worker's record and files alone, never from herdr: the worker (id, role,
chain, step, its directory, who answers), its question as on screen with
its options and fingerprint, the tool call it is about (`pending`), its
brief, and the project's docs. `routr show ID --context` prints them for
the main agent. The watcher gives them to Jev (`answer = jev`).
`routr wait` and the Pi extension give the agent the paths to read from
them ("Who answers").

- **The docs** are config `[questions] context`: paths relative to the
  repo root (the parent of `.routr`), in their order. Unset, they are the
  first that exists of `.design/SPEC.md`, `SPEC.md` and `README.md`.
  `context = []` means none. A listed path with no file there, or a
  directory, is left out and named under `missing`; the others still go.
- **From the main checkout.** The root is the main checkout's, also for a
  worker in a subdirectory or in its own worktree (`.routr/trees/…`). The
  docs there are what the user wrote. The worktree's copy is the worker's
  to edit, and a worker must not write the rules its own questions are
  decided by.
- **The brief** is the prompt as sent (`runs/<id>/brief.md`, with the
  role's preamble and the `routr done` lines), or the brief as queued if
  it was never sent. One over 8 KB is cut to its head and tail, with a
  line saying how many bytes were cut.
- **For the main agent** the docs are absolute paths with their sizes,
  and when it answers (`agent`) a line tells it to read them before it
  decides: it can read files itself, and pasting them into every wake
  costs context. `--json` gives `{worker, label, state, role, chain, step,
  cwd, answerer, question, options, fingerprint, lasting, pending, brief,
  brief_path, root, docs: [{path, size}], missing, answer}`. `answer` is
  who answers the question and how, a line each, as `routr wait` prints
  it ("Who answers"); it is absent when the worker asks nothing now.
- **Options only the user may pick** are `lasting`, numbered from 1: those
  that save a rule or last past this call, which `routr answer --by
  agent|jev` refuses ("Answering a question"). The text says "Do not pick
  option 2: it saves a rule or lasts past this call, so only the user
  may."
- **For Jev** the docs are their text, with paths relative to the root,
  shared out as a judge's files are: short ones whole, long ones cut
  alike to their head and tail, each with a line saying how many bytes
  were cut. They get what is left of 80 KB after the brief, the question
  and the tool call, so the whole state fits Jev's window as a judge's
  does. No less: a long doc cut further loses more of its middle, where
  the rule that forbids the call may be, and a call costs about a tenth
  of a cent. The options are numbered as `routr answer` takes them.
- **A worker that asks nothing now** gets the rest, under "It asks nothing
  now": the agent may look before a question comes, and a question
  answered in the meantime is no error. A run, judge or foreach step never
  asks: refused.

```
$ routr show 6 --context
6 implement · claude: role implement, chain fix-3, step implement, in /src/app/store
Who answers its questions: agent

It asks:
Do you want to proceed?
1. Yes
2. No
fingerprint: 6a128b4273b95faa
It asks about Bash: go test ./...
input:
{"command":"go test ./..."}

Its brief (/src/app/.routr/runs/6/brief.md):
Fix the flaky test in ./store.

Project docs: read them before you decide.
/src/app/SPEC.md (31 bytes)
Missing: docs/gone.md, listed in [questions] context but not in /src/app
```

## The watcher

`routr watch`, one per repo, does the herdr work between your calls. Any
command that changes the registry starts it, and it exits after 10 idle
minutes; you never need to start it. If the herdr socket is out of reach
(Codex's sandbox), `start`, `flow start` and `send` queue nothing and exit
1, naming the sandbox: run the same command again with escalated
permissions. What it does each tick is in the repo's
`docs/architecture.md`, "The watcher".

## Roles and config

User config `$XDG_CONFIG_HOME/routr/config.toml`, merged under
`<root>/.routr/config.toml` (repo wins per key):

```toml
permissions = "auto"            # for workers when --permissions isn't given; unset: the caller's mode
start_mcp_servers = false       # true: workers start your MCP servers and claude.ai connectors (see "MCP servers")

[env]                           # exported in every worker tab; read when a worker is queued, kept for its life
# CLAUDE_CONFIG_DIR = "~/.claude-alt"

[registry]                      # retention of finished workers (see "Where state lives")
keep_days = 14                  # drop them this long after they last changed; 0 = 14
max = 200                       # beyond this many workers, the oldest finished go first; 0 = 200

[questions]                     # who answers a worker's question (see "Who answers")
answer = "human"                # human, agent or jev; --answer and a flow step's answer win
fallback = "human"              # who answers when jev cannot decide: human or agent
context = ["SPEC.md", "README.md"]  # project docs for agent and jev, relative to the repo root
min_confidence = 0.8            # jev answers at or above it; above 0, at most 1

[harness.claude]
model = "sonnet"
# args = ["--add-dir", "/shared"]  # extra harness arguments, after routr's own
[harness.codex]
model = "gpt-6-astra"
[harness.pi]

[roles.review]
prefer = ["codex", "claude"]    # first one that is installed, logged in and allowed
avoid = "caller"                # not the harness of the agent that called routr
effort = "high"
write = false                   # read-only: no directory lock
verdict = true                  # the brief asks for a verdict
output = "REDTEAM.md"           # default output when neither flag nor flow names one
preamble = """You are reviewing another agent's work. …"""
```

Built-in roles (in the binary, `internal/config/builtin.toml`,
overridable): `task` (default; write), `plan` (write, interactive, output
PLAN.md), `review` (read-only, prefer codex then claude, avoid caller,
high effort, verdict), `research` (read-only, `models = {claude =
"sonnet"}`, low effort), `implement` (write, gate you by default),
`summarize` (read-only), `refute` (read-only, prefer codex then claude,
avoid caller, high effort; a default-reject verifier of one finding),
`hunt` (as `review`, but no verdict: it lists candidate findings for
others to verify, and finding some is no fail). A
role without `prefer` tries the caller's
harness, then claude, codex, pi. `write` unset means true. A role picks a
model per harness with `models = {claude = "opus"}`; then
`[harness.X] model`, else the harness default. Effort works the same way:
`--effort`, then the role's `efforts = {pi = "off"}`, its `effort`, then
`[harness.X] effort`. Each harness takes its own levels (claude `--effort`
low…max; codex `model_reasoning_effort` low, medium, high; pi `--thinking`
off, minimal, low…max), and a level the picked harness doesn't take is an
error. A model is passed to the harness as is: for Pi, any `provider/id`
it has a key for (`pi --list-models`), e.g.
`openrouter/deepseek/deepseek-v4-flash`. Unknown keys are errors.

| Harness | `--model` takes |
|---|---|
| claude | `haiku`, `sonnet`, `opus` (aliases; a made-up dated id fails) |
| codex | `gpt-6-astra` |
| pi | `provider/id`, e.g. `openrouter/deepseek/deepseek-v4-flash`; unset, Pi's configured default |

`routr roles` and `routr start --help` print the same list, from
`internal/route`.

**`[questions]`** defaults to `answer = "human"`, `fallback = "human"`
and `min_confidence = 0.8`. `context` unset means the first of
`.design/SPEC.md`, `SPEC.md` and `README.md` that exists, and `context =
[]` means no docs; its paths must stay inside the repo. Any other value,
or a `context` path outside the repo, is an error that names the key and
what it takes. `answer` picks who answers.
`context` is what `routr show ID --context` lists ("What a question is
decided with"). `fallback` and `min_confidence` are for `jev`, and are
read each time Jev decides a question: nothing sets them per run, and a
change applies to the next question.

A worker's **permissions** are `--permissions`, else config
`permissions`, else the caller's own mode, else `default`. routr knows
only Pi's mode: Pi asks for no approvals (its docs/security.md), so a Pi
caller's workers get `bypass`. Claude Code's and Codex's mode can't be
seen from outside, so their workers get `default` unless the model
passes its mode, as the skill asks. `routr doctor` prints the mode a
worker would get and why; `routr roles` prints it too.

**The caller** is detected from the environment, most specific first:
`PI_SESSION_ID` or `PI_CODING_AGENT` (Pi sets it for every command its
model runs) → pi; `CODEX_THREAD_ID` or
`CODEX_SANDBOX` → codex; `CLAUDECODE=1` → claude (a Pi or Codex started
from a shell in Claude Code inherits it); else unknown (then `avoid =
"caller"` has no effect, and permissions get no caller mode). `--caller H`
overrides, for permissions too.

**Permissions** map to harness flags:

| | default | auto | bypass |
|---|---|---|---|
| claude | — | `--permission-mode auto` (if `claude --help` lists it and the model is not Haiku, else `acceptEdits`) | `--dangerously-skip-permissions` |
| codex | — | `--approve-for-me` (if `codex --help` lists it, else `--full-auto`, else `--sandbox workspace-write --ask-for-approval on-request`) | `--dangerously-bypass-approvals-and-sandbox` |
| pi | — | — | — |

Claude Code 2.1.283 has no auto mode for Haiku: `--permission-mode auto`
starts it in manual mode, asking before every edit. Where `auto` falls
back to `acceptEdits`, the worker may also use WebSearch and WebFetch
without asking (auto mode allows them; a Haiku research worker otherwise
stops at its first search). **Other shell commands still ask**: a Haiku
worker under `auto` blocks on its first `python3`, `go test` or `grep`
until someone answers (`routr answer`). routr does not allow Bash
wholesale to make up for it, so the `research` role pins sonnet on Claude,
where auto mode works; choose Haiku (`--model haiku`, or `models` in a
role) only for work that runs no commands, or with `--permissions
bypass`, or add allow rules for the commands it needs to your Claude
settings. In every mode but
bypass, a worker may always run `routr done`: Claude gets `--settings
'{"permissions":{"allow":["Bash(/abs/path/routr done:*)"]}}'` (merged with
the user's settings; it matches the command as the brief spells it, see
"Environment a worker gets"), Codex gets `--add-dir <run dir>` (the run
dir is outside its sandbox's workspace when the worker's cwd is a
subdirectory or another worktree of the repo). In every mode, the same
`--settings` gives Claude a PermissionRequest hook, so the watcher knows
what a prompt is about (`pending`, "Answering a question").

**MCP servers.** A Claude worker starts with `--no-chrome`, in every
mode: Claude in Chrome drives your own browser, signed in to everything,
and `claudeInChromeDefaultEnabled` in `~/.claude.json` turns it on for
every interactive session. A Haiku fact-checker opened it to check
sources.
To give workers the browser, add `--chrome` to `[harness.claude] args`;
it comes later and wins.

Workers also start without your MCP servers and claude.ai connectors:
they reach your own accounts, and under `bypass` nothing asks before an
MCP tool runs, whatever the tool does. Claude gets
`--strict-mcp-config`, which drops the connectors too; Pi gets
`--no-mcp`, which turns off Pi's built-in MCP but not an MCP extension
such as pi-mcp-adapter, whose servers a Pi worker still gets; Codex has no switch for all of them, so it gets
`-c features.apps=false` (its own connectors, `codex_apps`), and routr
runs `codex mcp list --json` in the worker's cwd and env and turns each
enabled server off with `-c mcp_servers.NAME.enabled=false`. If that
list fails, the worker fails to start and says so; it never starts with
them by accident. `start_mcp_servers = true` in config starts workers
with them, as your own sessions do. It is read when a worker is queued
and kept for its life, resumes included, like `[env]`.
