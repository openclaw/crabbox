#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=release-config.sh
source "$ROOT/scripts/release-config.sh"

TAG=${1:-}
ASSET_DIR=${2:-"$ROOT/dist-release"}
TAG_OBJECT=${3:-}
TAG_COMMIT=${4:-}
VERIFIER_COMMIT=${5:-}
EXEC_ARCH=${CRABBOX_VERIFY_EXEC_ARCH:-}
VERIFY_MODE=${CRABBOX_VERIFY_MODE:-execute}

if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  [[ ! "$TAG_OBJECT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$TAG_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$VERIFIER_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
  echo "usage: $0 vX.Y.Z <asset-directory> <tag-object> <tag-commit> <verifier-commit>" >&2
  exit 2
fi
[[ "$(uname -s)" == Darwin ]] || {
  echo "native release verification must run on macOS" >&2
  exit 1
}
case "$VERIFY_MODE" in
  static | execute) ;;
  *)
    echo "CRABBOX_VERIFY_MODE must be static or execute" >&2
    exit 2
    ;;
esac
host_arch=$(uname -m)
[[ "$EXEC_ARCH" == arm64 || "$EXEC_ARCH" == x86_64 ]] || {
  echo "CRABBOX_VERIFY_EXEC_ARCH must be arm64 or x86_64" >&2
  exit 2
}
[[ "$host_arch" == "$EXEC_ARCH" ]] || {
  echo "native verifier host mismatch: expected $EXEC_ARCH, got $host_arch" >&2
  exit 1
}

assert_no_release_tokens() {
  local name
  for name in \
    GH_TOKEN GITHUB_TOKEN HOMEBREW_TAP_GITHUB_TOKEN HOMEBREW_GITHUB_API_TOKEN \
    ACTIONS_RUNTIME_TOKEN ACTIONS_ID_TOKEN_REQUEST_TOKEN CODESIGN_IDENTITY \
    MAC_RELEASE_CODESIGN_IDENTITY NOTARYTOOL_KEYCHAIN_PROFILE; do
    if [[ -n "${!name+x}" ]]; then
      echo "$name must be absent during release verification and candidate execution" >&2
      return 1
    fi
  done
}
assert_no_release_tokens
for tool in codesign git go lipo node shasum tar unzip; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

[[ "$(git -C "$ROOT" rev-parse HEAD)" == "$VERIFIER_COMMIT" ]] || {
  echo "protected verifier checkout does not match provenance" >&2
  exit 1
}
DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
RELEASE_TAG="$TAG" \
EXPECTED_TAG_OBJECT="$TAG_OBJECT" \
EXPECTED_TAG_COMMIT="$TAG_COMMIT" \
TRUSTED_HEAD="$VERIFIER_COMMIT" \
  "$ROOT/scripts/verify-release-source.sh" >/dev/null
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]] || {
  echo "protected verifier checkout is not clean" >&2
  exit 1
}

