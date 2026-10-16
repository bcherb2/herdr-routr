# Flows: how work is sequenced and data passed between workers

How to write a chain or a flow file. The `routr flow` commands are in
[cli.md](cli.md). Eleven flows ship built in: `routr flow list` names them,
and `routr flow show NAME --toml` prints one with its briefs, which is the
quickest start for a flow of your own ("Scratch flows").

## What a flow needs to say

Every chain of workers is built from these:

| Need | Example |
|---|---|
| A step: role + brief | review PLAN.md with the review role |
| Order | review after plan |
| Data between steps | review reads what plan wrote |
| A branch on the result | review fails → back to the planner |
| A loop with a cap | at most 3 rounds of plan ⇄ review |
| A gate | implementation waits for you |
| Fan-out and join | 4 research briefs in parallel, one synthesis after all of them |
| Parameters | the task, the input file |

The lock needs no syntax: it follows from the role (read-only roles skip
it).

## Rule 1: files are the data bus

Workers hand off through files. The convention is fixed, so neither flows
nor agents have to invent paths. Each worker has a run directory,
`.routr/runs/<worker id>/`, with its brief, its final messages and, for a
run or judge step, its log or Jev's answer (the full layout: cli.md,
"Where state lives"). It is keyed by worker id, not chain and step: a step
can run more than once in a loop, and a foreach step has many workers. The
output itself, PLAN.md …, is written where the brief says, normally the
repo root.

- `routr done --output PLAN.md --verdict fail` records the output and
  verdict. `routr result ID` prints the current run's result: its final
  message, or the reply to its last follow-up when that is newer.
  `routr result --path ID` prints the file that holds it. Take paths from
  routr (these, `final_path` in `--json`, the refs below), never from the
  layout: which run is current is routr's to decide.
- A brief refers to earlier steps by name: `{{plan.output}}`,
  `{{review.final}}`, `{{review.verdict}}`, `{{task}}` (the full list is
  the refs table in cli.md).
- Files that only the chain needs (findings, parts, notes) go in its work
  directory, `{{work}}`: `.routr/work/<chain>`, made when the flow starts.
  A second run of a flow gets a new chain and a new, empty directory, so
  it never reads the leftovers of the first; `flow start --chain NAME`
  refuses a name in use. `{{chain}}` is the chain's name.
- **Briefs are templated when the step starts, not when it's queued,**
  because the upstream files don't exist yet at queue time.
- Everything is plain files in the repo, so any agent can `cat` or grep a
  run. `.routr/runs/`, `.routr/state/`, `.routr/work/` and `.routr/trees/`
  (worktrees, "Worktrees") are gitignored;
  `.routr/flows/` and `.routr/config.toml` are committed.

## Rule 2: three ways to write the same thing

All three write the same entries into the registry. The watcher runs them
all the same way, and none of them holds a process open, so a Claude or
Codex tool timeout can't kill a chain.

### a. Pipes, for ad-hoc straight lines

Each `routr start` queues one step, prints its id and exits. `--after -`
reads the previous id from stdin.

```sh
routr start --role plan --in DECISIONS.md 'Write PLAN.md for: add OAuth' \
  | routr start --after - --role review 'Red-team {{prev.output}}' \
  | routr start --after - --role implement --gate you 'Implement PLAN.md' \
  | routr wait -            # optional: block (under the timeout) until the chain ends
```

Good for what the main agent makes up on the spot. It gets awkward past
straight lines: loops and fan-out don't fit in a pipe. `{{prev.*}}` reaches
one step back only; to name an older step, give it `--step plan` and say
`{{plan.output}}`, or write the file name.

### b. Flow files, for anything you run twice

These are named and take parameters. Flows are read from three places, and a later flow replaces an earlier one by name:

1. the built-ins, in the binary (`internal/flow/builtin.toml`);
2. `$XDG_CONFIG_HOME/routr/flows/*.toml` (default `~/.config`);
3. `<root>/.routr/flows/*.toml`, in the repo.

Two files in the same directory that define the same flow are an error,
since neither can win.

**Scratch flows.** A flow the main agent writes for one job, say
`deep-research` with 25 cheap diggers, or three on a stronger model,
isn't saved in any of those places. It goes in with `--file`:

