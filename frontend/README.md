# routr for Claude Code

A Claude Code plugin (a mod: a hooks module, not only markdown), and the
Claude Code front end for routr (see "Front ends over one CLI" in
[docs/architecture.md](../docs/architecture.md#design-choices)). It does for
Claude Code what the [Pi extension](../pi/README.md) does for Pi, on the same
core ([core/](core/index.ts)). So what wakes the model, when, and what the
message says are the same in both, and [pi/README.md](../pi/README.md) owns
that ("Waking the model", "How it knows which workers are mine"). This file
says what is Claude Code's own.

You ask Claude in plain words. Claude's model uses the
[routr skill](../skills/routr/SKILL.md), which the plugin ships, to start
workers, and the plugin does the rest:

- **Waking the model.** When a worker this session started is done, failed or
  needs someone, the plugin starts a turn with a `[routr]` message
  (`$.prompt.submit`) holding the result. A worker that needs someone while a
  turn runs goes into that turn (`$.session.append`). Other news waits for the
  turn's end, and after Esc it waits until your next turn ends. A system
  prompt section tells the model to start workers without `--wait` and end
  its turn.
  - A worker is this session's when its `caller.session` is Claude Code's
    session id (`CLAUDE_CODE_SESSION_ID`, which Claude Code exports to the
    model's Bash calls), or when the model's Bash output claims it. Claims are
    kept in the plugin's store per session id, so a reload of the plugin
    keeps them.
  - After `/clear` the session id changes. The plugin starts over as Pi does
    on `/new`: it wakes only for workers the new conversation starts.
  - A foreground `routr wait` covers its workers while it runs. A background
    one (the skill's path without the plugin) covers them until its task
    notification arrives: `completed` means the model has the result, and any
    other status lets the wake follow.
- **Steps** above the prompt (the default view), laid out as Pi's are. A
  worker that asks you gets one red line and an `[ answer N ]` button. The
  status line counts the workers (`routr: ▶2 !1`: running · needs you).
- **Worker cards** in a pane: `/routr view cards`. `/routr view steps` or
  closing the pane goes back. The cards show live tool calls, tailed from each
  worker's transcript. They are for you; none of it reaches the model.
- **Answering a question.** `/routr answer`, or the band's button, opens the
  picker in a pane: the question, its options numbered as on the worker's
  screen (digits pick), "Open its tab" (`t`) and "Later" (`l` or Esc). It
  works as Pi's does: `routr answer N K --by human --expect FP`, so a
  changed question is never answered; it closes when its question goes; with
  several questions it asks which worker first.
  - A mod can't bind a key, so there is no alt+q. After you press a band
    button the band keeps the keyboard: press ctrl+x Tab to reach the picker.
- **`/routr`**: `list`, `show N`, `hide N`, `result N`, `answer [N [K]]`,
  `approve N`, `cancel N`, `close N`, `wake mine|all|off` and
  `view steps|cards`, as in Pi. Their output is shown to you, not the model.
  Anything else, `/routr have codex red-team PLAN.md` for one, runs the routr
  skill with your words.
  - `/routr` is the skill's own command. Claude Code names a skill's command
    after it and refuses a plugin a command of the same name, so the plugin
    answers the skill's command instead of registering one. That holds for
    the skill it ships (`routr:routr`, typed as `/routr`) and for a copy of
    the skill of your own (`routr`).

## Install

It needs a Claude Code with mods, an early-access API that changes between
releases (tested on 2.1.299), and `routr` on `PATH` (or `ROUTR_BIN`). The repo's `install.sh` registers it:

```sh
claude plugin marketplace add /path/to/herdr-routr   # the repo is a marketplace (.claude-plugin/marketplace.json)
claude plugin install routr@routr
```

A plugin from a folder marketplace is read in place, from `frontend/`, so
`git pull` updates it; a running session picks the change up with
`/reload-plugins`. The skill is a link, `frontend/skills/routr` →
`skills/routr`, which Claude Code follows. A skill of your own at
`~/.claude/skills/routr` duplicates it: `install.sh` removes the link it made
there itself, and warns about anything else. You don't need the `/routr`
prompt template either.

To try it for one session without installing: `claude --plugin-dir
/path/to/herdr-routr/frontend`. To remove it: `claude plugin uninstall
routr@routr && claude plugin marketplace remove routr`; then, for the skill
without the plugin, link `skills/routr` into `~/.claude/skills`.

## Settings (environment)

| Var | Default | |
|---|---|---|
| `ROUTR_BIN` | `routr` | the CLI to run |
| `ROUTR_WAKE_HOLD` | `120` | the longest a result waits for its siblings, as in Pi |
| `ROUTR_STATE_DIR` | from git | the `.routr` directory, as for the CLI |

`/routr wake` and `/routr view` change the rest for the session.

## Claude Code APIs used

All from the types Claude Code writes for a mod (`.claude-plugin/types/`,
not committed) and its plugin-authoring reference, Claude Code 2.1.299:

| API | For |
|---|---|
| `session.start`, `$.clock.every` | poll `routr list --json` (2 s; there is no file watch) and tail transcripts (1 s) |
| `$.session.id()` at `turn.start` | the session the claims and wakes are for; a new id after `/clear` starts over |
| `$.store` | claims per session id, which survive a reload |
| `turn.start`, `turn.step`, `turn.complete` (reason) | when the model is busy, whether a mid-turn append was read, and Esc |
| `tool.call` on Bash | claims from the model's `routr` calls, and the wait cover; `run_in_background` calls by their task id |
| `session.append` with origin `task-notification` | a background shell's end, and its status |
| `$.prompt.submit`, `$.session.append` | wake the model, or put a question into the running turn |
| `prompt.compose` | the system prompt section |
| `command.run` on `routr` and `routr:routr` | `/routr`, served on the skill's own command |
| `ui.render` `AbovePrompt`, `$.ui.status` | the steps band, its answer buttons, and the count |
| `$.ui.open`, `ui.render` `Pane`, `ui.close` | the cards pane and the picker |
| `$.state` (atoms, [types/index.d.ts](types/index.d.ts)) | what the drawings read |
| `$.process.run`, `$.fs`, `$.env` | the CLI, transcript tails, settings |
| `$.ui.log`, `$.ui.toast` | `/routr` output and notices, shown to you only |

Not available to a mod, so missing next to Pi: a key binding (alt+q), and a
file watch.

## Why it is built this way

- **The plugin is `frontend/`, and the core sits inside it.** A mod may import
  only files inside its plugin folder, and Claude Code doesn't follow a
  symlinked directory for code (tried on 2.1.296). So the core can't live
  outside the plugin and be linked in. Pi imports it from here instead
  (`../frontend/core`). The repo root can't be the plugin: it would also load
  `commands/`, which holds the Codex and Pi templates. The skill is the one
  link that works, because a session follows it, though `claude plugin
  validate` warns about it.
- **Each front end delivers wakes its own way.** Pi's `sendMessage` hands a
  message over and returns. `$.prompt.submit` resolves only when its queued
  turn starts, so awaiting it mid-turn would stall the poller; `deliver` in
  [hooks/register.tsx](hooks/register.tsx) doesn't await it. The decisions
  stay in the core's `Coordinator`; only the sending differs.
- **News put into a turn can go unread.** `$.session.append` reaches the model
  at its next request. If the turn's last request is already running, the
  model reads the note only on its next turn and nothing wakes it. So the
  coordinator counts an append as delivered only when a `turn.step` starts
  after it (`stepStarted`), and otherwise sends it as a wake at the turn's end.
- **A queued `$.prompt.submit` can't be recalled.** That is why news other
  than a question waits for `turn.complete`: news the model reads for itself
  later in the turn would come back as a repeat.
- **`/routr` is the skill's command.** Claude Code refused the plugin
  `$.command.register("routr")` because `routr:routr` is the skill's command,
  so the plugin hooks `command.run` instead. A skill's command is not
  `immediate`, so `/routr answer` typed mid-turn may wait for the turn's end
  (not tried); the band's buttons work mid-turn.
- **Claude Code refuses a foreground `sleep` in Bash.** To keep a turn running
  in a test, use something like `python3 -c 'import time; time.sleep(60)'`.

## Develop

```sh
cd frontend
claude plugin validate .           # what the module hooks and calls, and what the engine would refuse
claude plugin test .               # hooks/register.test.ts, against the engine's test kit
npx -p typescript@5.6 tsc -p .     # type-check, once Claude Code has laid .claude-plugin/types/
```

`claude plugin test .` runs the tests but does not type-check `register.tsx`.
`tsc` needs `.claude-plugin/types/`, which Claude Code lays in the plugin
folder when it loads the plugin from there. It is not in git, and neither
`validate` nor `test` creates it, so on a clean checkout `tsc` fails with
TS5083.

The core's own tests run from `pi/` (`npm test`), as the core has no
Claude Code code in it.
