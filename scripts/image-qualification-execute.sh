#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
: "${QUALIFICATION_RELAY_URL:?missing qualification relay URL}"
: "${QUALIFICATION_EXECUTOR_TOKEN:?missing ephemeral executor token}"
: "${QUALIFICATION_BASE_AMI_ID:?missing fixed base AMI}"
: "${QUALIFICATION_AWS_REGION:?missing fixed AWS region}"
: "${QUALIFICATION_ARTIFACT_DIR:?missing candidate artifact directory}"
: "${QUALIFICATION_PROOF_DIR:?missing proof directory}"

if [[ "${QUALIFICATION_MODE:-mint}" == retained && "${QUALIFICATION_BOUNDED_CHILD:-0}" != 1 ]]; then
  remaining=$(node -e '
const seconds = Math.floor((Date.parse(process.env.QUALIFICATION_EXPIRES_AT) - Date.now()) / 1000) - 480;
if (!Number.isInteger(seconds) || seconds <= 0 || seconds > 1800) process.exit(1);
process.stdout.write(String(seconds));
')
  export QUALIFICATION_BOUNDED_CHILD=1
  exec timeout --signal=TERM --kill-after=480 "$remaining" "$0"
fi

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
retained_lease() {
  node -e '
const fs = require("node:fs");
if (!fs.existsSync(process.argv[1])) process.exit(1);
const rows = fs.readFileSync(process.argv[1], "utf8").trim().split("\n").reverse();
for (const row of rows) {
  let value;
  try { value = JSON.parse(row); } catch { continue; }
  if (/^cbx_[A-Za-z0-9_-]+$/.test(value.leaseId ?? "")) {
    process.stdout.write(value.leaseId);
    process.exit(0);
  }
}
process.exit(1);
' "$raw/retained-warmup.log"
}
release_retained_lease() {
  local lease
  if [[ "${QUALIFICATION_MODE:-mint}" == retained && ! -f "$raw/retained-released" ]]; then
    lease=$(retained_lease) || return 0
    "$CRABBOX_BIN" stop --provider aws --target linux "$lease" || return 1
    touch "$raw/retained-released"
  fi
}
executor_cleanup() {
  local status=$?
  trap - EXIT
  release_retained_lease || status=1
  rm -rf "$raw"
  exit "$status"
}
trap executor_cleanup EXIT
mkdir -p "$proof"
relay_headers="$raw/relay.headers"
printf 'Authorization: Bearer %s\n' "$QUALIFICATION_EXECUTOR_TOKEN" >"$relay_headers"
chmod 600 "$relay_headers"
scope_query="provider=aws&target=linux&region=$QUALIFICATION_AWS_REGION"
scope_args=(--provider aws --target linux --region "$QUALIFICATION_AWS_REGION" --type t3.small)
expected_launches=3
if [[ "${QUALIFICATION_MODE:-mint}" == retained ]]; then
  scope_query+="&serverType=t3.small&architecture=x86_64&os=ubuntu%3A24.04"
  scope_args+=(--architecture x86_64 --os ubuntu:24.04)
  expected_launches=1
fi

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
    "$QUALIFICATION_RELAY_URL/v1/images/$image_id?$scope_query"
}

assert_catalog_unchanged() {
  node -e '
const fs = require("node:fs");
const canonical = (value) =>
  Array.isArray(value)
    ? `[${value.map(canonical).join(",")}]`
    : value && typeof value === "object"
      ? `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`
      : JSON.stringify(value);
if (canonical(JSON.parse(fs.readFileSync(process.argv[1]))) !== canonical(JSON.parse(fs.readFileSync(process.argv[2])))) {
  throw new Error("denied probe mutated the image catalog");
}
' "$1" "$2"
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
  "$QUALIFICATION_RELAY_URL/qualification/shared/v1/images/$QUALIFICATION_BASE_AMI_ID/promote-cas?$scope_query")
probe_completed_at=$(node -e 'process.stdout.write(new Date().toISOString())')
[[ "$spoof_status" == 403 ]]
image_read "$QUALIFICATION_BASE_AMI_ID" "$after"
assert_catalog_unchanged "$before" "$after"
printf '{"status":403,"catalogUnchanged":true,"startedAt":"%s","completedAt":"%s"}\n' \
  "$probe_started_at" "$probe_completed_at" >"$proof/spoofed-admin.json"
sleep 2

