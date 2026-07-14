#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=scripts/release-config.sh
source "$ROOT/scripts/release-config.sh"

usage() {
  echo "usage: $0 <identifier> <arm64|x86_64> <binary>" >&2
  exit 2
}

[[ $# -eq 3 ]] || usage
IDENTIFIER=$1
ARCH=$(crabbox_release_normalize_macos_arch "$2")
BINARY=$3
crabbox_release_assert_identifier_arch "$IDENTIFIER" "$ARCH"
crabbox_release_assert_no_publication_tokens

[[ "$(uname -s)" == Darwin ]] || {
  echo "official macOS release signing must run on macOS" >&2
  exit 1
}
[[ -f "$BINARY" && ! -L "$BINARY" && -x "$BINARY" ]] || {
  echo "release binary must be a regular executable file: $BINARY" >&2
  exit 2
}

CODESIGN_IDENTITY=${CODESIGN_IDENTITY:-${MAC_RELEASE_CODESIGN_IDENTITY:-}}
[[ "$CODESIGN_IDENTITY" == "$CRABBOX_RELEASE_AUTHORITY" ]] || {
  echo "official macOS releases require $CRABBOX_RELEASE_AUTHORITY" >&2
  exit 1
}
[[ -n "${NOTARYTOOL_KEYCHAIN_PROFILE:-}" ]] || {
  echo "NOTARYTOOL_KEYCHAIN_PROFILE is required for official macOS releases" >&2
  exit 1
}
[[ -n "${MAC_RELEASE_CODESIGN_KEYCHAIN:-}" ]] || {
  echo "MAC_RELEASE_CODESIGN_KEYCHAIN is required for official macOS releases" >&2
  exit 1
}

for tool in codesign csreq ditto lipo node plutil xcrun; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

ACTUAL_ARCH=$(lipo -archs "$BINARY")
[[ "$ACTUAL_ARCH" == "$ARCH" ]] || {
  echo "release binary must be thin $ARCH; found: $ACTUAL_ARCH" >&2
  exit 1
}

REQUIREMENT=$(crabbox_release_designated_requirement "$IDENTIFIER")
EXPECTED_REQUIREMENT_CANONICAL=$(csreq -r "=$REQUIREMENT" -t)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-notary.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT
NOTARY_ARCHIVE="$WORK_DIR/$(basename "$BINARY").zip"
NOTARY_RESULT="$WORK_DIR/notary-result.json"

sign_args=(
  --force \
  --options runtime \
  --timestamp \
  --identifier "$IDENTIFIER" \
  --requirements "=designated => $REQUIREMENT" \
)
if [[ "$IDENTIFIER" == "$CRABBOX_RELEASE_VMD_IDENTIFIER" ]]; then
  VMD_ENTITLEMENTS="$ROOT/internal/applevmhelper/vmd-entitlements.plist"
  [[ -f "$VMD_ENTITLEMENTS" && ! -L "$VMD_ENTITLEMENTS" ]] || {
    echo "tracked Apple VM daemon entitlements policy is missing" >&2
    exit 1
  }
  sign_args+=(--entitlements "$VMD_ENTITLEMENTS")
fi
sign_args+=(--sign "$CODESIGN_IDENTITY" "$BINARY")
codesign "${sign_args[@]}"

codesign --verify --strict -R="$REQUIREMENT" --verbose=2 "$BINARY"
SIGNATURE=$(codesign -dvvv "$BINARY" 2>&1)
grep -Fx "Identifier=$IDENTIFIER" <<<"$SIGNATURE" >/dev/null
grep -Fx "Authority=$CRABBOX_RELEASE_AUTHORITY" <<<"$SIGNATURE" >/dev/null
grep -Fx "TeamIdentifier=$CRABBOX_RELEASE_TEAM_ID" <<<"$SIGNATURE" >/dev/null
grep -E '^CodeDirectory .*flags=.*\(runtime\)' <<<"$SIGNATURE" >/dev/null
TIMESTAMP=$(sed -n 's/^Timestamp=//p' <<<"$SIGNATURE")
case "$TIMESTAMP" in
  "" | none | None | NONE | *$'\n'*)
    echo "Developer ID signature has no secure timestamp" >&2
    exit 1
    ;;
esac
EMBEDDED_REQUIREMENT=$(codesign -d -r- "$BINARY" 2>&1)
ACTUAL_REQUIREMENT=$(sed -n 's/^designated => //p' <<<"$EMBEDDED_REQUIREMENT")
[[ -n "$ACTUAL_REQUIREMENT" && "$ACTUAL_REQUIREMENT" != *$'\n'* ]] || {
  echo "release binary must contain exactly one designated requirement" >&2
  exit 1
}
ACTUAL_REQUIREMENT_CANONICAL=$(csreq -r "=$ACTUAL_REQUIREMENT" -t)
[[ "$ACTUAL_REQUIREMENT_CANONICAL" == "$EXPECTED_REQUIREMENT_CANONICAL" ]] || {
  echo "embedded designated requirement does not match the Crabbox release policy" >&2
  exit 1
}
if [[ "$IDENTIFIER" == "$CRABBOX_RELEASE_VMD_IDENTIFIER" ]]; then
  ACTUAL_ENTITLEMENTS="$WORK_DIR/actual-entitlements.plist"
  ACTUAL_ENTITLEMENTS_JSON="$WORK_DIR/actual-entitlements.json"
  EXPECTED_ENTITLEMENTS_JSON="$WORK_DIR/expected-entitlements.json"
  codesign -d --entitlements - --xml "$BINARY" \
    >"$ACTUAL_ENTITLEMENTS" 2>"$WORK_DIR/entitlements.stderr"
  plutil -convert json -o "$ACTUAL_ENTITLEMENTS_JSON" "$ACTUAL_ENTITLEMENTS"
  plutil -convert json -o "$EXPECTED_ENTITLEMENTS_JSON" "$VMD_ENTITLEMENTS"
  node - "$EXPECTED_ENTITLEMENTS_JSON" "$ACTUAL_ENTITLEMENTS_JSON" <<'NODE'
const fs = require("node:fs");
const { isDeepStrictEqual } = require("node:util");
const expected = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const actual = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
if (!isDeepStrictEqual(actual, expected)) {
  throw new Error("Apple VM daemon entitlements do not exactly match the tracked release policy");
}
NODE
fi

ditto -c -k --keepParent "$BINARY" "$NOTARY_ARCHIVE"
if ! xcrun notarytool submit "$NOTARY_ARCHIVE" \
  --keychain-profile "$NOTARYTOOL_KEYCHAIN_PROFILE" \
  --keychain "$MAC_RELEASE_CODESIGN_KEYCHAIN" \
  --no-s3-acceleration \
  --wait \
  --output-format json >"$NOTARY_RESULT"; then
  echo "notarization submission failed" >&2
  exit 1
fi

NOTARY_STATUS=$(plutil -extract status raw -o - "$NOTARY_RESULT" 2>/dev/null || true)
NOTARY_ID=$(plutil -extract id raw -o - "$NOTARY_RESULT" 2>/dev/null || true)
[[ "$NOTARY_STATUS" == Accepted && "$NOTARY_ID" =~ ^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$ ]] || {
  echo "notarization failed or returned no submission ID: ${NOTARY_STATUS:-unknown status}" >&2
  exit 1
}
echo "Notarization accepted: $NOTARY_ID"

NOTARIZATION_READY=0
for _ in {1..12}; do
  if codesign --verify --strict --check-notarization -R=notarized "$BINARY" >/dev/null 2>&1; then
    NOTARIZATION_READY=1
    break
  fi
  sleep 5
done
[[ "$NOTARIZATION_READY" == 1 ]] || {
  echo "accepted notarization ticket did not become available through online codesign verification" >&2
  exit 1
}
codesign --verify --strict --check-notarization -R=notarized --verbose=2 "$BINARY"
