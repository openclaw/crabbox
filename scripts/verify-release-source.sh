#!/usr/bin/env bash
set -euo pipefail

: "${DEFAULT_BRANCH:?DEFAULT_BRANCH is required}"
: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${WORKFLOW_REF:?WORKFLOW_REF is required}"

if [[ "$WORKFLOW_REF" != "refs/heads/$DEFAULT_BRANCH" ]]; then
  echo "::error::release events must run from $DEFAULT_BRANCH"
  exit 1
fi
if [[ ! "$RELEASE_TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "::error::release tag must match vMAJOR.MINOR.PATCH"
  exit 1
fi

tag_ref="refs/tags/$RELEASE_TAG"
if ! git rev-parse --verify "$tag_ref^{commit}" >/dev/null; then
  echo "::error::release tag does not exist: $RELEASE_TAG"
  exit 1
fi
if ! git merge-base --is-ancestor "$tag_ref^{commit}" HEAD; then
  echo "::error::release tag is not in $DEFAULT_BRANCH history: $RELEASE_TAG"
  exit 1
fi
