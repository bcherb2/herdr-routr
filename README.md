# routr

**For agents in [herdr](https://herdr.dev).** routr is a CLI that a coding
agent calls to start other agents (Claude Code, Codex, Pi) as **workers**, each
in its own herdr tab, chain them, and get their results back. Use a built-in
flow, or have the agent write one on the fly.

You rarely type `routr` yourself. You ask your main agent in plain words ("have
codex red-team the plan", "fan out research on X"), and the routr skill teaches
it the CLI. Every output says what happened and what to run next, because the
reader is usually an agent. In Pi and Claude Code, a front end shows each
worker live and wakes the model when one finishes.

```sh
routr start --role review "Red-team PLAN.md: …"   # prints the worker id
routr wait 3 --timeout 9m                         # its final message; exit 3: still running
routr start --role plan "…" | routr start --after - --role review "Review {{prev.output}}" | routr wait -
routr --help                                      # every command
```

## Flows

A (work)flow is a named, multi-step chain of workers: parallel agents, a gate that
waits for you, review loops with a cap, a check that must pass, or a mixture.

```sh
routr flow list                                        # included flows
routr flow show adversarial-review                     # its steps and briefs
routr flow start adversarial-review --param target=main...HEAD
routr flow start plan-to-impl --task "add OAuth"       # stops at a gate until: routr approve
routr flow start compare-models --task "…"             # 7 open-weight models answer, claude ranks them
```

Built in: `plan-to-impl`, `plan-redteam`, `adversarial-review`,
`security-review`, `deep-research`, `research`, `fix-until-green`, `repro-fix`,
`debug-hypotheses`, `best-of-n` and `compare-models` — the last one sends
the same prompt to seven open-weight models over OpenRouter (deepseek,
glm, qwen, kimi, mistral, minimax, seed), each a pi worker in its
own tab, then a claude reviewer ranks their answers, so you can see how
the models differ on a prompt you care about. Write your own as a TOML
file in
`.routr/flows/` (repo) or `~/.config/routr/flows/` (user); see
[flows.md](skills/routr/flows.md). [docs/default-flows.md](docs/default-flows.md)
charts every built-in flow.

## Prerequisites

- **[herdr](https://herdr.dev)**, with a running server. routr starts every
  worker as a herdr tab and must itself run from a herdr pane.
- **Go 1.26** or newer (`go.mod`), to build the binary.
- **zsh**, which runs `install.sh`.
- **At least one harness** routr drives, installed and logged in:
  [Claude Code](https://claude.com/claude-code) (`claude`),
  [Codex](https://github.com/openai/codex) (`codex`) or Pi (`pi`). Pi's
  extension also needs Node 22.19 or newer ([pi/README.md](pi/README.md)).
  Workers can use different accounts of one harness, such as a work and a
  personal Claude, in the same flow: [cli.md](skills/routr/cli.md),
  "Environment a worker gets".
- **`OPENROUTER_API_KEY`** (optional), for the parts that call
  [OpenRouter](https://openrouter.ai): judge steps in flows, questions
  answered by Jev (below), and `compare-models`, whose Pi workers use
  OpenRouter models. Put it in the environment or in config `[env]`. Without
  it everything else works; `routr doctor` warns.

### Platforms

routr is built and tested on Linux. It also runs on macOS, with less:

- `install.sh` cannot restart a watcher still running the old binary after
  an update, since it finds them through `/proc`. Such a watcher stops once
  it has been idle for 10 minutes; the next routr command starts a new one.
- routr cannot read a worker tab's environment, so it looks for the
  harness's transcript (for live tool calls) with its own environment
  instead. Keep `CLAUDE_CONFIG_DIR` and `CODEX_HOME` the same in both.
- A run step left behind by a stopped watcher is told apart from a later
  process less surely.

Windows is not currently supported.

## Install

```sh
git clone https://github.com/bcherb2/herdr-routr && cd herdr-routr
./install.sh            # build, install, register the Claude Code plugin and Pi extension, link the skill for Codex
./install.sh --update   # git pull first; --test runs the tests first
routr doctor            # from a herdr pane: checks herdr, harnesses, config
```

`install.sh` puts the binary in `$BIN_DIR`, else `go env GOBIN`, else
`$(go env GOPATH)/bin`; that directory must be on your `PATH`. For the
harnesses it finds, it registers the Claude Code plugin
([frontend/README.md](frontend/README.md)) and the Pi extension, which both
ship the routr skill, and links the skill into `~/.codex/skills`. All of them
are read from the clone, so keep it where it is.

`routr doctor` prints one line per check, each problem with what to do about
it, and exits 1 if a check fails.

Each repo keeps its state in `.routr/`. Only `config.toml` and `flows/` there
are tracked by git. A per-repo watcher drives the workers and starts on demand,
so no command runs past its `--timeout`.

- **Config:** `~/.config/routr/config.toml` sets roles, harness defaults and
  `[env]` for worker tabs. A repo's `.routr/config.toml` overrides it;
  `routr roles` shows the result.

### Slash commands

Normally you just ask in plain words, and the agent picks up the routr
skill. A `/routr` command makes it certain: `/routr have codex red-team
PLAN.md` hands your words to the skill.

- **Claude Code and Pi:** the plugin and the extension give `/routr`
  themselves. It also runs `list`, `show N`, `result N`, `answer N`,
  `approve N` and the rest for you, its output kept out of the model's
  context.
- **Codex** has deprecated custom prompts in favour of skills: with the
  skill installed, `$routr` in a message invokes it. For
  `/prompts:routr have claude review it`, symlink
  [commands/codex](commands/codex/routr.md) to `~/.codex/prompts/routr.md`
  (`install.sh` doesn't).
- **Pi without the extension:** [commands/pi](commands/pi/routr.md) is the
  same template, at `~/.pi/agent/prompts/routr.md`.

## Questions and Jev

When a worker stops at a permission menu, someone has to pick an option.
By default that is you. In config, `[questions] answer = "agent"` lets your
main agent decide within the rules in [cli.md](skills/routr/cli.md) ("Who
answers"), and `answer = "jev"` lets Jev decide; `routr start --answer`
sets it for one worker.

Jev is not part of routr. It is TypeSafe's decision model
(`typesafe/jev-1.13`), which routr calls through OpenRouter with
`OPENROUTER_API_KEY`. It writes no text: given the facts and the options,
it picks one and says how sure it is. routr uses it for questions
(`answer = "jev"`) and for judge steps in flows. When Jev is unsure, fails,
or has no key, the question goes to you or your agent instead
(`[questions] fallback`).

## Docs

What an agent using routr reads ships with the skill, in `skills/routr/`:

- [SKILL.md](skills/routr/SKILL.md): when to use routr and how
- [cli.md](skills/routr/cli.md): commands, exit codes, state layout, who
  answers questions, roles and config
- [flows.md](skills/routr/flows.md): flow files and passing data between
  workers
- [examples.md](skills/routr/examples.md): worked examples

For people:

- [architecture](docs/architecture.md): why routr is shaped this way, what
  talks to what, the watcher, who decides what, and which doc says what
- [default-flows](docs/default-flows.md): the built-in flows as flowcharts
- [Pi extension](pi/README.md)
- [Claude Code plugin](frontend/README.md)

## Develop

Run `./install.sh` again after every change. It also restarts idle watchers
that are still on the old binary.

```sh
go test -race ./...                 # unit tests
go test -tags integration ./...     # also against throwaway herdr servers
go test -tags live ./...            # real harnesses
cd pi && npm ci && npm test         # Pi extension and the front-end core
cd frontend && claude plugin test . # Claude Code plugin
```

Never point tests or a dev build at your own herdr server;
`test/herdr-sandbox.zsh` has the isolation helpers.

## License

MIT, see [LICENSE](LICENSE). Copyright (c) 2026 bcherb2.
