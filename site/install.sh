#!/bin/sh
# Install StatusPan into ~/.local/bin. No sudo.
# Tests can point STATUSPAN_RELEASE_BASE at a local file server.

set -eu

tmp=""
cleanup() {
  if [ -n "$tmp" ]; then
    rm -f "$tmp"
  fi
}
trap cleanup EXIT

if [ -z "${HOME:-}" ]; then
  printf '%s\n' "statuspan: HOME is not set" >&2
  exit 1
fi

base="${STATUSPAN_RELEASE_BASE:-https://github.com/kirillzubovsky/statuspan/releases/latest/download}"
base="${base%/}"

sys=$(uname -s)
mach=$(uname -m)

case "$sys" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    printf '%s\n' "statuspan: unsupported operating system: $sys" >&2
    exit 1
    ;;
esac

case "$mach" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *)
    printf '%s\n' "statuspan: unsupported architecture: $mach" >&2
    exit 1
    ;;
esac

name="statuspan-${os}-${arch}"
url="${base}/${name}"
dest="${HOME}/.local/bin"
bin="${dest}/statuspan"

mkdir -p "$dest"
tmp="${dest}/.statuspan.partial.$$"

if command -v curl >/dev/null 2>&1; then
  if ! curl -fsSL "$url" -o "$tmp"; then
    printf '%s\n' "statuspan: download failed: $url" >&2
    exit 1
  fi
elif command -v wget >/dev/null 2>&1; then
  if ! wget -q -O "$tmp" "$url"; then
    printf '%s\n' "statuspan: download failed: $url" >&2
    exit 1
  fi
else
  printf '%s\n' "statuspan: curl or wget is required" >&2
  exit 1
fi

sig=$(LC_ALL=C dd if="$tmp" bs=1 count=1 2>/dev/null || true)
case "$sig" in
  ""|"<"|"{")
    printf '%s\n' "statuspan: download did not return a binary: $url" >&2
    exit 1
    ;;
esac

mv -f "$tmp" "$bin"
tmp=""
chmod +x "$bin"

printf '%s\n' "Installed ${bin}" "" "Next:" "  statuspan serve" "  open http://localhost:4477"

case ":${PATH}:" in
  *":${dest}:"*) ;;
  *)
    printf '%s\n' "" "${dest} is not on PATH. Add it with:" '  export PATH="$HOME/.local/bin:$PATH"'
    ;;
esac