fsr_body="$raw/fsr.json"
printf '{"fastSnapshotRestore":true,"fastSnapshotRestoreAvailabilityZones":["%sa"]}\n' \
  "$QUALIFICATION_AWS_REGION" >"$fsr_body"
fsr_status=$(curl --silent --show-error --max-time 30 -o "$raw/fsr-response.json" \
  -w '%{http_code}' -X POST -H "@$relay_headers" -H 'Content-Type: application/json' \
  --data-binary "@$fsr_body" \
  "$QUALIFICATION_RELAY_URL/v1/images/$QUALIFICATION_BASE_AMI_ID/promote?$scope_query")
[[ "$fsr_status" -ge 400 ]]
image_read "$QUALIFICATION_BASE_AMI_ID" "$raw/fsr-readback.json"
assert_catalog_unchanged "$after" "$raw/fsr-readback.json"
printf '{"rejected":true,"httpStatusClass":%s}\n' "$((fsr_status / 100))" >"$proof/fsr-denial.json"

candidate_bin="$QUALIFICATION_ARTIFACT_DIR/bin/crabbox"
chmod 700 "$candidate_bin" "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/"*.sh
export CRABBOX_COORDINATOR="$QUALIFICATION_RELAY_URL"
export CRABBOX_COORDINATOR_TOKEN="$QUALIFICATION_EXECUTOR_TOKEN"
export CRABBOX_COORDINATOR_ADMIN_TOKEN="$QUALIFICATION_EXECUTOR_TOKEN"
export CRABBOX_ENV_ALLOW="CI"

"$candidate_bin" image promote "$QUALIFICATION_BASE_AMI_ID" \
  "${scope_args[@]}" --json \
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

