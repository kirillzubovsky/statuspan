#!/usr/bin/env bash
# Notarize a StatusPan disk image and staple the ticket to it.
# The signing profile comes from APPLE_NOTARYTOOL_PROFILE. This script does
# not name a keychain profile.
#
# Usage:
#   APPLE_NOTARYTOOL_PROFILE=... macos/notarize.sh path/to/StatusPan.dmg

set -euo pipefail

if [[ $# -ne 1 || "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  echo "usage: APPLE_NOTARYTOOL_PROFILE=... $0 path-to-dmg" >&2
  exit 2
fi

DMG="$1"
if [[ -z "${APPLE_NOTARYTOOL_PROFILE:-}" ]]; then
  echo "error: APPLE_NOTARYTOOL_PROFILE is unset or empty" >&2
  exit 1
fi
if [[ ! -f "$DMG" ]]; then
  echo "error: dmg not found: ${DMG}" >&2
  exit 1
fi
if ! xcrun --find notarytool >/dev/null 2>&1; then
  echo "error: xcrun notarytool is not available" >&2
  exit 1
fi
if ! xcrun --find stapler >/dev/null 2>&1; then
  echo "error: xcrun stapler is not available" >&2
  exit 1
fi

echo "notarizing $(basename "$DMG")"
if ! out="$(xcrun notarytool submit "$DMG" --keychain-profile "$APPLE_NOTARYTOOL_PROFILE" --wait 2>&1)"; then
  printf '%s\n' "$out" >&2
  echo "error: notarytool submit failed for $(basename "$DMG")" >&2
  exit 1
fi
# Plain text is "status: Accepted". JSON output is "status": "Accepted".
if ! printf '%s\n' "$out" | grep -Eqi 'status["[:space:]]*:[[:space:]]*"?Accepted'; then
  printf '%s\n' "$out" >&2
  echo "error: notarization was not accepted for $(basename "$DMG")" >&2
  exit 1
fi
id="$(printf '%s\n' "$out" | awk '/^[[:space:]]*id:/{print $2}' | tail -n 1)"
echo "notarization accepted for $(basename "$DMG") (status: Accepted, id: ${id:-unknown})"

xcrun stapler staple "$DMG"
xcrun stapler validate "$DMG"
