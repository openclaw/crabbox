#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=release-config.sh
source "$ROOT/scripts/release-config.sh"

TAG=${1:-}
TAG_OBJECT=${2:-}
TAG_COMMIT=${3:-}
EXPECTED_CANDIDATE_MANIFEST_SHA256=${4:-}
INPUT_DIR=${5:-"$ROOT/dist-release-unsigned"}
OUT_DIR=${6:-"$ROOT/dist-release"}

if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  [[ ! "$EXPECTED_CANDIDATE_MANIFEST_SHA256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "usage: $0 vX.Y.Z <tag-object> <tag-commit> <candidate-manifest-sha256> [unsigned-directory] [output-directory]" >&2
  exit 2
fi
[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || {
  echo "official release packaging requires Apple Silicon macOS" >&2
  exit 1
}
for name in \
  GH_TOKEN GITHUB_TOKEN HOMEBREW_TAP_GITHUB_TOKEN HOMEBREW_GITHUB_API_TOKEN \
  ACTIONS_RUNTIME_TOKEN ACTIONS_ID_TOKEN_REQUEST_TOKEN; do
  if [[ -n "${!name+x}" ]]; then
    echo "$name must be absent during release packaging" >&2
    exit 1
  fi
done
for tool in cmp codesign git go lipo node shasum stat sw_vers tar unzip vtool xcodebuild zip; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

for name in ALLOWED_SIGNERS RELEASE_RECORD; do
  if [[ -n "${!name+x}" ]]; then
    echo "$name must not override protected release policy" >&2
    exit 1
  fi
done
VERIFIER_COMMIT=${VERIFIER_COMMIT:-$(git -C "$ROOT" rev-parse HEAD)}
[[ "$VERIFIER_COMMIT" =~ ^[0-9a-f]{40}$ && "$VERIFIER_COMMIT" == "$(git -C "$ROOT" rev-parse HEAD)" ]] || {
  echo "verifier commit must equal the checked-out protected release tooling" >&2
  exit 1
}
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]] || {
  echo "protected release-tooling checkout must be clean" >&2
  exit 1
}
origin_url=$(git -C "$ROOT" remote get-url origin)
[[ "$origin_url" == https://github.com/openclaw/crabbox ||
  "$origin_url" == https://github.com/openclaw/crabbox.git ||
  "$origin_url" == git@github.com:openclaw/crabbox.git ]] || {
  echo "release packaging requires the canonical openclaw/crabbox origin" >&2
  exit 1
}
remote_main=$(git -C "$ROOT" ls-remote origin "refs/heads/$CRABBOX_RELEASE_DEFAULT_BRANCH" | awk '{print $1}')
[[ "$remote_main" == "$VERIFIER_COMMIT" ]] || {
  echo "protected remote main must exactly equal the verifier commit before signing" >&2
  exit 1
}

DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
RELEASE_TAG="$TAG" \
EXPECTED_TAG_OBJECT="$TAG_OBJECT" \
EXPECTED_TAG_COMMIT="$TAG_COMMIT" \
TRUSTED_HEAD="$VERIFIER_COMMIT" \
ALLOWED_SIGNERS="$ROOT/.github/release-allowed-signers" \
RELEASE_RECORD="$ROOT/release/records/$TAG.json" \
REQUIRE_PUBLISHABLE=1 \
  "$ROOT/scripts/verify-release-source.sh" >/dev/null
[[ ! -e "$OUT_DIR" ]] || {
  echo "refusing to replace existing release directory: $OUT_DIR" >&2
  exit 1
}
out_parent=$(dirname "$OUT_DIR")
[[ -d "$out_parent" ]] || {
  echo "release output parent does not exist: $out_parent" >&2
  exit 1
}
OUT_DIR="$(cd "$out_parent" && pwd -P)/$(basename "$OUT_DIR")"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-release-package.XXXXXX")
PAYLOAD="$OUT_DIR.partial.$$"
[[ ! -e "$PAYLOAD" ]]
remove_tree() {
  local path=$1
  [[ -e "$path" ]] || return 0
  chmod -R u+w "$path" 2>/dev/null || true
  rm -rf "$path"
}
cleanup() {
  remove_tree "$WORK"
  [[ -z "${PAYLOAD:-}" ]] || remove_tree "$PAYLOAD"
}
trap cleanup EXIT
SOURCE="$WORK/source"
BUILD_HOME="$WORK/build-home"
BUILD_TMP="$WORK/build-tmp"
CANDIDATE="$WORK/candidate"
mkdir -m 700 "$PAYLOAD" "$BUILD_HOME" "$BUILD_TMP" "$CANDIDATE" "$CANDIDATE/.components"

version=${TAG#v}
expected_unsigned=$(crabbox_release_archive_names "$version" | LC_ALL=C sort)
actual_unsigned=$(find "$INPUT_DIR" -mindepth 1 -maxdepth 1 -type f -exec basename {} \; | LC_ALL=C sort)
[[ "$actual_unsigned" == "$expected_unsigned" ]] || {
  echo "unsigned candidate inventory mismatch" >&2
  diff -u <(printf '%s\n' "$expected_unsigned") <(printf '%s\n' "$actual_unsigned") >&2 || true
  exit 1
}
component_inventory=$(find "$INPUT_DIR/.components" -mindepth 1 -maxdepth 1 \
  -type f -exec basename {} \; | LC_ALL=C sort)
[[ "$component_inventory" == $'candidate-manifest.json\ncrabbox-apple-vm-vmd' ]] || {
  echo "private release-component inventory mismatch" >&2
  exit 1
}
while IFS= read -r name; do
  cp -p "$INPUT_DIR/$name" "$CANDIDATE/$name"
done <<<"$expected_unsigned"
cp -p "$INPUT_DIR/.components/candidate-manifest.json" \
  "$CANDIDATE/.components/candidate-manifest.json"
cp -p "$INPUT_DIR/.components/crabbox-apple-vm-vmd" \
  "$CANDIDATE/.components/crabbox-apple-vm-vmd"
runtime_pack_enabled=false
if git -C "$ROOT" cat-file -e "$TAG_COMMIT:cmd/crabbox-runtime/main.go" 2>/dev/null; then
  runtime_pack_enabled=true
fi
filesystem_identity_args=()
if git -C "$ROOT" cat-file -e "$TAG_COMMIT:internal/runner/development/main.go.txt" 2>/dev/null; then
  runtime_pack_enabled=filesystem
fi
# Clone frozen source as data and build only the protected artifact reader.
git clone --quiet --no-local --no-checkout "$ROOT" "$SOURCE"
git -C "$SOURCE" checkout --quiet --detach "$TAG_COMMIT"
"$ROOT/scripts/build-release-runtime-tool.sh" "$WORK/runtime-tool"
runtime_tool="$WORK/runtime-tool/runtime-artifacts"
if [[ "$runtime_pack_enabled" == filesystem ]]; then
  filesystem_build_id=$("$runtime_tool" source-id --source-directory "$SOURCE")
  [[ "$filesystem_build_id" =~ ^[0-9a-f]{64}$ ]]
  filesystem_identity_args=(--filesystem-build-id "$filesystem_build_id")
fi
candidate_manifest_sha=$(node "$ROOT/scripts/release-provenance.mjs" candidate-verify \
  --dir "$CANDIDATE" \
  --tag "$TAG" \
  --tag-object "$TAG_OBJECT" \
  --source-commit "$TAG_COMMIT" \
  --verifier-commit "$VERIFIER_COMMIT" \
  --runtime-pack "$runtime_pack_enabled" \
  ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"})
[[ "$candidate_manifest_sha" =~ ^[0-9a-f]{64}$ ]] || {
  echo "candidate manifest verifier returned an invalid digest" >&2
  exit 1
}
[[ "$candidate_manifest_sha" == "$EXPECTED_CANDIDATE_MANIFEST_SHA256" ]] || {
  echo "candidate manifest does not match the explicitly pinned producer digest" >&2
  exit 1
}
unsigned_vmd="$CANDIDATE/.components/crabbox-apple-vm-vmd"
[[ "$(lipo -archs "$unsigned_vmd")" == arm64 ]] || {
  echo "unsigned Apple VM daemon must be thin arm64" >&2
  exit 1
}
unsigned_vmd_build=$(vtool -show-build "$unsigned_vmd")
[[ "$(awk '$1 == "minos" { print $2 }' <<<"$unsigned_vmd_build")" == 13.0 ]] || {
  echo "unsigned Apple VM daemon must target macOS 13.0" >&2
  exit 1
}

go_bin=$(command -v go)
packager_go_version=$(env -i \
  GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" \
  HOME="$BUILD_HOME" \
  PATH="$PATH" \
  TMPDIR="$BUILD_TMP" \
  "$go_bin" env GOVERSION)
[[ "$packager_go_version" == "$CRABBOX_RELEASE_GO_VERSION" ]] || {
  echo "release packager Go version mismatch: $packager_go_version" >&2
  exit 1
}
packager_xcode_version_output=$(xcodebuild -version)
packager_xcode_version=$(awk '$1 == "Xcode" { print $2 }' <<<"$packager_xcode_version_output")
packager_xcode_build=$(awk '$1 == "Build" && $2 == "version" { print $3 }' <<<"$packager_xcode_version_output")
[[ -n "$packager_xcode_version" && -n "$packager_xcode_build" ]] || {
  echo "could not capture the Xcode packager toolchain" >&2
  exit 1
}

# Protected tooling, not candidate scripts, rebuilds the official helper. The
# signed daemon is copied into an ignored embed path so Go's VCS provenance
# remains bound to the exact clean tag commit.
[[ "$(git -C "$SOURCE" rev-parse "refs/tags/$TAG")" == "$TAG_OBJECT" ]]
[[ "$(git -C "$SOURCE" rev-parse HEAD)" == "$TAG_COMMIT" ]]
[[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]]
git -C "$SOURCE" grep -q 'ReleaseVMDTrustPolicyVersion = 1' \
  "$TAG_COMMIT" -- internal/applevmhelper/protocol.go || {
  echo "source tag does not implement Apple VM release trust policy version 1" >&2
  exit 1
}
git -C "$SOURCE" cat-file -e \
  "$TAG_COMMIT:internal/applevmhelper/vmd_release_mode_darwin_arm64.go" || {
  echo "source tag lacks the explicit vmdrelease build mode" >&2
  exit 1
}
git -C "$SOURCE" grep -q 'case "vmd-export":' \
  "$TAG_COMMIT" -- internal/applevmhelper/cli_darwin_arm64.go || {
  echo "source tag lacks the protected embedded-VMD export interface" >&2
  exit 1
}
tag_entitlements="$WORK/tag-vmd-entitlements.plist"
git -C "$ROOT" show \
  "$TAG_COMMIT:internal/applevmhelper/vmd-entitlements.plist" >"$tag_entitlements"
cmp "$tag_entitlements" "$ROOT/internal/applevmhelper/vmd-entitlements.plist" || {
  echo "source tag VMD entitlements differ from the protected release policy" >&2
  exit 1
}
git -C "$SOURCE" check-ignore -q internal/applevmhelper/embedded/crabbox-apple-vm-vmd || {
  echo "signed embedded VMD path must remain ignored for clean VCS provenance" >&2
  exit 1
}

runtime_pack=none
if git -C "$SOURCE" cat-file -e "$TAG_COMMIT:cmd/crabbox-runtime/main.go" 2>/dev/null; then
  runtime_pack=unsigned
fi
if [[ "$runtime_pack_enabled" == filesystem ]]; then
  runtime_pack=unsigned-filesystem
fi
runtime_names=(linux-amd64 linux-arm64)
if [[ "$runtime_pack_enabled" == filesystem ]]; then
  runtime_names=(darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64.exe windows-arm64.exe)
fi

# The protected reader validates bounded members before writing any file.
for platform in darwin linux windows; do
  for arch in amd64 arm64; do
    extension=tar.gz
    [[ "$platform" == windows ]] && extension=zip
    destination="$WORK/${platform}-${arch}"
    "$runtime_tool" extract \
      --archive "$CANDIDATE/crabbox_${version}_${platform}_${arch}.${extension}" \
      --directory "$destination" --os "$platform" --arch "$arch" \
      --runtime-pack "$runtime_pack" \
      ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} >/dev/null
    if [[ "$runtime_pack" != none ]]; then
      runtime_verification_args=()
      [[ "$runtime_pack_enabled" != filesystem ]] || runtime_verification_args=(filesystem)
      for runtime_name in "${runtime_names[@]}"; do
        runtime_platform=${runtime_name%%-*}
        runtime_arch=${runtime_name#*-}
        runtime_arch=${runtime_arch%.exe}
        node "$ROOT/scripts/verify-go-release-binary.mjs" \
          "$destination/crabbox-runtime/$runtime_name" \
          github.com/openclaw/crabbox/cmd/crabbox-runtime \
          "$TAG_COMMIT" "$runtime_platform" "$runtime_arch" "$CRABBOX_RELEASE_GO_VERSION" \
          ${runtime_verification_args[@]+"${runtime_verification_args[@]}"}
        # Every archive must carry the same unsigned companions before signing.
        if [[ "$destination" != "$WORK/darwin-amd64" ]]; then
          cmp "$WORK/darwin-amd64/crabbox-runtime/$runtime_name" "$destination/crabbox-runtime/$runtime_name"
        fi
      done
    fi
  done
done
amd64_stage="$WORK/darwin-amd64"
arm64_stage="$WORK/darwin-arm64"

node "$ROOT/scripts/verify-go-release-binary.mjs" \
  "$amd64_stage/crabbox" github.com/openclaw/crabbox/cmd/crabbox \
  "$TAG_COMMIT" darwin amd64 "$CRABBOX_RELEASE_GO_VERSION"
node "$ROOT/scripts/verify-go-release-binary.mjs" \
  "$arm64_stage/crabbox" github.com/openclaw/crabbox/cmd/crabbox \
  "$TAG_COMMIT" darwin arm64 "$CRABBOX_RELEASE_GO_VERSION"
node "$ROOT/scripts/verify-go-release-binary.mjs" \
  "$arm64_stage/crabbox-apple-vm-helper" \
  github.com/openclaw/crabbox/cmd/crabbox-apple-vm-helper \
  "$TAG_COMMIT" darwin arm64 "$CRABBOX_RELEASE_GO_VERSION"

sign_and_capture_notary_id() {
  local identifier=$1 arch=$2 binary=$3 output id signing_rc=0
  output=$("$ROOT/scripts/codesign-macos.sh" "$identifier" "$arch" "$binary") || signing_rc=$?
  printf '%s\n' "$output" >&2
  [[ "$signing_rc" -eq 0 ]] || return "$signing_rc"
  id=$(sed -n 's/^Notarization accepted: //p' <<<"$output")
  [[ "$id" =~ ^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$ ]] || {
    echo "signing helper did not return an exact notarization submission ID" >&2
    return 1
  }
  printf '%s\n' "$id"
}

official_vmd="$WORK/crabbox-apple-vm-vmd"
cp "$unsigned_vmd" "$official_vmd"
notary_vmd_arm64=$(sign_and_capture_notary_id \
  "$CRABBOX_RELEASE_VMD_IDENTIFIER" arm64 "$official_vmd")
embedded_vmd_sha=$(shasum -a 256 "$official_vmd" | awk '{print $1}')
embedded_vmd_size=$(stat -f '%z' "$official_vmd")
vmd_entitlements_sha=$(shasum -a 256 \
  "$ROOT/internal/applevmhelper/vmd-entitlements.plist" | awk '{print $1}')

mkdir -p "$SOURCE/internal/applevmhelper/embedded"
cp "$official_vmd" \
  "$SOURCE/internal/applevmhelper/embedded/crabbox-apple-vm-vmd"
[[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]] || {
  echo "embedding the signed VMD changed source VCS provenance" >&2
  exit 1
}

# Candidate source is compiled with an empty environment and no release
# credentials. This produces, but never executes, the official helper while
# the signing keychain/notary profile is available.
clean_build_path="${go_bin%/*}:/usr/bin:/bin:/usr/sbin:/sbin"
(
  cd "$SOURCE"
  env -i \
    CGO_ENABLED=0 \
    GOARCH=arm64 \
    GOCACHE="$WORK/gocache" \
    GOMODCACHE="$WORK/gomodcache" \
    GOOS=darwin \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" \
    GOWORK=off \
    HOME="$BUILD_HOME" \
    PATH="$clean_build_path" \
    TMPDIR="$BUILD_TMP" \
    "$go_bin" build \
      -buildvcs=true \
      -trimpath \
      -tags=vmdembed,vmdrelease \
      -ldflags="-s -w" \
      -o "$arm64_stage/crabbox-apple-vm-helper.official" \
      ./cmd/crabbox-apple-vm-helper
)
mv "$arm64_stage/crabbox-apple-vm-helper.official" \
  "$arm64_stage/crabbox-apple-vm-helper"
node "$ROOT/scripts/verify-go-release-binary.mjs" \
  "$arm64_stage/crabbox-apple-vm-helper" \
  github.com/openclaw/crabbox/cmd/crabbox-apple-vm-helper \
  "$TAG_COMMIT" darwin arm64 "$CRABBOX_RELEASE_GO_VERSION"

notary_cli_amd64=$(sign_and_capture_notary_id \
  "$CRABBOX_RELEASE_CLI_IDENTIFIER" x86_64 "$amd64_stage/crabbox")
notary_cli_arm64=$(sign_and_capture_notary_id \
  "$CRABBOX_RELEASE_CLI_IDENTIFIER" arm64 "$arm64_stage/crabbox")
notary_helper_arm64=$(sign_and_capture_notary_id \
  "$CRABBOX_RELEASE_HELPER_IDENTIFIER" arm64 "$arm64_stage/crabbox-apple-vm-helper")

runtime_notary_args=()
if [[ "$runtime_pack_enabled" == filesystem ]]; then
  for arch in amd64 arm64; do
    notary_runtime=$(sign_and_capture_notary_id \
      "$CRABBOX_RELEASE_RUNTIME_IDENTIFIER" "$arch" "$amd64_stage/crabbox-runtime/darwin-$arch")
    runtime_notary_args+=("--notary-runtime-$arch" "$notary_runtime")
    for platform in darwin linux windows; do
      for destination_arch in amd64 arm64; do
        destination="$WORK/$platform-$destination_arch/crabbox-runtime/darwin-$arch"
        [[ "$destination" == "$amd64_stage/crabbox-runtime/darwin-$arch" ]] || \
          cp -p "$amd64_stage/crabbox-runtime/darwin-$arch" "$destination"
      done
    done
  done
fi

# Manifests bind the final signed controller bytes, so create them only here.
for platform in darwin linux windows; do
  for arch in amd64 arm64; do
    destination="$WORK/${platform}-${arch}"
    binary=crabbox
    extension=tar.gz
    [[ "$platform" == windows ]] && binary=crabbox.exe extension=zip
    members=("$binary")
    [[ "$platform" == darwin && "$arch" == arm64 ]] && members+=(crabbox-apple-vm-helper)
    if [[ "$runtime_pack" != none ]]; then
      "$runtime_tool" prepare --directory "$destination/crabbox-runtime" \
        --controller "$destination/$binary" \
        ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} >"$destination/crabbox-runtime/manifest.json.partial"
      mv "$destination/crabbox-runtime/manifest.json.partial" "$destination/crabbox-runtime/manifest.json"
      "$runtime_tool" verify --directory "$destination/crabbox-runtime" \
        --controller "$destination/$binary" \
        ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} >/dev/null
      for runtime_name in "${runtime_names[@]}"; do
        members+=("crabbox-runtime/$runtime_name")
      done
      members+=(crabbox-runtime/manifest.json)
    fi
    archive="$PAYLOAD/crabbox_${version}_${platform}_${arch}.${extension}"
    if [[ "$runtime_pack" == none && "$platform" != darwin ]]; then
      cp -p "$CANDIDATE/$(basename "$archive")" "$archive"
    elif [[ "$platform" == windows ]]; then
      (cd "$destination" && zip -q "$archive" "${members[@]}")
    else
      COPYFILE_DISABLE=1 tar -czf "$archive" -C "$destination" "${members[@]}"
    fi
  done
done

provenance_runtime_args=(--runtime-pack "$runtime_pack_enabled" ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"})
if [[ "$runtime_pack_enabled" != false ]]; then
  final_runtime_pack=final
  [[ "$runtime_pack_enabled" != filesystem ]] || final_runtime_pack=final-filesystem
  mkdir -m 700 "$WORK/reports"
  for platform in darwin linux windows; do
    for arch in amd64 arm64; do
      extension=tar.gz
      [[ "$platform" == windows ]] && extension=zip
      name="crabbox_${version}_${platform}_${arch}.${extension}"
      "$runtime_tool" extract --archive "$PAYLOAD/$name" \
        --directory "$WORK/report-${platform}-${arch}" --os "$platform" --arch "$arch" \
        --runtime-pack "$final_runtime_pack" \
        ${filesystem_identity_args[@]+"${filesystem_identity_args[@]}"} >"$WORK/reports/$name.json"
    done
  done
  provenance_runtime_args+=(--runtime-reports "$WORK/reports")
fi

notes="$WORK/release-notes.md"
tagged_changelog="$WORK/tagged-changelog.md"
git -C "$ROOT" show "$TAG_COMMIT:CHANGELOG.md" >"$tagged_changelog"
"$ROOT/scripts/extract-release-notes.sh" "$TAG" \
  <"$tagged_changelog" >"$notes"

node "$ROOT/scripts/release-provenance.mjs" write "${provenance_runtime_args[@]}" \
  ${runtime_notary_args[@]+"${runtime_notary_args[@]}"} \
  --dir "$PAYLOAD" \
  --tag "$TAG" \
  --tag-object "$TAG_OBJECT" \
  --source-commit "$TAG_COMMIT" \
  --verifier-commit "$VERIFIER_COMMIT" \
  --notes "$notes" \
  --candidate-dir "$CANDIDATE" \
  --candidate-manifest-sha256 "$EXPECTED_CANDIDATE_MANIFEST_SHA256" \
  --embedded-vmd-sha256 "$embedded_vmd_sha" \
  --embedded-vmd-size "$embedded_vmd_size" \
  --vmd-entitlements-sha256 "$vmd_entitlements_sha" \
  --notary-cli-amd64 "$notary_cli_amd64" \
  --notary-cli-arm64 "$notary_cli_arm64" \
  --notary-helper-arm64 "$notary_helper_arm64" \
  --notary-vmd-arm64 "$notary_vmd_arm64" \
  --packager-go-version "$packager_go_version" \
  --packager-os "$(sw_vers -productVersion)" \
  --packager-arch "$(uname -m)" \
  --packager-xcode-version "$packager_xcode_version" \
  --packager-xcode-build "$packager_xcode_build"

checksums="$PAYLOAD/checksums.txt"
: >"$checksums"
while IFS= read -r name; do
  shasum -a 256 "$PAYLOAD/$name" | awk -v name="$name" '{ print $1 "  " name }' >>"$checksums"
done < <({ crabbox_release_archive_names "$version"; printf '%s\n' provenance.json; } | LC_ALL=C sort)

node "$ROOT/scripts/release-provenance.mjs" verify "${provenance_runtime_args[@]}" \
  --dir "$PAYLOAD" \
  --tag "$TAG" \
  --tag-object "$TAG_OBJECT" \
  --source-commit "$TAG_COMMIT" \
  --verifier-commit "$VERIFIER_COMMIT" \
  --notes "$notes"
(cd "$PAYLOAD" && shasum -a 256 -c checksums.txt)

mv "$PAYLOAD" "$OUT_DIR"
PAYLOAD=
echo "Packaged signed and notarized release payload: $OUT_DIR"
echo "Run scripts/verify-release.sh outside the signing wrapper before any draft mutation."
