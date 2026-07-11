#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=scripts/release-config.sh
source "$ROOT/scripts/release-config.sh"

FORMULA=openclaw/tap/crabbox
SCRIPT_PATH="$ROOT/scripts/verify-homebrew-release.sh"
PROTECTED_HOMEBREW_TOOLING=(
  .github/release-allowed-signers
  .goreleaser.yaml
  scripts/extract-release-notes.sh
  scripts/extract-release-vmd.mjs
  scripts/release-config.sh
  scripts/release-provenance.mjs
  scripts/render-homebrew-formula.mjs
  scripts/validate-release-publication.mjs
  scripts/verify-go-release-binary.mjs
  scripts/verify-homebrew-release.sh
  scripts/verify-macos-binary.sh
  scripts/verify-release.sh
  scripts/verify-release-source.sh
)

cleanup_homebrew_work() {
  local path=${CRABBOX_HOMEBREW_VERIFY_WORK:-}
  [[ -z "$path" ]] || rm -rf -- "$path"
}

usage() {
  echo "usage: $0 vX.Y.Z <asset-directory> <tag-object> <source-commit> <verifier-commit> <release-id> <public-verifier-run-id> <public-proof-zip-directory>" >&2
  exit 2
}

assert_no_downstream_credentials() {
  crabbox_release_assert_no_publication_tokens
  local name
  for name in \
    GH_ENTERPRISE_TOKEN \
    GITHUB_ENTERPRISE_TOKEN \
    HOMEBREW_GITHUB_PACKAGES_TOKEN \
    HOMEBREW_TAP_TOKEN \
    CODESIGN_IDENTITY \
    CODESIGN_KEYCHAIN \
    MACOS_CODESIGN_IDENTITY \
    MACOS_SIGNING_CERT_BASE64 \
    MACOS_SIGNING_CERT_PASSWORD \
    MAC_RELEASE_OP_ACCOUNT \
    MAC_RELEASE_OP_FIELDS \
    MAC_RELEASE_OP_ITEM \
    MAC_RELEASE_OP_VAULT \
    MAC_RELEASE_CODESIGN_IDENTITY \
    MAC_RELEASE_CODESIGN_KEYCHAIN \
    MAC_RELEASE_CODESIGN_KEYCHAIN_PASSWORD \
    MAC_RELEASE_CODESIGN_OP_ACCOUNT \
    MAC_RELEASE_CODESIGN_OP_ITEM \
    MAC_RELEASE_CODESIGN_OP_VAULT \
    NOTARYTOOL_KEYCHAIN_PROFILE \
    NOTARYTOOL_APPLE_ID \
    NOTARYTOOL_PASSWORD \
    NOTARYTOOL_TEAM_ID \
    APPLE_ID \
    APPLE_API_ISSUER \
    APPLE_API_KEY \
    APPLE_APP_SPECIFIC_PASSWORD \
    APPLE_TEAM_ID \
    AC_USERNAME \
    AC_PASSWORD \
    AC_PROVIDER \
    ASC_KEY_ID \
    ASC_ISSUER_ID \
    ASC_PRIVATE_KEY \
    OP_SERVICE_ACCOUNT_TOKEN; do
    if [[ -n "${!name+x}" ]]; then
      echo "$name must be unset before downstream Homebrew verification" >&2
      return 1
    fi
  done
}

assert_clean_homebrew_environment() {
  [[ "${CRABBOX_HOMEBREW_CLEAN_CHILD:-}" == 1 ]] || {
    echo "Homebrew verification must run through the credential-free launcher" >&2
    return 1
  }
  local name
  while IFS= read -r name; do
    case "$name" in
      CRABBOX_HOMEBREW_CLEAN_CHILD | \
        HOME | HOMEBREW_CACHE | HOMEBREW_NO_ANALYTICS | HOMEBREW_NO_AUTO_UPDATE | \
        HOMEBREW_NO_ENV_HINTS | HOMEBREW_NO_INSTALL_CLEANUP | \
        LC_ALL | LOGNAME | NONINTERACTIVE | PATH | PWD | SHLVL | TMPDIR | USER) ;;
      *)
        echo "Homebrew verification received an unexpected environment variable: $name" >&2
        return 1
        ;;
    esac
  done < <(compgen -e)
}

