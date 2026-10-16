# The default flows, as flowcharts

How each of the eleven built-in flows runs, step by step, for people
choosing one. The flow files are in `internal/flow/builtin.toml` (`routr
flow show NAME --toml` prints one with its briefs), and the step kinds are
in [flows.md](../skills/routr/flows.md). The first three write PLAN.md,
REDTEAM.md or `research/` in the repo; the other eight keep their
intermediate files and report in `{{work}}`, the chain's own scratch
directory.

Legend, the same in every chart:

```mermaid
flowchart LR
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef run fill:#dcfce7,stroke:#15803d,color:#052e16
    classDef jev fill:#fef3c7,stroke:#b45309,color:#3b2303
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    A["agent worker<br/>step · harness, if pinned"]:::agent
    F[["foreach: one worker per file<br/>(models: one per listed model)"]]:::fan
    R[/"run step: a command, exit status is the verdict"/]:::run
    J{"Jev judge step"}:::jev
    G{{"gate: waits for routr approve"}}:::gate
    O(["start or end"]):::out
    A --> F
    F -. "fail: resume, ≤N rounds" .-> A
```

- A harness is named where the flow pins it. Elsewhere the role picks it;
  `review`, `hunt` and `refute` avoid the caller's harness. A `hunt` step
  lists candidates for later steps to verify and gives no verdict.
- Steps with no arrow between them run side by side, and a step waits for
  every arrow into it. An arrow into or out of a box of steps means every
  step in it.
- The charts leave out briefs, file names and caps (how many copies a
  fan-out makes, how many run at once); `routr flow show NAME --toml` has
  them.
- **Dashed edges** are `on_fail` loops: the failing step resumes the
  worker it points at with the failure, then runs again once that worker
  is done (what else runs again: flows.md, "Review loops"). At the round
  cap the failing step needs you (`routr approve` accepts its result).
- A run or judge step that fails with no `on_fail` is done, with verdict
  fail, and the steps after it wait for `routr approve`. As the last
  step, it ends the chain with that verdict.
- A Jev judge below its confidence floor (0.6), or without a key, needs
  you; `routr approve` takes its choice or asks again.

## plan-redteam

`--task '…'`. A one-shot plan (role `task`, since the `plan` role talks
with you), then codex at high effort red-teams it.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>task"]):::out
    P["plan<br/>→ PLAN.md"]:::agent
    R["redteam · codex<br/>→ REDTEAM.md"]:::agent
    OK(["done: PLAN.md and REDTEAM.md"]):::out

    S --> P -- PLAN.md --> R --> OK
```

## plan-to-impl

`--task '…' --in DECISIONS.md`. You plan with a worker in its tab, a
reviewer on another harness red-teams the plan, and implementation waits
for you.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>task, in"]):::out
    P["plan<br/>you talk to it → PLAN.md"]:::agent
    R["review<br/>→ REDTEAM.md"]:::agent
    G{{"gate"}}:::gate
    IM["implement"]:::agent
    CR["code-review<br/>the diff against PLAN.md"]:::agent
    OK(["done: implemented and reviewed"]):::out

    S --> P --> R
    R -. "fail, ≤3" .-> P
    R -- pass --> G --> IM --> CR
    CR -. "fail, ≤2" .-> IM
    CR -- pass --> OK
```

## research

`--in DIR`. You (or your agent) write one brief per file in DIR; a
researcher takes each, and one synthesis joins them.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>in: a directory of briefs"]):::out
    D[["dig<br/>one per brief"]]:::fan
    SY["synthesize<br/>→ research/SUMMARY.md"]:::agent
    OK(["done"]):::out

    S --> D --> SY --> OK
