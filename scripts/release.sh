#!/usr/bin/env bash
# Build, Developer ID-sign, notarize, and publish StatusPan release binaries.
# The signing identity and the notarytool keychain profile come from the
# environment. This script does not name a certificate or a keychain profile.
#
# Usage:
#   APPLE_SIGNING_IDENTITY="..." APPLE_NOTARYTOOL_PROFILE="..." ./scripts/release.sh v0.1.1

set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

if [[ -z "${APPLE_SIGNING_IDENTITY:-}" ]]; then
  echo "error: APPLE_SIGNING_IDENTITY is unset or empty" >&2
  exit 1
fi
if [[ -z "${APPLE_NOTARYTOOL_PROFILE:-}" ]]; then
  echo "error: APPLE_NOTARYTOOL_PROFILE is unset or empty" >&2
  exit 1
fi

if [[ $# -ne 1 ]]; then
  echo "usage: APPLE_SIGNING_IDENTITY=... APPLE_NOTARYTOOL_PROFILE=... $0 vX.Y.Z" >&2
  exit 1
fi

TAG="$1"
case "$TAG" in
  v*) ;;
  *)
    echo "error: tag must match v* (for example v0.1.1)" >&2
    exit 1
    ;;
esac

VERSION="${TAG#v}"
if [[ -z "$VERSION" || "$VERSION" == *"/"* || "$VERSION" == *" "* || "$VERSION" == *$'\t'* || "$VERSION" == *"'"* ]]; then
  echo "error: invalid version derived from tag ${TAG}" >&2
  exit 1
fi

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command not found: $1" >&2
    exit 1
  fi
}

require_cmd git
require_cmd go
require_cmd gh
require_cmd codesign
require_cmd ditto
require_cmd xcrun

if ! xcrun --find notarytool >/dev/null 2>&1; then
  echo "error: xcrun notarytool is not available" >&2
  exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: working tree is not clean" >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$branch" != "main" ]]; then
  echo "error: current branch is ${branch}; release must run from main" >&2
  exit 1
fi

if ! gh auth status; then
  echo "error: gh is not authenticated" >&2
  exit 1
fi

# Fail before building when the tag or release is already present.
# gh release create is what creates the tag; this script never tags locally.
refuse_if_release_exists() {
  if git show-ref --verify --quiet "refs/tags/${TAG}"; then
    echo "error: tag ${TAG} already exists locally; refusing to overwrite" >&2
    exit 1
  fi

  local remote_status=0
  git ls-remote --exit-code --tags origin "refs/tags/${TAG}" >/dev/null 2>&1 || remote_status=$?
  if [[ "$remote_status" -eq 0 ]]; then
    echo "error: tag ${TAG} already exists on origin; refusing to overwrite" >&2
    exit 1
  fi
  if [[ "$remote_status" -ne 2 ]]; then
    echo "error: could not check origin for tag ${TAG} (git ls-remote exit ${remote_status})" >&2
    exit 1
  fi

  if gh release view "$TAG" >/dev/null 2>&1; then
    echo "error: GitHub release ${TAG} already exists; refusing to overwrite" >&2
    exit 1
  fi
}

echo "running tests"
go test ./...

refuse_if_release_exists

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

build_one() {
  local os="$1"
  local arch="$2"
  echo "building statuspan-${os}-${arch} version ${VERSION}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o "${TMP}/statuspan-${os}-${arch}" ./cmd/statuspan
}

build_one darwin arm64
build_one darwin amd64
build_one linux amd64
build_one linux arm64

sign_darwin() {
  local bin="$1"
  echo "signing $(basename "$bin")"
  codesign --force --timestamp --options runtime --sign "$APPLE_SIGNING_IDENTITY" "$bin"
  codesign --verify --strict --verbose=2 "$bin"
}

sign_darwin "${TMP}/statuspan-darwin-arm64"
sign_darwin "${TMP}/statuspan-darwin-amd64"

notarize_darwin() {
  local bin="$1"
  local zip="${bin}.zip"
  local out id
  echo "notarizing $(basename "$bin")"
  ditto -c -k "$bin" "$zip"
  if ! out="$(xcrun notarytool submit "$zip" --keychain-profile "$APPLE_NOTARYTOOL_PROFILE" --wait 2>&1)"; then
    printf '%s\n' "$out" >&2
    echo "error: notarytool submit failed for $(basename "$bin")" >&2
    exit 1
  fi
  # Plain text is "status: Accepted". JSON output is "status": "Accepted".
  if ! printf '%s\n' "$out" | grep -Eqi 'status["[:space:]]*:[[:space:]]*"?Accepted'; then
    printf '%s\n' "$out" >&2
    echo "error: notarization was not accepted for $(basename "$bin")" >&2
    exit 1
  fi
  id="$(printf '%s\n' "$out" | awk '/^[[:space:]]*id:/{print $2}' | tail -n 1)"
  echo "notarization accepted for $(basename "$bin") (status: Accepted, id: ${id:-unknown})"
}

notarize_darwin "${TMP}/statuspan-darwin-arm64"
notarize_darwin "${TMP}/statuspan-darwin-amd64"

echo "writing SHA256SUMS"
(
  cd "$TMP"
  shasum -a 256 \
    statuspan-darwin-arm64 \
    statuspan-darwin-amd64 \
    statuspan-linux-amd64 \
    statuspan-linux-arm64 \
    > SHA256SUMS
)

# Re-check immediately before publish so a tag created while signing cannot be overwritten.
refuse_if_release_exists

echo "creating GitHub release ${TAG}"
gh release create "$TAG" --target "$(git rev-parse HEAD)" \
  "$TMP/statuspan-darwin-arm64" \
  "$TMP/statuspan-darwin-amd64" \
  "$TMP/statuspan-linux-amd64" \
  "$TMP/statuspan-linux-arm64" \
  "$TMP/SHA256SUMS" \
  --title "$TAG" \
  --notes "Binaries for ${TAG}. macOS builds are Developer ID signed and notarized. SHA256SUMS lists the binaries. Install with: curl -fsSL https://statuspan.com/install.sh | sh"

git fetch --tags origin
echo "release ${TAG} published"
