#!/usr/bin/env zsh
# Live smoke test: real Pi, this extension and test/fake-routr, in a real
# terminal (a pane of an isolated herdr session), with one cheap model.
#
# Checks: the routr skill loads; the card renders (a seeded worker, then the
# new one with live tool calls); the model's own `routr start` is claimed;
# the wake message arrives
# and triggers a turn; nothing wakes for workers it didn't start; /routr
# answer reaches the CLI.
#
# Costs two short model turns. Needs DEEPSEEK_API_KEY (or set SMOKE_MODEL
# and a matching key; without one it skips and exits 0). Pi runs with an
# isolated PI_CODING_AGENT_DIR built from the env alone, so ~/.pi is never
# read or written; herdr only through test/herdr-sandbox.zsh.
#
#   zsh pi/test/smoke.zsh        # logs and screens in /tmp/routr-pi-state
#
# SMOKE_VIEWS=1 makes no model call: it starts a fake chain (plan, review,
# a foreach step of 3 copies, a gate) and saves the screen in the steps view
# and the cards view at the same moments, to compare them
# (screen-views-*.txt).
#
# SMOKE_ASK=1 makes no model call either: the fake CLI's plan worker asks a
# question, and it checks the hint in both views, that alt+q opens the
# picker, that an option goes as `routr answer … --by human --expect …`, that
# the picker closes when the question is answered in the worker's tab, and
# that a question with no menu offers only its tab (screen-ask-*.txt).
#
# SMOKE_ROUTR=bin/routr runs it against the real CLI instead: the model
# starts a Pi worker on the same cheap model (its own throwaway agent dir),
# and the checks are the card, the one wake, and the caller.session record.
# SANDBOX_SESSION / SANDBOX_STATE / SANDBOX_WORK override the isolated herdr
# session and the scratch dirs.

emulate -L zsh
setopt pipe_fail no_unset

here=${0:A:h}
pi_dir=${here:h}
repo=${pi_dir:h}
export SANDBOX_SESSION=${SANDBOX_SESSION:-routr-pi} SANDBOX_STATE=${SANDBOX_STATE:-/tmp/routr-pi-state} SANDBOX_WORK=${SANDBOX_WORK:-/tmp/routr-pi-work}
source $repo/test/herdr-sandbox.zsh

model=${SMOKE_MODEL:-deepseek/deepseek-v4-flash}
provider=${model%%/*}
real=${SMOKE_ROUTR:+${SMOKE_ROUTR:A}}
agent=/tmp/$SANDBOX_SESSION-agent          # isolated Pi config dir
wagent=/tmp/$SANDBOX_SESSION-worker-agent  # the real CLI's Pi workers' config dir
bin=/tmp/$SANDBOX_SESSION-bin              # `routr` → the fake, or SMOKE_ROUTR
dot=$SANDBOX_WORK/.routr
fails=0

ok()   { print -r -- "PASS $*" }
bad()  { print -r -- "FAIL $*"; (( fails++ )) }
screen() { hx pane read $pane --source visible 2>/dev/null }
# wait_for LABEL SECONDS COMMAND…: poll until COMMAND succeeds
wait_for() {
  local label=$1 secs=$2; shift 2
  local end=$(( SECONDS + secs ))
  while (( SECONDS < end )); do
    "$@" >/dev/null 2>&1 && { ok $label; return 0 }
    sleep 1
  done
  bad "$label (after ${secs}s)"; return 1
}
on_screen() { screen | grep -Eq -- "$1" }
# Pi's footer, "(provider) id"; the echoed launch command has provider/id, and
# text typed before Pi is up gets no autocomplete
pi_up() { on_screen "\\(${model%%/*}\\) ${model#*/}" }
session_file() { print -l $agent/sessions/**/*.jsonl(N.om[1]) }
# custom_message entries of our type, then whether an assistant reply follows the first
wakes() {
  local f=$(session_file); [[ -n $f ]] || return 1
  jq -c 'select(.type=="custom_message" and .customType=="routr-wake") | .content' $f
}
replied_after_wake() {
  local f=$(session_file); [[ -n $f ]] || return 1
  jq -s 'map(.type=="custom_message" and .customType=="routr-wake") as $w
    | ($w | index(true)) as $i
    | $i != null and ([.[($i+1):][] | select(.type=="message" and .message.role=="assistant")] | length > 0)' \
    $f | grep -q true
}

