#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=release-config.sh
source "$ROOT/scripts/release-config.sh"

TAG=${1:-}
TAG_OBJECT=${2:-}
TAG_COMMIT=${3:-}
VERIFIER_COMMIT=${4:-}
case $# in
  5)
    ASSET_DIR="$ROOT/dist-release"
    CONFIRM=$5
    ;;
  6)
    ASSET_DIR=$5
    CONFIRM=$6
    ;;
  *)
    ASSET_DIR="$ROOT/dist-release"
    CONFIRM=
    ;;
esac

if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  [[ ! "$TAG_OBJECT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$TAG_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$VERIFIER_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
  echo "usage: $0 vX.Y.Z <tag-object> <tag-commit> <verifier-commit> [asset-directory] <confirm-tag>" >&2
  exit 2
fi
[[ "$CONFIRM" == "$TAG" ]] || {
  echo "final confirmation must exactly equal $TAG" >&2
  exit 2
}
[[ "$(uname -s)" == Darwin ]] || {
  echo "draft creation requires the native macOS verifier host" >&2
  exit 1
}
for tool in gh git jq node shasum; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

[[ "$(git -C "$ROOT" rev-parse HEAD)" == "$VERIFIER_COMMIT" ]] || {
  echo "local protected verifier commit mismatch" >&2
  exit 1
}
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]] || {
  echo "release tooling checkout must be clean" >&2
  exit 1
}
DEFAULT_BRANCH="$CRABBOX_RELEASE_DEFAULT_BRANCH" \
RELEASE_TAG="$TAG" \
EXPECTED_TAG_OBJECT="$TAG_OBJECT" \
EXPECTED_TAG_COMMIT="$TAG_COMMIT" \
TRUSTED_HEAD="$VERIFIER_COMMIT" \
REQUIRE_PUBLISHABLE=1 \
  "$ROOT/scripts/verify-release-source.sh" >/dev/null

remote_tags=$(git -C "$ROOT" ls-remote --tags origin \
  "refs/tags/$TAG" "refs/tags/$TAG^{}")
remote_object=$(awk -v ref="refs/tags/$TAG" '$2 == ref { print $1 }' <<<"$remote_tags")
remote_commit=$(awk -v ref="refs/tags/$TAG^{}" '$2 == ref { print $1 }' <<<"$remote_tags")
[[ "$remote_object" == "$TAG_OBJECT" && "$remote_commit" == "$TAG_COMMIT" ]] || {
  echo "remote signed tag identity mismatch" >&2
  exit 1
}
remote_main=$(git -C "$ROOT" ls-remote origin "refs/heads/$CRABBOX_RELEASE_DEFAULT_BRANCH" | awk '{print $1}')
[[ "$remote_main" == "$VERIFIER_COMMIT" ]] || {
  echo "protected default branch moved after candidate production" >&2
  exit 1
}

# This credential-bearing gate must never execute candidate-controlled code.
# Native execution is a separate token-free operator step and later a clean
# dependent Actions job. Re-run only protected static verification here.
verify_home=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-draft-verify.XXXXXX")
trap 'rm -rf "$verify_home"' EXIT
native_arch=$(uname -m)
env -i \
  CRABBOX_VERIFY_EXEC_ARCH="$native_arch" \
  CRABBOX_VERIFY_MODE=static \
  HOME="$verify_home" LANG=C LC_ALL=C PATH="$PATH" TMPDIR="$verify_home" \
  "$ROOT/scripts/verify-release.sh" \
    "$TAG" "$ASSET_DIR" "$TAG_OBJECT" "$TAG_COMMIT" "$VERIFIER_COMMIT"

releases="$verify_home/releases.json"
gh api --paginate --slurp "repos/$CRABBOX_RELEASE_REPOSITORY/releases?per_page=100" >"$releases"
matches=$(jq --arg tag "$TAG" '[.[][] | select(.tag_name == $tag)] | length' "$releases")
[[ "$matches" == 0 ]] || {
  echo "release record already exists for $TAG; refusing to delete or replace it" >&2
  exit 1
}

