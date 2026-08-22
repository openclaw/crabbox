#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
attempt=""
repository="${CRABBOX_IMAGE_PROMOTION_RECEIPT_REPOSITORY:-}"
certificate_identity="${CRABBOX_IMAGE_PROMOTION_CERTIFICATE_IDENTITY:-}"

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --attempt | --repository | --certificate-identity)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      name="${1#--}"
      name="${name//-/_}"
      printf -v "$name" '%s' "$2"
      shift 2
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

[[ -f "$attempt" ]] || { printf '%s\n' "--attempt must name a file" >&2; exit 2; }
[[ "$repository" =~ ^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$ ]] || {
  printf 'repository must be a lowercase GHCR repository without a tag\n' >&2
  exit 2
}
[[ "$certificate_identity" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/devtools-image-promote\.yml@refs/heads/[A-Za-z0-9._/-]+$ ]] || {
  printf 'certificate identity must bind the protected promotion workflow\n' >&2
  exit 2
}
command -v oras >/dev/null || { printf '%s\n' "oras is required" >&2; exit 2; }
command -v cosign >/dev/null || { printf '%s\n' "cosign is required" >&2; exit 2; }
command -v go >/dev/null || { printf '%s\n' "go is required" >&2; exit 2; }

work="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-aws-promotion-receipt.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cp "$attempt" "$work/attempt.json"
chmod 600 "$work/attempt.json"
node "$ROOT/scripts/aws-image-promotion-receipt.mjs" build \
  --attempt "$work/attempt.json" \
  --output "$work/promotion-receipt.json" >/dev/null
(
  cd "$ROOT"
  go run ./scripts/validate-json-schema \
    recipes/aws/v1/promotion-receipt.schema.json \
    "$work/promotion-receipt.json"
)
verification="$(node "$ROOT/scripts/aws-image-promotion-receipt.mjs" verify --file "$work/promotion-receipt.json")"
receipt_digest="$(node -e '
const value = JSON.parse(process.argv[1]);
if (!/^sha256:[0-9a-f]{64}$/.test(value.receiptDigest ?? "")) process.exit(1);
process.stdout.write(value.receiptDigest);
' "$verification")"
tag="sha256-${receipt_digest#sha256:}"
tagged_ref="$repository:$tag"
set +e
oras manifest fetch "$tagged_ref" >/dev/null 2>"$work/tag-check.err"
tag_status=$?
set -e
if [[ "$tag_status" -eq 0 ]]; then
  printf 'refusing to overwrite existing promotion receipt tag: %s\n' "$tagged_ref" >&2
  exit 1
fi
grep -Eiq 'manifest unknown|not found|(^|[^0-9])404([^0-9]|$)' "$work/tag-check.err" || {
  printf 'could not prove promotion receipt tag absence\n' >&2
  exit 1
}

push_output="$(
  cd "$work"
  oras push "$tagged_ref" \
    --artifact-type application/vnd.crabbox.aws-image-promotion-receipt.v1 \
    "promotion-receipt.json:application/vnd.crabbox.aws-image-promotion-receipt.v1+json" \
    --format json
)"
oci_digest="$(node -e '
const value = JSON.parse(process.argv[1]);
const digest = value.digest ?? value.descriptor?.digest;
if (!/^sha256:[0-9a-f]{64}$/.test(digest ?? "")) process.exit(1);
process.stdout.write(digest);
' "$push_output")"
immutable_ref="$repository@$oci_digest"
oras manifest fetch "$immutable_ref" \
  --output "$work/manifest.raw.json" \
  --format json >"$work/manifest.json"
node -e '
const crypto = require("crypto");
const fs = require("fs");
const [manifestFile, receiptFile, expectedDigest] = process.argv.slice(1);
const raw = fs.readFileSync(manifestFile);
const receipt = fs.readFileSync(receiptFile);
const digest = (bytes) => `sha256:${crypto.createHash("sha256").update(bytes).digest("hex")}`;
const manifest = JSON.parse(raw);
if (
  digest(raw) !== expectedDigest ||
  manifest.artifactType !== "application/vnd.crabbox.aws-image-promotion-receipt.v1" ||
  !Array.isArray(manifest.layers) ||
  manifest.layers.length !== 1 ||
  manifest.layers[0].mediaType !==
    "application/vnd.crabbox.aws-image-promotion-receipt.v1+json" ||
  manifest.layers[0].digest !== digest(receipt) ||
  manifest.layers[0].size !== receipt.length
) process.exit(1);
' "$work/manifest.raw.json" "$work/promotion-receipt.json" "$oci_digest" || {
  printf 'pushed OCI manifest does not bind the promotion receipt\n' >&2
  exit 1
}
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
const [repository, tag, receiptDigest, ociDigest] = process.argv.slice(1);
process.stdout.write(`${JSON.stringify({
  schema: "crabbox-aws-image-promotion-receipt-publication/v1",
  repository,
  tag,
  receiptDigest,
  ociDigest,
  immutableRef: `${repository}@${ociDigest}`,
})}\n`);
' "$repository" "$tag" "$receipt_digest" "$oci_digest"