# ── model config, from the env only ────────────────────────────────────
# SMOKE_MODEL is provider/id. The provider's key is <PROVIDER>_API_KEY
# (deepseek: DEEPSEEK_API_KEY, openrouter: OPENROUTER_API_KEY), and
# provider_config below holds its models.json entry; add a case there for
# another provider. models.json names the key as "$NAME" and Pi reads it
# from its env at request time, so the key is never written to disk.
keyvar=${(U)provider//-/_}_API_KEY
provider_config() {
  case $provider in
    deepseek) print -r -- '{
      "baseUrl": "https://api.deepseek.com",
      "api": "openai-completions",
      "apiKey": "$DEEPSEEK_API_KEY",
      "compat": {"thinkingFormat": "deepseek"},
      "models": [
        {"id": "deepseek-v4-pro", "name": "DeepSeek V4 Pro", "reasoning": true, "input": ["text"], "contextWindow": 1000000, "maxTokens": 65536, "cost": {"input": 0.435, "output": 0.87, "cacheRead": 0.003625, "cacheWrite": 0}},
        {"id": "deepseek-v4-flash", "name": "DeepSeek V4 Flash", "reasoning": true, "input": ["text"], "contextWindow": 1000000, "maxTokens": 65536, "cost": {"input": 0.14, "output": 0.28, "cacheRead": 0.0028, "cacheWrite": 0}}
      ]}' ;;
    openrouter) jq -n --arg id ${model#*/} '{
      baseUrl: "https://openrouter.ai/api/v1",
      api: "openai-completions",
      apiKey: "$OPENROUTER_API_KEY",
      models: [{id: $id}]}' ;;
    *) return 1 ;;
  esac
}
config=$(provider_config) || {
  print -u2 -r -- "smoke: no config for provider '$provider' (SMOKE_MODEL=$model); add it to provider_config in ${0:t}"
  return 1
}
# Only the model path makes a model call; the other modes run without a key.
if [[ -z ${SMOKE_VIEWS:-}${SMOKE_ASK:-} && -z ${(P)keyvar:-} ]]; then
  print -r -- "skip: no $keyvar (set SMOKE_MODEL and a matching key)"
  return 0
fi

# ── setup ───────────────────────────────────────────────────────────────
rm -rf $agent $wagent $bin $dot
mkdir -p $agent $bin
jq -n --arg p $provider --argjson c $config '{providers: {($p): $c}}' > $agent/models.json
print -r -- '{"defaultThinkingLevel":"off","quietStartup":true}' > $agent/settings.json

sandbox_up || return 1
sandbox_workdir
if [[ -n $real ]]; then
  ln -s $real $bin/routr
  # The workers are Pi too, with their own throwaway config dir (routr's
  # readiness check wants a default provider there), exported by routr's [env].
  mkdir -p $wagent $dot
  cp $agent/models.json $wagent/
  print -r -- "{\"defaultProvider\":\"${model%%/*}\",\"defaultModel\":\"${model#*/}\",\"defaultThinkingLevel\":\"off\",\"quietStartup\":true}" > $wagent/settings.json
  print -rl -- '[env]' "PI_CODING_AGENT_DIR = \"$wagent\"" 'PI_OFFLINE = "1"' > $dot/config.toml
else
  ln -s $here/fake-routr $bin/routr
  ( cd $SANDBOX_WORK && ROUTR_STATE_DIR=$dot $bin/routr seed )