retained_qualification() (
  set -euo pipefail
  unset CRABBOX_AWS_AMI
  export CRABBOX_AWS_REGION="$QUALIFICATION_AWS_REGION" CRABBOX_AWS_ROOT_GB=400
  local lease="" receipt="$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json"
  retained_cleanup() {
    local status=$?
    local receipt="$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json"
    trap - EXIT
    # Restore first; the parent verifies retirement and stale CAS before release.
    # The protected authority remains the cleanup owner on process loss.
    if [[ -s "$receipt" ]]; then
      "$CRABBOX_BIN" image promote "${scope_args[@]}" --json \
        --restore-receipt "$receipt" "$QUALIFICATION_RETAINED_IMAGE_ID" || status=1
    fi
    exit "$status"
  }
  trap retained_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "$CRABBOX_BIN" image promote "$QUALIFICATION_RETAINED_IMAGE_ID" "${scope_args[@]}" \
    --json --expected-current-image capture
  "$CRABBOX_BIN" warmup --provider aws --target linux --os ubuntu:24.04 --arch x86_64 \
    --type t3.small --class standard --market on-demand --ttl 30m --idle-timeout 20m \
    --desktop --browser --timing-json >"$raw/retained-warmup.log" 2>&1
  lease=$(retained_lease)
  curl --fail --silent --show-error --max-time 30 -H "@$relay_headers" \
    "$QUALIFICATION_RELAY_URL/v1/leases/$lease?providerMetadata=authoritative" >"$raw/selection.json"
  node "$ROOT/scripts/image-qualification-control.mjs" retained-selection \
    "$raw/selection.json" "$receipt" "$raw/selection-proof.json"
  "$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync \
    --script "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/linux-readiness.generated.sh" \
    -- --verify linux-builder
  local archive_probe smoke_value
  archive_probe=$(bash -c '
source "$1"
node_pnpm_smoke_script
go_smoke_script
bun_smoke_script
rust_smoke_script
uv_smoke_script
' _ "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/install-linux-developer-tools.sh")
  printf -v smoke_value 'set -euo pipefail\nexport CRABBOX_LINUX_DESKTOP_TOOLS=1 CRABBOX_LINUX_BROWSER=1\nexpected_node_major=24\nexpected_pnpm_version=11.1.0\ndeveloper_archive_probe() {\n%s\n}\n%s' \
    "$archive_probe" "$(cat "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/devtools-image-smoke-linux.sh")"
  "$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync --shell -- "$smoke_value"
)

mint_status=0
if [[ "${QUALIFICATION_MODE:-mint}" == retained ]]; then
  set +e
  retained_qualification >"$raw/mint.stdout" 2>"$raw/mint.stderr"
  mint_status=$?
  set -e
else
  "$QUALIFICATION_ARTIFACT_DIR/candidate/scripts/mint-aws-devtools-image.sh" \
    --target linux --region "$QUALIFICATION_AWS_REGION" --type t3.small --no-desktop --no-browser --run \
    >"$raw/mint.stdout" 2>"$raw/mint.stderr" || mint_status=$?
fi
if [[ "$mint_status" -ne 86 ]]; then
  printf 'qualification execution failed before successful full-smoke injection (exit %s)\n' "$mint_status" >&2
  exit 1
fi
[[ -f "$QUALIFICATION_ADAPTER_STATE/injected" ]]
[[ -f "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json" ]]
[[ -f "$QUALIFICATION_ADAPTER_STATE/rollback-receipt.json" ]]
[[ "$(<"$QUALIFICATION_ADAPTER_STATE/launch-count")" -eq "$expected_launches" ]]
smoke_count=$(cat "$raw/mint.stdout" "$raw/mint.stderr" | grep -c 'devtools-smoke-ok')
[[ "$smoke_count" -ge "$expected_launches" ]]

failed_image=$(node -e '
const receipt = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
if (!/^ami-[0-9a-f]+$/.test(receipt?.image?.id ?? "")) process.exit(1);
process.stdout.write(receipt.image.id);
' "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json")
image_read "$QUALIFICATION_BASE_AMI_ID" "$raw/restored-readback.json"
failed_status=$(curl --silent --show-error --max-time 30 \
  -H "@$relay_headers" -o "$raw/failed-readback.json" -w '%{http_code}' \
  "$QUALIFICATION_RELAY_URL/v1/images/$failed_image?$scope_query")
[[ "$failed_status" == 200 || "$failed_status" == 404 ]]
# Keep inline programs out of heredoc pipes, which can block Bash before the
# consumer starts. The same node -e form is used by the earlier readback checks.
node -e '
const fs = require("node:fs");
const promotion = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
if (!/^ami-[0-9a-f]+$/.test(promotion?.image?.id ?? "") || typeof promotion?.image?.revision !== "string") {
  throw new Error("promotion receipt does not contain an exact failed image revision");
}
fs.writeFileSync(
  process.argv[2],
  `${JSON.stringify({
    expectedCurrent: {
      state: "present",
      imageId: promotion.image.id,
      revision: promotion.image.revision,
    },
  })}\n`,
  { mode: 0o600 },
);
' "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json" "$raw/stale-cas-request.json"
stale_status=$(curl --silent --show-error --max-time 30 -o "$raw/stale-cas-response.json" \
  -w '%{http_code}' -X POST -H "@$relay_headers" -H 'Content-Type: application/json' \
  --data-binary "@$raw/stale-cas-request.json" \
  "$QUALIFICATION_RELAY_URL/v1/images/$QUALIFICATION_BASE_AMI_ID/promote-cas?$scope_query")
[[ "$stale_status" == 409 ]]
image_read "$QUALIFICATION_BASE_AMI_ID" "$raw/stale-readback.json"
stale_failed_status=$(curl --silent --show-error --max-time 30 \
  -H "@$relay_headers" -o "$raw/stale-failed-readback.json" -w '%{http_code}' \
  "$QUALIFICATION_RELAY_URL/v1/images/$failed_image?$scope_query")
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
release_retained_lease
node -e '
const fs = require("node:fs");
const selection = Number(process.argv[2]) === 1 ? JSON.parse(fs.readFileSync(process.argv[1])) : {};
console.log(JSON.stringify({
  mintExit: 86, injectedAfterPromotedSmoke: true,
  launchCount: Number(process.argv[2]), smokeCount: Number(process.argv[3]), ...selection,
}));
' "$raw/selection-proof.json" "$expected_launches" "$smoke_count" >"$proof/execution-state.json"

# The wrapper job expects status 137. No executor trap or workflow artifact is
# allowed to own cleanup; the protected finalizer must recover from the registry.
printf '{"executorKilled":true,"signal":"SIGKILL","cloudCredentialsPresent":false,"cleanupOwner":"protected-finalizer"}\n' \
  >"$proof/executor-hard-kill.json"

{
  printf '%s\n' 'qualification mint exited 86 only after the promoted smoke'
  printf '%s\n' 'structured candidate API readbacks are authoritative; this log is supplemental'
  printf 'qualification mode: %s; launches: %s\n' "${QUALIFICATION_MODE:-mint}" "$expected_launches"
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
