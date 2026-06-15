#!/usr/bin/env bash
#
# install-on-device.sh — build the Crabbox app and install it on a connected
# iPhone, using free Apple-ID provisioning. Requires FULL Xcode (not just the
# Command Line Tools).
#
# One-time setup (after installing Xcode from the Mac App Store):
#   sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
#   sudo xcodebuild -runFirstLaunch
#   # In Xcode > Settings > Accounts, add your Apple ID (creates a free
#   # "Personal Team"). Note your Team ID (10 chars) from that screen.
#
# Usage:
#   DEVELOPMENT_TEAM=XXXXXXXXXX ./scripts/install-on-device.sh
#   # optional: BUNDLE_ID=sh.crabbox.Crabbox.<you>   (must be globally unique)
#
set -euo pipefail
cd "$(dirname "$0")/.."

if ! xcode-select -p 2>/dev/null | grep -q "Xcode.app"; then
  echo "ERROR: full Xcode not selected. Run:"
  echo "  sudo xcode-select -s /Applications/Xcode.app/Contents/Developer"
  exit 1
fi

: "${DEVELOPMENT_TEAM:?set DEVELOPMENT_TEAM=<your 10-char Apple Team ID> (Xcode > Settings > Accounts)}"
BUNDLE_ID="${BUNDLE_ID:-sh.crabbox.Crabbox}"

command -v xcodegen >/dev/null 2>&1 || brew install xcodegen
if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: Go is required because the iOS app links the CrabboxMobile Go core."
  echo "Install it with: brew install go"
  exit 1
fi
echo "==> Generating Xcode project"
xcodegen generate

# Find the connected physical device's UDID. Override with DEVICE_ID=… if the
# auto-detect picks the wrong one (list candidates with:
#   xcrun devicectl list devices    — or —   xcrun xctrace list devices).
echo "==> Locating connected device"
UDID="${DEVICE_ID:-}"
if [ -z "$UDID" ]; then
  UDID="$(xcrun xctrace list devices 2>&1 \
    | grep -iE 'iPhone|iPad' | grep -vi simulator \
    | head -1 | sed -E 's/.*\(([0-9A-Fa-f-]{8,})\).*/\1/')"
fi
if [ -z "${UDID:-}" ]; then
  echo "No physical device auto-detected. Connect + unlock your iPhone, tap Trust,"
  echo "then re-run with the UDID, e.g.: DEVICE_ID=<udid> $0"
  echo "Devices seen:"; xcrun xctrace list devices 2>&1 | sed -n '1,25p'
  exit 1
fi
echo "    device UDID: $UDID"

echo "==> Building + signing (free provisioning)"
xcodebuild \
  -project Crabbox.xcodeproj \
  -scheme Crabbox \
  -configuration Debug \
  -destination "id=$UDID" \
  -allowProvisioningUpdates \
  DEVELOPMENT_TEAM="$DEVELOPMENT_TEAM" \
  PRODUCT_BUNDLE_IDENTIFIER="$BUNDLE_ID" \
  build

APP="$(find ~/Library/Developer/Xcode/DerivedData -type d -name 'Crabbox.app' -path '*Debug-iphoneos*' 2>/dev/null | head -1)"
echo "==> Installing $APP onto device"
xcrun devicectl device install app --device "$UDID" "$APP"

echo
echo "DONE. On the iPhone: Settings > General > VPN & Device Management >"
echo "trust your developer certificate, then launch Crabbox."
echo "Enter your islo key in: Run tab > provider settings > islo.dev."
