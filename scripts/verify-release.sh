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
TOOLING_COMMIT=${CRABBOX_VERIFY_TOOLING_COMMIT:-$VERIFIER_COMMIT}
EXEC_ARCH=${CRABBOX_VERIFY_EXEC_ARCH:-}
VERIFY_MODE=${CRABBOX_VERIFY_MODE:-execute}

if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  [[ ! "$TAG_OBJECT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$TAG_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$VERIFIER_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$TOOLING_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
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

[[ "$(git -C "$ROOT" rev-parse HEAD)" == "$TOOLING_COMMIT" ]] || {
  echo "protected tooling checkout does not match tooling commit" >&2
  exit 1
}
git -C "$ROOT" merge-base --is-ancestor "$VERIFIER_COMMIT" "$TOOLING_COMMIT" || {
  echo "provenance verifier commit is not an ancestor of protected tooling" >&2
  exit 1
}
git -C "$ROOT" merge-base --is-ancestor "$TAG_COMMIT" "$VERIFIER_COMMIT" || {
  echo "release source commit is not an ancestor of the provenance verifier" >&2
  exit 1
}
DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
RELEASE_TAG="$TAG" \
EXPECTED_TAG_OBJECT="$TAG_OBJECT" \
EXPECTED_TAG_COMMIT="$TAG_COMMIT" \
TRUSTED_HEAD="$TOOLING_COMMIT" \
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
cleanup_release_work() {
  local primary_status=$? cleanup_status
  trap - EXIT
  # Toolchain caches have read-only directories. Repair only this private
  # tree's directories, without following symlinks or changing linked files.
  if find -P "$WORK" -type d -exec chmod u+w {} + && rm -rf -- "$WORK"; then
    return "$primary_status"
  else
    cleanup_status=$?
    echo "failed to remove release verification work directory: $WORK" >&2
    if [[ "$primary_status" -ne 0 ]]; then
      return "$primary_status"
    fi
    return "$cleanup_status"
  fi
}
trap cleanup_release_work EXIT
notes="$WORK/release-notes.md"
tagged_changelog="$WORK/tagged-changelog.md"
git -C "$ROOT" show "$TAG_COMMIT:CHANGELOG.md" >"$tagged_changelog"
"$ROOT/scripts/extract-release-notes.sh" "$TAG" \
  <"$tagged_changelog" >"$notes"

release_go_version=$(node "$ROOT/scripts/release-policy.mjs" "$ROOT" "$VERIFIER_COMMIT" --go-version)
runtime_pack=none
runtime_pack_enabled=false
filesystem_identity_args=()
runtime_metadata_args=()
if git -C "$ROOT" cat-file -e "$TAG_COMMIT:cmd/crabbox-runtime/main.go" 2>/dev/null; then
  runtime_pack=final
  runtime_pack_enabled=true
fi
"$ROOT/scripts/build-release-runtime-tool.sh" "$WORK/runtime-tool"
runtime_tool="$WORK/runtime-tool/runtime-artifacts"
mkdir -m 700 "$WORK/reports"
if [[ "$runtime_pack_enabled" == true ]] &&
  git -C "$ROOT" cat-file -e "$TAG_COMMIT:internal/runner/development/main.go.txt" 2>/dev/null; then
  runtime_pack=final-filesystem
  runtime_pack_enabled=filesystem
  # Hash frozen source data with protected tooling; never execute tagged code.
  mkdir -m 700 "$WORK/source"
  git -C "$ROOT" archive "$TAG_COMMIT" internal/runner | tar -xf - -C "$WORK/source"
  filesystem_build_id=$("$runtime_tool" source-id --source-directory "$WORK/source")
  [[ "$filesystem_build_id" =~ ^[0-9a-f]{64}$ ]]
  filesystem_identity_args=(--filesystem-build-id "$filesystem_build_id")
  runtime_metadata_args=(filesystem)
fi

for platform in darwin linux windows; do
  for arch in amd64 arm64; do
    extension=tar.gz
    binary=crabbox
    [[ "$platform" == windows ]] && extension=zip binary=crabbox.exe
    name="crabbox_${version}_${platform}_${arch}.${extension}"
    destination="$WORK/${platform}-${arch}"
    "$runtime_tool" extract --archive "$ASSET_DIR/$name" \
      --directory "$destination" --os "$platform" --arch "$arch" \
      --runtime-pack "$runtime_pack" ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} >"$WORK/reports/$name.json"
    if [[ "$runtime_pack" != none ]]; then
      "$runtime_tool" verify --directory "$destination/crabbox-runtime" \
        --controller "$destination/$binary" ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} >/dev/null
      runtime_platforms=(linux)
      [[ "$runtime_pack" == final-filesystem ]] && runtime_platforms=(darwin linux windows)
      for runtime_platform in "${runtime_platforms[@]}"; do
        for runtime_arch in amd64 arm64; do
          runtime_name="$runtime_platform-$runtime_arch"
          [[ "$runtime_platform" == windows ]] && runtime_name="$runtime_name.exe"
          node "$ROOT/scripts/verify-go-release-binary.mjs" \
            "$destination/crabbox-runtime/$runtime_name" \
            github.com/openclaw/crabbox/cmd/crabbox-runtime \
            "$TAG_COMMIT" "$runtime_platform" "$runtime_arch" "$release_go_version" \
            ${runtime_metadata_args[@]+"${runtime_metadata_args[@]}"}
        done
      done
    fi
    node "$ROOT/scripts/verify-go-release-binary.mjs" \
      "$destination/$binary" github.com/openclaw/crabbox/cmd/crabbox \
      "$TAG_COMMIT" "$platform" "$arch" "$release_go_version"
    if [[ "$platform" == darwin && "$arch" == arm64 ]]; then
      node "$ROOT/scripts/verify-go-release-binary.mjs" \
        "$destination/crabbox-apple-vm-helper" \
        github.com/openclaw/crabbox/cmd/crabbox-apple-vm-helper \
        "$TAG_COMMIT" darwin arm64 "$release_go_version"
    fi
  done
