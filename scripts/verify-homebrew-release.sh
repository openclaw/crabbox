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
  echo "usage: $0 vX.Y.Z <asset-directory> <tag-object> <source-commit> <verifier-commit> <release-id>" >&2
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
        CRABBOX_VERIFY_TOOLING_COMMIT | \
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
  local tooling_commit=${CRABBOX_VERIFY_TOOLING_COMMIT:-$verifier_commit}
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
    [[ "$tag_object" =~ ^[0-9a-f]{40}$ ]] &&
    [[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] &&
    [[ "$verifier_commit" =~ ^[0-9a-f]{40}$ ]] &&
    [[ "$tooling_commit" =~ ^[0-9a-f]{40}$ ]] || usage
}

require_publishable_source() {
  local tag=$1 tag_object=$2 source_commit=$3 verifier_commit=$4
  local tooling_commit=${CRABBOX_VERIFY_TOOLING_COMMIT:-$verifier_commit}
  [[ "$(git -C "$ROOT" rev-parse HEAD)" == "$tooling_commit" ]] || {
    echo "protected tooling checkout does not match the supplied tooling commit" >&2
    return 1
  }
  git -C "$ROOT" merge-base --is-ancestor "$verifier_commit" "$tooling_commit" || {
    echo "provenance verifier commit is not an ancestor of protected tooling" >&2
    return 1
  }
  git -C "$ROOT" merge-base --is-ancestor "$source_commit" "$verifier_commit" || {
    echo "release source commit is not an ancestor of the provenance verifier" >&2
    return 1
  }
  (
    cd "$ROOT"
    DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
      RELEASE_TAG="$tag" \
      EXPECTED_TAG_OBJECT="$tag_object" \
      EXPECTED_TAG_COMMIT="$source_commit" \
      TRUSTED_HEAD="$tooling_commit" \
      REQUIRE_PUBLISHABLE=1 \
      "$ROOT/scripts/verify-release-source.sh" >/dev/null
  )
}

require_protected_homebrew_tooling() {
  local verifier_commit=$1 tag=$2 tooling_status expected_remote remote_commit
  local tooling_commit=${CRABBOX_VERIFY_TOOLING_COMMIT:-$verifier_commit}
  tooling_status=$(git -C "$ROOT" status --porcelain=v1 --untracked-files=all -- \
    "${PROTECTED_HOMEBREW_TOOLING[@]}" "release/records/$tag.json")
  [[ -z "$tooling_status" ]] || {
    echo "protected downstream verifier tooling is dirty" >&2
    printf '%s\n' "$tooling_status" >&2
    return 1
  }
  expected_remote="https://github.com/$CRABBOX_RELEASE_REPOSITORY"
  case "$(git -C "$ROOT" remote get-url origin)" in
    "$expected_remote" | "$expected_remote.git") ;;
    *)
      echo "protected downstream verifier requires canonical origin $expected_remote" >&2
      return 1
      ;;
  esac
  remote_commit=$(git ls-remote "$expected_remote" \
    "refs/heads/$CRABBOX_RELEASE_DEFAULT_BRANCH" | awk '{print $1}')
  [[ "$remote_commit" =~ ^[0-9a-f]{40}$ ]] || {
    echo "canonical default-branch tip is invalid" >&2
    return 1
  }
  git -C "$ROOT" -c fetch.writeCommitGraph=false fetch --quiet --no-tags \
    "$expected_remote" "$remote_commit"
  git -C "$ROOT" merge-base --is-ancestor "$tooling_commit" "$remote_commit" || {
    echo "protected tooling commit is not in canonical default-branch history" >&2
    return 1
  }
  git -C "$ROOT" merge-base --is-ancestor "$verifier_commit" "$tooling_commit" || {
    echo "provenance verifier commit is not an ancestor of protected tooling" >&2
    return 1
  }
  git -C "$ROOT" diff --quiet "$tooling_commit" -- \
    "${PROTECTED_HOMEBREW_TOOLING[@]}" "release/records/$tag.json" || {
    echo "protected downstream verifier tooling differs from $tooling_commit" >&2
    return 1
  }
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

