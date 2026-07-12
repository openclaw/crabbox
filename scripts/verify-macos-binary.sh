#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=scripts/release-config.sh
source "$ROOT/scripts/release-config.sh"

usage() {
  echo "usage: $0 <identifier> <arm64|x86_64> <binary> [--execute]" >&2
  exit 2
}

[[ $# -eq 3 || ( $# -eq 4 && ${4:-} == --execute ) ]] || usage
IDENTIFIER=$1
ARCH=$(crabbox_release_normalize_macos_arch "$2")
BINARY=$3
EXECUTE=${CRABBOX_VERIFY_EXECUTE:-0}
[[ $# -eq 3 ]] || EXECUTE=1
[[ "$EXECUTE" == 0 || "$EXECUTE" == 1 ]] || {
  echo "CRABBOX_VERIFY_EXECUTE must be 0 or 1" >&2
  exit 2
}

crabbox_release_assert_identifier_arch "$IDENTIFIER" "$ARCH"
crabbox_release_assert_no_publication_tokens
[[ "$(uname -s)" == Darwin ]] || {
  echo "native macOS release verification must run on macOS" >&2
  exit 1
}
[[ -f "$BINARY" && ! -L "$BINARY" && -x "$BINARY" ]] || {
  echo "release binary must be a regular executable file: $BINARY" >&2
  exit 2
}

for tool in arch codesign csreq env lipo node plutil; do
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
  VMD_ENTITLEMENTS="$ROOT/internal/applevmhelper/vmd-entitlements.plist"
  [[ -f "$VMD_ENTITLEMENTS" && ! -L "$VMD_ENTITLEMENTS" ]] || {
    echo "tracked Apple VM daemon entitlements policy is missing" >&2
    exit 1
  }
  ENTITLEMENTS_WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-vmd-entitlements.XXXXXX")
  trap 'rm -rf "$ENTITLEMENTS_WORK"' EXIT
  codesign -d --entitlements - --xml "$BINARY" \
    >"$ENTITLEMENTS_WORK/actual.plist" 2>"$ENTITLEMENTS_WORK/codesign.stderr"
  plutil -convert json -o "$ENTITLEMENTS_WORK/actual.json" "$ENTITLEMENTS_WORK/actual.plist"
  plutil -convert json -o "$ENTITLEMENTS_WORK/expected.json" "$VMD_ENTITLEMENTS"
  node - "$ENTITLEMENTS_WORK/expected.json" "$ENTITLEMENTS_WORK/actual.json" <<'NODE'
const fs = require("node:fs");
const { isDeepStrictEqual } = require("node:util");
const expected = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const actual = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
if (!isDeepStrictEqual(actual, expected)) {
  throw new Error("Apple VM daemon entitlements do not exactly match the tracked release policy");
}
NODE
fi
codesign --verify --strict --check-notarization -R=notarized --verbose=2 "$BINARY"

if [[ "$EXECUTE" == 1 ]]; then
  [[ "$IDENTIFIER" == "$CRABBOX_RELEASE_CLI_IDENTIFIER" ]] || {
    echo "candidate execution is supported only for the Crabbox CLI" >&2
    exit 2
  }
  HOST_ARCH=$(uname -m)
  if [[ "$HOST_ARCH" == "$ARCH" ]]; then
    env -i LC_ALL=C PATH=/usr/bin:/bin:/usr/sbin:/sbin "$BINARY" --version
  elif [[ "$HOST_ARCH" == arm64 && "$ARCH" == x86_64 ]]; then
    env -i LC_ALL=C PATH=/usr/bin:/bin:/usr/sbin:/sbin arch -x86_64 "$BINARY" --version
  else
    echo "cannot execute $ARCH candidate on $HOST_ARCH" >&2
    exit 1
  fi
fi
