#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
: "${QUALIFICATION_RELAY_URL:?missing qualification relay URL}"
: "${QUALIFICATION_EXECUTOR_TOKEN:?missing ephemeral executor token}"
: "${QUALIFICATION_BASE_AMI_ID:?missing fixed base AMI}"
: "${QUALIFICATION_AWS_REGION:?missing fixed AWS region}"
: "${QUALIFICATION_ARTIFACT_DIR:?missing candidate artifact directory}"
: "${QUALIFICATION_PROOF_DIR:?missing proof directory}"

for name in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN CLOUDFLARE_API_TOKEN \
  QUALIFICATION_CONTROLLER_TOKEN QUALIFICATION_ADMIN_TOKEN QUALIFICATION_SHARED_TOKEN; do
  if [[ -n "${!name+x}" ]]; then
    printf '%s must be absent from the credentialless executor\n' "$name" >&2
    exit 1
  fi
done

umask 077
proof="$QUALIFICATION_PROOF_DIR"
raw=$(mktemp -d "${RUNNER_TEMP:-/tmp}/image-qualification-execute.XXXXXX")
trap 'rm -rf "$raw"' EXIT
mkdir -p "$proof"
relay_headers="$raw/relay.headers"
printf 'Authorization: Bearer %s\n' "$QUALIFICATION_EXECUTOR_TOKEN" >"$relay_headers"
chmod 600 "$relay_headers"

