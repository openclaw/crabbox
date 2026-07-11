#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=release-config.sh
source "$ROOT/scripts/release-config.sh"

INPUT=${1:-}
OUTPUT=${2:-}
TAG=${3:-}
RELEASE_ID=${4:-}
SOURCE_COMMIT=${5:-}
EXPECTED_DRAFT=${6:-}

[[ -d "$INPUT/assets" && -f "$INPUT/release.json" ]] || {
  echo "opaque release input is incomplete" >&2
  exit 1
}
[[ ! -e "$OUTPUT" ]] || {
  echo "release output already exists" >&2
  exit 1
}
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 2
[[ "$RELEASE_ID" =~ ^[1-9][0-9]*$ ]] || exit 2
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || exit 2
[[ "$EXPECTED_DRAFT" == true || "$EXPECTED_DRAFT" == false ]] || exit 2

release="$INPUT/release.json"
version=${TAG#v}
expected_assets=$(crabbox_release_asset_names "$version" | LC_ALL=C sort)
actual_assets=$(jq -r '.assets[].name' "$release" | LC_ALL=C sort)
[[ "$actual_assets" == "$expected_assets" ]] || {
  echo "release input asset inventory mismatch" >&2
  exit 1
}
jq -e \
  --arg repository "$CRABBOX_RELEASE_REPOSITORY" \
  --argjson id "$RELEASE_ID" \
  --arg tag "$TAG" \
  --argjson draft "$EXPECTED_DRAFT" '
    .id == $id and
    .tag_name == $tag and
    .name == $tag and
    .draft == $draft and
    .prerelease == false and
    (if $draft then .published_at == null else (.published_at | type) == "string" end) and
    ([.assets[].name] | length) == ([.assets[].name] | unique | length) and
    ([.assets[].id] | length) == ([.assets[].id] | unique | length) and
    all(.assets[];
      (.id | type) == "number" and .id > 0 and
      (.size | type) == "number" and .size > 0 and
      .state == "uploaded" and
      (.digest | test("^sha256:[0-9a-f]{64}$")) and
      .url == ("https://api.github.com/repos/" + $repository + "/releases/assets/" + (.id | tostring))
    )
  ' "$release" >/dev/null

expected_ids=$(jq -r '.assets[].id' "$release" | LC_ALL=C sort -n)
actual_ids=$(find "$INPUT/assets" -mindepth 1 -maxdepth 1 -type f -exec basename {} \; | LC_ALL=C sort -n)
[[ "$actual_ids" == "$expected_ids" ]] || {
  echo "opaque release input file inventory mismatch" >&2
  exit 1
}

notes=$(mktemp "${TMPDIR:-/tmp}/crabbox-release-notes.XXXXXX")
tagged_changelog=$(mktemp "${TMPDIR:-/tmp}/crabbox-tagged-changelog.XXXXXX")
partial="$OUTPUT.partial"
actual_notes="$OUTPUT.notes.partial.$$"
trap 'rm -rf "$notes" "$tagged_changelog" "$actual_notes" "$partial"' EXIT
git -C "$ROOT" show "$SOURCE_COMMIT:CHANGELOG.md" >"$tagged_changelog"
"$ROOT/scripts/extract-release-notes.sh" "$TAG" <"$tagged_changelog" >"$notes"
node - "$release" "$actual_notes" <<'NODE'
const fs = require('node:fs');
const release = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
fs.writeFileSync(process.argv[3], release.body ?? '', { mode: 0o600 });
NODE
cmp "$notes" "$actual_notes"
rm -f "$actual_notes"

mkdir -m 700 "$partial"
while IFS=$'\t' read -r asset_id asset_name expected_size expected_digest; do
  source_file="$INPUT/assets/$asset_id"
  destination="$partial/$asset_name"
  cp "$source_file" "$destination"
  actual_size=$(stat -f '%z' "$destination")
  actual_digest="sha256:$(shasum -a 256 "$destination" | awk '{print $1}')"
  [[ "$actual_size" == "$expected_size" && "$actual_digest" == "$expected_digest" ]] || {
    echo "opaque release asset bytes do not match metadata: $asset_name" >&2
    exit 1
  }
done < <(jq -r '.assets[] | [.id, .name, .size, .digest] | @tsv' "$release")
mv "$partial" "$OUTPUT"
trap - EXIT
rm -f "$notes" "$tagged_changelog"
