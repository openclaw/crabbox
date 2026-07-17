#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <owner/repository> <release-id> <public-verifier-run-id> <output-dir>" >&2
  exit 2
}

[[ $# -eq 4 ]] || usage
repository=$1
release_id=$2
run_id=$3
output_dir=$4

[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage
[[ "$release_id" =~ ^[1-9][0-9]*$ && "$run_id" =~ ^[1-9][0-9]*$ ]] || usage
[[ ! -e "$output_dir" && ! -L "$output_dir" ]] || {
  echo "public witness output must not already exist" >&2
  exit 1
}

for credential in GH_TOKEN GITHUB_TOKEN HOMEBREW_GITHUB_API_TOKEN HOMEBREW_TAP_GITHUB_TOKEN; do
  [[ -z "${!credential:-}" ]] || {
    echo "public witness fetch must not receive $credential" >&2
    exit 1
  }
done

umask 077
mkdir -m 700 "$output_dir"
complete=0
cleanup() {
  if [[ "$complete" != 1 ]]; then
    rm -rf "$output_dir"
  fi
}
trap cleanup EXIT

fetch_json() {
  local path=$1 name=$2 raw
  raw="$output_dir/.$name.raw"
  curl --fail --silent --show-error --location \
    --retry-all-errors --retry 6 --retry-delay 10 --retry-max-time 120 \
    --header 'Accept: application/vnd.github+json' \
    --header 'X-GitHub-Api-Version: 2026-03-10' \
    --output "$raw" "https://api.github.com/$path"
  case "$name" in
    release.json)
      jq -eS '
        select(type == "object") |
        {
          id, tag_name, target_commitish, name, draft, immutable, prerelease,
          created_at, updated_at, published_at, body,
          assets: ([.assets[] | {
            id, name, size, state, digest, updated_at, url
          }] | sort_by(.name))
        }
      ' "$raw" >"$output_dir/$name"
      ;;
    run.json)
      jq -eS '
        select(type == "object") |
        {
          id, name, display_title, path, event, status, conclusion,
          head_branch, head_sha, workflow_id, workflow_url, run_attempt,
          created_at, run_started_at,
          repository: {full_name: .repository.full_name},
          head_repository: {full_name: .head_repository.full_name},
          head_commit: {id: .head_commit.id}
        }
      ' "$raw" >"$output_dir/$name"
      ;;
    workflow.json)
      jq -eS '
        select(type == "object") | {id, name, path, state, url}
      ' "$raw" >"$output_dir/$name"
      ;;
    artifacts.json)
      jq -eS '
        select(type == "object") |
        {
          total_count,
          artifacts: ([.artifacts[] | {
            id, name, size_in_bytes, expired, digest, created_at, updated_at,
            workflow_run: {
              id: .workflow_run.id,
              head_branch: .workflow_run.head_branch,
              head_sha: .workflow_run.head_sha
            }
          }] | sort_by(.name))
        }
      ' "$raw" >"$output_dir/$name"
      ;;
    *)
      echo "unexpected public witness file: $name" >&2
      return 1
      ;;
  esac
  rm -f "$raw"
}

fetch_json "repos/$repository/releases/$release_id" release.json
fetch_json "repos/$repository/actions/runs/$run_id" run.json
workflow_id=$(jq -er '.workflow_id | select(type == "number" and . > 0)' "$output_dir/run.json")
fetch_json "repos/$repository/actions/workflows/$workflow_id" workflow.json
fetch_json "repos/$repository/actions/runs/$run_id/artifacts?per_page=100" artifacts.json

(
  cd "$output_dir"
  for name in artifacts.json release.json run.json workflow.json; do
    shasum -a 256 "$name"
  done >manifest.sha256
)

complete=1
trap - EXIT