freeze_public_release() {
  [[ $# -eq 8 ]] || usage
  local tag=$1 asset_dir=$2 tag_object=$3 source_commit=$4 verifier_commit=$5
  local release_id=$6 work=$7 node_bin=$8
  local repository=$CRABBOX_RELEASE_REPOSITORY version=${tag#v}
  [[ "$release_id" =~ ^[1-9][0-9]*$ ]] || usage

  local expected_names="$work/expected-assets.txt" notes="$work/expected-notes.md"
  crabbox_release_asset_names "$version" | LC_ALL=C sort >"$expected_names"
  git -C "$ROOT" show "$source_commit:CHANGELOG.md" >"$work/tagged-changelog.md"
  "$ROOT/scripts/extract-release-notes.sh" "$tag" <"$work/tagged-changelog.md" >"$notes"

  find "$asset_dir" -mindepth 1 -maxdepth 1 -exec basename {} \; | LC_ALL=C sort >"$work/actual-assets.txt"
  cmp -s "$expected_names" "$work/actual-assets.txt" || {
    echo "public asset inventory is not exact" >&2
    return 1
  }
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
    curl --disable --fail --silent --show-error --location --retry 3 \
      --header 'Accept: application/vnd.github+json' \
      --header 'X-GitHub-Api-Version: 2026-03-10' \
      "https://api.github.com/$1"
  }
  public_api_get "repos/$repository/releases/$release_id" >"$work/public-release.json"
  env -i \
    CRABBOX_PUBLISH_REPOSITORY="$repository" \
    CRABBOX_PUBLISH_RELEASE_ID="$release_id" \
    CRABBOX_PUBLISH_TAG="$tag" \
    CRABBOX_PUBLISH_TAG_OBJECT="$tag_object" \
    CRABBOX_PUBLISH_SOURCE_COMMIT="$source_commit" \
    CRABBOX_PUBLISH_VERIFIER_COMMIT="$verifier_commit" \
    HOME="${HOME:-/tmp}" LANG=C LC_ALL=C PATH="$PATH" TMPDIR="$work" \
    "$node_bin" "$ROOT/scripts/validate-release-publication.mjs" public-release \
      "$work/public-release.json" "$expected_names" "$notes" "$frozen" >"$work/validated-release.json"
}

verify_homebrew_formula() {
  local node_bin=$1 metadata_file=$2 tag=$3 archive_name=$4 archive_sha=$5
  "$node_bin" - "$metadata_file" "$tag" "$archive_name" "$archive_sha" <<'NODE'
const fs = require("node:fs");
const [file, tag, archive, sha256] = process.argv.slice(2);
const { formulae } = JSON.parse(fs.readFileSync(file, "utf8"));
const formula = formulae?.[0];
const url = `https://github.com/openclaw/crabbox/releases/download/${tag}/${archive}`;
if (
  formulae?.length !== 1 || formula?.name !== "crabbox" ||
  formula.full_name !== "openclaw/tap/crabbox" || formula.tap !== "openclaw/tap" ||
  formula.versions?.stable !== tag.slice(1) || formula.urls?.stable?.url !== url ||
  !/^[0-9a-f]{64}$/.test(sha256) || formula.urls.stable.checksum !== sha256
) throw new Error("Homebrew formula metadata does not match the selected release archive");
NODE
}

homebrew_phase() {
  [[ $# -eq 9 ]] || usage
  local tag=$1 asset_dir=$2 tag_object=$3 source_commit=$4 verifier_commit=$5
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
      CRABBOX_VERIFY_MODE=static \
      CRABBOX_VERIFY_TOOLING_COMMIT="${CRABBOX_VERIFY_TOOLING_COMMIT:-$verifier_commit}" \
      "$ROOT/scripts/verify-release.sh" \
      "$tag" "$asset_dir" "$tag_object" "$source_commit" "$verifier_commit"
  )

  version=${tag#v}
  archive_arch=amd64
  [[ "$native_arch" == arm64 ]] && archive_arch=arm64
  local archive_name="crabbox_${version}_darwin_${archive_arch}.tar.gz"
  local native_archive="$asset_dir/$archive_name"
  local archive_sha
  archive_sha=$(sha256_file "$native_archive")
  local metadata_file="$work/formula.json"
  "$brew_bin" tap openclaw/tap
  "$brew_bin" update --force
  # Tap maintainers own executable formulae; metadata is not a Ruby sandbox.
  "$brew_bin" info --json=v2 --formula "$FORMULA" >"$metadata_file"
  verify_homebrew_formula "$node_bin" "$metadata_file" "$tag" "$archive_name" "$archive_sha"
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
  [[ $# -eq 6 ]] || usage
  local tag=$1 asset_dir=$2 tag_object=$3 source_commit=$4 verifier_commit=$5
  local release_id=$6
  [[ "$release_id" =~ ^[1-9][0-9]*$ ]] || usage
  local native_arch brew_bin node_bin go_bin work clean_path user_name homebrew_home homebrew_cache tooling_commit
  validate_release_identity "$tag" "$tag_object" "$source_commit" "$verifier_commit"
  tooling_commit=${CRABBOX_VERIFY_TOOLING_COMMIT:-$verifier_commit}
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

  # This blocked/ready decision precedes the credential-free child, which
  # repeats it before upstream candidate execution or any brew command.
  require_protected_homebrew_tooling "$verifier_commit" "$tag"
  require_publishable_source "$tag" "$tag_object" "$source_commit" "$verifier_commit"

  brew_bin=$(command -v brew)
  node_bin=$(command -v node)
  go_bin=$(command -v go)
  [[ "$brew_bin" == /* && "$node_bin" == /* && "$go_bin" == /* &&
    -x "$brew_bin" && -x "$node_bin" && -x "$go_bin" ]] || {
    echo "absolute Homebrew, Node, and Go executables are required" >&2
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
    "$release_id" "$work/public-preflight" "$node_bin"
  asset_dir="$work/public-preflight/public-assets"
  clean_path="${brew_bin%/*}:${node_bin%/*}:${go_bin%/*}:/usr/bin:/bin:/usr/sbin:/sbin"
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
    CRABBOX_VERIFY_TOOLING_COMMIT="$tooling_commit" \
    /bin/bash -c 'source "$1"; shift; homebrew_phase "$@"' \
      crabbox-homebrew-phase "$SCRIPT_PATH" \
      "$tag" "$asset_dir" "$tag_object" "$source_commit" "$verifier_commit" \
      "$native_arch" "$brew_bin" "$node_bin" "$work"

  require_protected_homebrew_tooling "$verifier_commit" "$tag"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
