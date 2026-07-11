#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=release-config.sh
source "$ROOT/scripts/release-config.sh"
TAG=${1:-}
TAG_OBJECT=${2:-}
TAG_COMMIT=${3:-}
OUT_DIR=${4:-"$ROOT/dist-release-unsigned"}

if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 vX.Y.Z <tag-object> <tag-commit> [output-directory]" >&2
  exit 2
fi

for name in \
  GH_TOKEN GITHUB_TOKEN HOMEBREW_TAP_GITHUB_TOKEN HOMEBREW_GITHUB_API_TOKEN \
  ACTIONS_RUNTIME_TOKEN ACTIONS_ID_TOKEN_REQUEST_TOKEN CODESIGN_IDENTITY \
  MAC_RELEASE_CODESIGN_IDENTITY NOTARYTOOL_KEYCHAIN_PROFILE; do
  if [[ -n "${!name+x}" ]]; then
    echo "$name must be absent during credential-free release builds" >&2
    exit 1
  fi
done
[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || {
  echo "credential-free release production requires Apple Silicon macOS" >&2
  exit 1
}
for tool in git go goreleaser lipo node shasum swift sw_vers tar vtool xcodebuild; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done
goreleaser_version_output=$(goreleaser --version)
goreleaser_version=$(awk '$1 == "GitVersion:" { print $2 }' <<<"$goreleaser_version_output")
[[ "$goreleaser_version" == "$CRABBOX_RELEASE_GORELEASER_VERSION" ]] || {
  echo "official builds require GoReleaser $CRABBOX_RELEASE_GORELEASER_VERSION, got ${goreleaser_version:-unknown}" >&2
  exit 1
}

DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
RELEASE_TAG="$TAG" \
EXPECTED_TAG_OBJECT="$TAG_OBJECT" \
EXPECTED_TAG_COMMIT="$TAG_COMMIT" \
TRUSTED_HEAD=HEAD \
  "$ROOT/scripts/verify-release-source.sh" >/dev/null
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]] || {
  echo "protected release-tooling checkout must be clean" >&2
  exit 1
}
VERIFIER_COMMIT=$(git -C "$ROOT" rev-parse HEAD)
[[ ! -e "$OUT_DIR" ]] || {
  echo "refusing to replace existing candidate directory: $OUT_DIR" >&2
  exit 1
}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-release-build.XXXXXX")
stage=
remove_tree() {
  local path=$1
  [[ -e "$path" ]] || return 0
  chmod -R u+w "$path" 2>/dev/null || true
  rm -rf "$path"
}
cleanup() {
  remove_tree "$WORK"
  [[ -z "$stage" ]] || remove_tree "$stage"
}
trap cleanup EXIT
SOURCE="$WORK/source"
BUILD_HOME="$WORK/home"
BUILD_TMP="$WORK/tmp"
mkdir -m 700 "$BUILD_HOME" "$BUILD_TMP"
producer_go_version=$(env -i \
  GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" \
  HOME="$BUILD_HOME" \
  PATH="$PATH" \
  TMPDIR="$BUILD_TMP" \
  go env GOVERSION)
[[ "$producer_go_version" == "$CRABBOX_RELEASE_GO_VERSION" ]] || {
  echo "credential-free producer Go version mismatch: $producer_go_version" >&2
  exit 1
}
producer_swift_version_output=$(swift --version)
producer_swift_version=$(sed -n '1p' <<<"$producer_swift_version_output")
producer_xcode_version_output=$(xcodebuild -version)
producer_xcode_version=$(awk '$1 == "Xcode" { print $2 }' <<<"$producer_xcode_version_output")
producer_xcode_build=$(awk '$1 == "Build" && $2 == "version" { print $3 }' <<<"$producer_xcode_version_output")
producer_os=$(sw_vers -productVersion)
producer_arch=$(uname -m)
[[ -n "$producer_swift_version" && -n "$producer_xcode_version" && -n "$producer_xcode_build" ]] || {
  echo "could not capture the Swift/Xcode producer toolchain" >&2
  exit 1
}
git clone --quiet --no-local --no-checkout "$ROOT" "$SOURCE"
git -C "$SOURCE" checkout --quiet --detach "$TAG_COMMIT"
[[ "$(git -C "$SOURCE" rev-parse "refs/tags/$TAG")" == "$TAG_OBJECT" ]]
[[ "$(git -C "$SOURCE" rev-parse HEAD)" == "$TAG_COMMIT" ]]
[[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]]

(
  cd "$SOURCE"
  env -i \
    GOCACHE="$WORK/gocache" \
    GOMODCACHE="$WORK/gomodcache" \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" \
    GOWORK=off \
    HOME="$BUILD_HOME" \
    PATH="$PATH" \
    TMPDIR="$BUILD_TMP" \
    goreleaser release --clean --skip=publish --parallelism 1 --config "$ROOT/.goreleaser.yaml"
)

version=${TAG#v}
expected=$(printf '%s\n' \
  "crabbox_${version}_darwin_amd64.tar.gz" \
  "crabbox_${version}_darwin_arm64.tar.gz" \
  "crabbox_${version}_linux_amd64.tar.gz" \
  "crabbox_${version}_linux_arm64.tar.gz" \
  "crabbox_${version}_windows_amd64.zip" \
  "crabbox_${version}_windows_arm64.zip" | LC_ALL=C sort)
actual=$(find "$SOURCE/dist" -mindepth 1 -maxdepth 1 -type f \
  \( -name 'crabbox_*.tar.gz' -o -name 'crabbox_*.zip' \) -exec basename {} \; | LC_ALL=C sort)
[[ "$actual" == "$expected" ]] || {
  echo "credential-free archive inventory mismatch" >&2
  diff -u <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2 || true
  exit 1
}

stage="$OUT_DIR.partial.$$"
[[ ! -e "$stage" ]]
mkdir -m 700 "$stage"
while IFS= read -r name; do
  cp -p "$SOURCE/dist/$name" "$stage/$name"
done <<<"$expected"

# The raw Swift daemon is a private producer input, never a release asset. The
# credential-bearing package gate signs and notarizes this exact token-free
# output before embedding it into the official helper.
unsigned_vmd="$SOURCE/vmd/.build/release/crabbox-apple-vm-vmd"
[[ -f "$unsigned_vmd" && ! -L "$unsigned_vmd" && -x "$unsigned_vmd" ]] || {
  echo "credential-free build did not produce the raw Apple VM daemon" >&2
  exit 1
}
[[ "$(lipo -archs "$unsigned_vmd")" == arm64 ]] || {
  echo "credential-free Apple VM daemon must be thin arm64" >&2
  exit 1
}
unsigned_vmd_build=$(vtool -show-build "$unsigned_vmd")
[[ "$(awk '$1 == "minos" { print $2 }' <<<"$unsigned_vmd_build")" == 13.0 ]] || {
  echo "credential-free Apple VM daemon must target macOS 13.0" >&2
  exit 1
}
mkdir -m 700 "$stage/.components"
cp -p "$unsigned_vmd" "$stage/.components/crabbox-apple-vm-vmd"

manifest_sha=$(node "$ROOT/scripts/release-provenance.mjs" candidate-write \
  --dir "$stage" \
  --tag "$TAG" \
  --tag-object "$TAG_OBJECT" \
  --source-commit "$TAG_COMMIT" \
  --verifier-commit "$VERIFIER_COMMIT" \
  --producer-os "$producer_os" \
  --producer-arch "$producer_arch" \
  --go-version "$producer_go_version" \
  --goreleaser-version "$goreleaser_version" \
  --swift-version "$producer_swift_version" \
  --xcode-version "$producer_xcode_version" \
  --xcode-build "$producer_xcode_build")
[[ "$manifest_sha" =~ ^[0-9a-f]{64}$ ]] || {
  echo "credential-free producer returned an invalid candidate manifest digest" >&2
  exit 1
}

mv "$stage" "$OUT_DIR"
stage=
echo "Credential-free release candidate: $OUT_DIR"
echo "Candidate manifest SHA-256: $manifest_sha"
