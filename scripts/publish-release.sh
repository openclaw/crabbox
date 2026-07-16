#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

usage() {
  echo "usage: $0 <release-id> <tag> <tag-object> <source-commit> <verifier-commit> <native-verifier-run-id> <confirm-tag>" >&2
  exit 2
}

[[ $# -eq 7 ]] || usage
RELEASE_ID=$1
TAG=$2
TAG_OBJECT=$3
SOURCE_COMMIT=$4
VERIFIER_COMMIT=$5
VERIFIER_RUN_ID=$6
CONFIRM_TAG=$7

[[ "$RELEASE_ID" =~ ^[1-9][0-9]*$ ]] || usage
[[ "$VERIFIER_RUN_ID" =~ ^[1-9][0-9]*$ ]] || usage
[[ "$TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || usage
[[ "$TAG_OBJECT" =~ ^[0-9a-f]{40}$ ]] || usage
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || usage
[[ "$VERIFIER_COMMIT" =~ ^[0-9a-f]{40}$ ]] || usage
[[ "$CONFIRM_TAG" == "$TAG" ]] || {
  echo "publication confirmation must exactly equal $TAG" >&2
  exit 2
}

for tool in cmp gh git jq node shasum unzip; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

PROTECTED_TOOLING=(
  .github/CODEOWNERS
  .github/release-allowed-signers
  .github/workflows/release-assets.yml
  "release/records/$TAG.json"
  scripts/extract-release-notes.sh
  scripts/publish-release.sh
  scripts/release-config.sh
  scripts/validate-release-publication.mjs
  scripts/verify-github-release-policy.mjs
  scripts/verify-release-source.sh
)
actual_head=$(git -C "$ROOT" rev-parse --verify 'HEAD^{commit}')
[[ "$actual_head" == "$VERIFIER_COMMIT" ]] || {
  echo "local HEAD must exactly equal protected verifier commit $VERIFIER_COMMIT" >&2
  exit 1
}
tooling_status=$(git -C "$ROOT" status --porcelain=v1 --untracked-files=all -- "${PROTECTED_TOOLING[@]}")
[[ -z "$tooling_status" ]] || {
  echo "protected release tooling is dirty; publish only from the exact clean verifier commit" >&2
  printf '%s\n' "$tooling_status" >&2
  exit 1
}
git -C "$ROOT" diff --quiet "$VERIFIER_COMMIT" -- "${PROTECTED_TOOLING[@]}" || {
  echo "protected release tooling does not match verifier commit $VERIFIER_COMMIT" >&2
  exit 1
}
[[ "${CRABBOX_RELEASE_SERIALIZATION_CONFIRMED:-}" == "$TAG:$RELEASE_ID" ]] || {
  echo "publication requires an exclusive administrative release freeze; set CRABBOX_RELEASE_SERIALIZATION_CONFIRMED=$TAG:$RELEASE_ID only after confirming it" >&2
  exit 1
}

# shellcheck source=release-config.sh
# shellcheck disable=SC1091
source "$ROOT/scripts/release-config.sh"

REPOSITORY=$CRABBOX_RELEASE_REPOSITORY
DEFAULT_BRANCH=$CRABBOX_RELEASE_DEFAULT_BRANCH
WORKFLOW_PATH=.github/workflows/release-assets.yml
WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-publish-release.XXXXXX")
chmod 700 "$WORK"
trap 'rm -rf "$WORK"' EXIT
git -C "$ROOT" show "$VERIFIER_COMMIT:release/records/$TAG.json" >"$WORK/protected-release-record.json" || {
  echo "protected verifier commit does not contain the release record for $TAG" >&2
  exit 1
}
git -C "$ROOT" show "$VERIFIER_COMMIT:.github/release-allowed-signers" >"$WORK/protected-release-allowed-signers" || {
  echo "protected verifier commit does not contain the release signer policy" >&2
  exit 1
}

api_get() {
  gh api --method GET \
    --header 'Accept: application/vnd.github+json' \
    --header 'X-GitHub-Api-Version: 2022-11-28' \
    "$1"
}

api_artifact_download() {
  gh api --method GET \
    --header 'Accept: application/vnd.github+json' \
    --header 'X-GitHub-Api-Version: 2022-11-28' \
    "$1"
}

api_asset_download() {
  gh api --method GET \
    --header 'Accept: application/octet-stream' \
    --header 'X-GitHub-Api-Version: 2022-11-28' \
    "$1"
}

publication_environment() {
  env -i \
    "CRABBOX_PUBLISH_REPOSITORY=$REPOSITORY" \
    "CRABBOX_PUBLISH_RELEASE_ID=$RELEASE_ID" \
    "CRABBOX_PUBLISH_TAG=$TAG" \
    "CRABBOX_PUBLISH_TAG_OBJECT=$TAG_OBJECT" \
    "CRABBOX_PUBLISH_SOURCE_COMMIT=$SOURCE_COMMIT" \
    "CRABBOX_PUBLISH_VERIFIER_COMMIT=$VERIFIER_COMMIT" \
    "CRABBOX_PUBLISH_VERIFIER_RUN_ID=$VERIFIER_RUN_ID" \
    "CRABBOX_PUBLISH_DEFAULT_BRANCH=$DEFAULT_BRANCH" \
    "CRABBOX_PUBLISH_WORKFLOW_PATH=$WORKFLOW_PATH" \
    "HOME=${HOME:-/tmp}" LANG=C LC_ALL=C "PATH=$PATH" "TMPDIR=$WORK" \
    "$@"
}

verify_protected_source() {
  local prefix=$1
  api_get "repos/$REPOSITORY" >"$WORK/$prefix-repository.json"
  api_get "repos/$REPOSITORY/branches/$DEFAULT_BRANCH" >"$WORK/$prefix-branch.json"
  api_get "repos/$REPOSITORY/rulesets?per_page=100" >"$WORK/$prefix-ruleset-list.json"
  mkdir -m 700 "$WORK/$prefix-rulesets"
  while IFS= read -r ruleset_id; do
    [[ "$ruleset_id" =~ ^[1-9][0-9]*$ ]] || {
      echo "release ruleset inventory contains an invalid ID" >&2
      exit 1
    }
    api_get "repos/$REPOSITORY/rulesets/$ruleset_id" \
      >"$WORK/$prefix-rulesets/$ruleset_id.json"
  done < <(jq -r '.[].id' "$WORK/$prefix-ruleset-list.json")
  ruleset_count=$(find "$WORK/$prefix-rulesets" -type f -name '*.json' | wc -l | tr -d '[:space:]')
  [[ "$ruleset_count" -gt 0 ]] || {
    echo "repository has no release protection rulesets" >&2
    exit 1
  }
  jq -s '.' "$WORK/$prefix-rulesets"/*.json >"$WORK/$prefix-rulesets.json"
  api_get "repos/$REPOSITORY/git/ref/tags/$TAG" >"$WORK/$prefix-tag-ref.json"
  api_get "repos/$REPOSITORY/git/tags/$TAG_OBJECT" >"$WORK/$prefix-tag-object.json"

  jq -e \
    --arg repository "$REPOSITORY" \
    --arg branch "$DEFAULT_BRANCH" '
      .full_name == $repository and .default_branch == $branch
    ' "$WORK/$prefix-repository.json" >/dev/null || {
    echo "repository default branch does not match the protected release contract" >&2
    exit 1
  }
  node "$ROOT/scripts/verify-github-release-policy.mjs" \
    "$WORK/$prefix-repository.json" "$WORK/$prefix-rulesets.json" \
    "$REPOSITORY" "$DEFAULT_BRANCH" "$TAG" >/dev/null
  jq -e \
    --arg branch "$DEFAULT_BRANCH" \
    --arg commit "$VERIFIER_COMMIT" '
      .name == $branch and .protected == true and .commit.sha == $commit
    ' "$WORK/$prefix-branch.json" >/dev/null || {
    echo "protected default-branch head does not match the verifier commit" >&2
    exit 1
  }
  jq -e \
    --arg ref "refs/tags/$TAG" \
    --arg object "$TAG_OBJECT" \
    --arg url "https://api.github.com/repos/$REPOSITORY/git/tags/$TAG_OBJECT" '
      .ref == $ref and .object.type == "tag" and .object.sha == $object and .object.url == $url
    ' "$WORK/$prefix-tag-ref.json" >/dev/null || {
    echo "remote annotated tag object drifted" >&2
    exit 1
  }
  jq -e \
    --arg tag "$TAG" \
    --arg commit "$SOURCE_COMMIT" \
    --arg url "https://api.github.com/repos/$REPOSITORY/git/commits/$SOURCE_COMMIT" '
      .tag == $tag and
      .object.type == "commit" and
      .object.sha == $commit and
      .object.url == $url and
      .verification.verified == true and
      .verification.reason == "valid"
    ' "$WORK/$prefix-tag-object.json" >/dev/null || {
    echo "remote tag signature or peeled commit does not match the pinned source" >&2
    exit 1
  }

  DEFAULT_BRANCH=$DEFAULT_BRANCH \
  RELEASE_TAG=$TAG \
  EXPECTED_TAG_OBJECT=$TAG_OBJECT \
  EXPECTED_TAG_COMMIT=$SOURCE_COMMIT \
  TRUSTED_HEAD=$VERIFIER_COMMIT \
  RELEASE_RECORD="$WORK/protected-release-record.json" \
  ALLOWED_SIGNERS="$WORK/protected-release-allowed-signers" \
  REQUIRE_PUBLISHABLE=1 \
    "$ROOT/scripts/verify-release-source.sh" >/dev/null
}

version=${TAG#v}
scripts_asset_names="$WORK/expected-assets.txt"
crabbox_release_asset_names "$version" | LC_ALL=C sort >"$scripts_asset_names"
[[ "$(wc -l <"$scripts_asset_names" | tr -d '[:space:]')" == 8 ]] || {
  echo "release configuration must define exactly eight assets" >&2
  exit 1
}
git -C "$ROOT" show "$SOURCE_COMMIT:CHANGELOG.md" >"$WORK/tagged-changelog.md"
"$ROOT/scripts/extract-release-notes.sh" "$TAG" \
  <"$WORK/tagged-changelog.md" >"$WORK/expected-notes.md"

# Initial trust check. This is repeated after every remote byte is re-downloaded.
verify_protected_source initial

api_get "repos/$REPOSITORY/actions/runs/$VERIFIER_RUN_ID" >"$WORK/run.json"
workflow_id=$(jq -er '.workflow_id | select(type == "number" and . > 0)' "$WORK/run.json")
api_get "repos/$REPOSITORY/actions/workflows/$workflow_id" >"$WORK/workflow.json"
api_get "repos/$REPOSITORY/releases/$RELEASE_ID" >"$WORK/initial-release.json"
api_get "repos/$REPOSITORY/actions/runs/$VERIFIER_RUN_ID/artifacts?per_page=100" >"$WORK/artifacts.json"

artifact_names=$(jq -r '.artifacts[].name' "$WORK/artifacts.json" | LC_ALL=C sort)
[[ "$artifact_names" == $'release-input\nverified-assets-arm64\nverified-assets-x86_64' ]] || {
  echo "native verifier input/proof artifacts are missing or ambiguous" >&2
  exit 1
}

for arch in arm64 x86_64; do
  artifact_id=$(jq -er --arg name "verified-assets-$arch" '
    [.artifacts[] | select(.name == $name)] |
    if length == 1 then .[0].id else error("ambiguous artifact") end
  ' "$WORK/artifacts.json")
  expected_zip_size=$(jq -er --arg name "verified-assets-$arch" '
    [.artifacts[] | select(.name == $name)] |
    if length == 1 then .[0].size_in_bytes else error("ambiguous artifact") end
  ' "$WORK/artifacts.json")
  expected_zip_digest=$(jq -er --arg name "verified-assets-$arch" '
    [.artifacts[] | select(.name == $name)] |
    if length == 1 then .[0].digest else error("ambiguous artifact") end
  ' "$WORK/artifacts.json")
  api_artifact_download "repos/$REPOSITORY/actions/artifacts/$artifact_id/zip" >"$WORK/proof-$arch.zip"
  actual_zip_size=$(wc -c <"$WORK/proof-$arch.zip" | tr -d '[:space:]')
  actual_zip_digest="sha256:$(shasum -a 256 "$WORK/proof-$arch.zip" | awk '{print $1}')"
  [[ "$actual_zip_size" == "$expected_zip_size" && "$actual_zip_digest" == "$expected_zip_digest" ]] || {
    echo "downloaded $arch proof artifact does not match its exact GitHub digest" >&2
    exit 1
  }
  proof_listing=$(unzip -Z1 "$WORK/proof-$arch.zip")
  [[ "$proof_listing" == verified-assets.json ]] || {
    echo "$arch proof artifact must contain only verified-assets.json" >&2
    exit 1
  }
  mkdir -m 700 "$WORK/proof-$arch"
  unzip -qq "$WORK/proof-$arch.zip" -d "$WORK/proof-$arch"
done

publication_environment node "$ROOT/scripts/validate-release-publication.mjs" preflight \
  "$WORK/initial-release.json" \
  "$WORK/run.json" \
  "$WORK/workflow.json" \
  "$WORK/artifacts.json" \
  "$WORK/proof-arm64/verified-assets.json" \
  "$WORK/proof-x86_64/verified-assets.json" \
  "$scripts_asset_names" \
  "$WORK/expected-notes.md" \
  "$WORK/trusted-draft.json"

# Re-read the numeric draft before downloading any candidate byte by its pinned asset ID.
api_get "repos/$REPOSITORY/releases/$RELEASE_ID" >"$WORK/predownload-release.json"
publication_environment node "$ROOT/scripts/validate-release-publication.mjs" state \
  "$WORK/predownload-release.json" "$WORK/trusted-draft.json" \
  "$scripts_asset_names" "$WORK/expected-notes.md" draft >"$WORK/predownload-state.json"

mkdir -m 700 "$WORK/release-assets"
while IFS=$'\t' read -r asset_id asset_name expected_size expected_sha; do
  api_asset_download "repos/$REPOSITORY/releases/assets/$asset_id" >"$WORK/release-assets/$asset_name"
  actual_size=$(wc -c <"$WORK/release-assets/$asset_name" | tr -d '[:space:]')
  actual_sha=$(shasum -a 256 "$WORK/release-assets/$asset_name" | awk '{print $1}')
  [[ "$actual_size" == "$expected_size" && "$actual_sha" == "$expected_sha" ]] || {
    echo "release asset bytes drifted after native verification: $asset_name" >&2
    exit 1
  }
done < <(jq -r '.assets[] | [.id, .name, .size, .sha256] | @tsv' "$WORK/trusted-draft.json")

# Close races during the downloads: require the draft record to remain byte-for-byte bound to the proof.
api_get "repos/$REPOSITORY/releases/$RELEASE_ID" >"$WORK/postdownload-release.json"
publication_environment node "$ROOT/scripts/validate-release-publication.mjs" state \
  "$WORK/postdownload-release.json" "$WORK/trusted-draft.json" \
  "$scripts_asset_names" "$WORK/expected-notes.md" draft >"$WORK/postdownload-state.json"
cmp "$WORK/predownload-state.json" "$WORK/postdownload-state.json"

# Prepare the sole mutation body before the final trust and draft reads. GitHub's
# release API has no documented conditional PATCH, so the required administrative
# freeze is the serialization boundary; no local command intervenes after the
# final comparison except the exact draft=false PATCH.
printf '{"draft":false}\n' >"$WORK/publish.json"
api_get "repos/$REPOSITORY/immutable-releases" >"$WORK/immutable-releases.json"
jq -e '.enabled == true and .enforced_by_owner == true' \
  "$WORK/immutable-releases.json" >/dev/null || {
  echo "organization-enforced release immutability is required before publication" >&2
  exit 1
}
verify_protected_source final
api_get "repos/$REPOSITORY/releases/$RELEASE_ID" >"$WORK/final-draft.json"
publication_environment node "$ROOT/scripts/validate-release-publication.mjs" state \
  "$WORK/final-draft.json" "$WORK/trusted-draft.json" \
  "$scripts_asset_names" "$WORK/expected-notes.md" draft >"$WORK/final-draft-state.json"
cmp "$WORK/postdownload-state.json" "$WORK/final-draft-state.json"

gh api --method PATCH \
  --header 'Accept: application/vnd.github+json' \
  --header 'X-GitHub-Api-Version: 2022-11-28' \
  "repos/$REPOSITORY/releases/$RELEASE_ID" \
  --input "$WORK/publish.json" >"$WORK/publish-response.json"

publication_environment node "$ROOT/scripts/validate-release-publication.mjs" state \
  "$WORK/publish-response.json" "$WORK/trusted-draft.json" \
  "$scripts_asset_names" "$WORK/expected-notes.md" published >"$WORK/publish-response-state.json"

api_get "repos/$REPOSITORY/releases/$RELEASE_ID" >"$WORK/public-release.json"
publication_environment node "$ROOT/scripts/validate-release-publication.mjs" state \
  "$WORK/public-release.json" "$WORK/trusted-draft.json" \
  "$scripts_asset_names" "$WORK/expected-notes.md" published >"$WORK/public-release-state.json"
cmp "$WORK/publish-response-state.json" "$WORK/public-release-state.json"
verify_protected_source published

echo "Published exact verified release $TAG (release $RELEASE_ID) from native verifier run $VERIFIER_RUN_ID"