fi
pane=$(sandbox_pane routr-pi)
log "pane $pane"

views=${SMOKE_VIEWS:-} ask=${SMOKE_ASK:-} wake=mine
if [[ -n $views ]]; then secs=10 view=steps
elif [[ -n $ask ]]; then secs=300 view=steps wake=off
else secs=30 view=cards; fi
hj pane run $pane "cd $SANDBOX_WORK && env PATH=$bin:\$PATH ROUTR_BIN=$bin/routr ROUTR_STATE_DIR=$dot FAKE_ROUTR_RUN_SECS=$secs ROUTR_PI_VIEW=$view ROUTR_PI_WAKE=$wake PI_CODING_AGENT_DIR=$agent PI_OFFLINE=1 pi -e $pi_dir/src/index.ts --no-context-files --model $model --thinking off" >/dev/null

# the registry as the real CLI prints it; never with this shell's herdr env
registry() { ( cd $SANDBOX_WORK && $SANDBOX_SERVER_SCRUB ROUTR_STATE_DIR=$dot $bin/routr list --all --json ) }
pi_session_id() { head -1 $(session_file) | jq -r .id }

# ── views: steps and cards on the same fake chain, no model ────────────
if [[ -n $views ]]; then
{
  wait_for "pi started" 40 pi_up
  # both views of the screen now, as screen-views-N-LABEL-{steps,cards}.txt
  snap() {
    screen > $SANDBOX_STATE/screen-views-$1-steps.txt
    hj pane run $pane "/routr view cards" >/dev/null; sleep 2
    screen > $SANDBOX_STATE/screen-views-$1-cards.txt
    hj pane run $pane "/routr view steps" >/dev/null; sleep 1
  }
  ( cd $SANDBOX_WORK && ROUTR_STATE_DIR=$dot $bin/routr flow start demo >/dev/null 2>&1 )
  wait_for "the chain, implement not expanded yet" 20 on_screen 'implement ×\?'
  on_screen '⏸ merge' && ok "the gate row" || bad "no gate row"
  snap 1-plan
  wait_for "implement as ×3, two at once" 40 on_screen 'implement ×3 .*max 2 at once'
  snap 2-fanout
  wait_for "implement done" 30 on_screen '✔ implement ×3'
  snap 3-gate
  hj pane run $pane "/routr view cards" >/dev/null
  wait_for "/routr view switches to cards" 10 on_screen 'routr · .*needs you'
} always {
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  sandbox_down
}
print -r -- "screens: $SANDBOX_STATE/screen-views-*"
(( fails == 0 )) && print -r -- "SMOKE OK" || { print -r -- "SMOKE FAILED ($fails)"; return 1 }
return 0
fi

