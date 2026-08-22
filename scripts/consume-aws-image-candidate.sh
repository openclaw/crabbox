#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
candidate_ref=""
repository=""
output=""
declare -a verifier_args=()

usage() {
  cat <<'USAGE'
Usage: scripts/consume-aws-image-candidate.sh \
  --candidate-ref ghcr.io/owner/repository@sha256:<64hex> \
  --repository ghcr.io/owner/repository \
  --source-repository owner/repository \
  --source-commit <40hex> \
  --workflow-ref owner/repository/.github/workflows/name.yml@refs/heads/branch \
  --workflow-run-id <integer> \
  --workflow-run-attempt <integer> \
  --target linux|windows \
  --region us-west-2 \
  --instance-type m7i.large \
  --architecture x86_64|arm64 \
  --base-image ami-... \
  --ami-id ami-... \
  --profile linux-builder \
  --image-recipe recipes/aws/v1/linux-devtools.json \
  --readiness-recipe recipes/linux/v1/linux-builder.json \
  --output qualification-input.json

Verify and consume one immutable AWS image-candidate evidence artifact. This
command reads registry evidence only; it does not boot, promote, publish, or
change an AWS image or Crabbox coordinator.
USAGE
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --candidate-ref)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      candidate_ref="$2"
      shift 2
      ;;
    --repository)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      repository="$2"
      verifier_args+=("$1" "$2")
      shift 2
      ;;
    --output)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      output="$2"
      shift 2
      ;;
    --source-repository | --source-commit | --workflow-ref | --workflow-run-id | --workflow-run-attempt | --target | --region | --instance-type | --architecture | --base-image | --ami-id | --profile | --image-recipe | --readiness-recipe)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      verifier_args+=("$1" "$2")
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

[[ -n "$candidate_ref" ]] || { printf '%s\n' "--candidate-ref is required" >&2; exit 2; }
[[ -n "$repository" ]] || { printf '%s\n' "--repository is required" >&2; exit 2; }
[[ -n "$output" ]] || { printf '%s\n' "--output is required" >&2; exit 2; }
[[ "$repository" =~ ^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$ ]] || {
  printf 'repository must be a lowercase GHCR repository without a tag or digest\n' >&2
  exit 2
}
[[ "$candidate_ref" =~ ^(ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+)@(sha256:[0-9a-f]{64})$ ]] || {
  printf 'candidate reference must be an immutable lowercase GHCR digest reference\n' >&2
  exit 2
}
[[ "${BASH_REMATCH[1]}" == "$repository" ]] || {
  printf 'candidate reference repository does not match --repository\n' >&2
  exit 2
}
candidate_digest="${BASH_REMATCH[3]}"

command -v oras >/dev/null || { printf '%s\n' "oras is required" >&2; exit 2; }
command -v cosign >/dev/null || { printf '%s\n' "cosign is required" >&2; exit 2; }
command -v node >/dev/null || { printf '%s\n' "node is required" >&2; exit 2; }

workflow_ref=""
for ((index = 0; index < ${#verifier_args[@]}; index += 2)); do
  if [[ "${verifier_args[index]}" == "--workflow-ref" ]]; then
    workflow_ref="${verifier_args[index + 1]}"
    break
  fi
done
[[ -n "$workflow_ref" ]] || { printf '%s\n' "--workflow-ref is required" >&2; exit 2; }

node "$ROOT/scripts/consume-aws-image-candidate.mjs" preflight \
  --candidate-ref "$candidate_ref" \
  "${verifier_args[@]}"

work="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-aws-image-consume.XXXXXX")"
cleanup() {
  rm -rf "$work"
}
trap cleanup EXIT

resolved="$(oras resolve "$candidate_ref")"
[[ "$resolved" == "$candidate_digest" ]] || {
  printf 'ORAS resolved digest does not match candidate reference\n' >&2
  exit 1
}

oras manifest fetch "$candidate_ref" \
  --output "$work/manifest.raw.json" \
  --format json >"$work/manifest.json"

oras discover "$candidate_ref" \
  --depth 1 \
  --format json >"$work/referrers.json"

signature_ref="$(
  node "$ROOT/scripts/consume-aws-image-candidate.mjs" inspect-signature \
    --candidate-ref "$candidate_ref" \
    --manifest "$work/manifest.json" \
    --manifest-raw "$work/manifest.raw.json" \
    --referrers "$work/referrers.json"
)"

oras manifest fetch "$signature_ref" \
  --output "$work/signature-manifest.raw.json" \
  --format json >"$work/signature-manifest.json"

signature_bundle_ref="$(
  node "$ROOT/scripts/consume-aws-image-candidate.mjs" inspect-signature-manifest \
    --candidate-ref "$candidate_ref" \
    --manifest "$work/manifest.json" \
    --manifest-raw "$work/manifest.raw.json" \
    --referrers "$work/referrers.json" \
    --signature-manifest "$work/signature-manifest.json" \
    --signature-manifest-raw "$work/signature-manifest.raw.json"
)"

oras blob fetch \
  --output "$work/signature.bundle.json" \
  "$signature_bundle_ref" >/dev/null

node "$ROOT/scripts/consume-aws-image-candidate.mjs" verify-signature-evidence \
  --candidate-ref "$candidate_ref" \
  --manifest "$work/manifest.json" \
  --manifest-raw "$work/manifest.raw.json" \
  --referrers "$work/referrers.json" \
  --signature-manifest "$work/signature-manifest.json" \
  --signature-manifest-raw "$work/signature-manifest.raw.json" \
  --signature-bundle "$work/signature.bundle.json"

certificate_identity="https://github.com/$workflow_ref"
certificate_issuer="https://token.actions.githubusercontent.com"

cosign verify-blob \
  --new-bundle-format \
  --bundle "$work/signature.bundle.json" \
  --certificate-identity "$certificate_identity" \
  --certificate-oidc-issuer "$certificate_issuer" \
  "$candidate_digest" >/dev/null

mkdir "$work/bundle"
oras pull "$candidate_ref" --output "$work/bundle" >/dev/null

node "$ROOT/scripts/aws-image-candidate.mjs" verify \
  --dir "$work/bundle" >"$work/bundle-verification.json"

node "$ROOT/scripts/consume-aws-image-candidate.mjs" verify \
  --candidate-ref "$candidate_ref" \
  --manifest "$work/manifest.json" \
  --manifest-raw "$work/manifest.raw.json" \
  --referrers "$work/referrers.json" \
  --signature-manifest "$work/signature-manifest.json" \
  --signature-manifest-raw "$work/signature-manifest.raw.json" \
  --signature-bundle "$work/signature.bundle.json" \
  --bundle "$work/bundle" \
  --bundle-verification "$work/bundle-verification.json" \
  --output "$output" \
  "${verifier_args[@]}"