```sh
routr flow show deep-research --toml > /tmp/wide.toml   # the flow as written
# edit: dig gets model = "haiku", max_items = 25, parallel = 25
routr flow start --file /tmp/wide.toml --task '…'        # or --file - with a heredoc
```

It is checked as any flow file is, runs once, and changes nothing else: it
isn't in `flow list`, and a scratch `deep-research` doesn't replace the
built-in one. The chain keeps it as `.routr/work/<chain>/flow.toml`;
moving that into `.routr/flows/` makes it a named flow of the repo.

```toml
[flow.plan-to-impl]
description = "plan with you, red-team it, gate, implement, review the code"
params = ["task", "in"]           # routr flow start plan-to-impl --task "…" --in DECISIONS.md

[[flow.plan-to-impl.step]]
id = "plan"
role = "plan"
interactive = true                # you talk to it; it ends only on `routr done`
output = "PLAN.md"
brief = "Write PLAN.md for: {{task}}. Decisions so far are in {{in}}."

[[flow.plan-to-impl.step]]
id = "review"
role = "review"                   # the role says "not the author's model"
after = ["plan"]
output = "REDTEAM.md"
brief = "Red-team {{plan.output}}. Write REDTEAM.md."   # on_fail adds the ask for a verdict
on_fail = { resume = "plan", brief = "Address {{review.output}}.", max = 3 }

[[flow.plan-to-impl.step]]
id = "implement"
role = "implement"                # a write role, so it takes the directory lock
after = ["review"]
gate = "you"
brief = "Implement {{plan.output}}."

[[flow.plan-to-impl.step]]
id = "code-review"
role = "review"
after = ["implement"]
brief = "Review the diff since the chain started against {{plan.output}}."
on_fail = { resume = "implement", brief = "Fix {{code-review.final}}.", max = 2 }
```

### Review loops

An `on_fail` round sends the reviewer's findings to the `resume` worker,
and the reviewer waits for it, as well as for its own `after`, before it
reviews again. Run and judge steps between the two (a check after
`implement` that the review comes after) are queued again: they wait for
the revision, and for the checks before them, so they check the revision
before the review sees it. An agent step between them is not rerun; the
reviewer's note names it, and the checks after it see its earlier result.
A step waits for every step upstream of it, not only its `after`, so a
step not started yet in a side branch below the `resume` worker waits for
the revision too. `resume` must name a step upstream of the reviewer.
The round starts the `resume` worker, the reviewer and the checks it
queues again over: each one's next run writes a final message of its own
and its verdict is cleared. The `on_fail` brief is rendered before, so a
`{{step.final}}` in it names the failing round's file, which stays.

A fail verdict without `on_fail` halts the steps after it, whatever kind
of step gave it, until `routr approve`. A step whose output is a list for
later steps to weigh, not a pass or fail, should not ask for a verdict:
the built-in `hunt` role does not. A step that only writes into `{{work}}` (plans, notes) can say
`write = false`, so it does not take the repo's directory lock.

### Fan-out

Fan-out uses a glob: one worker per matched file. When the glob is
expanded depends on `after`:

- **No `after`:** when the flow starts. The files must exist then, so the
  main agent writes them first.
- **With `after`:** when those steps are done. A planner step writes the
  files, then one worker runs per file. Until then the step is one worker
  that waits; then it becomes the copies, and it waits on them.

Either way the glob may use params, `{{chain}}` and `{{work}}` only: an
earlier step's output is a file, not a directory to glob. Name the
directory in the planner's brief and in the glob; `{{work}}/parts/*.md`
keeps a second run of the flow from globbing the first one's parts. A step
that comes `after` a fan-out step waits for every copy. A fan-out from files
the main agent wrote:

```toml
[flow.research]
description = "one research worker per brief file, then one synthesis"
params = ["in"]                   # a directory of briefs

[[flow.research.step]]
id = "dig"
role = "research"
foreach = "{{in}}/*.md"           # one worker per brief file
output = "research/{{item.name}}"
brief = "Research the question in {{item.path}}. Write your findings to research/{{item.name}}."

[[flow.research.step]]
id = "synthesize"
role = "summarize"
after = ["dig"]                   # join: waits for all copies
output = "research/SUMMARY.md"
brief = "Synthesize {{dig.outputs}} into research/SUMMARY.md."
```