# ── ask: the hint and the picker on the fake's question, no model ──────
if [[ -n $ask ]]; then
{
  # the fake CLI as the watcher and the worker's tab would act; never with this shell's herdr env
  r() { ( cd $SANDBOX_WORK && $SANDBOX_SERVER_SCRUB ROUTR_STATE_DIR=$dot FAKE_ROUTR_RUN_SECS=$secs $bin/routr "$@" ) }
  n=0; snap() { (( n++ )); screen > $SANDBOX_STATE/screen-ask-$n-$1.txt }
  called() { grep -q -- "$1" $dot/fake-calls.log }
  picker_open() { on_screen 'Open its tab' }
  hint='⚠ claude 7 asks about Bash: ls .* · alt\+q answer'
  wait_for "pi started" 40 pi_up
  r flow start demo >/dev/null 2>&1
  wait_for "plan asks (the fake blocks it)" 20 r ask 7
  wait_for "the hint in the steps view" 10 on_screen $hint
  sleep 3; ! picker_open && ok "nothing opens by itself" || bad "a picker opened by itself"
  snap hint-steps
  hj pane run $pane "/routr view cards" >/dev/null
  wait_for "the hint in the cards view" 10 eval 'on_screen "routr · .*needs? you" && on_screen $hint'
  snap hint-cards
  hj pane run $pane "/routr view steps" >/dev/null; sleep 1

  hx pane send-keys $pane alt+q >/dev/null
  wait_for "alt+q opens the picker" 10 picker_open
  on_screen '2\. Yes, and don.t ask again .*\(lasts past this call\)' && ok "the option that lasts is marked" || bad "no lasts marker"
  on_screen 'Later' && ok "Later is offered" || bad "no Later"
  snap picker
  hx pane send-keys $pane enter >/dev/null   # 1. Yes, the first item
  wait_for "option 1 goes as --by human --expect" 10 called 'answer 7 1 --by human --expect 6a128b4273b95faa'
  wait_for "the picker and the hint go" 10 eval '! picker_open && ! on_screen "alt\+q answer"'
  snap answered

  r ask 7 >/dev/null
  wait_for "it asks again" 10 on_screen $hint
  hx pane send-keys $pane alt+q >/dev/null
  wait_for "alt+q opens the picker again" 10 picker_open
  r answer 7 1 >/dev/null   # in its tab
  wait_for "the picker closes when the question is answered in its tab" 10 eval '! picker_open'
  on_screen '7 asks nothing now' && ok "it says why it closed" || bad "no notice"
  snap closed
  called 'answer 7 1 --by human' && (( $(grep -c ' answer 7 ' $dot/fake-calls.log) == 2 )) \
    && ok "nothing was pressed from the closed picker" || bad "answer calls: $(grep ' answer 7 ' $dot/fake-calls.log)"

  r ask 7 --menu none >/dev/null
  wait_for "a question with no menu: its own hint" 10 on_screen '⚠ claude 7 asks: What should the file be called\? · alt\+q answer'
  hx pane send-keys $pane alt+q >/dev/null
  wait_for "the picker without options" 10 eval 'picker_open && on_screen "no options to pick here" && ! on_screen "1\. Yes"'
  snap no-menu
  hx pane send-keys $pane enter >/dev/null   # Open its tab
  wait_for "Open its tab runs routr show --focus" 10 called 'show 7 --focus'
  [[ -n $(session_file) ]] && cp $(session_file) $SANDBOX_STATE/session.jsonl
  cp $dot/fake-calls.log $SANDBOX_STATE/fake-calls.log
} always {
  hx pane send-keys $pane escape >/dev/null 2>&1; sleep 0.3
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  sandbox_down
}
print -r -- "screens: $SANDBOX_STATE/screen-ask-*"
(( fails == 0 )) && print -r -- "SMOKE OK" || { print -r -- "SMOKE FAILED ($fails)"; return 1 }
return 0
fi

