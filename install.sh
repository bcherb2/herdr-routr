#!/usr/bin/env zsh
# Build routr and install it with its skill (Codex), the Claude Code plugin and
# the Pi extension. Run it again to update: after a git pull, or with --update to pull
# first. Idempotent; links point into this repo, so they follow it.
#
#   ./install.sh [--update] [--test]
#
#   -u, --update  git pull --ff-only first (refuses with uncommitted changes)
#   -t, --test    go vet and go test -race before installing
#
# The binary goes to $BIN_DIR, else `go env GOBIN`, else `go env GOPATH`/bin.
# Watchers still running an old binary are restarted when no worker is in
# flight; busy ones are left alone and reported.
setopt err_exit no_unset pipe_fail

root=${0:A:h}
integer update=0 run_tests=0
for arg; do
  case $arg in
    -u|--update) update=1 ;;
    -t|--test) run_tests=1 ;;
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' ${0:A}; exit 0 ;;
    *) print -u2 "install.sh: unknown argument: $arg (try --help)"; exit 2 ;;
  esac
done

say()  { print -P -- "%F{green}==>%f $*" }
warn() { print -P -u2 -- "%F{yellow}warning:%f $*" }
die()  { print -P -u2 -- "%F{red}error:%f $*"; exit 1 }

if (( update )); then
  [[ -z $(git -C $root status --porcelain --untracked-files=no) ]] \
    || die "$root has uncommitted changes; commit or stash them, or run without --update"
  say "pulling $(git -C $root rev-parse --abbrev-ref HEAD)"
  git -C $root pull --ff-only
  exec $root/install.sh ${argv:#(-u|--update)}  # the pull may have changed this script
fi

# --- binary -----------------------------------------------------------------
command -v go >/dev/null || die "go is not on PATH"
bin_dir=${BIN_DIR:-$(go env GOBIN)}
[[ -n $bin_dir ]] || bin_dir=$(go env GOPATH)/bin
mkdir -p $bin_dir
dest=${bin_dir:A}/routr

if (( run_tests )); then
  say "testing"
  (cd $root && go vet ./... && go test -race ./...)
fi

say "building routr ($(git -C $root describe --always --dirty 2>/dev/null || print unknown))"
# Build beside the target and rename over it: a running watcher keeps its
# old inode, and nothing ever sees a half-written binary.
tmpd=$(mktemp -d $bin_dir/.routr-build.XXXXXX)
trap 'rm -rf $tmpd' EXIT
# srcDir lets routr version and routr doctor compare the binary with this
# checkout, whose skill and Pi extension are linked live.
(cd $root && go build -trimpath -ldflags "-X 'main.srcDir=$root'" -o $tmpd/routr ./cmd/routr)
if [[ -f $dest ]] && cmp -s $tmpd/routr $dest; then
  say "$dest is up to date"
else
  mv -f $tmpd/routr $dest
  say "installed $dest"
fi
[[ $(whence -p routr 2>/dev/null) == $dest ]] \
  || warn "the routr on PATH is $(whence -p routr 2>/dev/null || print none), not $dest"

# --- user config --------------------------------------------------------------
# The first time, a template with every key commented out and explained; a
# file that exists is never touched (routr config example prints the template).
$dest config init

# --- skill --------------------------------------------------------------------
# link TARGET LINK: create or repoint a symlink; never replace a real file.
link() {
  local target=$1 l=$2
  if [[ -L $l ]]; then
    if [[ ${l:A} == ${target:A} ]]; then say "$l is linked"; return; fi
    ln -sfn $target $l
    say "repointed $l"
  elif [[ -e $l ]]; then
    warn "$l exists and is not a symlink; left alone (remove it and run again)"
  else
    mkdir -p ${l:h}
    ln -s $target $l
    say "linked $l"
  fi
}

claude_dir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
codex_dir=${CODEX_HOME:-$HOME/.codex}
integer harnesses=0
if [[ -d $codex_dir ]] || command -v codex >/dev/null; then
  link $root/skills/routr $codex_dir/skills/routr; harnesses+=1
fi

# --- Claude Code plugin -----------------------------------------------------------
# The repo is a plugin marketplace whose routr plugin (frontend/) is read in
# place, so like the Pi extension it is registered once and follows the repo.
# The plugin ships the skill: a skill linked into the config dir duplicates it.
if command -v claude >/dev/null; then
  harnesses+=1
  if [[ $(claude plugin list --json 2>/dev/null) == *"\"readFromFolder\": \"$root/frontend\""* ]]; then
    say "Claude Code plugin is registered"
  else
    claude plugin marketplace add $root >/dev/null && claude plugin install routr@routr >/dev/null \
      || die "could not register the Claude Code plugin; try: claude plugin marketplace add $root && claude plugin install routr@routr"
    say "registered the Claude Code plugin"
  fi
  claude_skill=$claude_dir/skills/routr
  if [[ -L $claude_skill && ${claude_skill:A} == $root/skills/routr ]]; then
    rm $claude_skill
    say "removed $claude_skill: the plugin ships the skill"
  elif [[ -e $claude_skill ]]; then
    warn "$claude_skill duplicates the skill the plugin ships; remove it"
  fi
elif [[ -d $claude_dir ]]; then
  link $root/skills/routr $claude_dir/skills/routr; harnesses+=1  # no CLI to register the plugin with
fi

# --- Pi extension ---------------------------------------------------------------
# A local package is loaded in place, so it needs registering once; running
# Pi sessions pick up changes on restart (or /reload).
if command -v pi >/dev/null; then
  harnesses+=1
  if pi list 2>/dev/null | sed 's/^ *//' | grep -qxF $root/pi; then
    say "Pi extension is registered"
  else
    (cd $root && pi install ./pi)
    say "registered the Pi extension"
  fi
  pi_skill=${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}/skills/routr
  [[ -e $pi_skill ]] && warn "$pi_skill duplicates the skill the extension ships; remove it or set ROUTR_PI_SKILL=0"
fi
(( harnesses )) || warn "found no Claude Code, Codex or Pi; installed the binary only"

# --- watchers on the old binary ---------------------------------------------------
# A watcher runs until it has been idle for 10m, so after an update it can keep
# running the replaced binary. One with no worker in flight holds no state
# worth keeping: stop it and start it again with its own environment (workers'
# tabs inherit it). One mid-turn keeps what it has seen of its panes in memory,
# so it is left to finish.
restart_watchers() {
  [[ -d /proc ]] || return 0
  local pid state dir
  local -a envs
  for pid in $(pgrep -x routr 2>/dev/null || true); do
    [[ $(tr '\0' ' ' </proc/$pid/cmdline 2>/dev/null) == *' watch --foreground '* ]] || continue
    [[ $(readlink /proc/$pid/exe 2>/dev/null) == "$dest (deleted)" ]] || continue
    dir=$(readlink /proc/$pid/cwd) || continue
    envs=(${(0)"$(</proc/$pid/environ)"})
    state=${${(M)envs:#ROUTR_STATE_DIR=*}#ROUTR_STATE_DIR=}
    if (cd $dir && { [[ -z $state ]] || export ROUTR_STATE_DIR=$state } && $dest list --all --json 2>/dev/null) \
        | grep -Eq '"state": ?"(starting|running|blocked|maybe_done)"'; then
      warn "the watcher for $dir (pid $pid) runs the old routr and has workers in flight; left running." \
        "It switches once it idles out, or stop it when they finish: kill $pid"
      continue
    fi
    kill -TERM $pid 2>/dev/null || continue
    repeat 50; do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
    if kill -0 $pid 2>/dev/null; then
      warn "the watcher for $dir (pid $pid) did not stop; the next routr command there uses it"
      continue
    fi
    (cd $dir && env -i $envs $dest watch >/dev/null)
    say "restarted the watcher for $dir"
  done
}
restart_watchers

say "done: $($dest version | head -1). Check the setup from a herdr pane: routr doctor"
