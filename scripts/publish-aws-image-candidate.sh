#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bundle_dir=""
repository="${CRABBOX_IMAGE_CANDIDATE_REPOSITORY:-}"
certificate_identity="${CRABBOX_IMAGE_CANDIDATE_CERTIFICATE_IDENTITY:-}"

usage() {
  cat <<'USAGE'
Usage: scripts/publish-aws-image-candidate.sh --bundle DIR --repository ghcr.io/owner/name

Publish one immutable AWS image-candidate evidence bundle and keylessly sign its
resolved OCI digest. The provider AMI and EBS snapshot bytes remain in AWS.
USAGE
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --bundle)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      bundle_dir="$2"
      shift 2
      ;;
    --repository)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      repository="$2"
      shift 2
      ;;
    --certificate-identity)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      certificate_identity="$2"
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

[[ -n "$bundle_dir" ]] || { printf '%s\n' "--bundle is required" >&2; exit 2; }
[[ "$repository" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+$ ]] || {
  printf 'repository must be a lowercase GHCR repository without a tag\n' >&2
  exit 2
}
command -v oras >/dev/null || { printf '%s\n' "oras is required" >&2; exit 2; }
command -v cosign >/dev/null || { printf '%s\n' "cosign is required" >&2; exit 2; }

verification="$(node "$ROOT/scripts/aws-image-candidate.mjs" verify --dir "$bundle_dir")"
readarray -t verified < <(node -e '
const value = JSON.parse(process.argv[1]);
if (
  !/^sha256:[0-9a-f]{64}$/.test(value.candidateDigest ?? "") ||
  !/^ghcr[.]io\/[a-z0-9_.-]+(?:\/[a-z0-9_.-]+)+$/.test(value.ociRepository ?? "") ||
  !value.certificateIdentity?.startsWith("https://github.com/")
) process.exit(1);
process.stdout.write(`${value.candidateDigest}\n${value.ociRepository}\n${value.certificateIdentity}\n`);
' "$verification")
candidate_digest="${verified[0]}"
bundle_repository="${verified[1]}"
bundle_identity="${verified[2]}"
[[ "$repository" == "$bundle_repository" ]] || {
  printf 'repository does not match immutable bundle contract\n' >&2
  exit 2
}
if [[ -z "$certificate_identity" ]]; then
  certificate_identity="$bundle_identity"
fi
[[ "$certificate_identity" == "$bundle_identity" ]] || {
  printf 'certificate identity does not match immutable bundle contract\n' >&2
  exit 2
}
tag="sha256-${candidate_digest#sha256:}"
tagged_ref="$repository:$tag"

if oras manifest fetch "$tagged_ref" >/dev/null 2>&1; then
  printf 'refusing to overwrite existing candidate tag: %s\n' "$tagged_ref" >&2
  exit 1
fi

oras push "$tagged_ref" \
  --artifact-type application/vnd.crabbox.aws-image-evidence.v1 \
  "$bundle_dir/bundle.json:application/vnd.crabbox.aws-image-evidence-manifest.v1+json" \
  "$bundle_dir/candidate.json:application/vnd.crabbox.aws-image-candidate.v1+json" \
  "$bundle_dir/recipe.json:application/json" \
  "$bundle_dir/sbom.spdx.json:application/spdx+json" \
  "$bundle_dir/provenance.intoto.jsonl:application/vnd.in-toto+json" \
  "$bundle_dir/scrub-report.json:application/json" \
  >/dev/null

oci_digest="$(oras resolve "$tagged_ref")"
[[ "$oci_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  printf 'oras did not resolve an immutable OCI digest\n' >&2
  exit 1
}
immutable_ref="$repository@$oci_digest"
COSIGN_EXPERIMENTAL=1 cosign sign \
  --new-bundle-format \
  --registry-referrers-mode oci-1-1 \
  --yes \
  "$immutable_ref" >/dev/null

COSIGN_EXPERIMENTAL=1 cosign verify \
  --new-bundle-format \
  --experimental-oci11 \
  --certificate-identity "$certificate_identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$immutable_ref" >/dev/null

node -e '
const [repository, tag, candidateDigest, ociDigest] = process.argv.slice(1);
process.stdout.write(`${JSON.stringify({
  schema: "crabbox-aws-image-candidate-publication/v1",
  repository,
  tag,
  candidateDigest,
  ociDigest,
  immutableRef: `${repository}@${ociDigest}`,
})}\n`);
' "$repository" "$tag" "$candidate_digest" "$oci_digest"
