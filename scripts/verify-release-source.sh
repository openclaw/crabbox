#!/usr/bin/env bash
set -euo pipefail

: "${DEFAULT_BRANCH:?DEFAULT_BRANCH is required}"
: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${EXPECTED_TAG_OBJECT:?EXPECTED_TAG_OBJECT is required}"
: "${EXPECTED_TAG_COMMIT:?EXPECTED_TAG_COMMIT is required}"

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TRUSTED_HEAD=${TRUSTED_HEAD:-HEAD}
ALLOWED_SIGNERS=${ALLOWED_SIGNERS:-"$ROOT/.github/release-allowed-signers"}
RELEASE_RECORD=${RELEASE_RECORD:-"$ROOT/release/records/$RELEASE_TAG.json"}

if [[ -n "${WORKFLOW_REF:-}" && "$WORKFLOW_REF" != "refs/heads/$DEFAULT_BRANCH" ]]; then
  echo "::error::release events must run from $DEFAULT_BRANCH"
  exit 1
fi
if [[ ! "$RELEASE_TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "::error::release tag must match vMAJOR.MINOR.PATCH"
  exit 1
fi
if [[ ! "$EXPECTED_TAG_OBJECT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ ! "$EXPECTED_TAG_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::tag object and commit must be full lowercase SHA-1 values"
  exit 1
fi
if [[ ! -f "$ALLOWED_SIGNERS" ]]; then
  echo "::error::release allowed-signers file is missing"
  exit 1
fi
if [[ ! -f "$RELEASE_RECORD" ]]; then
  echo "::error::protected release record is missing: $RELEASE_TAG"
  exit 1
fi
node - "$RELEASE_RECORD" "$RELEASE_TAG" "$EXPECTED_TAG_OBJECT" "$EXPECTED_TAG_COMMIT" \
  "${REQUIRE_PUBLISHABLE:-0}" <<'NODE'
const fs = require('node:fs');
const [file, tag, tagObject, sourceCommit, requirePublishable] = process.argv.slice(2);
const record = JSON.parse(fs.readFileSync(file, 'utf8'));
if (
  record.schemaVersion !== 1 ||
  record.repository !== 'openclaw/crabbox' ||
  record.tag !== tag ||
  record.tagObject !== tagObject ||
  record.sourceCommit !== sourceCommit ||
  !['blocked', 'ready'].includes(record.publicationStatus) ||
  (record.publicationStatus === 'blocked' && typeof record.blocker !== 'string')
) throw new Error('protected release record does not match the requested source identity');
if (requirePublishable === '1' && record.publicationStatus !== 'ready') {
  throw new Error(`release ${tag} is blocked: ${record.blocker}`);
}
NODE

tag_ref="refs/tags/$RELEASE_TAG"
if ! git rev-parse --verify "$tag_ref" >/dev/null; then
  echo "::error::release tag does not exist: $RELEASE_TAG"
  exit 1
fi
if [[ "$(git cat-file -t "$tag_ref")" != tag ]]; then
  echo "::error::release tag must be an annotated tag object: $RELEASE_TAG"
  exit 1
fi
actual_tag_object=$(git rev-parse "$tag_ref")
actual_tag_commit=$(git rev-parse "$tag_ref^{commit}")
if [[ "$actual_tag_object" != "$EXPECTED_TAG_OBJECT" ]]; then
  echo "::error::release tag object mismatch: $RELEASE_TAG"
  exit 1
fi
if [[ "$actual_tag_commit" != "$EXPECTED_TAG_COMMIT" ]]; then
  echo "::error::release tag commit mismatch: $RELEASE_TAG"
  exit 1
fi
tag_subject=$(git for-each-ref --format='%(contents:subject)' "$tag_ref")
if [[ "$tag_subject" != "$RELEASE_TAG" ]]; then
  echo "::error::release tag annotation must equal $RELEASE_TAG"
  exit 1
fi
if ! git -c gpg.format=ssh -c gpg.ssh.allowedSignersFile="$ALLOWED_SIGNERS" \
  tag -v "$RELEASE_TAG" >/dev/null 2>&1; then
  echo "::error::release tag is not signed by a repository-allowed key: $RELEASE_TAG"
  exit 1
fi
if [[ -n "${WORKFLOW_SHA:-}" && "$(git rev-parse "$TRUSTED_HEAD")" != "$WORKFLOW_SHA" ]]; then
  echo "::error::protected verifier checkout does not match workflow SHA"
  exit 1
fi
if ! git merge-base --is-ancestor "$EXPECTED_TAG_COMMIT" "$TRUSTED_HEAD"; then
  echo "::error::release tag is not in $DEFAULT_BRANCH history: $RELEASE_TAG"
  exit 1
fi

printf '%s %s\n' "$actual_tag_object" "$actual_tag_commit"