version=${TAG#v}
expected_assets=$(crabbox_release_asset_names "$version" | LC_ALL=C sort)
actual_assets=$(find "$ASSET_DIR" -mindepth 1 -maxdepth 1 -type f -exec basename {} \; | LC_ALL=C sort)
[[ "$actual_assets" == "$expected_assets" ]] || {
  echo "release asset inventory mismatch" >&2
  diff -u <(printf '%s\n' "$expected_assets") <(printf '%s\n' "$actual_assets") >&2 || true
  exit 1
}

checksums="$ASSET_DIR/checksums.txt"
expected_checksum_names=$({ crabbox_release_archive_names "$version"; printf '%s\n' provenance.json; } | LC_ALL=C sort)
actual_checksum_names=$(awk 'NF == 2 { print $2 }' "$checksums" | LC_ALL=C sort)
[[ "$actual_checksum_names" == "$expected_checksum_names" ]] || {
  echo "checksum inventory mismatch" >&2
  exit 1
}
awk 'NF != 2 || $1 !~ /^[[:xdigit:]]{64}$/ || $2 ~ /\// { exit 1 }' "$checksums" || {
  echo "checksums must contain one SHA-256 and basename per line" >&2
  exit 1
}
(cd "$ASSET_DIR" && shasum -a 256 -c checksums.txt)

WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-release-verify.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
notes="$WORK/release-notes.md"
tagged_changelog="$WORK/tagged-changelog.md"
git -C "$ROOT" show "$TAG_COMMIT:CHANGELOG.md" >"$tagged_changelog"
"$ROOT/scripts/extract-release-notes.sh" "$TAG" \
  <"$tagged_changelog" >"$notes"
node "$ROOT/scripts/release-provenance.mjs" verify \
  --dir "$ASSET_DIR" \
  --tag "$TAG" \
  --tag-object "$TAG_OBJECT" \
  --source-commit "$TAG_COMMIT" \
  --verifier-commit "$VERIFIER_COMMIT" \
  --notes "$notes"

extract_archive() {
  local name=$1 destination=$2 expected=$3 listing
  mkdir -m 700 "$destination"
  if [[ "$name" == *.zip ]]; then
    listing=$(unzip -Z1 "$ASSET_DIR/$name" | LC_ALL=C sort)
    [[ "$listing" == "$expected" ]] || {
      echo "unexpected archive members: $name" >&2
      exit 1
    }
    unzip -q "$ASSET_DIR/$name" -d "$destination"
  else
    listing=$(tar -tzf "$ASSET_DIR/$name" | LC_ALL=C sort)
    [[ "$listing" == "$expected" ]] || {
      echo "unexpected archive members: $name" >&2
      exit 1
    }
    tar -xzf "$ASSET_DIR/$name" -C "$destination"
  fi
}

for platform in darwin linux windows; do
  for arch in amd64 arm64; do
    extension=tar.gz
    binary=crabbox
    [[ "$platform" == windows ]] && extension=zip binary=crabbox.exe
    name="crabbox_${version}_${platform}_${arch}.${extension}"
    destination="$WORK/${platform}-${arch}"
    expected=$binary
    [[ "$platform" == darwin && "$arch" == arm64 ]] && expected=$'crabbox\ncrabbox-apple-vm-helper'
    extract_archive "$name" "$destination" "$expected"
    node "$ROOT/scripts/verify-go-release-binary.mjs" \
      "$destination/$binary" github.com/openclaw/crabbox/cmd/crabbox \
      "$TAG_COMMIT" "$platform" "$arch" "$CRABBOX_RELEASE_GO_VERSION"
    if [[ "$platform" == darwin && "$arch" == arm64 ]]; then
      node "$ROOT/scripts/verify-go-release-binary.mjs" \
        "$destination/crabbox-apple-vm-helper" \
        github.com/openclaw/crabbox/cmd/crabbox-apple-vm-helper \
        "$TAG_COMMIT" darwin arm64 "$CRABBOX_RELEASE_GO_VERSION"
    fi
  done
done

"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_CLI_IDENTIFIER" x86_64 "$WORK/darwin-amd64/crabbox"
"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_CLI_IDENTIFIER" arm64 "$WORK/darwin-arm64/crabbox"
"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_HELPER_IDENTIFIER" arm64 "$WORK/darwin-arm64/crabbox-apple-vm-helper"

embedded_vmd="$WORK/crabbox-apple-vm-vmd"
node "$ROOT/scripts/extract-release-vmd.mjs" \
  "$WORK/darwin-arm64/crabbox-apple-vm-helper" \
  "$ASSET_DIR/provenance.json" \
  "$embedded_vmd"
"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_VMD_IDENTIFIER" arm64 "$embedded_vmd"
provenance_vmd_sha=$(node -e '
  const p = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
  const helper = p.payloads.flatMap((entry) => entry.binaries).find((entry) => entry.name === "crabbox-apple-vm-helper");
  process.stdout.write(helper?.embeddedVmd?.sha256 ?? "");
' "$ASSET_DIR/provenance.json")

if [[ "$VERIFY_MODE" == static ]]; then
  echo "Statically verified $TAG release assets from $TAG_COMMIT with protected tooling $VERIFIER_COMMIT"
  exit 0
fi

# Candidate-controlled code runs only after every static trust decision. CI
# executes this phase in a dependent clean job after the proof artifact has
# already been frozen, so candidate writes cannot change publication evidence.
assert_no_release_tokens
execution_home="$WORK/home"
mkdir -m 700 "$execution_home"
if [[ "$EXEC_ARCH" == arm64 ]]; then
  vmd_info=$(env -i \
    HOME="$execution_home" PATH=/usr/bin:/bin:/usr/sbin:/sbin TMPDIR="$WORK" \
    "$WORK/darwin-arm64/crabbox-apple-vm-helper" vmd-info)
  node -e '
    const value = JSON.parse(process.argv[1]);
    if (
      value.embedded !== true ||
      value.releaseTrust !== true ||
      value.trustPolicyVersion !== 1 ||
      value.sha256 !== process.argv[2]
    ) process.exit(1);
  ' "$vmd_info" "$provenance_vmd_sha" || {
    echo "embedded Apple VM daemon trust marker or digest mismatch" >&2
    exit 1
  }
fi

if [[ "$EXEC_ARCH" == arm64 ]]; then
  candidate="$WORK/darwin-arm64/crabbox"
else
  candidate="$WORK/darwin-amd64/crabbox"
fi
actual_version=$(env -i \
  HOME="$execution_home" PATH=/usr/bin:/bin:/usr/sbin:/sbin TMPDIR="$WORK" \
  "$candidate" --version)
[[ "$actual_version" == "$version" ]] || {
  echo "native candidate version mismatch: $actual_version" >&2
  exit 1
}

echo "Verified $TAG release assets from $TAG_COMMIT with protected tooling $VERIFIER_COMMIT"