```

## adversarial-review

`--param target=main...HEAD` (or a file or a directory). Four lenses on
two vendors find problems; a cold refuter tries to disprove each one;
only what survives counts. REVIEW.md lists the confirmed problems, then
the rejected ones with reasons, and Jev decides whether it can ship as it
is.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef jev fill:#fef3c7,stroke:#b45309,color:#3b2303
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>target"]):::out
    subgraph hunt["four hunters, side by side"]
        direction TB
        C["correctness<br/>codex"]:::agent
        D["design<br/>claude"]:::agent
        R["risk<br/>codex"]:::agent
        P["premortem<br/>claude"]:::agent
    end
    M["merge<br/>one file per problem"]:::agent
    X[["refute<br/>one per problem"]]:::fan
    W["report<br/>→ REVIEW.md"]:::agent
    J{"ship? · Jev"}:::jev
    OK(["done: ship"]):::out
    NO(["done, verdict fail:<br/>fix first"]):::out

    S --> hunt --> M --> X --> W --> J
    J -- pass --> OK
    J -- fail --> NO
```

## security-review

`--param scope=DIR` (or files, or a git range). A threat model steers
three hunters: inject (untrusted data into sinks), access (authn, authz,
secrets) and robust (races, exhaustion, environment). A verifier per
candidate checks reachability, impact and existing defenses.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef jev fill:#fef3c7,stroke:#b45309,color:#3b2303
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>scope"]):::out
    MAP["map · claude<br/>→ THREATS.md"]:::agent
    subgraph hunt["three hunters, side by side"]
        direction TB
        I["inject<br/>codex"]:::agent
        A["access<br/>claude"]:::agent
        R["robust<br/>codex"]:::agent
    end
    M["merge<br/>one file per candidate"]:::agent
    V[["verify<br/>one per candidate"]]:::fan
    W["report<br/>→ SECURITY.md"]:::agent
    J{"ship? · Jev"}:::jev
    OK(["done: nothing<br/>high or critical"]):::out
    NO(["done, verdict fail:<br/>a confirmed high<br/>or critical"]):::out

    S --> MAP --> hunt --> M --> V --> W --> J
    J -- pass --> OK
    J -- fail --> NO
```

## deep-research

`--task 'the question'`. The flow splits the question itself, researches
the parts in parallel, and fact-checks its own synthesis. The researchers
look on the web unless the question names this repository or its files.
The fact-check does not trust the findings files: it opens the cited URL
or file:line of every claim the report rests on (at least five) and fails
if one of them is contradicted or could not be opened.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>task"]):::out
    SP["split<br/>into 2–5 briefs"]:::agent
    D[["dig<br/>one per brief"]]:::fan
    SY["synthesize<br/>→ REPORT.md"]:::agent
    F["factcheck<br/>opens each cited source"]:::agent
    OK(["done: REPORT.md"]):::out
    CAP{{"needs you"}}:::gate

    S --> SP --> D --> SY --> F
    F -- pass --> OK
    F -. "fail, ≤2" .-> SY
    F -- "fail at the cap" --> CAP
```

## fix-until-green

`--task '…' --param check='go test ./...'`. Tests decide when it is done,
then a reviewer checks that the implementer's claims are true. The
implementer may not weaken tests or commit.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef run fill:#dcfce7,stroke:#15803d,color:#052e16
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>task, check"]):::out
    G{{"gate"}}:::gate
    IM["implement"]:::agent
    V[/"verify<br/>run the check"/]:::run
    AU["audit<br/>its claims against the diff"]:::agent
    GR[/"green<br/>run the check again"/]:::run
    OK(["done: green and audited"]):::out

    S --> G --> IM --> V
    V -. "fail, ≤5" .-> IM
    V -- pass --> AU
    AU -. "fail, ≤2" .-> IM
    AU -- pass --> GR
    GR -. "fail, ≤2" .-> IM
    GR -- pass --> OK