done

provenance_runtime_args=(--runtime-pack "$runtime_pack_enabled")
[[ "$runtime_pack_enabled" != false ]] && provenance_runtime_args+=(--runtime-reports "$WORK/reports")
node "$ROOT/scripts/release-provenance.mjs" verify "${provenance_runtime_args[@]}" \
  ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} \
  --dir "$ASSET_DIR" \
  --tag "$TAG" \
  --tag-object "$TAG_OBJECT" \
  --source-commit "$TAG_COMMIT" \
  --verifier-commit "$VERIFIER_COMMIT" \
  --notes "$notes"

"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_CLI_IDENTIFIER" x86_64 "$WORK/darwin-amd64/crabbox"
"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_CLI_IDENTIFIER" arm64 "$WORK/darwin-arm64/crabbox"
"$ROOT/scripts/verify-macos-binary.sh" \
  "$CRABBOX_RELEASE_HELPER_IDENTIFIER" arm64 "$WORK/darwin-arm64/crabbox-apple-vm-helper"

if [[ "$runtime_pack" == final-filesystem ]]; then
  # Each archive carries the same signed companions, authenticated by provenance.
  # Verify signatures and online notarization without executing either runtime.
  CRABBOX_VERIFY_EXECUTE=0 "$ROOT/scripts/verify-macos-binary.sh" \
    "$CRABBOX_RELEASE_RUNTIME_IDENTIFIER" x86_64 "$WORK/darwin-amd64/crabbox-runtime/darwin-amd64"
  CRABBOX_VERIFY_EXECUTE=0 "$ROOT/scripts/verify-macos-binary.sh" \
    "$CRABBOX_RELEASE_RUNTIME_IDENTIFIER" arm64 "$WORK/darwin-amd64/crabbox-runtime/darwin-arm64"
fi

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
  echo "Statically verified $TAG release assets from $TAG_COMMIT with provenance $VERIFIER_COMMIT and protected tooling $TOOLING_COMMIT"
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

echo "Verified $TAG release assets from $TAG_COMMIT with provenance $VERIFIER_COMMIT and protected tooling $TOOLING_COMMIT"