notes="$verify_home/release-notes.md"
tagged_changelog="$verify_home/tagged-changelog.md"
git -C "$ROOT" show "$TAG_COMMIT:CHANGELOG.md" >"$tagged_changelog"
"$ROOT/scripts/extract-release-notes.sh" "$TAG" \
  <"$tagged_changelog" >"$notes"
assets=()
while IFS= read -r name; do
  assets+=("$ASSET_DIR/$name")
done < <(crabbox_release_asset_names "${TAG#v}")

# Sole mutation in this gate: create one private draft and upload the already
# verified bytes. Any partial failure is preserved for inspection, never deleted.
gh release create "$TAG" \
  --repo "$CRABBOX_RELEASE_REPOSITORY" \
  --draft \
  --verify-tag \
  --title "$TAG" \
  --notes-file "$notes" \
  "${assets[@]}"

gh api --paginate --slurp "repos/$CRABBOX_RELEASE_REPOSITORY/releases?per_page=100" >"$releases"
jq --arg tag "$TAG" \
  '[.[][] | select(.tag_name == $tag and .draft == true and .prerelease == false)]
   | if length == 1 then .[0] else error("expected exactly one private draft") end' \
  "$releases" >"$verify_home/release.json"
release_id=$(jq -r '.id' "$verify_home/release.json")
[[ "$release_id" =~ ^[1-9][0-9]*$ ]]
gh api --method GET "repos/$CRABBOX_RELEASE_REPOSITORY/releases/$release_id" \
  >"$verify_home/release.json"

EXPECTED_ASSETS=$(crabbox_release_asset_names "${TAG#v}" | LC_ALL=C sort) \
NOTES_FILE="$notes" \
RELEASE_FILE="$verify_home/release.json" \
RELEASE_ID="$release_id" \
RELEASE_TAG="$TAG" \
node <<'NODE'
const fs = require('node:fs');
const release = JSON.parse(fs.readFileSync(process.env.RELEASE_FILE, 'utf8'));
const expectedAssets = process.env.EXPECTED_ASSETS.split('\n').filter(Boolean);
const actualAssets = release.assets.map((asset) => asset.name).sort();
const notes = fs.readFileSync(process.env.NOTES_FILE, 'utf8');
if (
  release.id !== Number(process.env.RELEASE_ID) ||
  release.tag_name !== process.env.RELEASE_TAG ||
  release.name !== process.env.RELEASE_TAG ||
  release.body !== notes ||
  release.draft !== true ||
  release.immutable !== false ||
  release.prerelease !== false ||
  release.published_at !== null ||
  JSON.stringify(actualAssets) !== JSON.stringify(expectedAssets) ||
  new Set(actualAssets).size !== expectedAssets.length ||
  !release.assets.every((asset) =>
    Number.isInteger(asset.id) && asset.id > 0 && asset.size > 0 && asset.state === 'uploaded' &&
    /^sha256:[0-9a-f]{64}$/.test(asset.digest ?? '')
  )
) throw new Error('created draft does not match the immutable release record');
NODE

download="$verify_home/download"
mkdir -m 700 "$download"
while IFS=$'\t' read -r asset_id asset_name expected_size expected_digest; do
  gh api --method GET --header 'Accept: application/octet-stream' \
    "repos/$CRABBOX_RELEASE_REPOSITORY/releases/assets/$asset_id" >"$download/$asset_name"
  [[ "$(stat -f '%z' "$download/$asset_name")" == "$expected_size" ]]
  [[ "sha256:$(shasum -a 256 "$download/$asset_name" | awk '{print $1}')" == "$expected_digest" ]]
  cmp "$ASSET_DIR/$asset_name" "$download/$asset_name"
done < <(jq -r '.assets[] | [.id, .name, .size, .digest] | @tsv' "$verify_home/release.json")

echo "Created immutable private draft release_id=$release_id tag=$TAG"
