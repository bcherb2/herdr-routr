# routr for Pi

A Pi extension, and a front end for routr (see "Front ends over one
CLI" in [docs/architecture.md](../docs/architecture.md#design-choices)).
You ask Pi in plain words ("have codex red-team the plan"). Pi's model uses the
[routr skill](../skills/routr/SKILL.md) to start workers with the `routr`
CLI, and the extension does the rest:

- **Steps** above the editor (the default view): one block per chain, one
  row per step, like a todo list. A foreach step's copies are one row,
  `implement ×3  ✔1 ▶2 · max 2 at once` (`×?` until it expands); a review
  loop shows `round 2/3`; a step at a gate shows `⏸` and the `/routr
  approve` that opens it; a step that asks you shows what it asks and the
  key that answers it ("Answering a question", below). Workers started
  outside a flow are an "ad hoc"
  block, those of one role started in one run on one row. A running
  single-worker step shows its latest tool call. Past 12 lines, the steps
  that went well fold into one line, then the queued ones. A finished
  chain shrinks to one line, then goes.
- **Worker cards**, the other view (`/routr view cards`): one per worker in the repo, showing id,
  role, chain/step, harness·model·effort, state and age. A worker that is
  blocked, gated or needs you turns red with what gets you to it: for a
  question, what it asks and the key that answers it, for a gate the
  `/routr approve` that opens it. The footer shows `routr ▶2 !1`
  (running · needs you).
- **Answering a question.** A worker that asks you has one red line in
  either view: `⚠ claude 7 asks about Bash: go test ./... · alt+q answer`.
  It names the tool call the question is about, or else the question's
  first line, cut short so the key stays on screen.
  - **alt+q**, or `/routr answer` with no arguments (for a terminal without
    alt keys), opens a picker: the question, its options numbered as on
    the worker's screen, then "Open its tab" and "Later". An option that
    saves a rule or lasts past this call says "(lasts past this call)":
    you may pick it, the model and Jev may not. A question with no menu
    gets only "Open its tab" and "Later".
  - An option goes as `routr answer N K --by human --expect FP`, so it is
    pressed only on the question you saw. If the worker asks something
    else by then, nothing is pressed, and it says so: press alt+q again to
    see the new question. "Open its tab" runs `routr show N --focus`: the
    worker's pane comes beside Pi's, with the keys in it.
  - The picker closes itself when its question goes (answered in the
    worker's tab, or by the model) or changes, and says which.
  - Several workers asking: alt+q asks which one first, the oldest
    question at the top. `/routr answer N` goes straight to worker N.
  - Nothing opens by itself. A dialog that pops up takes the keys you were
    typing, and `1`, `y` or Enter could pick an option by accident.
  - Every question but one Jev decides gets the line and the picker. With
    `answer = "agent"` the model decides first, but it may hand the
    question to you in words, which the extension can't see, and you may
    always answer: `--expect` keeps the slower of you two from pressing a
    key on the next question.
  - alt+q is free in Pi on Linux and macOS. On Windows and WSL Pi binds it
    to restoring queued messages; the extension takes it over there, and
    Pi warns about it at startup.
- **Live tool calls** on each running worker's card, tailed from its
  transcript (Claude Code, Codex or Pi JSONL). They are for you; none of it
  reaches the model.
- **Waking the model.** When a worker this session started is done,
  maybe_done or failed, or blocked, gated or needs you, the extension sends
  Pi's model a `[routr]` message with `triggerTurn`. The message holds the
  state, the `routr result` text and the output file, so the model carries
  on without being asked.
  - Workers started together are reported together: a 4-way research
    fan-out wakes the model once, when the last one finishes.
  - A chain wakes the model at its gate, with the plan and the review.
    A step that fails or stops (maybe_done, cancelled) wakes it too: the
    steps queued after it can't start, so nothing is held for them.
  - Nothing is held longer than `ROUTR_WAKE_HOLD` (2 minutes): a plan
    whose chain implements for an hour is reported 2 minutes after it.
  - A worker that needs a person, or that failed, isn't held for its
    group.
  - While the model's turn runs, a worker that needs a person goes into
    that turn (steer); other news waits for the turn's end, so news the
    model reads itself meanwhile (`routr result`, `routr wait`) is not
    sent again. After you interrupt a turn (Esc), nothing is sent until
    your next turn ends: you are about to type. The status count still
    shows it.
  - A worker cancelled (`routr cancel`, `/routr cancel`) wakes it for
    nothing it had to say before; a done result stays news. A wake Pi
    already queued can't be recalled.
  - For a question, the message says who answers it, as routr decided
    (`answerer`; [cli.md](../skills/routr/cli.md), "Who answers"). By
    default that is you: the model is told what it asks about and not to
    choose an option itself. You answer with alt+q, in the worker's tab,
    with `/routr answer N K`, or by telling the model, which then runs
    `routr answer` with `--by human` and the question's fingerprint. With `answer =
    "agent"` the message gives the model the rules `routr wait` gives,
    with the project docs and the worker's brief to read, and the model
    says in one line what it chose and why. The extension doesn't write
    these rules: it prints the lines `routr show N --context --json`
    gives (`answer`), and if routr can't say, it tells the model to run
    `routr wait N` first. With `answer = "jev"` the model is not
    woken: the card says "Jev decides its question", and the worker runs
    on once Jev answered. When Jev hands the question over (`fallback`,
    you or the model), the message says what Jev made of it, then who
    answers, as above.
  - While the model runs `routr wait` (or `start --wait`, `send --wait`),
    the workers that call will return don't wake it. If the call ends
    without returning a result (it timed out, or was aborted), the wake
    follows.
- **`/routr`**: `list`, `show N`, `hide N`, `result N`, `answer N K`,
  `approve N`, `cancel N`, `close N` run the CLI for you.
  - `answer N K` passes `--by human` and, when the extension saw worker
    N's question, `--expect` with its fingerprint. `answer` and `answer N`
    open the picker.
  - `wake mine|all|off` sets whose workers wake the model.
  - `view steps|cards` switches the view; `view` alone toggles it.
  - Anything else goes to the model as "Use the routr skill to do this: …".
- **The routr skill** is loaded with the extension, so you don't install it
  separately for Pi.

The extension only calls the CLI (`routr list --json`: active workers and
those changed in the last hour; `routr result` when it wakes the model;
`routr show N --context --json` for a question the model answers, and for
the picker's options; `routr answer N K --by human --expect FP` from the
picker; …).
It watches `.routr/state/` to poll sooner after a change, and never reads
the registry.

## Install

It needs Pi ≥ 1.0, `routr` on `PATH` (or `ROUTR_BIN`) and Node ≥ 22.19. It
has no runtime dependencies beyond what Pi provides.

Try it for one run:

```sh
pi -e /path/to/herdr-routr/pi/src/index.ts
```

Keep it: add the directory as a local package, from this repo:

```sh
pi install ./pi          # writes the path into ~/.pi/agent/settings.json "packages"
```

Or add it to `"packages"` in `~/.pi/agent/settings.json` by hand, or to
`.pi/settings.json` for one project. Pi reads `package.json` → `pi.extensions`.
A local path is loaded in place, so `git pull` updates it.

The skill is found at `../skills/routr` relative to this directory, and the
extension imports the shared front-end core from `../frontend/core`. If you
copy `pi/` somewhere else, copy `skills/routr` to `pi/skills/routr` and
`frontend/core` to `frontend/core` beside it.
If you already installed the skill globally (`~/.pi/agent/skills/routr`),
set `ROUTR_PI_SKILL=0`, or Pi warns about the duplicate name and keeps the
first one it finds.

With the extension loaded you don't need the `/routr` prompt template in
[commands/pi](../commands/pi/routr.md): extension commands take precedence.

## Settings (environment)

| Var | Default | |
|---|---|---|
| `ROUTR_BIN` | `routr` | the CLI to run |
| `ROUTR_PI_WAKE` | `mine` | `mine`: wake for workers this session started; `all`: any worker in the repo; `off`: cards only. `/routr wake …` changes it for the session |
| `ROUTR_WAKE_HOLD` | `120` | the longest a result waits for its siblings: seconds, or `90s`, `2m`, `1500ms`; `0` holds nothing |
| `ROUTR_PI_VIEW` | `steps` | `steps` or `cards`: the view above the editor. `/routr view …` changes it for the session |
| `ROUTR_PI_SKILL` | on | `0`: don't load the bundled skill |
| `ROUTR_STATE_DIR` | from git | the `.routr` directory, as for the CLI |

## How it knows which workers are "mine"

First from the registry: `routr start` records `caller: {harness,
session}`, the session from `PI_SESSION_ID`, which Pi's bash tool exports
to every command. A worker whose `caller.session` is this Pi session is
mine, and is grouped with the others created in the same agent run.

As a fallback, for workers without that record, and to know which workers
the model waits on, the extension reads the model's own bash calls (Pi
merges stderr into their output):
- "routr: queued worker ID …", which `routr start` and `flow start` print on
  stderr for every worker, also in pipes and in `id=$(routr start …)`;
- the id that `routr start` prints, or its `--json` record;
- the chain id from `routr flow start`;
- the id in `routr send ID`;
- the id in a timed-out `--wait` ("Run: routr wait ID").

The rest of a chain is found through the worker's `chain` field. The claims
are saved in the session (`appendEntry`), so `pi -r` keeps them. Results the
model already read with `routr wait`, `--wait` or `routr result` don't wake
it again.

The CLI detects Pi from `PI_CODING_AGENT`, which Pi sets for every command
its model runs, or `PI_SESSION_ID`. It checks them before `CLAUDECODE=1`
and Codex's variables, so a Pi started from a shell inside Claude Code still
counts as Pi. Workers a Pi caller starts get `--permissions bypass`, as Pi
asks for no approvals itself, unless the model passes `--permissions` or
routr's config sets `permissions` ([cli.md](../skills/routr/cli.md), "Roles and config").

On start or resume the first snapshot is a baseline. Workers that finished
before the session was looking get a card but don't wake the model.

## Pi APIs used

All from the Pi package's `docs/extensions.md` and its type declarations (Pi 1.0.4):

| API | For |
|---|---|
| `pi.on("session_start" / "session_shutdown")` | start and stop polling, restore claims from `ctx.sessionManager.getEntries()`; check the routr binary against this checkout (`routr version --json`, then `git diff --quiet REV HEAD -- cmd internal go.mod go.sum`) and warn if it is older |
| `pi.on("resources_discover")` → `skillPaths` | ship the routr skill (docs/extensions.md, "Resource Events"; example `dynamic-resources/`) |
| `pi.on("tool_call")` | note a running `routr wait` / `--wait`, and hold the wakes it covers |
| `pi.on("tool_result")` | read the model's `routr` bash calls and their output |
| `pi.on("tool_execution_end")`, `pi.on("agent_end")` | a wait that ended without a result covers nothing any more |
| `pi.on("before_agent_start")` → `systemPromptOptions.sections.routr` | a `<routr>` section, not a whole `systemPrompt`, so Pi records only its changes and keeps the cached prefix: "start workers without --wait; you'll be woken; leave out --permissions" (routr gives a Pi caller's workers `bypass`); while the binary is older than this checkout, "pass --permissions bypass, and tell the user to run install.sh" instead, since an old binary may not |
| `pi.on("agent_start")` | group workers started in the same run |
| `pi.sendMessage({customType, content, display, details}, {triggerTurn: true, deliverAs})` | wake the model: `steer` when a worker needs a person, `followUp` otherwise ("pi.sendMessage") |
| `pi.registerMessageRenderer("routr-wake")` | the wake as a compact card; expand for the full text ("Message and Entry Rendering") |
| `ctx.ui.setWidget(key, factory)`, `ctx.ui.setStatus` | the steps or cards and the footer count ("Widgets, Status, and Footer"; docs/tui.md, Pattern 5) |
| `pi.appendEntry` + `pi.registerEntryRenderer("routr-output")` | claims that survive `pi -r`; `/routr list` output shown in the chat but kept out of the model's context |
| `pi.registerCommand("routr", {getArgumentCompletions})` | `/routr …` |
| `pi.registerShortcut("alt+q")` | open the picker (docs/keybindings.md: unbound on Linux and macOS) |
| `ctx.ui.select(title, items, {signal})` | the picker; the signal closes it when its question goes ("Manual Dismissal with AbortSignal") |
| `pi.sendUserMessage` | `/routr <free text>` |
| `truncateToWidth`, `Text` from `@earendil-works/pi-tui` | rendering (docs/tui.md) |

## Develop

```sh
cd pi && npm install     # dev only: Pi's types, TypeScript
npm test                 # node:test, no build step (Node strips the types)
npm run typecheck
zsh test/smoke.zsh       # live: see below
```

**Unit tests** (`npm test` runs these and the core's, in
[../frontend/core/test](../frontend/core/test)):
- `dialogs.test.ts`: the picker's and the chooser's items, and what each
  label means;
- `tail.test.ts`: the transcript tailer.

The core's tests:
- `model.spec.ts`: list JSON → cards;
- `steps.spec.ts`: list JSON → the steps view (chains, foreach copies, loops, ad hoc groups, folding);
- `transcript.spec.ts`: Claude, Codex and Pi lines;
- `claims.spec.ts`: which bash calls claim which workers;
- `wake.spec.ts`: when to wake, grouping, and the message text;
- `coordinator.spec.ts`: the session's wake decisions, driven by its
  events alone (turns, shell calls, snapshots, deliveries);
- `question.spec.ts`: who waits for you, a question's identity, when a
  picker closes, and the `routr answer` it runs.

`extension.test.ts` loads the whole extension against a mock `ExtensionAPI`
and [test/fake-routr](test/fake-routr), a stand-in CLI whose workers move
through their states on a clock.

**Smoke test** ([test/smoke.zsh](test/smoke.zsh)): real Pi in a real terminal.
- It runs in a pane of an isolated herdr session (the repo's `test/herdr-sandbox.zsh`), with the
  fake CLI and a throwaway `PI_CODING_AGENT_DIR`, so `~/.pi` is never read or
  written. Pi writes `auth.json` and `models-store.json` into its agent dir.
- It checks that the skill loads, and that the cards render with live tool
  calls.
- It checks that the model's `routr start` is claimed, and that exactly one
  wake arrives and triggers a turn, with none for other workers.
- It checks that `/routr answer` reaches the CLI.
- It costs two short turns on `deepseek/deepseek-v4-flash` (`SMOKE_MODEL` to
  change it). It writes screens and the session to `/tmp/routr-pi-state`.
- `SMOKE_VIEWS=1` makes no model call: the fake CLI starts a chain (plan,
  review, a foreach step of 3 copies, a gate) and it saves the screen in
  both views at the same moments, `screen-views-*-{steps,cards}.txt`, to
  compare them.
- `SMOKE_ASK=1` makes no model call either: the fake's plan worker asks a
  question. It checks the line in both views, that alt+q (sent with `herdr
  pane send-keys`) opens the picker, that an option goes as `routr answer
  … --by human --expect …`, that the picker closes when the question is
  answered in the worker's tab, and that a question with no menu offers
  only its tab. Screens: `screen-ask-*.txt`.
- `SMOKE_ROUTR=bin/routr` runs it against the real CLI (`go build -o
  bin/routr ./cmd/routr`): the model starts a Pi worker on the same model,
  with its own throwaway agent dir (routr's `[env]`), and it checks the card,
  exactly one wake, and that `caller.session` is the Pi session. One more
  short worker turn.
