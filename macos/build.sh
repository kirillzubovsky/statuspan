#!/usr/bin/env bash
# Build a universal StatusPan.app and a UDZO disk image into macos/.build/.
#
# Usage:
#   macos/build.sh VERSION [--sign]
#
# VERSION is written into the app plist and into the bundled daemon
# (-X main.version=VERSION). --sign requires APPLE_SIGNING_IDENTITY.
# Already-built thin daemon binaries can be passed as
# STATUSPAN_BIN_ARM64 and STATUSPAN_BIN_AMD64 (Go arch names).

set -euo pipefail

MACOS="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$MACOS/.." && pwd)"
cd "$ROOT"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command not found: $1" >&2
    exit 1
  fi
}

if [[ $# -lt 1 || "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  echo "usage: macos/build.sh VERSION [--sign]" >&2
  exit 2
fi

VERSION="$1"
shift
SIGN=0
if [[ $# -gt 0 ]]; then
  if [[ $# -ne 1 || "$1" != "--sign" ]]; then
    echo "usage: macos/build.sh VERSION [--sign]" >&2
    exit 2
  fi
  SIGN=1
fi

case "$VERSION" in
  "" | -* | *"/"* | *" "* | *"'"* | *'"'* )
    echo "error: invalid version ${VERSION}" >&2
    exit 1
    ;;
esac
if [[ "$VERSION" == *$'\t'* || "$VERSION" == *$'\n'* ]]; then
  echo "error: invalid version" >&2
  exit 1
fi

require_cmd swiftc
require_cmd lipo
require_cmd hdiutil
require_cmd xcrun
require_cmd ditto

if [[ "$SIGN" -eq 1 ]]; then
  require_cmd codesign
  if [[ -z "${APPLE_SIGNING_IDENTITY:-}" ]]; then
    echo "error: APPLE_SIGNING_IDENTITY is unset or empty" >&2
    exit 1
  fi
fi

if [[ ! -f "$MACOS/StatusPan.icns" ]]; then
  echo "error: macos/StatusPan.icns is missing. Run macos/make-icon.sh." >&2
  exit 1
fi
if [[ ! -f "$MACOS/StatusPan.entitlements" ]]; then
  echo "error: macos/StatusPan.entitlements is missing" >&2
  exit 1
fi

need_go=0
if [[ -z "${STATUSPAN_BIN_ARM64:-}" || -z "${STATUSPAN_BIN_AMD64:-}" ]]; then
  need_go=1
fi
if [[ "$need_go" -eq 1 ]]; then
  require_cmd go
fi

xml_escape() {
  local s=$1
  s=${s//&/&amp;}
  s=${s//</&lt;}
  s=${s//>/&gt;}
  printf '%s' "$s"
}

daemon_one() {
  local goarch=$1
  local dest=$2
  local override=""
  case "$goarch" in
    arm64) override="${STATUSPAN_BIN_ARM64:-}" ;;
    amd64) override="${STATUSPAN_BIN_AMD64:-}" ;;
    *)
      echo "error: unsupported daemon arch ${goarch}" >&2
      exit 1
      ;;
  esac
  if [[ -n "$override" ]]; then
    if [[ ! -f "$override" ]]; then
      echo "error: binary not found: ${override}" >&2
      exit 1
    fi
    cp "$override" "$dest"
    chmod +x "$dest"
    return
  fi
  echo "building statuspan daemon ${goarch} version ${VERSION}"
  CGO_ENABLED=0 GOOS=darwin GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$dest" ./cmd/statuspan
}

BUILD="$MACOS/.build"
OBJ="$BUILD/obj"
APP="$BUILD/StatusPan.app"
DMG="$BUILD/StatusPan.dmg"
STAGE=""

cleanup() {
  if [[ -n "$STAGE" ]]; then
    rm -rf "$STAGE"
  fi
}
trap cleanup EXIT

rm -rf "$APP" "$OBJ"
mkdir -p "$OBJ" "$APP/Contents/MacOS" "$APP/Contents/Resources"

SDK="$(xcrun --sdk macosx --show-sdk-path)"
sources=(
  "$MACOS/StatusPan/main.swift"
  "$MACOS/StatusPan/BoardEndpoint.swift"
  "$MACOS/StatusPan/DaemonChild.swift"
  "$MACOS/StatusPan/OfflinePage.swift"
  "$MACOS/StatusPan/AppDelegate.swift"
)

compile_app() {
  local target=$1
  local dest=$2
  echo "compiling StatusPan ${target}"
  swiftc -swift-version 5 -target "$target" -sdk "$SDK" -O -wmo \
    -module-name StatusPan \
    -module-cache-path "$BUILD/ModuleCache" \
    -framework AppKit \
    -framework WebKit \
    -o "$dest" \
    "${sources[@]}"
}

# Each slice is named StatusPan / statuspan so the linker id matches across arches.
mkdir -p "$OBJ/arm64" "$OBJ/x86_64"
compile_app arm64-apple-macos12.0 "$OBJ/arm64/StatusPan"
compile_app x86_64-apple-macos12.0 "$OBJ/x86_64/StatusPan"
lipo -create "$OBJ/arm64/StatusPan" "$OBJ/x86_64/StatusPan" -output "$APP/Contents/MacOS/StatusPan"

mkdir -p "$OBJ/daemon-arm64" "$OBJ/daemon-amd64"
daemon_one arm64 "$OBJ/daemon-arm64/statuspan"
daemon_one amd64 "$OBJ/daemon-amd64/statuspan"
lipo -create "$OBJ/daemon-arm64/statuspan" "$OBJ/daemon-amd64/statuspan" -output "$APP/Contents/Resources/statuspan"
chmod +x "$APP/Contents/MacOS/StatusPan" "$APP/Contents/Resources/statuspan"

cp "$MACOS/StatusPan.icns" "$APP/Contents/Resources/StatusPan.icns"
printf 'APPL????' > "$APP/Contents/PkgInfo"

VERSION_XML="$(xml_escape "$VERSION")"
cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleExecutable</key>
  <string>StatusPan</string>
  <key>CFBundleIconFile</key>
  <string>StatusPan</string>
  <key>CFBundleIdentifier</key>
  <string>com.statuspan.app</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>StatusPan</string>
  <key>CFBundleDisplayName</key>
  <string>StatusPan</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>${VERSION_XML}</string>
  <key>CFBundleVersion</key>
  <string>${VERSION_XML}</string>
  <key>LSMinimumSystemVersion</key>
  <string>12.0</string>
  <key>LSApplicationCategoryType</key>
  <string>public.app-category.developer-tools</string>
  <key>NSHighResolutionCapable</key>
  <true/>
  <key>NSHumanReadableCopyright</key>
  <string>MIT License</string>
  <key>NSPrincipalClass</key>
  <string>NSApplication</string>
  <key>NSAppTransportSecurity</key>
  <dict>
    <key>NSAllowsLocalNetworking</key>
    <true/>
  </dict>
</dict>
</plist>
EOF

if [[ "$SIGN" -eq 1 ]]; then
  echo "signing bundled daemon"
  codesign --force --timestamp --options runtime --sign "$APPLE_SIGNING_IDENTITY" \
    "$APP/Contents/Resources/statuspan"
  echo "signing StatusPan.app"
  codesign --force --timestamp --options runtime \
    --entitlements "$MACOS/StatusPan.entitlements" \
    --sign "$APPLE_SIGNING_IDENTITY" \
    "$APP"
  codesign --verify --deep --strict --verbose=2 "$APP"
fi

STAGE="$(mktemp -d)"
ditto "$APP" "$STAGE/StatusPan.app"
ln -s /Applications "$STAGE/Applications"
echo "creating disk image"
hdiutil create -volname StatusPan -srcfolder "$STAGE" -ov -format UDZO "$DMG"
if [[ "$SIGN" -eq 1 ]]; then
  codesign --force --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$DMG"
  codesign --verify --verbose=2 "$DMG"
fi

echo "app: macos/.build/StatusPan.app"
echo "dmg: macos/.build/StatusPan.dmg"
lipo -info "macos/.build/StatusPan.app/Contents/MacOS/StatusPan"
lipo -info "macos/.build/StatusPan.app/Contents/Resources/statuspan"
