#!/bin/sh
# The herdr plugin's build step: put a routr binary at bin/routr in this
# checkout. Run it from the checkout root, as herdr-plugin.toml's [[build]]
# does.
#
# It downloads the release that matches herdr-plugin.toml's version
# (tag v<version>) for this OS and CPU, checks it against the release's
# checksums.txt and unpacks it. When there is no such release, or it cannot
# be fetched, it builds from this checkout with go instead. It installs
# nothing else.
#
#   ROUTR_RELEASE_BASE  where the release assets are, instead of
#                       https://github.com/bcherb2/herdr-routr/releases/download/v<version>:
#                       a URL, a file:// URL or a directory, such as a
#                       goreleaser snapshot's dist/
#   ROUTR_BIN_LINK      the PATH link to keep pointing at this install,
#                       instead of ~/.local/bin/routr (routr setup's default)
#   ROUTR_PLUGIN_LINK   the link the front ends are added from, to keep
#                       pointing at this install, instead of
#                       ${XDG_DATA_HOME:-~/.local/share}/routr/plugin (routr setup's)
set -eu

say() { printf 'routr build: %s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

[ -f herdr-plugin.toml ] || die "no herdr-plugin.toml in $(pwd); run this from the routr checkout's root"

# The top-level version = "x.y.z", before any [table].
version=$(awk '
  /^[[:space:]]*\[/ { exit }
  /^[[:space:]]*version[[:space:]]*=[[:space:]]*"/ { sub(/^[^"]*"/, ""); sub(/".*/, ""); print; exit }
' herdr-plugin.toml)
case $version in
  '' | *[!0-9A-Za-z.+-]*) die "herdr-plugin.toml has no usable top-level version = \"x.y.z\" (found '$version')" ;;
esac

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) os= ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) arch= ;;
esac

base=${ROUTR_RELEASE_BASE:-https://github.com/bcherb2/herdr-routr/releases/download/v$version}
asset=routr_${version}_${os}_${arch}.tar.gz

tmp=$(mktemp -d "${TMPDIR:-/tmp}/routr-build.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# fetch NAME: copy the release asset NAME into $tmp.
fetch() {
  case $base in
    file://*) cp "${base#file://}/$1" "$tmp/$1" 2>/dev/null ;;
    http://* | https://*)
      if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 2 -o "$tmp/$1" "$base/$1"
      elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$tmp/$1" "$base/$1"
      else
        say "neither curl nor wget is installed"
        return 1
      fi ;;
    *) cp "$base/$1" "$tmp/$1" 2>/dev/null ;;
  esac
}

# sha256 FILE: print FILE's SHA-256; fails when there is no tool for it.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    set -- "$(sha256sum "$1")"
  elif command -v shasum >/dev/null 2>&1; then
    set -- "$(shasum -a 256 "$1")"
  else
    return 1
  fi
  printf '%s\n' "${1%% *}"
}

# install FILE: move FILE to bin/routr. The rename leaves a routr that is
# running on its old inode, and nothing sees a half-written binary.
install_bin() {
  chmod 755 "$1"
  mv -f "$1" bin/routr
  say "installed bin/routr: $(bin/routr version | head -n 1)"
  relink
}

# relink: herdr builds each update in a new install directory and later
# removes the old one, so move the links routr setup made to this one: the
# PATH link to bin/routr, and the link to the install the front ends are
# added from.
relink() {
  repoint "${ROUTR_BIN_LINK:-${HOME:-}/.local/bin/routr}" /bin/routr
  repoint "${ROUTR_PLUGIN_LINK:-${XDG_DATA_HOME:-${HOME:-}/.local/share}/routr/plugin}" ''
}

# repoint LINK PATH: point LINK at PATH under this install. Only a link that
# already points at PATH in a herdr-managed routr install is touched;
# anything else is the user's.
repoint() {
  [ -L "$1" ] || return 0
  old=$(readlink "$1")
  case $old in
    */herdr/plugins/github/routr-*"$2") ;;
    *) return 0 ;;
  esac
  [ "$old" = "$PWD$2" ] && return 0
  ln -sfn "$PWD$2" "$1"
  say "repointed $1 from the previous install to this one"
}

mkdir -p bin

if [ -z "$os" ] || [ -z "$arch" ]; then
  say "no release binary for $(uname -s) $(uname -m)"
elif fetch "$asset" && fetch checksums.txt; then
  # A release that does not match its checksums is never used or worked around.
  want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
  [ -n "$want" ] || die "checksums.txt from $base does not list $asset"
  got=$(sha256 "$tmp/$asset") || die "cannot verify $asset: neither sha256sum nor shasum is installed"
  [ "$got" = "$want" ] || die "$asset from $base fails its checksum (sha256 $got, want $want); not installing it"
  tar -xzf "$tmp/$asset" -C "$tmp" routr || die "$asset from $base has no routr binary"
  cp "$tmp/routr" bin/.routr.new
  say "v$version release for ${os}_$arch, checksum verified"
  install_bin bin/.routr.new
  exit 0
else
  say "could not fetch $asset and checksums.txt from $base"
fi

command -v go >/dev/null 2>&1 ||
  die "no v$version release binary to install, and go is not installed to build one. Install a release version of the plugin, or install Go (https://go.dev/dl/) and run the build again"
say "building routr from this checkout with $(go version)"
# srcDir, as install.sh sets it, lets routr version and routr doctor compare
# the binary with this checkout.
go build -trimpath -ldflags "-X 'main.srcDir=$PWD'" -o bin/.routr.new ./cmd/routr
install_bin bin/.routr.new