A planner, then one worker per part it wrote, then a join:

```toml
[flow.split]
description = "split a question, one researcher per part, then one synthesis"
params = ["task"]

[[flow.split.step]]
id = "plan"
output = "{{work}}/parts/README.md"
brief = "Split this question into independent parts: {{task}}. Write one file per part in {{work}}/parts/, named NN-slug.md, and list them in {{work}}/parts/README.md."

[[flow.split.step]]
id = "dig"
role = "research"
after = ["plan"]                  # globbed once plan is done
foreach = "{{work}}/parts/[0-9]*.md"   # one worker per part, of this run only
max_items = 6                     # more files need you; default 12
parallel = 3                      # at most 3 at once
output = "research/{{item.name}}"
brief = "Research the part in {{item.path}}. Write your findings to research/{{item.name}}."

[[flow.split.step]]
id = "synthesize"
role = "summarize"
after = ["dig"]                   # join: waits for every copy
output = "research/SUMMARY.md"
brief = "Synthesize {{dig.outputs}} into research/SUMMARY.md. The question is in {{plan.output}}."
```

Copies of a write role still take turns on the directory lock, so
`parallel` matters for read-only roles, unless the step has `worktree =
true` (each copy its own worktree, "Worktrees").

If the glob matches nothing, or more than `max_items`, nothing starts.
Without `after`, `routr flow start` fails, naming the step and the glob:
write the files, then start it again. With `after`, the step needs you,
with a note naming the glob and the directory. A planner that wrote
nothing is usually a bug. Have the files written (or remove the extra
ones), then `routr approve ID` makes it look again; or `routr cancel
CHAIN`.

Where finding nothing is a fine outcome (a review whose merge step wrote
no findings), say so with `allow_empty = true`: a glob that matches
nothing is then done at once with no copies (its note: "matched no
files"), and the steps after it run. In their briefs `{{dig.outputs}}`
reads `(none)`; in a run step's command it is no word at all. A judge
after it gets no result from it; its state lists the step under `empty`.
Too many matches still need you.

```toml
[[flow.review.step]]
id = "refute"
after = ["merge"]
foreach = "{{work}}/merged/[0-9]*.md"
allow_empty = true                # no findings: nothing to refute, go on
brief = "Try to refute the finding in {{item.path}}."
```

### Fan out over models

`models` is the other fan-out: one worker per model in the list, all with
the same brief, instead of one per matched file. The step must pin its
`harness` — the ids are that harness's own (`pi` takes
`openrouter/vendor/model`, `claude` takes `haiku`, `sonnet`, `opus`). The
list is known when the flow starts, so its copies are too: a `models`
step may come first, or after other steps. Each copy's model is the list
entry; in its templates `{{item.model}}` is the id as listed and
`{{item.name}}` the id made file-safe (`/` and `:` to `-`), so an output
like `{{work}}/answers/{{item.name}}.md` gives each model its own file.
The join is the same as a foreach's: a step after it waits for every
copy, and `{{step.outputs}}` lists every copy's output.

```toml
[[flow.bake-off.step]]
id      = "field"
role    = "research"              # write = false: copies write only into {{work}}
harness = "pi"
parallel = 4
models   = [
  "openrouter/deepseek/deepseek-v4-flash",
  "openrouter/z-ai/glm-5.3-flash",
  "openrouter/qwen/qwen3.8-flash",
]
output   = "{{work}}/answers/{{item.name}}.md"
brief    = """
{{task}}

You are one of several models answering this same prompt. Write your
answer to {{work}}/answers/{{item.name}}.md."""

[[flow.bake-off.step]]
id     = "compare"
after  = ["field"]
harness = "claude"
brief  = "Rank the answers in {{field.outputs}}."
```

`worktree = true` gives each copy its own worktree (a best-of-n over
models), and a pick judge over a `models` step offers each copy. A
`models` step has no `on_fail`, `gate`, `max_items` or `allow_empty`:
the list is the items, so gate a run step before it if you must. The
built-in `compare-models` flow is this shape with seven open-weight models
and a reviewer (`routr flow show compare-models --toml`).

### Run steps