validate_release_identity() {
  local tag=${1:-} tag_object=${2:-} source_commit=${3:-} verifier_commit=${4:-}
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
    [[ "$tag_object" =~ ^[0-9a-f]{40}$ ]] &&
    [[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] &&
    [[ "$verifier_commit" =~ ^[0-9a-f]{40}$ ]] || usage
}

require_publishable_source() {
  local tag=$1 tag_object=$2 source_commit=$3 verifier_commit=$4
  [[ "$(git -C "$ROOT" rev-parse HEAD)" == "$verifier_commit" ]] || {
    echo "protected verifier checkout does not match the supplied verifier commit" >&2
    return 1
  }
  (
    cd "$ROOT"
    DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
      RELEASE_TAG="$tag" \
      EXPECTED_TAG_OBJECT="$tag_object" \
      EXPECTED_TAG_COMMIT="$source_commit" \
      TRUSTED_HEAD="$verifier_commit" \
      REQUIRE_PUBLISHABLE=1 \
      "$ROOT/scripts/verify-release-source.sh" >/dev/null
  )
}

require_protected_homebrew_tooling() {
  local verifier_commit=$1 tag=$2 tooling_status
  tooling_status=$(git -C "$ROOT" status --porcelain=v1 --untracked-files=all -- \
    "${PROTECTED_HOMEBREW_TOOLING[@]}" "release/records/$tag.json")
  [[ -z "$tooling_status" ]] || {
    echo "protected downstream verifier tooling is dirty" >&2
    printf '%s\n' "$tooling_status" >&2
    return 1
  }
  git -C "$ROOT" diff --quiet "$verifier_commit" -- \
    "${PROTECTED_HOMEBREW_TOOLING[@]}" "release/records/$tag.json" || {
    echo "protected downstream verifier tooling differs from $verifier_commit" >&2
    return 1
  }
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

freeze_public_release() {
  [[ $# -eq 10 ]] || usage
  local tag=$1 asset_dir=$2 tag_object=$3 source_commit=$4 verifier_commit=$5
  local release_id=$6 run_id=$7 proof_dir=$8 work=$9 node_bin=${10}
  local repository=$CRABBOX_RELEASE_REPOSITORY version=${tag#v} workflow_id arch
  [[ "$release_id" =~ ^[1-9][0-9]*$ && "$run_id" =~ ^[1-9][0-9]*$ ]] || usage
  [[ -d "$proof_dir" && ! -L "$proof_dir" ]] || {
    echo "public proof ZIP directory must be a real directory" >&2
    return 1
  }
  local proof_names
  proof_names=$(find "$proof_dir" -mindepth 1 -maxdepth 1 -type f -exec basename {} \; | LC_ALL=C sort)
  [[ "$proof_names" == $'verified-assets-arm64.zip\nverified-assets-x86_64.zip' ]] || {
    echo "public proof ZIP inventory is not exact" >&2
    return 1
  }

  local expected_names="$work/expected-assets.txt" notes="$work/expected-notes.md"
  crabbox_release_asset_names "$version" | LC_ALL=C sort >"$expected_names"
  git -C "$ROOT" show "$source_commit:CHANGELOG.md" >"$work/tagged-changelog.md"
  "$ROOT/scripts/extract-release-notes.sh" "$tag" <"$work/tagged-changelog.md" >"$notes"

  local frozen="$work/public-assets"
  mkdir -m 700 "$frozen"
  while IFS= read -r name; do
    [[ -f "$asset_dir/$name" && ! -L "$asset_dir/$name" ]] || {
      echo "public asset is missing, non-regular, or a symlink: $name" >&2
      return 1
    }
    cp "$asset_dir/$name" "$frozen/$name"
  done <"$expected_names"

  public_api_get() {
    curl --fail --silent --show-error --location --retry 3 \
      --header 'Accept: application/vnd.github+json' \
      --header 'X-GitHub-Api-Version: 2026-03-10' \
      "https://api.github.com/$1"
  }
  public_api_get "repos/$repository/releases/$release_id" >"$work/public-release.json"
  public_api_get "repos/$repository/actions/runs/$run_id" >"$work/public-run.json"
  workflow_id=$(jq -er '.workflow_id | select(type == "number" and . > 0)' "$work/public-run.json")
  public_api_get "repos/$repository/actions/workflows/$workflow_id" >"$work/public-workflow.json"
  public_api_get "repos/$repository/actions/runs/$run_id/artifacts?per_page=100" \
    >"$work/public-artifacts.json"

  for arch in arm64 x86_64; do
    local zip="$proof_dir/verified-assets-$arch.zip" artifact_json="$work/artifact-$arch.json"
    jq -e --arg name "verified-assets-$arch" '
      [.artifacts[] | select(.name == $name)] |
      if length == 1 then .[0] else error("ambiguous public proof artifact") end
    ' "$work/public-artifacts.json" >"$artifact_json"
    local expected_size expected_digest
    expected_size=$(jq -er '.size_in_bytes | select(type == "number" and . > 0)' "$artifact_json")
    expected_digest=$(jq -er '.digest | select(test("^sha256:[0-9a-f]{64}$"))' "$artifact_json")
    [[ "$(stat -f '%z' "$zip")" == "$expected_size" && "sha256:$(sha256_file "$zip")" == "$expected_digest" ]] || {
      echo "$arch public proof ZIP does not match its GitHub artifact digest" >&2
      return 1
    }
    [[ "$(unzip -Z1 "$zip")" == verified-assets.json ]] || {
      echo "$arch public proof ZIP must contain only verified-assets.json" >&2
      return 1
    }
    mkdir -m 700 "$work/proof-$arch"
    unzip -qq "$zip" -d "$work/proof-$arch"
  done

  env -i \
    CRABBOX_PUBLISH_REPOSITORY="$repository" \
    CRABBOX_PUBLISH_RELEASE_ID="$release_id" \
    CRABBOX_PUBLISH_TAG="$tag" \
    CRABBOX_PUBLISH_TAG_OBJECT="$tag_object" \
    CRABBOX_PUBLISH_SOURCE_COMMIT="$source_commit" \
    CRABBOX_PUBLISH_VERIFIER_COMMIT="$verifier_commit" \
    CRABBOX_PUBLISH_VERIFIER_RUN_ID="$run_id" \
    CRABBOX_PUBLISH_DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
    CRABBOX_PUBLISH_WORKFLOW_PATH=.github/workflows/release-assets.yml \
    HOME="${HOME:-/tmp}" LANG=C LC_ALL=C PATH="$PATH" TMPDIR="$work" \
    "$node_bin" "$ROOT/scripts/validate-release-publication.mjs" public-proof \
      "$work/public-release.json" "$work/public-run.json" "$work/public-workflow.json" \
      "$work/public-artifacts.json" \
      "$work/proof-arm64/verified-assets.json" \
      "$work/proof-x86_64/verified-assets.json" \
      "$expected_names" "$notes" "$frozen" >"$work/public-proof.json"
}

verify_homebrew_formula() {
  local node_bin=$1 formula_file=$2 tag=$3
  local darwin_amd64_sha=$4 darwin_arm64_sha=$5 linux_amd64_sha=$6 linux_arm64_sha=$7
  local canonical
  canonical=$(mktemp "${TMPDIR:-/tmp}/crabbox-formula.XXXXXX")
  if ! "$node_bin" "$ROOT/scripts/render-homebrew-formula.mjs" \
    "$tag" "$darwin_amd64_sha" "$darwin_arm64_sha" \
    "$linux_amd64_sha" "$linux_arm64_sha" >"$canonical"; then
    rm -f "$canonical"
    return 1
  fi
  if ! cmp -s "$canonical" "$formula_file"; then
    rm -f "$canonical"
    echo "Homebrew formula is not the exact protected canonical program" >&2
    return 1
  fi
  rm -f "$canonical"
  "$node_bin" - \
    "$formula_file" "$tag" \
    "$darwin_amd64_sha" "$darwin_arm64_sha" \
    "$linux_amd64_sha" "$linux_arm64_sha" <<'NODE'
const fs = require("node:fs");

const [formulaFile, tag, darwinAmd64, darwinArm64, linuxAmd64, linuxArm64] =
  process.argv.slice(2);
const version = tag.slice(1);
const digestPattern = /^[0-9a-f]{64}$/;
for (const digest of [darwinAmd64, darwinArm64, linuxAmd64, linuxArm64]) {
  if (!digestPattern.test(digest)) throw new Error("expected archive SHA-256 is invalid");
}

const expected = new Map([
  [
    "darwin/amd64",
    {
      url: `https://github.com/openclaw/crabbox/releases/download/${tag}/crabbox_${version}_darwin_amd64.tar.gz`,
      template:
        "https://github.com/openclaw/crabbox/releases/download/v#{version}/crabbox_#{version}_darwin_amd64.tar.gz",
      sha256: darwinAmd64,
    },
  ],
  [
    "darwin/arm64",
    {
      url: `https://github.com/openclaw/crabbox/releases/download/${tag}/crabbox_${version}_darwin_arm64.tar.gz`,
      template:
        "https://github.com/openclaw/crabbox/releases/download/v#{version}/crabbox_#{version}_darwin_arm64.tar.gz",
      sha256: darwinArm64,
    },
  ],
  [
    "linux/amd64",
    {
      url: `https://github.com/openclaw/crabbox/releases/download/${tag}/crabbox_${version}_linux_amd64.tar.gz`,
      template:
        "https://github.com/openclaw/crabbox/releases/download/v#{version}/crabbox_#{version}_linux_amd64.tar.gz",
      sha256: linuxAmd64,
    },
  ],
  [
    "linux/arm64",
    {
      url: `https://github.com/openclaw/crabbox/releases/download/${tag}/crabbox_${version}_linux_arm64.tar.gz`,
      template:
        "https://github.com/openclaw/crabbox/releases/download/v#{version}/crabbox_#{version}_linux_arm64.tar.gz",
      sha256: linuxArm64,
    },
  ],
]);

const lines = fs.readFileSync(formulaFile, "utf8").split(/\r?\n/);
const versions = lines
  .map((line) => /^\s*version "([^"]+)"\s*$/.exec(line)?.[1])
  .filter(Boolean);
if (versions.length !== 1 || versions[0] !== version) {
  throw new Error("Homebrew formula version does not match the exact release");
}
if (lines.filter((line) => line.trim() === "class Crabbox < Formula").length !== 1) {
  throw new Error("Homebrew formula class is not exactly Crabbox");
}

let osContext;
let osIndent = -1;
let archContext;
let archIndent = -1;
let pending;
const actual = new Map();
for (let index = 0; index < lines.length; index += 1) {
  const raw = lines[index];
  const trimmed = raw.trim();
  if (!trimmed || trimmed.startsWith("#")) continue;
  if (raw.includes("\t")) throw new Error("Homebrew formula must use spaces for indentation");
  const indent = raw.length - raw.trimStart().length;
  if (archContext && indent <= archIndent) {
    archContext = undefined;
    archIndent = -1;
  }
  if (osContext && indent <= osIndent) {
    osContext = undefined;
    osIndent = -1;
  }

  if (trimmed === "on_macos do" || trimmed === "on_linux do") {
    if (pending) throw new Error("Homebrew URL is missing its adjacent SHA-256");
    osContext = trimmed === "on_macos do" ? "darwin" : "linux";
    osIndent = indent;
    archContext = undefined;
    continue;
  }
  if (/^if Hardware::CPU\.intel\?(?: && Hardware::CPU\.is_64_bit\?)?$/.test(trimmed)) {
    if (!osContext) throw new Error("Homebrew Intel selector is outside an OS block");
    archContext = "amd64";
    archIndent = indent;
    continue;
  }
  if (/^if Hardware::CPU\.arm\?(?: && Hardware::CPU\.is_64_bit\?)?$/.test(trimmed)) {
    if (!osContext) throw new Error("Homebrew arm selector is outside an OS block");
    archContext = "arm64";
    archIndent = indent;
    continue;
  }

  if (/^url(?:\s|\()/.test(trimmed)) {
    const match = /^url "([^"]+)"$/.exec(trimmed);
    if (!match || !osContext || !archContext || pending) {
      throw new Error(`non-literal, misplaced, or duplicate Homebrew URL at line ${index + 1}`);
    }
    pending = { key: `${osContext}/${archContext}`, url: match[1] };
    continue;
  }
  if (/^sha256(?:\s|\()/.test(trimmed)) {
    const match = /^sha256 "([0-9a-f]{64})"$/.exec(trimmed);
    if (!match || !pending || pending.key !== `${osContext}/${archContext}`) {
      throw new Error(`non-literal or misplaced Homebrew SHA-256 at line ${index + 1}`);
    }
    if (actual.has(pending.key)) throw new Error(`duplicate Homebrew target: ${pending.key}`);
    actual.set(pending.key, { url: pending.url, sha256: match[1] });
    pending = undefined;
  }
}
if (pending) throw new Error("Homebrew URL is missing its adjacent SHA-256");
if (actual.size !== expected.size) throw new Error("Homebrew formula target inventory is not exact");
for (const [key, expectedEntry] of expected) {
  const actualEntry = actual.get(key);
  if (
    !actualEntry ||
    (actualEntry.url !== expectedEntry.url && actualEntry.url !== expectedEntry.template) ||
    actualEntry.sha256 !== expectedEntry.sha256
  ) {
    throw new Error(`Homebrew formula does not match the frozen ${key} archive`);
  }
  const resolvedUrl = actualEntry.url.replaceAll("#{version}", version);
  if (resolvedUrl !== expectedEntry.url) {
    throw new Error(`Homebrew formula does not resolve to the frozen ${key} archive`);
  }
}

const helperInstalls = lines
  .map((line) => line.trim())
  .filter((line) => line.includes('bin.install "crabbox-apple-vm-helper"'));
if (
  helperInstalls.length === 0 ||
  helperInstalls.some(
    (line) =>
      line !==
      'bin.install "crabbox-apple-vm-helper" if OS.mac? && Hardware::CPU.arm?',
  )
) {
  throw new Error("Homebrew helper install must be restricted to macOS arm64");
}
if (!lines.some((line) => line.trim() === 'bin.install "crabbox"')) {
  throw new Error("Homebrew formula does not install the Crabbox CLI");
}
NODE
}

homebrew_phase() {
  [[ $# -eq 9 ]] || usage
  local tag=$1 asset_dir=$2 tag_object=$3 source_commit=$4 verifier_commit=$5 archive
  local native_arch=$6 brew_bin=$7 node_bin=$8 work=$9 version archive_arch
  assert_clean_homebrew_environment
  validate_release_identity "$tag" "$tag_object" "$source_commit" "$verifier_commit"
  assert_no_downstream_credentials
  [[ "$(uname -s)" == Darwin ]] || {
    echo "downstream Homebrew verification must run natively on macOS" >&2
    return 1
  }
  [[ "$(uname -m)" == "$native_arch" ]] || {
    echo "Homebrew verifier architecture changed before execution" >&2
    return 1
  }
  [[ "$native_arch" == arm64 || "$native_arch" == x86_64 ]] || {
    echo "unsupported native Homebrew verifier architecture: $native_arch" >&2
    return 1
  }
  [[ -x "$brew_bin" && -x "$node_bin" ]] || {
    echo "Homebrew and Node executables must be absolute executable paths" >&2
    return 1
  }
  [[ "$brew_bin" == /* && "$node_bin" == /* ]] || {
    echo "Homebrew and Node executables must be absolute executable paths" >&2
    return 1
  }
  [[ -d "$asset_dir" && -d "$work" ]] || {
    echo "release assets or Homebrew verification work directory is missing" >&2
    return 1
  }
  # shellcheck disable=SC2153 # Required clean-child environment supplied by main.
  [[ "$HOME" == "$work/home" && "$HOMEBREW_CACHE" == "$work/cache" ]] || {
    echo "Homebrew verification must use work-local HOME and cache directories" >&2
    return 1
  }
  [[ -d "$HOME" && ! -L "$HOME" && -d "$HOMEBREW_CACHE" && ! -L "$HOMEBREW_CACHE" ]] || {
    echo "Homebrew verification HOME or cache is missing or is a symlink" >&2
    return 1
  }

  # Repeat the protected-record decision inside the credential-free child so
  # direct/private-phase invocation cannot bypass a newly blocked release.
  require_publishable_source "$tag" "$tag_object" "$source_commit" "$verifier_commit"
  (
    cd "$ROOT"
    CRABBOX_VERIFY_EXEC_ARCH="$native_arch" \
      "$ROOT/scripts/verify-release.sh" \
      "$tag" "$asset_dir" "$tag_object" "$source_commit" "$verifier_commit"
  )

  version=${tag#v}
  local darwin_amd64_archive="$asset_dir/crabbox_${version}_darwin_amd64.tar.gz"
  local darwin_arm64_archive="$asset_dir/crabbox_${version}_darwin_arm64.tar.gz"
  local linux_amd64_archive="$asset_dir/crabbox_${version}_linux_amd64.tar.gz"
  local linux_arm64_archive="$asset_dir/crabbox_${version}_linux_arm64.tar.gz"
  for archive in \
    "$darwin_amd64_archive" "$darwin_arm64_archive" \
    "$linux_amd64_archive" "$linux_arm64_archive"; do
    [[ -f "$archive" && ! -L "$archive" ]] || {
      echo "frozen release archive is missing or is a symlink: $archive" >&2
      return 1
    }
  done

  local darwin_amd64_sha darwin_arm64_sha linux_amd64_sha linux_arm64_sha
  darwin_amd64_sha=$(sha256_file "$darwin_amd64_archive")
  darwin_arm64_sha=$(sha256_file "$darwin_arm64_archive")
  linux_amd64_sha=$(sha256_file "$linux_amd64_archive")
  linux_arm64_sha=$(sha256_file "$linux_arm64_archive")

  local formula_file="$work/crabbox.rb"
  "$brew_bin" update --force
  "$brew_bin" cat "$FORMULA" >"$formula_file"
  verify_homebrew_formula \
    "$node_bin" "$formula_file" "$tag" \
    "$darwin_amd64_sha" "$darwin_arm64_sha" \
    "$linux_amd64_sha" "$linux_arm64_sha"
  # Force a fresh public download into the per-run empty cache. A previously
  # cached archive can never stand in for a missing or inaccessible release URL.
  "$brew_bin" fetch --force --formula "$FORMULA"

  if "$brew_bin" list --formula "$FORMULA" >/dev/null 2>&1; then
    "$brew_bin" reinstall "$FORMULA"
  else
    "$brew_bin" install "$FORMULA"
  fi

  local prefix
  prefix=$("$brew_bin" --prefix "$FORMULA")
  [[ -n "$prefix" && "$prefix" != *$'\n'* && -d "$prefix" ]] || {
    echo "Homebrew returned an invalid Crabbox prefix" >&2
    return 1
  }

  local installed_cli="$prefix/bin/crabbox"
  [[ -f "$installed_cli" && ! -L "$installed_cli" && -x "$installed_cli" ]] || {
    echo "Homebrew Crabbox CLI is not a regular executable" >&2
    return 1
  }
  archive_arch=amd64
  [[ "$native_arch" == arm64 ]] && archive_arch=arm64
  local native_archive="$asset_dir/crabbox_${version}_darwin_${archive_arch}.tar.gz"
  local extracted="$work/extracted"
  mkdir -m 700 "$extracted"
  local expected_members=crabbox
  [[ "$native_arch" == arm64 ]] && expected_members=$'crabbox\ncrabbox-apple-vm-helper'
  [[ "$(tar -tzf "$native_archive" | LC_ALL=C sort)" == "$expected_members" ]] || {
    echo "native release archive member inventory changed" >&2
    return 1
  }
  tar -xzf "$native_archive" -C "$extracted"
  cmp -s "$extracted/crabbox" "$installed_cli" || {
    echo "Homebrew-installed Crabbox CLI differs from the frozen release archive" >&2
    return 1
  }
  [[ "$(lipo -archs "$installed_cli")" == "$native_arch" ]] || {
    echo "Homebrew-installed Crabbox CLI architecture is not $native_arch" >&2
    return 1
  }
  "$ROOT/scripts/verify-macos-binary.sh" \
    "$CRABBOX_RELEASE_CLI_IDENTIFIER" "$native_arch" "$installed_cli"

  local installed_helper="$prefix/bin/crabbox-apple-vm-helper"
  if [[ "$native_arch" == arm64 ]]; then
    [[ -f "$installed_helper" && ! -L "$installed_helper" && -x "$installed_helper" ]] || {
      echo "Homebrew arm64 install is missing the Apple VM helper" >&2
      return 1
    }
    cmp -s "$extracted/crabbox-apple-vm-helper" "$installed_helper" || {
      echo "Homebrew-installed Apple VM helper differs from the frozen release archive" >&2
      return 1
    }
    [[ "$(lipo -archs "$installed_helper")" == arm64 ]] || {
      echo "Homebrew-installed Apple VM helper is not arm64" >&2
      return 1
    }
    "$ROOT/scripts/verify-macos-binary.sh" \
      "$CRABBOX_RELEASE_HELPER_IDENTIFIER" arm64 "$installed_helper"
  elif [[ -e "$installed_helper" || -L "$installed_helper" ]]; then
    echo "Homebrew installed the arm-only Apple VM helper on Intel" >&2
    return 1
  fi

  assert_no_downstream_credentials
  "$brew_bin" test "$FORMULA"

  # Candidate execution is the final proof after formula, bytes, architecture,
  # Developer ID, online notarization, and Homebrew's own test all pass.
  local candidate_home="$work/candidate-home" actual_version
  mkdir -m 700 "$candidate_home"
  assert_no_downstream_credentials
  if [[ "$native_arch" == arm64 ]]; then
    local vmd_info provenance_vmd_sha
    vmd_info=$(HOME="$candidate_home" "$installed_helper" vmd-info)
    provenance_vmd_sha=$("$node_bin" -e '
      const p = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
      const helper = p.payloads.flatMap((entry) => entry.binaries)
        .find((entry) => entry.name === "crabbox-apple-vm-helper");
      process.stdout.write(helper?.embeddedVmd?.sha256 ?? "");
    ' "$asset_dir/provenance.json")
    "$node_bin" -e '
      const value = JSON.parse(process.argv[1]);
      const expectedSha = process.argv[2];
      if (
        !/^[0-9a-f]{64}$/.test(expectedSha) ||
        value.embedded !== true ||
        value.releaseTrust !== true ||
        value.trustPolicyVersion !== 1 ||
        value.sha256 !== expectedSha
      ) process.exit(1);
    ' "$vmd_info" "$provenance_vmd_sha" || {
      echo "Homebrew-installed helper has the wrong embedded VMD trust policy" >&2
      return 1
    }
  fi
  actual_version=$(HOME="$candidate_home" "$installed_cli" --version)
  [[ "$actual_version" == "$version" ]] || {
    echo "Homebrew-installed Crabbox version mismatch: $actual_version" >&2
    return 1
  }
  printf 'Verified Homebrew %s on %s\n' "$tag" "$native_arch"
}

main() {
  [[ $# -eq 8 ]] || usage
  local tag=$1 asset_dir=$2 tag_object=$3 source_commit=$4 verifier_commit=$5
  local release_id=$6 public_run_id=$7 proof_dir=$8
  local native_arch brew_bin node_bin work clean_path user_name homebrew_home homebrew_cache source_asset_dir
  validate_release_identity "$tag" "$tag_object" "$source_commit" "$verifier_commit"
  assert_no_downstream_credentials
  [[ "$(uname -s)" == Darwin ]] || {
    echo "downstream Homebrew verification must run natively on macOS" >&2
    exit 1
  }
  native_arch=$(uname -m)
  [[ "$native_arch" == arm64 || "$native_arch" == x86_64 ]] || {
    echo "unsupported native Homebrew verifier architecture: $native_arch" >&2
    exit 1
  }
  [[ -d "$asset_dir" && ! -L "$asset_dir" ]] || {
    echo "release asset directory must be a real directory" >&2
    exit 1
  }
  asset_dir=$(cd "$asset_dir" && pwd -P)
  source_asset_dir=$asset_dir
  [[ -d "$proof_dir" && ! -L "$proof_dir" ]] || usage
  proof_dir=$(cd "$proof_dir" && pwd -P)

  # This blocked/ready decision precedes the credential-free child, which
  # repeats it before upstream candidate execution or any brew command.
  require_protected_homebrew_tooling "$verifier_commit" "$tag"
  require_publishable_source "$tag" "$tag_object" "$source_commit" "$verifier_commit"

  brew_bin=$(command -v brew)
  node_bin=$(command -v node)
  [[ "$brew_bin" == /* && "$node_bin" == /* && -x "$brew_bin" && -x "$node_bin" ]] || {
    echo "absolute Homebrew and Node executables are required" >&2
    exit 1
  }
  work=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-homebrew-verify.XXXXXX")
  CRABBOX_HOMEBREW_VERIFY_WORK=$work
  trap cleanup_homebrew_work EXIT
  homebrew_home="$work/home"
  homebrew_cache="$work/cache"
  mkdir -m 700 "$homebrew_home" "$homebrew_cache"
  mkdir -m 700 "$work/public-preflight"
  freeze_public_release \
    "$tag" "$asset_dir" "$tag_object" "$source_commit" "$verifier_commit" \
    "$release_id" "$public_run_id" "$proof_dir" "$work/public-preflight" "$node_bin"
  asset_dir="$work/public-preflight/public-assets"
  clean_path="${brew_bin%/*}:${node_bin%/*}:/usr/bin:/bin:/usr/sbin:/sbin"
  user_name=$(id -un)

  # shellcheck disable=SC2016 # Expanded by the credential-free child shell.
  /usr/bin/env -i \
    HOME="$homebrew_home" \
    HOMEBREW_CACHE="$homebrew_cache" \
    USER="$user_name" \
    LOGNAME="$user_name" \
    PATH="$clean_path" \
    TMPDIR="${TMPDIR:-/tmp}" \
    LC_ALL=C \
    HOMEBREW_NO_ANALYTICS=1 \
    HOMEBREW_NO_AUTO_UPDATE=1 \
    HOMEBREW_NO_ENV_HINTS=1 \
    HOMEBREW_NO_INSTALL_CLEANUP=1 \
    NONINTERACTIVE=1 \
    CRABBOX_HOMEBREW_CLEAN_CHILD=1 \
    /bin/bash -c 'source "$1"; shift; homebrew_phase "$@"' \
      crabbox-homebrew-phase "$SCRIPT_PATH" \
      "$tag" "$asset_dir" "$tag_object" "$source_commit" "$verifier_commit" \
      "$native_arch" "$brew_bin" "$node_bin" "$work"

  # Close the downstream verification window with a fresh unauthenticated read.
  # A concurrent release, run, artifact, proof, or public-byte change is an
  # incident and must invalidate this host's proof rather than silently pass.
  require_protected_homebrew_tooling "$verifier_commit" "$tag"
  mkdir -m 700 "$work/public-postflight"
  freeze_public_release \
    "$tag" "$source_asset_dir" "$tag_object" "$source_commit" "$verifier_commit" \
    "$release_id" "$public_run_id" "$proof_dir" "$work/public-postflight" "$node_bin"
  cmp "$work/public-preflight/public-proof.json" "$work/public-postflight/public-proof.json"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
