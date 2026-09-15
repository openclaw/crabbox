#!/usr/bin/env bash
# shellcheck disable=SC2034 # Constants are consumed by scripts that source this file.
set -euo pipefail

CRABBOX_RELEASE_REPOSITORY=openclaw/crabbox
CRABBOX_RELEASE_DEFAULT_BRANCH=main
CRABBOX_RELEASE_GO_VERSION=go1.26.4
CRABBOX_RELEASE_GORELEASER_VERSION=2.17.0
CRABBOX_RELEASE_TEAM_ID=FWJYW4S8P8
CRABBOX_RELEASE_AUTHORITY="Developer ID Application: OpenClaw Foundation (${CRABBOX_RELEASE_TEAM_ID})"
CRABBOX_RELEASE_CLI_IDENTIFIER=org.openclaw.crabbox
CRABBOX_RELEASE_HELPER_IDENTIFIER=org.openclaw.crabbox.apple-vm-helper
CRABBOX_RELEASE_VMD_IDENTIFIER=org.openclaw.crabbox.apple-vm-vmd
CRABBOX_RELEASE_JJ_IDENTIFIER=org.openclaw.crabbox.jj-source
readonly \
  CRABBOX_RELEASE_REPOSITORY \
  CRABBOX_RELEASE_DEFAULT_BRANCH \
  CRABBOX_RELEASE_GO_VERSION \
  CRABBOX_RELEASE_GORELEASER_VERSION \
  CRABBOX_RELEASE_TEAM_ID \
  CRABBOX_RELEASE_AUTHORITY \
  CRABBOX_RELEASE_CLI_IDENTIFIER \
  CRABBOX_RELEASE_HELPER_IDENTIFIER \
  CRABBOX_RELEASE_VMD_IDENTIFIER \
  CRABBOX_RELEASE_JJ_IDENTIFIER

crabbox_release_normalize_version() {
  local version=${1:-}
  version=${version#v}
  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "release version must be X.Y.Z or vX.Y.Z" >&2
    return 2
  fi
  printf '%s\n' "$version"
}

# Existing signed Go-only source tags retain schema 1. The native source
# manifest declares schema 2; archive/provenance contents cannot downgrade it.
crabbox_release_source_schema() {
  local root=${1:-} commit=${2:-} entry
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || return 2
  git -C "$root" --no-replace-objects cat-file -e "$commit^{commit}" || return
  entry=$(git -C "$root" --no-replace-objects ls-tree "$commit" -- tools/jj-source/manifest.json) || return
  if [[ -z "$entry" ]]; then
    printf '%s\n' 1
  elif [[ "$entry" == "100644 blob "* || "$entry" == "100755 blob "* ]]; then
    printf '%s\n' 2
  else
    echo "native release source manifest must be a regular tagged file" >&2
    return 1
  fi
}

crabbox_release_prepare_source_contract() {
  local root=${1:-} commit=${2:-} manifest=${3:-}
  [[ -n "$manifest" ]] || return 2
  CRABBOX_RELEASE_SOURCE_SCHEMA=$(crabbox_release_source_schema "$root" "$commit") || return
  # Keep this nonempty: stock macOS Bash treats empty arrays as unset with -u.
  CRABBOX_RELEASE_PROVENANCE_ARGS=(--schema-version "$CRABBOX_RELEASE_SOURCE_SCHEMA")
  if [[ "$CRABBOX_RELEASE_SOURCE_SCHEMA" == 2 ]]; then
    git -C "$root" --no-replace-objects show "$commit:tools/jj-source/manifest.json" >"$manifest" || return
    CRABBOX_RELEASE_PROVENANCE_ARGS+=(--native-source-manifest "$manifest")
  fi
}

crabbox_release_archive_names() {
  local version
  version=$(crabbox_release_normalize_version "${1:-}") || return
  printf '%s\n' \
    "crabbox_${version}_darwin_amd64.tar.gz" \
    "crabbox_${version}_darwin_arm64.tar.gz" \
    "crabbox_${version}_linux_amd64.tar.gz" \
    "crabbox_${version}_linux_arm64.tar.gz" \
    "crabbox_${version}_windows_amd64.zip" \
    "crabbox_${version}_windows_arm64.zip"
}

crabbox_release_asset_names() {
  crabbox_release_archive_names "${1:-}" || return
  printf '%s\n' checksums.txt provenance.json
}

crabbox_release_archive_members() {
  local schema=${1:-} platform=${2:-} arch=${3:-} suffix=
  [[ "$schema" == 1 || "$schema" == 2 ]] || return 2
  [[ "$arch" == amd64 || "$arch" == arm64 ]] || return 2
  case "$platform" in darwin | linux) ;; windows) suffix=.exe ;; *) return 2 ;; esac
  {
    printf '%s\n' "crabbox$suffix"
    if [[ "$platform" == darwin && "$arch" == arm64 ]]; then printf '%s\n' crabbox-apple-vm-helper; fi
    if [[ "$schema" == 2 ]]; then printf '%s\n' "crabbox-jj-source$suffix" crabbox-jj-source.json crabbox-jj-source.NOTICES.txt attribution.json; fi
  } | LC_ALL=C sort
}

