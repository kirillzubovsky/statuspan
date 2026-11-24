#!/usr/bin/env bash
# Render site/favicon.svg into macos/StatusPan.icns using only macOS tools.
# A normal app build uses the checked-in icns and does not run this script.

set -euo pipefail

MACOS="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$MACOS/.." && pwd)"
SVG="$ROOT/site/favicon.svg"

if [[ ! -f "$SVG" ]]; then
  echo "error: favicon not found: site/favicon.svg" >&2
  exit 1
fi

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command not found: $1" >&2
    exit 1
  fi
}

require_cmd swiftc
require_cmd sips
require_cmd iconutil
require_cmd xcrun

SDK="$(xcrun --sdk macosx --show-sdk-path)"
ARCH="$(uname -m)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "rendering icon"
swiftc -swift-version 5 -target "${ARCH}-apple-macos12.0" -sdk "$SDK" \
  -framework AppKit -O \
  -o "$WORK/render-icon" \
  "$MACOS/render-icon.swift"
"$WORK/render-icon" "$SVG" "$WORK/icon-1024.png"

ICONSET="$WORK/StatusPan.iconset"
mkdir -p "$ICONSET"

# 16, 32, 128, 256, 512 at 1x and 2x. sips -z takes height then width.
sips -z 16 16 "$WORK/icon-1024.png" --out "$ICONSET/icon_16x16.png" >/dev/null
sips -z 32 32 "$WORK/icon-1024.png" --out "$ICONSET/icon_16x16@2x.png" >/dev/null
sips -z 32 32 "$WORK/icon-1024.png" --out "$ICONSET/icon_32x32.png" >/dev/null
sips -z 64 64 "$WORK/icon-1024.png" --out "$ICONSET/icon_32x32@2x.png" >/dev/null
sips -z 128 128 "$WORK/icon-1024.png" --out "$ICONSET/icon_128x128.png" >/dev/null
sips -z 256 256 "$WORK/icon-1024.png" --out "$ICONSET/icon_128x128@2x.png" >/dev/null
sips -z 256 256 "$WORK/icon-1024.png" --out "$ICONSET/icon_256x256.png" >/dev/null
sips -z 512 512 "$WORK/icon-1024.png" --out "$ICONSET/icon_256x256@2x.png" >/dev/null
sips -z 512 512 "$WORK/icon-1024.png" --out "$ICONSET/icon_512x512.png" >/dev/null
sips -z 1024 1024 "$WORK/icon-1024.png" --out "$ICONSET/icon_512x512@2x.png" >/dev/null

iconutil -c icns "$ICONSET" -o "$WORK/StatusPan.icns"
mv "$WORK/StatusPan.icns" "$MACOS/StatusPan.icns"
echo "wrote macos/StatusPan.icns"
