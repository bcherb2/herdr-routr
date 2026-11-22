#!/usr/bin/env zsh
# Build routr, then run `routr setup`: it checks the binary's directory is on
# PATH, writes the user config the first time, restarts idle watchers on the
# old binary, checks herdr and the harnesses, and prints the command that adds
# routr's front end (Claude Code plugin, Pi extension, Codex skill) to each
# harness it finds. Neither installs nor registers anything outside routr.
# Run it again to update: after a git pull, or with --update to pull first.
#
#   ./install.sh [--update] [--test]
#
#   -u, --update  git pull --ff-only first (refuses with uncommitted changes)
#   -t, --test    go vet and go test -race before installing
#
# The binary goes to $BIN_DIR, else `go env GOBIN`, else `go env GOPATH`/bin.
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
die()  { print -P -u2 -- "%F{red}error:%f $*"; exit 1 }

if (( update )); then
  [[ -z $(git -C $root status --porcelain --untracked-files=no) ]] \
    || die "$root has uncommitted changes; commit or stash them, or run without --update"
  say "pulling $(git -C $root rev-parse --abbrev-ref HEAD)"
  git -C $root pull --ff-only
  exec $root/install.sh ${argv:#(-u|--update)}  # the pull may have changed this script
fi

# --- binary -----------------------------------------------------------------
command -v go >/dev/null \
  || die "go is not on PATH: install Go (go.mod names the version) to build routr, then run install.sh again"
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
# checkout, and routr setup name the front ends in it.
(cd $root && go build -trimpath -ldflags "-X 'main.srcDir=$root'" -o $tmpd/routr ./cmd/routr)
if [[ -f $dest ]] && cmp -s $tmpd/routr $dest; then
  say "$dest is up to date"
else
  mv -f $tmpd/routr $dest
  say "installed $dest"
fi

# --- setup --------------------------------------------------------------------
# The binary is already in $bin_dir, so setup's link there finds it in place.
$dest setup --bin-dir $bin_dir