crabbox_release_pack_archive() {
  local schema=${1:-} platform=${2:-} arch=${3:-} source=${4:-} archive=${5:-} member_list member
  local members=()
  [[ "$source" == /* && "$archive" == /* && ! -e "$archive" && ! -L "$archive" ]] || {
    echo "archive packing requires an absolute source and a new absolute output" >&2; return 2;
  }
  member_list=$(crabbox_release_archive_members "$schema" "$platform" "$arch") || return
  while IFS= read -r member; do members+=("$member"); done <<<"$member_list"
  for member in "${members[@]}"; do
    [[ -f "$source/$member" && ! -L "$source/$member" ]] || {
      echo "archive member must be a regular file: $member" >&2; return 1;
    }
  done
  if [[ "$platform" == windows ]]; then
    (cd "$source" && zip -q "$archive" "${members[@]}")
  else
    COPYFILE_DISABLE=1 tar -czf "$archive" -C "$source" "${members[@]}"
  fi
}

crabbox_release_designated_requirement() {
  local identifier=${1:-}
  case "$identifier" in
    "$CRABBOX_RELEASE_CLI_IDENTIFIER" | \
      "$CRABBOX_RELEASE_HELPER_IDENTIFIER" | \
      "$CRABBOX_RELEASE_JJ_IDENTIFIER" | \
      "$CRABBOX_RELEASE_VMD_IDENTIFIER") ;;
    *)
      echo "unsupported Crabbox release identifier: ${identifier:-<empty>}" >&2
      return 2
      ;;
  esac
  printf '%s\n' \
    "identifier \"$identifier\" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"$CRABBOX_RELEASE_TEAM_ID\""
}

crabbox_release_normalize_macos_arch() {
  case "${1:-}" in
    arm64) printf '%s\n' arm64 ;;
    amd64 | x86_64) printf '%s\n' x86_64 ;;
    *)
      echo "macOS release architecture must be arm64, amd64, or x86_64" >&2
      return 2
      ;;
  esac
}

crabbox_release_assert_identifier_arch() {
  local identifier=${1:-} arch
  arch=$(crabbox_release_normalize_macos_arch "${2:-}") || return
  case "$identifier:$arch" in
    "$CRABBOX_RELEASE_CLI_IDENTIFIER:arm64" | \
      "$CRABBOX_RELEASE_CLI_IDENTIFIER:x86_64" | \
      "$CRABBOX_RELEASE_HELPER_IDENTIFIER:arm64" | \
      "$CRABBOX_RELEASE_JJ_IDENTIFIER:arm64" | \
      "$CRABBOX_RELEASE_JJ_IDENTIFIER:x86_64" | \
      "$CRABBOX_RELEASE_VMD_IDENTIFIER:arm64") ;;
    *)
      echo "unsupported Crabbox release identifier/architecture: ${identifier:-<empty>}/${arch}" >&2
      return 2
      ;;
  esac
}

crabbox_release_assert_no_publication_tokens() {
  local name
  for name in \
    GH_TOKEN \
    GITHUB_TOKEN \
    HOMEBREW_GITHUB_API_TOKEN \
    HOMEBREW_TAP_GITHUB_TOKEN \
    ACTIONS_ID_TOKEN_REQUEST_TOKEN \
    ACTIONS_RUNTIME_TOKEN; do
    if [[ -n "${!name+x}" ]]; then
      echo "$name must be unset during Crabbox release signing and verification" >&2
      return 1
    fi
  done
}

if [[ "${BASH_SOURCE[0]:-}" == "$0" ]]; then
  [[ $# -eq 2 ]] || {
    echo "usage: $0 <assets|archives> <version>" >&2
    exit 2
  }
  case "$1" in
    assets) crabbox_release_asset_names "$2" ;;
    archives) crabbox_release_archive_names "$2" ;;
    *)
      echo "usage: $0 <assets|archives> <version>" >&2
      exit 2
      ;;
  esac
fi