sanitize() {
  node -e '
let text = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { text += chunk; });
process.stdin.on("end", () => {
  text = text
    .replaceAll(process.env.QUALIFICATION_EXECUTOR_TOKEN ?? "", "[executor-token]")
    .replace(/https?:\/\/[^\s"<>]+/g, "[url]")
    .replace(/\b(?:ami|i|vol|snap|key)-[0-9a-f]{8,}\b/g, "[aws-resource]")
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[ip]");
  process.stdout.write(text.slice(0, 262144));
});'
}

image_read() {
  local image_id=$1
  local output=$2
  curl --fail --silent --show-error --max-time 30 \
    -H "@$relay_headers" \
    -o "$output" \
    "$QUALIFICATION_RELAY_URL/v1/images/$image_id?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION"
}

before="$raw/base-before.json"
after="$raw/base-after.json"
image_read "$QUALIFICATION_BASE_AMI_ID" "$before"
spoof_body="$raw/spoof.json"
printf '{"expectedCurrent":{"state":"capture"}}\n' >"$spoof_body"
probe_started_at=$(node -e 'process.stdout.write(new Date().toISOString())')
spoof_status=$(curl --silent --show-error --max-time 30 -o "$raw/spoof-response.json" \
  -w '%{http_code}' -X POST -H "@$relay_headers" -H 'Content-Type: application/json' \
  --data-binary "@$spoof_body" \
  "$QUALIFICATION_RELAY_URL/qualification/shared/v1/images/$QUALIFICATION_BASE_AMI_ID/promote-cas?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
probe_completed_at=$(node -e 'process.stdout.write(new Date().toISOString())')
[[ "$spoof_status" == 403 ]]
image_read "$QUALIFICATION_BASE_AMI_ID" "$after"
node - "$before" "$after" <<'NODE'
const fs = require("node:fs");
const canonical = (value) =>
  Array.isArray(value)
    ? `[${value.map(canonical).join(",")}]`
    : value && typeof value === "object"
      ? `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`
      : JSON.stringify(value);
if (canonical(JSON.parse(fs.readFileSync(process.argv[2]))) !== canonical(JSON.parse(fs.readFileSync(process.argv[3])))) {
  throw new Error("shared-token admin probe mutated the image catalog");
}
NODE
printf '{"status":403,"catalogUnchanged":true,"startedAt":"%s","completedAt":"%s"}\n' \
  "$probe_started_at" "$probe_completed_at" >"$proof/spoofed-admin.json"
sleep 2

fsr_body="$raw/fsr.json"
printf '{"availabilityZones":["%sa"]}\n' "$QUALIFICATION_AWS_REGION" >"$fsr_body"
fsr_status=$(curl --silent --show-error --max-time 30 -o "$raw/fsr-response.json" \
  -w '%{http_code}' -X POST -H "@$relay_headers" -H 'Content-Type: application/json' \
  --data-binary "@$fsr_body" \
  "$QUALIFICATION_RELAY_URL/v1/images/$QUALIFICATION_BASE_AMI_ID/fast-snapshot-restore?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
[[ "$fsr_status" -ge 400 ]]
printf '{"rejected":true,"httpStatusClass":%s}\n' "$((fsr_status / 100))" >"$proof/fsr-denial.json"

candidate_bin="$QUALIFICATION_ARTIFACT_DIR/bin/crabbox"
chmod 700 "$candidate_bin" "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/"*.sh
export CRABBOX_COORDINATOR="$QUALIFICATION_RELAY_URL"
export CRABBOX_COORDINATOR_TOKEN="$QUALIFICATION_EXECUTOR_TOKEN"
export CRABBOX_COORDINATOR_ADMIN_TOKEN="$QUALIFICATION_EXECUTOR_TOKEN"
export CRABBOX_ENV_ALLOW="CI"

"$candidate_bin" image promote "$QUALIFICATION_BASE_AMI_ID" \
  --provider aws --target linux --region "$QUALIFICATION_AWS_REGION" --type t3.small --json \
  >"$raw/seed.json" 2>"$raw/seed.err"
image_read "$QUALIFICATION_BASE_AMI_ID" "$raw/seed-readback.json"

export QUALIFICATION_REAL_CRABBOX="$candidate_bin"
export QUALIFICATION_ADAPTER_STATE="$raw/adapter"
export CRABBOX_BIN="$ROOT/scripts/image-qualification-crabbox-adapter.sh"
export CRABBOX_IMAGE_RUN=1
export CRABBOX_IMAGE_PROMOTE=1
export CRABBOX_IMAGE_KEEP_LEASE=0
export CRABBOX_IMAGE_DESKTOP=0
export CRABBOX_IMAGE_BROWSER=0
export CRABBOX_IMAGE_TYPE=t3.small
export CRABBOX_IMAGE_REGION="$QUALIFICATION_AWS_REGION"
export CRABBOX_IMAGE_NAME="image-qualification-${GITHUB_RUN_ID:-local}"
export CRABBOX_IMAGE_LOG_DIR="$raw/mint"
export CRABBOX_IMAGE_TTL=90m
export CRABBOX_IMAGE_IDLE_TIMEOUT=20m
export CRABBOX_IMAGE_WAIT_TIMEOUT=45m
mkdir -p "$CRABBOX_IMAGE_LOG_DIR"

mint_status=0
"$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/mint-aws-devtools-image.sh" \
  --target linux --region "$QUALIFICATION_AWS_REGION" --type t3.small --no-desktop --no-browser --run \
  >"$raw/mint.stdout" 2>"$raw/mint.stderr" || mint_status=$?
[[ "$mint_status" -eq 86 ]]
[[ -f "$QUALIFICATION_ADAPTER_STATE/injected" ]]
[[ -f "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json" ]]
[[ -f "$QUALIFICATION_ADAPTER_STATE/rollback-receipt.json" ]]
[[ "$(<"$QUALIFICATION_ADAPTER_STATE/launch-count")" -eq 3 ]]
smoke_count=$(cat "$raw/mint.stdout" "$raw/mint.stderr" | grep -c 'devtools-smoke-ok')
[[ "$smoke_count" -ge 3 ]]

failed_image=$(node -e '
const receipt = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
if (!/^ami-[0-9a-f]+$/.test(receipt?.image?.id ?? "")) process.exit(1);
process.stdout.write(receipt.image.id);
' "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json")
image_read "$QUALIFICATION_BASE_AMI_ID" "$raw/restored-readback.json"
failed_status=$(curl --silent --show-error --max-time 30 \
  -H "@$relay_headers" -o "$raw/failed-readback.json" -w '%{http_code}' \
  "$QUALIFICATION_RELAY_URL/v1/images/$failed_image?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
[[ "$failed_status" == 200 || "$failed_status" == 404 ]]
node - "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json" "$raw/stale-cas-request.json" <<'NODE'
const fs = require("node:fs");
const promotion = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
if (!/^ami-[0-9a-f]+$/.test(promotion?.image?.id ?? "") || typeof promotion?.image?.revision !== "string") {
  throw new Error("promotion receipt does not contain an exact failed image revision");
}
fs.writeFileSync(
  process.argv[3],
  `${JSON.stringify({
    expectedCurrent: {
      state: "present",
      imageId: promotion.image.id,
      revision: promotion.image.revision,
    },
  })}\n`,
  { mode: 0o600 },
);
NODE
stale_status=$(curl --silent --show-error --max-time 30 -o "$raw/stale-cas-response.json" \
  -w '%{http_code}' -X POST -H "@$relay_headers" -H 'Content-Type: application/json' \
  --data-binary "@$raw/stale-cas-request.json" \
  "$QUALIFICATION_RELAY_URL/v1/images/$QUALIFICATION_BASE_AMI_ID/promote-cas?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
[[ "$stale_status" == 409 ]]
image_read "$QUALIFICATION_BASE_AMI_ID" "$raw/stale-readback.json"
stale_failed_status=$(curl --silent --show-error --max-time 30 \
  -H "@$relay_headers" -o "$raw/stale-failed-readback.json" -w '%{http_code}' \
  "$QUALIFICATION_RELAY_URL/v1/images/$failed_image?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
[[ "$stale_failed_status" == 200 || "$stale_failed_status" == 404 ]]
node "$ROOT/scripts/image-qualification-control.mjs" verify-catalog \
  "$raw/seed.json" \
  "$raw/seed-readback.json" \
  "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json" \
  "$QUALIFICATION_ADAPTER_STATE/rollback-receipt.json" \
  "$raw/restored-readback.json" \
  "$raw/failed-readback.json" \
  "$failed_status" \
  "$raw/stale-cas-response.json" \
  "$stale_status" \
  "$raw/stale-readback.json" \
  "$raw/stale-failed-readback.json" \
  "$stale_failed_status" \
  "$proof/catalog-rollback.json"
printf '{"mintExit":86,"injectedAfterPromotedSmoke":true,"launchCount":3,"smokeCount":%s}\n' \
  "$smoke_count" >"$proof/execution-state.json"

# The wrapper job expects status 137. No executor trap or workflow artifact is
# allowed to own cleanup; the protected finalizer must recover from the registry.
printf '{"executorKilled":true,"signal":"SIGKILL","cloudCredentialsPresent":false,"cleanupOwner":"protected-finalizer"}\n' \
  >"$proof/executor-hard-kill.json"

{
  printf '%s\n' 'qualification mint exited 86 only after the promoted smoke'
  printf '%s\n' 'structured candidate API readbacks are authoritative; this log is supplemental'
  printf '%s\n' 'launch order: source ami-base, candidate ami-created, promoted ami-created'
  cat "$raw/mint.stdout"
  cat "$raw/mint.stderr"
} | sanitize >"$proof/candidate-execution.log"

(
  cd "$proof"
  checksums=$(mktemp)
  find . -maxdepth 1 -type f -print0 |
    sort -z |
    xargs -0 sha256sum >"$checksums"
  mv "$checksums" checksums.sha256
)

rm -rf "$raw"
trap - EXIT
kill -KILL "$$"
exit 137
