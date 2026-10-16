# Throwaway herdr server for tests. Source from zsh; call herdr only through hx/hj.
#
# Safety: the caller may itself run inside a herdr server and inherit HERDR_SOCKET_PATH.
# Every herdr call here strips all HERDR_* vars and pins the sandbox session's socket, and
# the `default` herdr server's PID is checked before and after, so it is never touched.

SANDBOX_SESSION=${SANDBOX_SESSION:-routr-sandbox}
SANDBOX_SOCK=${XDG_CONFIG_HOME:-$HOME/.config}/herdr/sessions/$SANDBOX_SESSION/herdr.sock
SANDBOX_WORK=${SANDBOX_WORK:-/tmp/routr-sandbox-work}
SANDBOX_STATE=${SANDBOX_STATE:-/tmp/routr-sandbox-state}
SANDBOX_ROOT=${0:A:h}
typeset -ga SANDBOX_SCRUB=(env -u HERDR_SOCKET_PATH -u HERDR_CLIENT_SOCKET_PATH -u HERDR_ENV
  -u HERDR_SESSION -u HERDR_WORKSPACE_ID -u HERDR_TAB_ID -u HERDR_PANE_ID -u HERDR_BIN_PATH)
# The sandbox server may be spawned from inside a Claude Code session; without this its child
# harnesses think they are nested Claude sessions (e.g. transcript saving gets disabled) or
# pick up that session's config dir.
typeset -ga SANDBOX_SERVER_SCRUB=($SANDBOX_SCRUB -u CLAUDECODE -u CLAUDE_PID -u CLAUDE_EFFORT
  -u CLAUDE_CONFIG_DIR)
for _v in ${(M)${(k)parameters}:#CLAUDE_CODE_*}; do SANDBOX_SERVER_SCRUB+=(-u $_v); done; unset _v

mkdir -p $SANDBOX_STATE

die() { print -u2 "FATAL: $*"; return 1 }
log() { print -u2 "[$(date +%T.%3N)] $*" }

default_server_pid() { pgrep -f -x '.*/herdr server' | head -1 }
sandbox_server_pid() { pgrep -f -x "herdr --session $SANDBOX_SESSION server" | head -1 }

guard_default() {
  local want=$(<$SANDBOX_STATE/default.pid) now=$(default_server_pid)
  [[ -n $want && $want == $now ]] || die "default herdr server PID changed ($want -> $now)"
}

hx() {
  [[ $SANDBOX_SOCK == */sessions/$SANDBOX_SESSION/herdr.sock && $SANDBOX_SESSION != default ]] \
    || die "refusing: socket $SANDBOX_SOCK is not the sandbox session"
  $SANDBOX_SCRUB HERDR_SOCKET_PATH=$SANDBOX_SOCK herdr --session $SANDBOX_SESSION "$@"
}

# hx, but fail when the JSON reply carries .error (herdr exits 0 on API errors).
hj() {
  local out; out=$(hx "$@") || return 1
  if [[ $out == \{* ]] && print -r -- $out | jq -e '.error' >/dev/null 2>&1; then
    print -u2 -r -- "herdr $1 $2 error: $out"; return 1
  fi
  print -r -- $out
}

sandbox_up() {
  [[ -s $SANDBOX_STATE/default.pid ]] || default_server_pid > $SANDBOX_STATE/default.pid
  guard_default || return 1
  if [[ -z $(sandbox_server_pid) ]]; then
    ( cd /tmp && $SANDBOX_SERVER_SCRUB herdr --session $SANDBOX_SESSION server > $SANDBOX_STATE/server.log 2>&1 &! )
    for _ in {1..50}; do [[ -S $SANDBOX_SOCK ]] && break; sleep 0.1; done
  fi
  [[ -S $SANDBOX_SOCK && -n $(sandbox_server_pid) ]] || die "sandbox server failed to start"
  log "sandbox server pid=$(sandbox_server_pid) socket=$SANDBOX_SOCK; default pid=$(<$SANDBOX_STATE/default.pid)"
}

sandbox_down() {
  local pid=$(sandbox_server_pid)
  if [[ -n $pid ]]; then
    kill $pid
    for _ in {1..50}; do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
    log "stopped sandbox server $pid"
  fi
  guard_default && log "default server untouched"
}

# Scratch git repo the harnesses work in.
sandbox_workdir() {
  [[ -d $SANDBOX_WORK/.git ]] && return 0
  mkdir -p $SANDBOX_WORK && cd $SANDBOX_WORK
  git init -q
  printf 'def add(a, b):\n    return a - b  # bug\n' > calc.py
  git add . && git -c user.name=sandbox -c user.email=sandbox@local commit -qm init
  cd - >/dev/null
}

# New workspace with a shell pane in the scratch repo; prints the pane id.
sandbox_pane() {
  hj workspace create --label ${1:-sandbox} --cwd $SANDBOX_WORK --no-focus | jq -r .result.root_pane.pane_id
}
