---
name: routr
description: Start other coding agents (Claude Code, Codex, Pi) as workers in their own herdr tabs and get their results back, with the `routr` CLI. Use when the user wants another agent or model to do part of the work, e.g. "have codex red-team the plan", "get a second opinion", "review this with a different model", "fan out research on X", "hand this off", "plan this with a planner, then implement". Also use when resuming a session in a repo that may have routr workers running. If you are a routr worker (`ROUTR_ID` is set) you do not need this skill; your brief ends with how to finish.
---

# routr

`routr` starts whole harnesses (Claude Code, Codex, Pi) as **workers**, each in
its own herdr tab. You write a brief. routr picks the harness, model and effort
from the role, and gives you back the worker's final message and the file it
wrote. The user can watch any worker or talk to it in its tab. The CLI is the
reference: `routr help` lists every command and what it does, and `routr help
CMD` gives its flags and examples. Read these on demand:
[examples.md](examples.md) for worked examples, [flows.md](flows.md) to write
a flow file, and [cli.md](cli.md) for what `routr help` leaves out (state
layout, a worker's environment, who answers questions, roles and config).

## When to use it

| The user says | Do |
|---|---|
| "red-team / poke holes / second opinion / review with another model" | `routr start --role review` (routr avoids your own harness) |
| "dig into / research / fan out on X" | one `--role research` worker per sub-question, started together. Read-only, so they run in parallel. More workers only when the user asks for depth. |
| "hand this off", a well-defined task | `--role task` |
| "design / talk it through with X", going back and forth yourself | one worker, not `--interactive` (that's for the user talking in its tab; you'd never hear its first answer). Say in the brief that more turns follow and that it runs `routr done` only when you ask. Go on with `routr send ID '…'`. Between turns it shows `maybe_done`, which is expected here. Every turn stays in its one session, even after its tab closes. |
| "write a plan for X" | `--role task --output PLAN.md`: it writes the plan and finishes. `--role plan` is for planning *with the user*: it talks with them in its tab and finishes only when they agree, so don't `--wait` on it. Tell the user it's ready for them (`routr show ID`). |
| "plan it, then implement" | a chain or a flow (below). `implement` always stops at a gate. |
| "review this properly / is it ready to ship" | `routr flow start adversarial-review --param target=main...HEAD` (or a file or directory): four lenses on two vendors, a cold refuter per finding, then Jev's ship-or-fix call |
| "security review / look for vulnerabilities" | `flow start security-review --param scope=DIR` (or a git range): threat model, three hunters, a verifier per candidate |
| "research X in depth" | `flow start deep-research --task '…'`: it splits the question itself, then synthesizes and fact-checks |
| "fix X / make the tests pass" | `flow start fix-until-green --task '…' --param check='go test ./...'`: loops until the check passes, then audits what the implementer claims |
| a bug with a clear symptom | `flow start repro-fix --param bug='…'`: a failing test first, then the fix, which may not touch that test |
| a bug whose cause is unclear | `flow start debug-hypotheses --param bug='…'`: competing hypotheses tested in parallel, Jev picks the cause |
| "try a few ways / which approach is best" | `flow start best-of-n --task '…' --param check='…'`: Claude, Codex and Pi each in a git worktree; about 3× the cost, so only when asked |
| "how do different models do on X" / "compare models on this" | `flow start compare-models --task '…'`: seven open-weight models answer the same prompt at once, a Claude reviewer ranks them. Cheap (read-only answers), so fine to run when asked |

Don't delegate what you can finish in a minute yourself.

**Resuming a session, or asked what's running:** run `routr list` first. Pick
up from the files (PLAN.md, REDTEAM.md) and `routr result ID`. Don't start
work again that already exists.

## Write a brief the worker can act on alone

The worker sees only the brief, not this conversation. Include:
- the goal, and what "done" looks like;
- the file to write (`--output REDTEAM.md`);
- which files to read, by path (don't paste their contents);
- constraints, and decisions already made.

Before a plan, write DECISIONS.md (decisions, constraints, open questions) and
pass `--in DECISIONS.md`. Never paste the whole chat. routr adds the role's
preamble and tells the worker to finish with `routr done`.

## Commands

How you get a worker's result depends on your harness:
- **Pi with the routr extension:** start the worker and end your turn. A
  `[routr]` message wakes you with the result.
- **Claude Code with the routr plugin** (your system prompt says "The routr
  plugin is active"): start the worker without `--wait` and end your turn. A
  `[routr]` message wakes you with the result.
- **Claude Code without it:** start it with `--wait --timeout 9m` in one Bash call with
  `run_in_background: true`, tell the user it's running, and end your turn.
  The shell's exit wakes you with the result. Don't block on `routr wait` in
  the foreground: the user can't talk to you meanwhile. Shell calls die at 10
  min, so keep `--timeout` at 9m or less.
- **Codex:** start the worker, then `routr wait --timeout 9m ID`. routr
  talks to herdr through a socket your sandbox blocks: when it says "nothing
  was queued … sandbox", run the same command again with escalated
  permissions.

```sh
routr start --role review --title 'red-team plan' --output REDTEAM.md 'Red-team PLAN.md: …'   # prints the id
routr start --role research - <<'EOF'      # brief from stdin
…
EOF
routr wait --timeout 9m 3     # blocks; prints the final message (Claude Code: in the background)
routr result 3                # final message, then output: / verdict: lines
routr result --path 3         # only the file holding that result, to cat or grep
routr send 3 'Why does point 2 hold?'   # follow-up, same session (block: routr send --wait --timeout 9m 3 '…')
routr list                    # id, title, chain/step, role, harness·model·effort, state, age, output, note
routr show 3                  # its tab beside the user's pane (routr hide 3 puts it back)
routr answer 3 2 --by human --expect FP   # option 2 of a blocked worker's menu (see "When a worker asks")
routr retry 3                 # a failed worker (e.g. its start failed) again, in its place in the chain
routr cancel 3                # stop it (routr close 3 only closes the tab; the session stays resumable)
routr roles                   # roles, with the harness, model and effort each resolves to
```

Take a worker's result and files from routr: `routr result ID`, `routr result
--path ID`, the `output:` line, `final_path` in `--json`, and refs in a brief.
Never build a `.routr/runs/` path yourself. Which file holds the current result
is routr's to decide; a guessed `runs/ID/final.md` is usually missing or stale.

Other `start` flags: `--title T` (2–4 words naming the work, for its tab;
default: the brief's first line), `--harness claude|codex|pi`, `--model M`
(Claude: `haiku|sonnet|opus`; don't invent dated ids, retired ones fail. Codex:
the one `routr roles` names. Pi takes any `provider/id` it has a key for, e.g.
`openrouter/deepseek/deepseek-v4-flash`), `--effort low|medium|high` (Claude
also `xhigh`, `max`; Pi also `off`, `minimal`, `xhigh`, `max`; the research role
runs at low, so pass `--effort high` for open-ended brainstorming or synthesis),
`--cwd D`, `--interactive` (the user talks to the
worker; it ends on `routr done`), `--json` (any command). Two Claude
accounts, or an extra flag for one worker: `--env
CLAUDE_CONFIG_DIR=~/.claude-alt`, `--arg --chrome` (also `flow start --env`);
never edit routr's config to switch.

**Permissions:** add `--permissions` for the mode you run in yourself (the
examples leave it out): auto or accept-edits → `auto`; skip-permissions /
full access → `bypass`; asking for each approval → `default`. Pi runs
without approvals → `bypass`, and routr gives it to a Pi caller's workers
by itself: from Pi, leave `--permissions` out, so a mode the user set in
routr's config still wins. With the wrong mode, workers stall on prompts.

## Exit codes

`--wait`, `routr wait` and `routr send --wait` exit with:

| Exit | Means | Do |
|---|---|---|
| 0 | done | read the final message and the output file |
| 3 | still running (the timeout ran out) | nothing's wrong: run `routr wait --timeout 9m ID` again, as the output says (Claude Code: in the background) |
| 4 | needs you: blocked, gated, needs_you, closed before it finished, or a step waits on one that stopped or failed a check | tell the user the question or error from the output (a question the output says you answer: "When a worker asks"); see gates below. After an API error (e.g. a login problem) the user fixes, `routr send ID 'carry on'` resumes it, as it does a closed one. |
| 5 | failed or cancelled | report it; `routr result ID` has the details. The failure ends with its next step: `routr retry ID` for one that never started or has no session, else `routr send ID …` |
| 1 / 2 | error / usage | read the message; `routr <cmd> --help`. If routr itself seems broken (herdr unreachable, a harness not logged in), `routr doctor` says what and the next step |

`maybe_done` means the worker went idle without running `routr done`. Check its
output file before you trust the result.

## Gates: the user decides

Implementation waits at a gate (`gated`). Summarize the plan and the review for
the user and ask. Run `routr approve ID` (or the chain id) **only after the
user says yes in this conversation**. Never approve on your own judgment.

## When a worker asks

A blocked worker asks a question: a permission prompt or a menu. routr
decides who answers it (`answerer` in `routr list --json`), and `routr wait`
and the `[routr]` message say who and give the commands. It is set by
routr's config `[questions]`, a flow step, or `--answer` on `start` and
`flow start`; pass `--answer` only when the user asks for it.

**human** (the default): the user answers.
- **Never choose an option yourself**, even an obvious one. The user's
  answer to one question does not cover the next.
- Tell the user what it asks, not only "Do you want to proceed?": the output
  says what it is about ("It asks about Bash: rm -rf build").
- They answer in its tab (`routr show ID`), or tell you an option. Then run
  the command the output gives: `routr answer ID N --by human --expect FP`.
  `--expect` sends the key only to the question the user saw. If another took
  its place, nothing is pressed, and it exits 4 and prints the new one: tell
  the user and ask again.

**agent**: you answer, for the user.
- Before you decide, read the project docs the output names, even when the
  answer looks obvious: they can forbid what the brief asks. Read its brief
  too if you did not write it.
- Answer only if what it asks about is allowed by the docs, within the
  brief, and can be undone, and the docs and brief settle which option is
  right. Deleting files, pushing, sending or installing cannot be undone.
- Run `routr answer ID N --by agent --expect FP` (`--expect` is required),
  then tell the user in one line what you chose and why.
- Never pick an option that saves a rule or lasts past this call ("Yes, and
  don't ask again …", "always allow …", "… for this session"). routr refuses it.
- Otherwise ask the user, as for human, and pass their choice with `--by human`.
- A question with no menu is the user's.

**jev**: Jev, TypeSafe's `typesafe/jev-1.13` model called through OpenRouter, decides by the rules for agent.
- While it decides, there is nothing for you to do: `routr wait` goes on
  waiting. When it answers, the worker runs on.
- When it does not (unsure, a call it may not allow, an error), it hands
  the question to the user or to you (config `[questions] fallback`). The
  output then says, under the question, what Jev made of it ("Jev did not
  answer it: …") and who answers: follow human or agent above.

## Chains and flows

Each `start` in a pipe prints its id. The next one reads it with `--after -`
and joins the same chain. `--step NAME` names a step for `{{NAME.output}}`.

```sh
routr start --step plan --role plan --interactive --in DECISIONS.md 'Write PLAN.md for: add OAuth. Decisions: {{in}}' \
  | routr start --after - --step review --role review 'Red-team {{plan.output}}; write REDTEAM.md; give a verdict' \
  | routr start --after - --role implement 'Implement {{plan.output}}; address {{review.output}}'
```

Refs: `{{task}}`, `{{in}}`, `{{prev.output}}`, `{{prev.final}}`,
`{{STEP.output}}`, `{{STEP.verdict}}`, `{{chain}}`, `{{work}}` (the chain's
scratch directory); all of them: cli.md, "Commands". `routr start --after - --run 'go test ./...'` queues a
command instead of a worker: its exit status is the verdict, and a fail
stops the steps after it (they wait for you; `wait` exits 4). As the last
step, the fail is the chain's answer: `wait` exits 0 with `verdict: fail`. In a flow, `judge = "jev"` asks Jev a
closed question about the steps before it: pass/fail, or `pick = true` for
the best of them (flows.md, "Judge steps"). Any step's fail verdict without `on_fail`
halts the steps after it until `routr approve`; a step whose findings are
the work, not a no, should not be asked for a verdict (role `hunt`).
A step that only writes into `{{work}}` (plans, notes): `write = false`, so
it does not take the repo's lock. Named flows cover the common shapes:
`routr flow list`, `routr flow show NAME`, `routr flow start NAME --task '…'
--in DECISIONS.md`. This prints a chain id; `routr wait CHAIN` waits for the
whole chain. Prefer a flow when one fits.

When one nearly fits, run a scratch flow rather than writing to
`.routr/flows/`: `routr flow show NAME --toml` prints it, change what the job
needs (a step's `model`, `harness`, `effort`, a foreach's `max_items` and
`parallel`, a step more or less), then `routr flow start --file FILE` (or
`--file -` with a heredoc). It runs once and replaces nothing. Say what you
changed and why before you start it; ask first when it is much bigger than
the original (many more workers, or a stronger model on many). If the user
wants to keep it, move the chain's `.routr/work/<chain>/flow.toml` into
`.routr/flows/`.

## If you are the worker

If `ROUTR_ID` is set, you are a routr worker. Do the brief. Then finish with
the `routr done` command exactly as the brief spells it (the binary's full
path, which your permissions allow), with a one-line summary of what you did
in single quotes.
Don't start workers of your own unless the brief says to.