# ── checks: the real CLI ────────────────────────────────────────────────
if [[ -n $real ]]; then
{
  wait_for "pi started" 40 pi_up
  hx pane send-text $pane "/skill:rou" >/dev/null
  wait_for "routr skill loaded (/skill:routr completes)" 10 on_screen 'skill:routr'
  for _ in {1..10}; do hx pane send-keys $pane backspace >/dev/null; done

  hj pane run $pane "Run this bash command exactly once: routr start --harness pi --model $model 'Reply with the single word pong.' -- then reply with only the number it printed and stop." >/dev/null
  wait_for "card for the model's worker" 90 on_screen '1 task'
  screen > $SANDBOX_STATE/screen-2-running.txt
  wait_for "the worker finished" 240 eval '[[ $(registry | jq -r ".[0].state") == (done|maybe_done|closed) ]]'
  wait_for "wake message in the session" 60 eval '[[ -n "$(wakes)" ]]'
  wait_for "the wake triggered a turn" 90 replied_after_wake
  sleep 5
  screen > $SANDBOX_STATE/screen-3-woken.txt
  [[ $(wakes | wc -l) -eq 1 ]] && ok "exactly one wake" || bad "wake count $(wakes | wc -l)"
  # the CLI titles the worker from its brief; the wake quotes it (jq -c escapes the quotes)
  wakes | grep -Eq 'worker 1 \\"reply-with-the-[^"]*\\" · task' && ok "wake names worker 1" || bad "wake content: $(wakes)"
  registry > $SANDBOX_STATE/registry.json
  typeset sid=$(pi_session_id) rec=$(registry | jq -c '.[0].caller')
  [[ -n $sid && $rec == "{\"harness\":\"pi\",\"session\":\"$sid\"}" ]] \
    && ok "caller.session is this Pi session" || bad "caller $rec, session $sid"

  hj pane run $pane "/routr list" >/dev/null
  wait_for "/routr list reached the CLI" 10 eval 'grep -q "\"customType\":\"routr-output\"" $(session_file)'
  [[ -n $(session_file) ]] && cp $(session_file) $SANDBOX_STATE/session.jsonl
} always {
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  sandbox_down
  # the watcher the CLI started for this root
  pkill -f "^$real watch" 2>/dev/null
}
print -r -- "screens and session: $SANDBOX_STATE"
(( fails == 0 )) && print -r -- "SMOKE OK" || { print -r -- "SMOKE FAILED ($fails)"; return 1 }
return 0
fi

# ── checks: the fake CLI ────────────────────────────────────────────────
{
  wait_for "pi started" 40 pi_up
  wait_for "card for another session's worker" 20 on_screen '3 research'
  screen > $SANDBOX_STATE/screen-1-start.txt
  on_screen '2 task' && bad "old finished worker has a card" || ok "no card for the old finished worker"

  # the extension ships the skill: Pi offers /skill:routr (typed, not sent)
  hx pane send-text $pane "/skill:rou" >/dev/null
  wait_for "routr skill loaded (/skill:routr completes)" 10 on_screen 'skill:routr'
  for _ in {1..10}; do hx pane send-keys $pane backspace >/dev/null; done

  hj pane run $pane "Run this bash command exactly once: routr start --role review --permissions auto 'Red-team PLAN.md' -- then reply with only the number it printed and stop." >/dev/null
  wait_for "card for the model's worker, running" 60 on_screen '7 review .*running'
  wait_for "live tool calls on its card" 20 on_screen '⎿ (Read|Grep|Bash)'
  screen > $SANDBOX_STATE/screen-2-running.txt

  wait_for "wake message in the session" 60 eval '[[ -n "$(wakes)" ]]'
  wait_for "the wake triggered a turn" 90 replied_after_wake
  sleep 3
  screen > $SANDBOX_STATE/screen-3-woken.txt
  [[ $(wakes | wc -l) -eq 1 ]] && ok "exactly one wake" || bad "wake count $(wakes | wc -l)"
  wakes | grep -q 'worker 7 · review' && ok "wake names worker 7" || bad "wake content: $(wakes)"
  wakes | grep -Eq 'worker (2|3) ' && bad "woke for a worker it didn't start" || ok "no wake for workers 2 and 3"
  grep -q 'result 7' $dot/fake-calls.log && ok "fetched routr result 7" || bad "no routr result call"

  hj pane run $pane "/routr answer 7 2" >/dev/null
  wait_for "/routr answer reached the CLI" 10 grep -q 'answer 7 2' $dot/fake-calls.log
  [[ -n $(session_file) ]] && cp $(session_file) $SANDBOX_STATE/session.jsonl
} always {
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  hx pane send-keys $pane ctrl+c >/dev/null 2>&1; sleep 0.5
  sandbox_down
}

print -r -- "screens and session: $SANDBOX_STATE"
(( fails == 0 )) && print -r -- "SMOKE OK" || { print -r -- "SMOKE FAILED ($fails)"; return 1 }
