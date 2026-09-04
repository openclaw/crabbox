#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
: "${QUALIFICATION_CANDIDATE_URL:?missing candidate coordinator URL}"
: "${QUALIFICATION_ADMIN_TOKEN:?missing candidate admin token}"
: "${QUALIFICATION_SHARED_TOKEN:?missing candidate shared token}"
: "${QUALIFICATION_BASE_AMI_ID:?missing fixed base AMI}"
: "${QUALIFICATION_AWS_REGION:?missing fixed AWS region}"
: "${QUALIFICATION_ARTIFACT_DIR:?missing candidate artifact directory}"
: "${QUALIFICATION_PROOF_DIR:?missing proof directory}"

for name in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN CLOUDFLARE_API_TOKEN QUALIFICATION_CONTROLLER_TOKEN; do
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
admin_headers="$raw/admin.headers"
shared_headers="$raw/shared.headers"
printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$QUALIFICATION_ADMIN_TOKEN" >"$admin_headers"
printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$QUALIFICATION_SHARED_TOKEN" >"$shared_headers"
chmod 600 "$admin_headers" "$shared_headers"

sanitize() {
  node -e '
let text = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { text += chunk; });
process.stdin.on("end", () => {
  text = text
    .replaceAll(process.env.QUALIFICATION_ADMIN_TOKEN ?? "", "[admin-token]")
    .replaceAll(process.env.QUALIFICATION_SHARED_TOKEN ?? "", "[shared-token]")
    .replace(/https?:\/\/[^\s"<>]+/g, "[url]")
    .replace(/\b(?:ami|i|vol|snap|key)-[0-9a-f]{8,}\b/g, "[aws-resource]")
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[ip]");
  process.stdout.write(text.slice(0, 262144));
});'
}

catalog() {
  curl --fail --silent --show-error --max-time 30 \
    -H "@$admin_headers" \
    "$QUALIFICATION_CANDIDATE_URL/v1/images?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION"
}

probe_started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
before="$raw/catalog-before.json"
after="$raw/catalog-after.json"
catalog >"$before"
spoof_body="$raw/spoof.json"
printf '{"expected":{"state":"capture"},"image":{"target":"linux"}}\n' >"$spoof_body"
spoof_status=$(curl --silent --show-error --max-time 30 -o "$raw/spoof-response.json" \
  -w '%{http_code}' -X POST -H "@$shared_headers" --data-binary "@$spoof_body" \
  "$QUALIFICATION_CANDIDATE_URL/v1/images/$QUALIFICATION_BASE_AMI_ID/promote-cas?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
[[ "$spoof_status" == 403 ]]
catalog >"$after"
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
probe_completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '{"status":403,"catalogUnchanged":true,"startedAt":"%s","completedAt":"%s"}\n' \
  "$probe_started_at" "$probe_completed_at" >"$proof/spoofed-admin.json"
sleep 2

fsr_body="$raw/fsr.json"
printf '{"availabilityZones":["%sa"]}\n' "$QUALIFICATION_AWS_REGION" >"$fsr_body"
fsr_status=$(curl --silent --show-error --max-time 30 -o "$raw/fsr-response.json" \
  -w '%{http_code}' -X POST -H "@$admin_headers" --data-binary "@$fsr_body" \
  "$QUALIFICATION_CANDIDATE_URL/v1/images/$QUALIFICATION_BASE_AMI_ID/fast-snapshot-restore?provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION")
[[ "$fsr_status" -ge 400 ]]
printf '{"rejected":true,"httpStatusClass":%s}\n' "$((fsr_status / 100))" >"$proof/fsr-denial.json"

candidate_bin="$QUALIFICATION_ARTIFACT_DIR/bin/crabbox"
chmod 700 "$candidate_bin" "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/"*.sh
export CRABBOX_COORDINATOR="$QUALIFICATION_CANDIDATE_URL"
export CRABBOX_COORDINATOR_TOKEN="$QUALIFICATION_ADMIN_TOKEN"
export CRABBOX_COORDINATOR_ADMIN_TOKEN="$QUALIFICATION_ADMIN_TOKEN"
export CRABBOX_OWNER="image-qualification@example.invalid"
export CRABBOX_ORG="image-qualification"
export CRABBOX_ENV_ALLOW="CI"

"$candidate_bin" image promote "$QUALIFICATION_BASE_AMI_ID" \
  --provider aws --target linux --region "$QUALIFICATION_AWS_REGION" --type t3.small --json \
  >"$raw/seed.json" 2>"$raw/seed.err"

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
grep -q 'restored previous default image=' "$raw/mint.stderr"
grep -q -- '--retire-expected-catalog' "$raw/mint.stderr"
[[ "$(<"$QUALIFICATION_ADAPTER_STATE/launch-count")" -eq 3 ]]
[[ "$(cat "$raw/mint.stdout" "$raw/mint.stderr" | grep -c 'devtools-smoke-ok')" -ge 3 ]]

# The wrapper job expects status 137. No executor trap or workflow artifact is
# allowed to own cleanup; the protected finalizer must recover from the registry.
printf '{"executorKilled":true,"signal":"SIGKILL","cloudCredentialsPresent":false,"cleanupOwner":"protected-finalizer"}\n' \
  >"$proof/executor-hard-kill.json"

{
  printf '%s\n' 'qualification mint exited 86 only after the promoted smoke'
  printf '%s\n' 'candidate rollback restored the seeded prior default and retired the failed revision'
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
