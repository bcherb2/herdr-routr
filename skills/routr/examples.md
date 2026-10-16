# routr: worked examples

Read on demand. `routr help` is the authority on flags, and
[flows.md](flows.md) on flow files; these show shape.

## Second opinion on a plan, then argue with it

```sh
id=$(routr start --role review --output REDTEAM.md - <<'EOF'
Red-team PLAN.md (the auth rework). Look for: wrong assumptions about the token
store, missing migration steps, anything that breaks existing sessions.
Constraints already decided: no new services; Postgres only (see DECISIONS.md).
Write REDTEAM.md: numbered findings, each with severity and a fix. Verdict:
fail if any finding blocks implementation.
EOF
)
routr wait --timeout 9m "$id"       # Codex; Claude Code and Pi wait differently (SKILL.md, "Commands")
```

The user disagrees with a finding, so ask the same worker. It keeps its context:

```sh
routr send --wait --timeout 9m "$id" 'Finding 2 assumes tokens are cached per node; PLAN.md §3 says they are not. Does 2 still hold?'
```

## Research fan-out

Split the question and write one brief per worker. Start them all before
waiting on any, so they run in parallel.

```sh
mkdir -p research
for topic in queues retries idempotency; do
  routr start --role research --output research/$topic.md \
    "How do Temporal, Inngest and Restate handle $topic? Cite sources. Write research/$topic.md."
done
routr list                           # ids, then: routr wait --timeout 9m ID for each
```

Synthesize from the files in research/, not from memory of the final messages.

To have a worker do the split, use a flow: a planner step writes one file
per part into `{{work}}`, and a `foreach` step with `after` starts one
worker per file once the planner is done (flows.md, "Fan-out": the `split`
flow). If the planner wrote no files (or too many), that step needs you;
once the files are right, `routr approve ID` makes it look again.

## Implement until the tests pass

A run step runs a command instead of a harness; its exit status is the
verdict. The built-in `fix-until-green` loops the implementer on it:

```sh
routr flow start fix-until-green --task '…' --param check='go test ./...'
```

In a pipe: `… | routr start --after - --run 'go test ./...'`. A fail there
stops the steps after it, even ones piped on later: they wait for you
(`routr wait` exits 4; `routr result ID` shows the end of the log). Only on
the user's yes, `routr approve ID` on the check lets them go on past it;
its verdict stays fail. A failing last step just ends the chain with
`verdict: fail`. Writing one into a flow file: flows.md, "Run steps".

## Let Jev decide between steps

A judge step asks Jev (OpenRouter, `OPENROUTER_API_KEY`) a closed question
about the results of its `after` steps: pass or fail ("ship or fix?"), or
`pick = true` for the best of several attempts. How to write one:
flows.md, "Judge steps"; attempts that write side by side each get a git
worktree: flows.md, "Worktrees".

A judge that needs you (Jev unsure, no key, an error) says so in its note;
tell the user Jev's choice and probabilities (`routr result ID`), and only
on their yes `routr approve ID` (takes Jev's choice, or asks again). The
worktrees of a best-of-n are kept after the chain (`routr list` and
`routr result ID` show them), their changes usually uncommitted: merge the
winner, then `git worktree remove PATH`.

## Hand off a task

```sh
routr start --role task --output NOTES.md - <<'EOF'
Goal: make `go test ./internal/flow/...` pass on Go 1.25 (it fails on
maps.Keys, which returns an iterator now). Only touch internal/flow.
Done when the tests pass and NOTES.md lists what changed and why.
EOF
```

## Plan, review, gate, implement

1. Write DECISIONS.md from the conversation: what's decided, the constraints,
   and the open questions.
2. Queue the chain. The plan worker is interactive: the user argues with it in
   its tab until it runs `routr done`.

```sh
routr start --step plan --role plan --interactive --in DECISIONS.md \
    'Write PLAN.md for: {{in}}. Ask the user about the open questions.' \
  | routr start --after - --step review --role review \
    'Red-team {{plan.output}} against {{in}}. Write REDTEAM.md; verdict fail if anything blocks.' \
  | routr start --after - --role implement \
    'Implement {{plan.output}}. Address the findings in {{review.output}}.'
```

Or use the named flow, if `routr flow list` has one:

```sh
routr flow start plan-to-impl --task 'add OAuth login' --in DECISIONS.md   # prints the chain id
```

3. When the chain reaches `gated` (exit 4 from `routr wait`, or a `[routr]`
   message in Pi), summarize PLAN.md and REDTEAM.md for the user. Ask whether
   to implement. On their yes: `routr approve ID`.

## Coming back to a repo

```sh
routr list            # what's queued, running, blocked, gated or finished
routr result 7        # a finished worker's final message and output file
routr send 7 '…'      # continue a closed worker; its session resumes in a new tab
```

## Structured output

`--json` prints records as data (cli.md, "Commands"):

```sh
routr list --json | jq -r '.[] | select(.state=="blocked") | "\(.id) \(.question)"'
```