```

After an audit round the implementer revises, `verify` runs the check on
the revision, and the audit looks again. `green` runs the check once more
after the audit passes.

## repro-fix

`--param bug='…'`. A failing test is written first and becomes the spec;
the fixer may not change it, and hashes prove it didn't.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef run fill:#dcfce7,stroke:#15803d,color:#052e16
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>bug"]):::out
    RE["reproduce<br/>a failing test, no fix"]:::agent
    FA[/"fails<br/>the test must fail"/]:::run
    SN[/"snapshot<br/>hash the tests"/]:::run
    G{{"gate"}}:::gate
    FX["fix<br/>tests untouched"]:::agent
    RV["review<br/>the cause, not the symptom?"]:::agent
    VE[/"verify<br/>same hashes, test passes"/]:::run
    OK(["done: fixed, test untouched"]):::out

    S --> RE --> FA
    FA -. "passed, ≤2" .-> RE
    FA -- "failed" --> SN --> G --> FX --> RV
    RV -. "fail, ≤2" .-> FX
    RV -- pass --> VE
    VE -. "fail, ≤3" .-> FX
    VE -- pass --> OK
```

## debug-hypotheses

`--param bug='…'`. Competing explanations are tested in parallel, each by
a worker trying to prove it false; Jev picks the cause from the evidence.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef jev fill:#fef3c7,stroke:#b45309,color:#3b2303
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>bug"]):::out
    T["triage<br/>2–4 hypotheses"]:::agent
    IN[["investigate<br/>one per hypothesis:<br/>try to falsify it"]]:::fan
    P{"pick · Jev<br/>the root cause"}:::jev
    G{{"gate"}}:::gate
    FX["fix<br/>and a regression test"]:::agent
    RV["review"]:::agent
    OK(["done: fixed"]):::out

    S --> T --> IN --> P --> G --> FX --> RV
    RV -. "fail, ≤2" .-> FX
    RV -- pass --> OK
```

## best-of-n

`--task '…' --param check='…'`. Three harnesses attempt the same task,
each in its own git worktree; a check runs in each; Jev picks. About three
times the cost of one attempt, so only when asked.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef run fill:#dcfce7,stroke:#15803d,color:#052e16
    classDef jev fill:#fef3c7,stroke:#b45309,color:#3b2303
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#450a0a
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>task, check"]):::out
    G{{"go<br/>one gate for all three"}}:::gate
    subgraph tries["each in its own worktree"]
        direction TB
        A["a · claude"]:::agent --> CA[/"check a"/]:::run
        B["b · codex"]:::agent --> CB[/"check b"/]:::run
        C["c · pi"]:::agent --> CC[/"check c"/]:::run
    end
    P{"pick · Jev"}:::jev
    RV["review<br/>the winner → REVIEW.md"]:::agent
    OK(["done: merge the<br/>winner yourself"]):::out

    S --> G --> tries --> P --> RV --> OK
```

Each check runs in its attempt's worktree and takes that worktree's lock,
so the three run side by side. A check records its exit status and never
fails; Jev picks a green check first, then the smallest, clearest change,
and the review says how to merge the winner. Codex's sandbox can't write the main repo's
`.git`, so Codex can't commit in its worktree, and its attempt stays
uncommitted there.

## compare-models

`--task '…'`. Seven open-weight models — deepseek, glm, qwen, kimi,
mistral, minimax and seed, all over OpenRouter through pi — answer the same
prompt at once, each writing its own answer file; a claude reviewer ranks
them. The point is seeing how the models differ on a prompt you care
about, not the work itself: the copies are read-only (role `research`) and
write only into `{{work}}`.

```mermaid
flowchart TD
    classDef agent fill:#f1f5f9,stroke:#64748b,color:#1e293b
    classDef fan fill:#e0e7ff,stroke:#4338ca,color:#1e1b4b
    classDef out fill:#ffffff,stroke:#94a3b8,color:#334155

    S(["start<br/>task"]):::out
    F[["field<br/>one pi worker per model,<br/>all seven at once"]]:::fan
    C["compare · claude<br/>→ COMPARISON.md"]:::agent
    OK(["done: the ranking"]):::out

    S --> F --> C --> OK
```

Pi resolves each `openrouter/...` id against its own providers, so the
list needs the ones it serves (and `OPENROUTER_API_KEY`). A copy that
names a model pi cannot serve fails its worker, loudly. The same shape
with `worktree = true` is a best-of-n over models; the models list is
yours to change: copy the flow into `.routr/flows/` and edit it
(`routr flow show compare-models --toml` prints it).
