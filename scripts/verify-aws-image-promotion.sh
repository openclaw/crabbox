#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
qualification_ref=""
repository=""
source_repository=""
source_commit=""
workflow_ref=""
workflow_run_id=""
workflow_run_attempt=""
aws_account_id=""
output=""

usage() {
  cat <<'USAGE'
Usage: scripts/verify-aws-image-promotion.sh \
  --qualification-ref ghcr.io/owner/repository@sha256:<64hex> \
  --repository ghcr.io/owner/repository \
  --source-repository owner/repository \
  --source-commit <40hex> \
  --workflow-ref owner/repository/.github/workflows/devtools-image-qualify.yml@refs/heads/main \
  --workflow-run-id <integer> \
  --workflow-run-attempt <integer> \
  --aws-account-id <12 digits> \
  --output promotion-evidence.json
USAGE
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --qualification-ref | --repository | --source-repository | --source-commit | --workflow-ref | --workflow-run-id | --workflow-run-attempt | --aws-account-id | --output)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      name="${1#--}"
      name="${name//-/_}"
      printf -v "$name" '%s' "$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ "$repository" =~ ^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$ ]] || {
  printf 'repository must be a lowercase GHCR repository without a tag or digest\n' >&2
  exit 2
}
[[ "$qualification_ref" =~ ^(ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+)@(sha256:[0-9a-f]{64})$ ]] || {
  printf 'qualification reference must be an immutable lowercase GHCR digest reference\n' >&2
  exit 2
}
[[ "${BASH_REMATCH[1]}" == "$repository" ]] || {
  printf 'qualification reference repository does not match --repository\n' >&2
  exit 2
}
qualification_digest="${BASH_REMATCH[3]}"
[[ -n "$source_repository" && -n "$source_commit" && -n "$workflow_ref" ]] || {
  printf 'source and workflow expectations are required\n' >&2
  exit 2
}
[[ -n "$workflow_run_id" && -n "$workflow_run_attempt" && -n "$aws_account_id" ]] || {
  printf 'workflow run and AWS account expectations are required\n' >&2
  exit 2
}
[[ -n "$output" ]] || { printf '%s\n' "--output is required" >&2; exit 2; }
command -v oras >/dev/null || { printf '%s\n' "oras is required" >&2; exit 2; }
command -v cosign >/dev/null || { printf '%s\n' "cosign is required" >&2; exit 2; }
command -v node >/dev/null || { printf '%s\n' "node is required" >&2; exit 2; }

work="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-aws-promotion-verify.XXXXXX")"
trap 'rm -rf "$work"' EXIT
resolved="$(oras resolve "$qualification_ref")"
[[ "$resolved" == "$qualification_digest" ]] || {
  printf 'ORAS resolved digest does not match qualification reference\n' >&2
  exit 1
}
oras manifest fetch "$qualification_ref" \
  --output "$work/manifest.raw.json" \
  --format json >"$work/manifest.json"
oras discover "$qualification_ref" --depth 1 --format json >"$work/referrers.json"

certificate_identity="https://github.com/$workflow_ref"
COSIGN_EXPERIMENTAL=1 cosign verify \
  --new-bundle-format \
  --experimental-oci11 \
  --certificate-identity "$certificate_identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$qualification_ref" >/dev/null

mkdir "$work/bundle"
oras pull "$qualification_ref" --output "$work/bundle" >/dev/null
node "$ROOT/scripts/aws-image-qualification.mjs" verify \
  --file "$work/bundle/qualification.json" >/dev/null
node "$ROOT/scripts/consume-aws-image-candidate.mjs" \
  validate-qualification-input \
  --file "$work/bundle/qualification-input.json"

temporary_output="$work/promotion-evidence.json"
node "$ROOT/scripts/aws-image-promotion-evidence.mjs" verify \
  --qualification-ref "$qualification_ref" \
  --manifest-raw "$work/manifest.raw.json" \
  --referrers "$work/referrers.json" \
  --qualification "$work/bundle/qualification.json" \
  --qualification-input "$work/bundle/qualification-input.json" \
  --source-repository "$source_repository" \
  --source-commit "$source_commit" \
  --workflow-ref "$workflow_ref" \
  --workflow-run-id "$workflow_run_id" \
  --workflow-run-attempt "$workflow_run_attempt" \
  --aws-account-id "$aws_account_id" >"$temporary_output"
[[ ! -e "$output" ]] || { printf 'promotion evidence output already exists: %s\n' "$output" >&2; exit 1; }
mv "$temporary_output" "$output"
cat "$output"
