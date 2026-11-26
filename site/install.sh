#!/bin/sh
# Install the StatusPan CLI.
#
# Default destination:
#   /usr/local/bin  when running as root, or when that directory is writable
#   $HOME/.local/bin  otherwise
# Override with STATUSPAN_BIN_DIR.
# Tests can point STATUSPAN_RELEASE_BASE at a local file server.
# This script does not edit shell rc files.

set -eu

tmp=""
cleanup() {
  if [ -n "$tmp" ]; then
    rm -f "$tmp"
  fi
}
trap cleanup EXIT

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

if [ -n "${STATUSPAN_BIN_DIR:-}" ]; then
  dest=$STATUSPAN_BIN_DIR
elif [ "$(id -u)" -eq 0 ] || [ -w /usr/local/bin ]; then
  dest=/usr/local/bin
else
  if [ -z "${HOME:-}" ]; then
    printf '%s\n' "statuspan: HOME is not set" >&2
    exit 1
  fi
  dest="${HOME}/.local/bin"
fi

while [ "$dest" != "/" ] && [ "${dest%/}" != "$dest" ]; do
  dest=${dest%/}
done

if [ -z "$dest" ]; then
  printf '%s\n' "statuspan: install directory is empty" >&2
  exit 1
fi

name="statuspan-${os}-${arch}"
url="${base}/${name}"
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

cmd=statuspan
path_ok=0
case ":${PATH}:" in
  *":${dest}:"*) path_ok=1 ;;
esac

printf '%s\n' "Installed ${bin}" ""

if [ "$path_ok" -eq 0 ]; then
  cmd=$bin
  rc_name="~/.bashrc"
  case "${SHELL:-}" in
    */zsh) rc_name="~/.zshrc" ;;
    */bash) rc_name="~/.bashrc" ;;
  esac
  printf '%s\n' \
    "${dest} is not on PATH. Add it for this shell with:" \
    "  export PATH=\"${dest}:\$PATH\"" \
    "" \
    "To keep it for new shells, append that line to ${rc_name}:" \
    "  echo 'export PATH=\"${dest}:\$PATH\"' >> ${rc_name}" \
    "" \
    "Or run the binary by its full path right now:" \
    "  ${bin} agent install" \
    ""
fi

printf '%s\n' "Next:" "  ${cmd} serve" "  open http://localhost:4477" \
  "" \
  "AI agents: run '${cmd} agent' for instructions, or '${cmd} agent install' to install the skill."
