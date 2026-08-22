#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
receipt=""
qualification_input=""
repository="${CRABBOX_IMAGE_QUALIFICATION_REPOSITORY:-}"
certificate_identity="${CRABBOX_IMAGE_QUALIFICATION_CERTIFICATE_IDENTITY:-}"

usage() {
  cat <<'USAGE'
Usage: scripts/publish-aws-image-qualification.sh \
  --receipt qualification.json \
  --qualification-input qualification-input.json \
  --repository ghcr.io/owner/repository \
  --certificate-identity https://github.com/owner/repository/.github/workflows/devtools-image-qualify.yml@refs/heads/main
USAGE
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --receipt | --qualification-input | --repository | --certificate-identity)
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

[[ -f "$receipt" ]] || { printf '%s\n' "--receipt must name a file" >&2; exit 2; }
[[ -f "$qualification_input" ]] || {
  printf '%s\n' "--qualification-input must name a file" >&2
  exit 2
}
[[ "$repository" =~ ^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$ ]] || {
  printf 'repository must be a lowercase GHCR repository without a tag\n' >&2
  exit 2
}
[[ "$certificate_identity" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/devtools-image-qualify\.yml@refs/heads/[A-Za-z0-9._/-]+$ ]] || {
  printf 'certificate identity must bind the protected qualification workflow\n' >&2
  exit 2
}
command -v oras >/dev/null || { printf '%s\n' "oras is required" >&2; exit 2; }
command -v cosign >/dev/null || { printf '%s\n' "cosign is required" >&2; exit 2; }
command -v go >/dev/null || { printf '%s\n' "go is required" >&2; exit 2; }

work="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-aws-qualification-publish.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cp "$receipt" "$work/qualification.json"
cp "$qualification_input" "$work/qualification-input.json"
chmod 600 "$work/qualification.json" "$work/qualification-input.json"
receipt="$work/qualification.json"
qualification_input="$work/qualification-input.json"
verification="$(node "$ROOT/scripts/aws-image-qualification.mjs" verify --file "$receipt")"
(
  cd "$ROOT"
  go run ./scripts/validate-json-schema \
    recipes/aws/v1/qualification-input.schema.json \
    "$qualification_input"
)
node "$ROOT/scripts/consume-aws-image-candidate.mjs" \
  validate-qualification-input --file "$qualification_input"
node -e '
const crypto = require("crypto");
const fs = require("fs");
const [receiptFile, inputFile] = process.argv.slice(1);
const receipt = JSON.parse(fs.readFileSync(receiptFile, "utf8"));
const input = fs.readFileSync(inputFile);
const actual = `sha256:${crypto.createHash("sha256").update(input).digest("hex")}`;
if (receipt.candidate?.qualificationInputDigest !== actual) process.exit(1);
' "$receipt" "$qualification_input" || {
  printf 'qualification input digest does not match receipt\n' >&2
  exit 1
}
receipt_digest="$(
  node -e '
const value = JSON.parse(process.argv[1]);
if (!/^sha256:[0-9a-f]{64}$/.test(value.receiptDigest ?? "")) process.exit(1);
process.stdout.write(value.receiptDigest);
' "$verification"
)"
tag="sha256-${receipt_digest#sha256:}"
tagged_ref="$repository:$tag"
set +e
oras manifest fetch "$tagged_ref" >/dev/null 2>"$work/tag-check.err"
tag_check_status=$?
set -e
if [[ "$tag_check_status" -eq 0 ]]; then
  printf 'refusing to overwrite existing qualification tag: %s\n' "$tagged_ref" >&2
  exit 1
fi
if ! grep -Eiq 'manifest unknown|not found|(^|[^0-9])404([^0-9]|$)' "$work/tag-check.err"; then
  printf 'could not prove qualification tag absence\n' >&2
  exit 1
fi

push_output="$(
  cd "$work"
  oras push "$tagged_ref" \
    --artifact-type application/vnd.crabbox.aws-image-qualification.v1 \
    "qualification.json:application/vnd.crabbox.aws-image-qualification.v1+json" \
    "qualification-input.json:application/vnd.crabbox.aws-image-qualification-input.v1+json" \
    --format json
)"
oci_digest="$(
  node -e '
const value = JSON.parse(process.argv[1]);
const digest = value.digest ?? value.descriptor?.digest;
if (!/^sha256:[0-9a-f]{64}$/.test(digest ?? "")) process.exit(1);
process.stdout.write(digest);
' "$push_output"
)"
[[ "$oci_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  printf 'oras push did not return an immutable OCI digest\n' >&2
  exit 1
}
immutable_ref="$repository@$oci_digest"
oras manifest fetch "$immutable_ref" \
  --output "$work/manifest.raw.json" \
  --format json >"$work/manifest.json"
node -e '
const crypto = require("crypto");
const fs = require("fs");
const [manifestFile, receiptFile, inputFile, expectedDigest] = process.argv.slice(1);
const raw = fs.readFileSync(manifestFile);
const manifest = JSON.parse(raw);
const receipt = fs.readFileSync(receiptFile);
const input = fs.readFileSync(inputFile);
const digest = (bytes) => `sha256:${crypto.createHash("sha256").update(bytes).digest("hex")}`;
if (digest(raw) !== expectedDigest) process.exit(1);
if (
  manifest.artifactType !== "application/vnd.crabbox.aws-image-qualification.v1" ||
  !Array.isArray(manifest.layers) ||
  manifest.layers.length !== 2 ||
  manifest.layers[0].mediaType !== "application/vnd.crabbox.aws-image-qualification.v1+json" ||
  manifest.layers[0].digest !== digest(receipt) ||
  manifest.layers[0].size !== receipt.length ||
  manifest.layers[1].mediaType !== "application/vnd.crabbox.aws-image-qualification-input.v1+json" ||
  manifest.layers[1].digest !== digest(input) ||
  manifest.layers[1].size !== input.length
) process.exit(1);
' "$work/manifest.raw.json" "$receipt" "$qualification_input" "$oci_digest" || {
  printf 'pushed OCI manifest does not bind the qualification evidence\n' >&2
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
  schema: "crabbox-aws-image-qualification-publication/v1",
  repository,
  tag,
  receiptDigest,
  ociDigest,
  immutableRef: `${repository}@${ociDigest}`,
})}\n`);
' "$repository" "$tag" "$receipt_digest" "$oci_digest"
