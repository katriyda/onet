#!/bin/sh
# onet installer: download a release binary, verify its SHA256, install atomically.
set -eu

REPO=katriyda/onet
BASE=https://github.com/katriyda/onet/releases
DEFAULT_DIR=${HOME}/.local/bin

usage() {
  cat <<'EOF'
onet installer - download and install the onet binary.

usage: install.sh [-v VERSION] [-d DIR]
  -v, --version VERSION   install a specific release tag (e.g. v0.3.1)
  -d, --dir DIR           install directory (default: $HOME/.local/bin)
  -h, --help              show this help

env: ONET_VERSION, ONET_INSTALL_DIR
EOF
}

die() {
  printf 'error: %s\n' "$1" >&2
  exit "${2:-1}"
}

VERSION=${ONET_VERSION:-}
DIR=${ONET_INSTALL_DIR:-$DEFAULT_DIR}

while [ $# -gt 0 ]; do
  case "$1" in
    -v|--version)
      [ $# -ge 2 ] || { printf 'error: %s requires a value\n' "$1" >&2; exit 2; }
      VERSION=$2
      shift 2
      ;;
    -d|--dir)
      [ $# -ge 2 ] || { printf 'error: %s requires a value\n' "$1" >&2; exit 2; }
      DIR=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'error: unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# Normalize and validate an explicit version tag.
if [ -n "$VERSION" ]; then
  VERSION=${VERSION#v}
  case "$VERSION" in
    *[!0-9.]*|.*|*.|*..*|*.*.*.*)
      die "invalid version tag (expected x.y.z): $VERSION" 2
      ;;
    *.*.*) ;;
    *)
      die "invalid version tag (expected x.y.z): $VERSION" 2
      ;;
  esac
  major=${VERSION%%.*}
  rest=${VERSION#*.}
  minor=${rest%%.*}
  patch=${rest#*.}
  case "$major$minor$patch" in
    *[!0-9]*|'') die "invalid version tag (expected x.y.z): $VERSION" 2 ;;
  esac
fi

# Platform detection.
os=$(uname -s 2>/dev/null || echo unknown)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  FreeBSD) os=freebsd ;;
  MINGW*|MSYS*|CYGWIN*)
    printf 'error: Windows is not supported by install.sh\n' >&2
    printf 'use: irm https://katriyda.github.io/onet/install.ps1 | iex\n' >&2
    exit 1
    ;;
  *)
    die "unsupported OS: $os"
    ;;
esac

machine=$(uname -m 2>/dev/null || echo unknown)
case "$machine" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7l|armv7|armhf) arch=arm-armv7 ;;
  *)
    die "unsupported architecture: $machine"
    ;;
esac

case "$os-$arch" in
  linux-amd64|linux-arm64|linux-arm-armv7|darwin-amd64|darwin-arm64|freebsd-amd64) ;;
  *)
    printf 'error: no release for %s/%s\n' "$os" "$arch" >&2
    printf 'see: %s\n' "$BASE/latest" >&2
    exit 1
    ;;
esac

asset="onet-$os-$arch.tar.gz"

if [ -n "$VERSION" ]; then
  base="$BASE/download/v$VERSION"
else
  base="$BASE/latest/download"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "neither curl nor wget found"
fi

tmp=
cleanup() {
  if [ -n "$tmp" ] && [ -d "$tmp" ]; then
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT INT TERM

tmp=$(mktemp -d) || die "cannot create a temporary directory"

fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
tar -xzf "$tmp/$asset" -C "$tmp" || die "cannot extract $asset"

src="$tmp/onet-$os-$arch/onet"
if [ ! -f "$src" ]; then
  src=$(find "$tmp" -type f -name onet -print | head -n 1)
fi
[ -n "$src" ] && [ -f "$src" ] || die "binary not found in $asset"

# SHA256 verification against checksums.txt from the same release.
if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
  expected=$(awk -v f="$asset" '{ gsub(/^\*/, "", $2) } $2==f { print $1; exit }' "$tmp/checksums.txt")
  [ -n "$expected" ] || die "$asset is not listed in checksums.txt"

  actual=
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
  elif command -v sha256 >/dev/null 2>&1; then
    actual=$(sha256 -q "$tmp/$asset")
  else
    printf 'warning: no SHA256 tool found; skipping verification\n' >&2
  fi

  if [ -n "$actual" ]; then
    expected_cmp=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
    actual_cmp=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
    [ "$expected_cmp" = "$actual_cmp" ] || die "checksum mismatch for $asset"
  fi
else
  printf 'warning: checksums.txt not available for this release; skipping verification\n' >&2
fi

mkdir -p "$DIR" 2>/dev/null || die "cannot create install directory: $DIR (pass -d to choose another)"

if ! cp "$src" "$DIR/.onet.$$" 2>/dev/null \
  || ! chmod 755 "$DIR/.onet.$$" 2>/dev/null \
  || ! mv -f "$DIR/.onet.$$" "$DIR/onet" 2>/dev/null; then
  rm -f "$DIR/.onet.$$"
  die "cannot install to $DIR (pass -d to choose another)"
fi

if ! ver_out=$("$DIR/onet" version 2>/dev/null); then
  die "downloaded binary failed to start"
fi
ver=$(printf '%s' "$ver_out" | tr -d '\r\n')
ver=${ver#v}
[ -n "$ver" ] || ver=${VERSION:-latest}

printf 'onet %s (%s/%s)\n' "$ver" "$os" "$arch"
printf 'installed: %s/onet\n' "$DIR"
printf 'run: onet\n'

case ":$PATH:" in
  *":$DIR:"*) ;;
  *) printf 'path: export PATH="%s:$PATH"\n' "$DIR" ;;
esac