Programs decide what programs can decide: tests, a linter, `git diff
--quiet`. A run step runs a shell command instead of a harness; no tab, no
session, and its exit status is the verdict.

```toml
[flow.fix]
description = "implement, then the check decides; loop until it passes"
params = ["task", "check"]        # --param check="go test ./..."

[[flow.fix.step]]
id = "implement"
role = "implement"
brief = "{{task}}"

[[flow.fix.step]]
id = "check"
after = ["implement"]
run = "{{check}}"                 # rendered like a brief, then sh -c in the step's cwd
expect = "pass"                   # default; "fail" = it must fail (a repro test, before the fix)
timeout = "10m"                   # default; a timeout is a fail
on_fail = { resume = "implement", brief = "The check failed; its log is {{check.output}}. Fix the code, not the test.", max = 5 }
```

- **Verdict:** pass when the exit status matches `expect` (0 for pass,
  anything else for fail); a timeout or a signal is a fail either way. A
  fail with `on_fail` loops as a review step does: the resumed worker gets
  the brief, the run step waits for it and runs again. A fail without
  `on_fail` is done, with verdict fail, and stops the steps after it: they
  wait for you, saying which check failed and where its log is (`routr
  wait` exits 4). `routr approve ID` on the run step lets them go on past
  it; its verdict stays fail. As a chain's last step, the fail is simply
  the chain's answer.
- **Output:** stdout and stderr go to `run.log` in its run directory
  (the first and last 512 KB of it); `{{check.output}}` is that log.
  Its final message has the command, exit status, verdict and the last 40
  lines.
- **Refs:** params go in as written, since whoever set them wrote shell.
  Every other ref (`{{x.output}}`, `{{x.final}}`, `{{work}}` …) is quoted
  for `sh`, so a file name cannot inject a command: write them bare, not
  inside quotes.
- **Lock and cwd:** a run step takes the directory lock, so it sees a
  tree no writer is changing. After exactly one worktree step (or a pick
  judge over worktree steps: its winner's), it runs in that worktree and
  takes that worktree's lock, so the checks on two attempts run side by
  side. Otherwise, after none or several, it runs in the flow's cwd and
  takes the main tree's lock.
- **Environment:** the watcher's, plus config `[env]`, the step's `env` and
  the `ROUTR_*` a worker gets; not `routr flow start --env`, which is for
  agent steps only.
- `routr cancel` kills its process group; a watcher that stopped mid-run
  leaves it to the next, which kills what is left of it (keeping the
  directory lock until it is gone) and runs it again. A server the command
  detached from its group (`setsid`, a compiler cache's) is left running
  and does not hold up the next run. A group whose command is gone but
  which still has processes may be another's that took its pid: routr
  leaves it, and the step fails saying how to look (`pgrep -ag`).

A pipe queues one with `routr start --run CMD` (`--expect`,
`--run-timeout`): `… | routr start --after - --run 'go test ./...'`.

### Judge steps

A closed question between steps goes to Jev:
"ship or fix?", "which attempt is best?". A judge step asks it instead of a
harness; no tab, no session, a fraction of a cent. The brief, rendered, is
Jev's instructions; the results of its `after` steps are the state.

```toml
[flow.review-panel]
params = ["task"]

[[flow.review-panel.step]]
id = "implement"
role = "implement"
brief = "{{task}}"

[[flow.review-panel.step]]
id = "sec"
role = "review"
after = ["implement"]
output = "{{work}}/SEC.md"
brief = "Review the diff for security. Write {{work}}/SEC.md."

[[flow.review-panel.step]]
id = "perf"
role = "review"
after = ["implement"]
output = "{{work}}/PERF.md"
brief = "Review the diff for speed. Write {{work}}/PERF.md."

[[flow.review-panel.step]]
id = "ship"
after = ["sec", "perf"]
judge = "jev"
brief = "Do the reviews leave a confirmed finding that must be fixed before this ships?"
pass = "ship as is"
fail = "at least one confirmed finding must be fixed first"
on_fail = { resume = "implement", brief = "Fix the confirmed findings in {{sec.output}} and {{perf.output}}.", max = 2 }
```

Best of N: `pick = true` asks Jev to pick one upstream worker. The options
are the agent workers of `after`, each foreach copy on its own; run steps
and judges in `after` are not options but context: their verdict, summary,
log or output and (a run step) command are in the state, with `after`
naming the entries each came after, so Jev sees which check tested which
attempt:

```toml
[[flow.best.step]]
id = "try"
foreach = "{{work}}/approaches/*.md"
output = "{{work}}/try-{{item.name}}"
brief = "Implement the approach in {{item.path}} in a scratch file, {{work}}/try-{{item.name}}."

[[flow.best.step]]
id = "best"
after = ["try"]
judge = "jev"
pick = true
brief = "Which attempt solves the task most simply and correctly?"

[[flow.best.step]]
id = "apply"
role = "implement"
after = ["best"]
gate = "you"
brief = "Apply {{best.output}}."
```

- **Verdict** (the default): Jev picks `pass` or `fail`; `pass` and `fail`
  say what each means (defaults: the work holds up / must be revised
  first). The verdict feeds `on_fail` as a review's does; a fail without
  `on_fail` is done and stops the steps after it, as a run step's does
  (`routr approve` on the judge lets them go on; as the last step, the fail
  is the chain's answer: "fix first"). A verdict
  judge has no output: refer to `{{ship.verdict}}` or `{{ship.final}}`.
- **Pick:** the verdict is always pass, and `{{best.output}}` is the
  winner's output (its final message if it wrote none). One candidate (a
  foreach that matched one file) wins without asking Jev; none (only run
  steps and judges, or an empty foreach) fails. A pick has no `on_fail`,
  `pass` or `fail`. With checks:

  ```toml
  [[flow.best.step]]
  id = "check-a"
  after = ["a"]
  run = "{{check}}"                 # one per attempt; a pass or a fail is information here

  [[flow.best.step]]
  id = "best"
  after = ["a", "b", "check-a", "check-b"]   # options: a and b; the checks are context
  judge = "jev"
  pick = true
  brief = "Which attempt passes its check and is simplest?"
  ```

  A check that fails stops the pick until you approve past it (`routr
  approve` on the check lets the chain go on with the fail in the state); give it `expect` or
  wrap the command (`{{check}} || true`) if a fail should not stop it.
- **State** (`state.json`; option ids are `<step>-<worker id>`):

  ```json
  {
    "judge": {"chain": "c1", "step": "ship", "mode": "verdict", "round": 1},
    "upstream": [
      {"id": "sec-2", "worker": 2, "step": "sec", "role": "review",
       "harness": "codex · high", "after": ["implement-1"], "verdict": "fail",
       "summary": "one injection", "output_path": ".routr/work/c1/SEC.md",
       "output": "…", "final": "…"}
    ],
    "files": [{"path": "DECISIONS.md", "content": "…"}]
  }
  ```

  `item` is a foreach copy's file name; `after` the ids of the entries it
  came after; `command` a run step's command as it ran; `output` and
  `final` are the text of the worker's output file (a run step's log) and
  of its current run's final message; `empty` lists foreach steps that matched no files
  (`allow_empty`); `files` are what `read = ["path", …]` names (refs as in a
  brief, inside the repo). All the text together is cut to about 80 KB,
  split fairly between the files (short ones whole, long ones
  keep their head and tail, with a line saying how much was cut), so it
  fits Jev's 32K-token context. After an `on_fail` round the state also
  has the worker the loop sent back, and `round` goes up.
- **Confidence:** a choice below `min_confidence` (default 0.6) needs you,
  with Jev's choice and probabilities in the note. `routr approve ID`
  takes Jev's choice; to decide otherwise, `routr cancel` the chain and go
  on by hand.
- **Never passes by default:** no `OPENROUTER_API_KEY` (in the watcher's
  environment or config `[env]`), or a Jev error after the client's
  retries, needs you; `routr approve ID` asks again.
- **Files:** `brief.md` is the instructions, `state.json` the state,
  `jev.json` the question, options and Jev's decision (id, probabilities,
  confidence, cost), its final message the choice, every option's probability,
  what it judged and the cost.
- It takes no directory lock. `routr cancel` drops its call; a watcher
  that stopped mid-call leaves it to the next, which asks again.

### Worktrees

Writers take turns on the directory lock: one at a time per tree. Where
several should write at once (best of N attempts, blind implementations,
one migration per file), give the step `worktree = true`: each of its
workers (each foreach copy) gets its own git worktree, works there side
by side with the others, and never touches the main tree.

```toml
[flow.best]
params = ["task", "check"]

[[flow.best.step]]
id = "a"
worktree = true
harness = "claude"
brief = "{{task}}"

[[flow.best.step]]
id = "b"
worktree = true
harness = "codex"
brief = "{{task}}"

[[flow.best.step]]
id = "check-a"
after = ["a"]
run = "cd {{a.worktree}} && {{check}}"

[[flow.best.step]]
id = "check-b"
after = ["b"]
run = "cd {{b.worktree}} && {{check}}"

[[flow.best.step]]
id = "best"
after = ["a", "b", "check-a", "check-b"]
judge = "jev"
pick = true
brief = "Which attempt passes its check and solves {{task}} most simply?"

[[flow.best.step]]
id = "merge"
after = ["best"]
gate = "you"
brief = "Merge the changes on {{best.branch}} (in {{best.worktree}}) into this tree."
```

- **Where:** `.routr/trees/<chain>/<step>-<id>`, on a new branch
  `routr/<chain>/<step>-<id>` (`<id>` the worker's id, never given out
  twice, so neither is ever reused). The watcher names it when it admits
  the worker and makes it (`git worktree add -b`) when the worker starts.
  `.routr/trees/` is gitignored.
- **From:** the commit HEAD pointed at in the flow's directory when the
  flow started, recorded then, so every attempt starts from the same
  commit even if the main tree moves on. Uncommitted changes in the main
  tree are not in it: the flow's base is the last commit, and `flow
  start` says so when the tree is dirty. Commit first what the attempts
  should see. A flow with a worktree step outside a git repo, or in one
  with no commit, does not start.
- **Cwd and lock:** the worker's cwd is its worktree (the same
  subdirectory of it, if the flow started in one), and the directory lock
  it takes is that worktree's, so it waits for no writer in the main tree
  or another worktree.
- **Files:** `{{work}}` and outputs under it stay in the main repo's
  `.routr/work/<chain>`, so the steps after it and judges find them. An
  output elsewhere (`output = "NOTES.md"`) is in the worktree. A foreach
  item from the repo (`foreach = "src/*.go"`) is the file in each copy's
  own worktree. As in every brief, these paths are absolute in the
  worker's brief and its `routr done --output`. The harness may write the
  work directory though it lies outside its cwd: Codex gets it as a
  writable root (`--add-dir`, as its run directory), Claude as an added
  directory (`--add-dir`); Pi has no sandbox. `routr done` finds the registry from the worktree
  (`ROUTR_STATE_DIR`, and git's common dir without it).
- **Refs:** `{{a.worktree}}` (absolute) and `{{a.branch}}` name a worktree
  step's, in a brief, a run step's command (quoted for `sh`) or a
  judge's `read`. A pick judge over worktree steps has its winner's:
  `{{best.branch}}`. A foreach worktree step has none (each copy has its
  own; `routr list` shows them). Other refs in a run step's command are
  relative to its own cwd: read them before you `cd`
  (`cat {{a.output}} && cd {{a.worktree}} && …`), or use `git -C
  {{a.worktree}}`. A run step after exactly one worktree step runs in that
  worktree and takes its lock ("Run steps"), so `cd {{a.worktree}}` is a
  no-op there; after several, it runs in the main tree and takes its lock.
- **Afterwards:** the worktree and its branch are kept: the worker's
  changes are in it, usually uncommitted (a Codex sandbox cannot write
  the repo's `.git`). `routr list` and `routr result ID` show where; you
  or the main agent merge or cherry-pick the winner (`git -C PATH add -A
  && git -C PATH commit`, then merge the branch). `routr cancel` leaves
  them. When retention drops the chain (cli.md, "Where state lives") it runs
  `git worktree remove` on each: a worktree with uncommitted or untracked
  changes stays where it is, and a branch is deleted only if nothing was
  committed on it. Remove one by hand with `git worktree remove --force
  PATH` and `git branch -D BRANCH`.
- **Failure:** a worktree that cannot be made (the branch or directory
  exists, git fails) fails the worker with git's message; it never runs
  in the main tree instead.

### What a flow file may say

| Key | Is |
|---|---|
| `description` | one line, for `routr flow list` |
| `params` | names every `routr flow start` must give (`--task`, `--in`, `--param k=v`); no others are accepted |
| `step.id` | letters, digits, `-`, `_`; unique; not `prev` or `item` |
| `step.role` | default `task`; the role supplies write/lock, harness preference, a default output and gate |
| `step.brief` | required, except on a run step; a template, rendered when the step starts |
| `step.after` | earlier step ids; after a fan-out step (foreach or models) means after every copy; no `after` means it starts at once. Steps that don't depend on each other run at the same time (write roles still take turns on the directory lock); a step after several waits for all of them |
| `step.gate` | `"you"` or nothing |
| `step.output` | a relative path inside the directory (or the work directory); may use params, `{{chain}}`, `{{work}}` and `{{item.*}}` |
| `step.interactive` | you talk to it; it ends only on `routr done` |
| `step.harness`, `model`, `effort` | pins; `effort` in a flow file is low, medium or high, the levels every harness takes (`--effort` on the command line also takes a harness's own, such as claude's `max`). `routr flow start --harness`, `--model` and `--effort` win over them for every agent step; a `--harness` other than the step's drops its `model` unless `--model` is given ([cli.md](cli.md), "Commands") |
| `step.env` | `{ K = "V" }`: exported in its tab (a run step's command too), over config `[env]`; `routr flow start --env` wins over it on an agent step, and does not reach a run step. Kept for the worker's life, resumes included; a leading `~/` is expanded; routr's own `ROUTR_*` variables are refused ([cli.md](cli.md), "Environment a worker gets"). Not on a judge step |
| `step.args` | `["--chrome"]`: extra harness arguments, after config's `[harness.*] args`; the ones routr manages (`--session-id`, `--resume`, `--continue`, `--print`, Codex's `resume` …) are refused. Not on a run or judge step |
| `step.answer` | who answers its questions: `human`, `agent` or `jev` ([cli.md](cli.md), "Who answers"); `routr flow start --answer` wins over it; default config `[questions] answer`, else `human`. Not on a run or judge step |
| `step.verdict` | the brief asks for pass/fail (`on_fail` implies it); a fail without `on_fail` halts the steps after it until `routr approve` |
| `step.write` | `false`: it takes no directory lock, whatever its role (a step that writes only into `{{work}}`: plans, notes); default the role's. Not on a run or judge step |
| `step.on_fail` | `{ resume, brief, max }`: `resume` an upstream step, `max` > 0 |
| `step.foreach` | a glob over params, `{{chain}}` and `{{work}}`, relative to the working directory; globbed when the flow starts, or with `after` once those are done |
| `step.models` | a models step: one worker per model in the list, the step's pinned `harness` own ids (`openrouter/...` for pi); the list is the items, so no `max_items` or `allow_empty`; not on a run or judge step |
| `step.max_items` | a foreach step: more matches need you; > 0, default 12 |
| `step.parallel` | a fan-out step (foreach or models): at most this many copies run at once (others wait with a note); > 0, default no limit |
| `step.worktree` | its own git worktree per worker (each fan-out copy), from HEAD when the flow started ("Worktrees"); not on a run or judge step |
| `step.allow_empty` | a foreach step: a glob that matches nothing is done with no copies instead of needing you; `{{x.outputs}}` then reads `(none)` |
| `step.run` | a run step: a shell command instead of a brief, a template like one; its exit status is the verdict |
| `step.expect` | a run step: `"pass"` (default: it must exit 0) or `"fail"` (it must not) |
| `step.timeout` | a run step: a Go duration > 0 (`90s`, `10m`); default 10m |
| `step.judge` | `"jev"`: a judge step; the brief is Jev's instructions, the `after` steps' results its state |
| `step.pick` | a judge step: pick one upstream agent worker instead of pass/fail; run steps and judges in `after` are context, not options |
| `step.pass`, `fail` | a verdict judge: what each verdict means, for Jev |
| `step.min_confidence` | a judge step: above 0, at most 1; a choice below it needs you; default 0.6 |
| `step.read` | a judge step: more files for the state, paths inside the repo; refs as in a brief |

Loading checks all of this and fails on anything else, naming the file,
flow and step: unknown keys, `after` or `resume` naming a later or
missing step, a loop without a `max`, and every ref in a template:

- `{{param}}` must be a declared param. `chain` and `work` are no
  param's names: `{{chain}}` and `{{work}}` work in every template.
- `{{step.*}}` must name an **upstream** step: one in `after`, directly or
  through another step. An earlier step that runs in parallel may not be
  done yet, so its files may not exist. `{{prev.*}}` is the first step in
  `after`.
- A fan-out step (foreach or models) has only `{{step.outputs}}`; any
  other step has `output`, `final` and `verdict`. `{{step.worktree}}` and
  `{{step.branch}}` only for a step with `worktree = true` that is not a
  fan-out step, or a pick judge with such a step in its `after`.
- `{{item.path}}` and `{{item.name}}` only inside a foreach step;
  `{{item.model}}` and `{{item.name}}` only inside a models step.
- `on_fail.brief` may also name the failing step itself
  (`{{review.output}}`).
- `foreach` may use params, chain and work only; `output` those and
  item.
- `max_items` only on a foreach step; `parallel` on a fan-out step
  (foreach or models), and > 0; `allow_empty` only on a foreach step.
- A fan-out step can't have `on_fail`, and `resume` can't name one:
  there would be no single worker to send the findings to.
- A run step has `run` and no `brief`, `role`, `harness`, `model`,
  `effort`, `answer`, `interactive`, `output`, `verdict` or `foreach` (nor
  `max_items`, `parallel`, `allow_empty`, `worktree`, `write`, `models`); `expect` and
  `timeout` only on a run step. Its `run` has the refs a brief has. It may have `gate`
  and `on_fail`, but `resume` can't name a run step: it has no session
  to resume.
- A judge step has `judge = "jev"`, `after` and a `brief`, and none of
  `role`, `harness`, `model`, `effort`, `answer`, `interactive`, `output`,
  `verdict`, `foreach`, `run`, `expect`, `timeout`, `worktree`, `write`
  (nor `max_items`, `parallel`, `allow_empty`, `models`). `pick`, `pass`, `fail`, `min_confidence`
  and `read` only on a judge step; `pass` and `fail` only without `pick`.
  A pick needs two agent steps (not run or judge steps) in `after`, or an
  agent fan-out step, and has no `on_fail`. A `read` entry is one path
  (no `{{x.outputs}}`), and without refs a relative one. `resume` can't
  name a judge step, and `{{x.output}}` can't name a verdict judge.

### Built-in flows

`routr flow list` names them and `routr flow show NAME --toml` prints one,
briefs and all. `plan-to-impl` and `research` are the two flows shown above;
`plan-redteam` is a one-shot plan, then a codex red-team of it. The others
(`adversarial-review`, `security-review`, `deep-research`, `fix-until-green`,
`repro-fix`, `debug-hypotheses`, `best-of-n`, `compare-models`) keep their
intermediate files and report in `{{work}}`, and use the `hunt` and `refute`
roles (cli.md, "Roles and config"). Their findings formats and the "first
line CONFIRMED / REJECTED" convention are in their briefs.

### c. The main agent, for judgment calls

The skill teaches the main agent to:
- start a named flow;
- build a chain with pipes;
- step into one that's running: `routr list`, `routr send`, `routr close`,
  or insert a step with `--after`.

Anything a flow can't express is decided here, not in the flow file.

## Rule 3: flow files stay dumb

A flow file has steps, `after`, `gate`, `on_fail` with a `max`,
`foreach` over a glob (with `max_items`, `parallel` and `allow_empty`),
`worktree`, `run`, and `judge`. It has no expressions, variables or conditions beyond pass/fail
and a judge's pick. Every workflow format that grows those becomes a bad
programming language (GitHub Actions, Argo). When a flow needs a real
decision, it gets one of these:

- **A judge step:** **Jev** (`judge = "jev"`, "Judge steps") reads the
  upstream files and gives a verdict or picks one upstream result;
  picking from calibrated options is what it is built for: "is this plan
  done?", "which attempt is best?". A cheap review worker with `verdict`
  is the other judge, for questions that need prose.
- **A run step:** a program decides (tests, a linter).
- **A gate:** you decide.
- **The main agent** changes the chain while it runs.
