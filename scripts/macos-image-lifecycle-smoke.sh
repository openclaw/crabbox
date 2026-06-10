#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
CRABBOX_REMEDIATION_BIN="${CRABBOX_REMEDIATION_BIN:-crabbox}"

region="${CRABBOX_MACOS_REGION:-eu-west-1}"
regions_raw="${CRABBOX_MACOS_REGIONS:-${CRABBOX_CAPACITY_REGIONS:-}}"
region_preflight="${CRABBOX_MACOS_REGION_PREFLIGHT:-auto}"
region_preflight_script="${CRABBOX_MACOS_REGION_PREFLIGHT_SCRIPT:-$ROOT/scripts/macos-host-region-preflight.sh}"
region_preflight_command="${CRABBOX_MACOS_REGION_PREFLIGHT_COMMAND:-scripts/macos-host-region-preflight.sh}"
iam_apply_command="${CRABBOX_MACOS_IAM_APPLY_COMMAND:-scripts/apply-macos-image-iam-policy.sh}"
quota_request_command="${CRABBOX_MACOS_QUOTA_REQUEST_COMMAND:-scripts/request-macos-host-quota.sh}"
instance_type="${CRABBOX_MACOS_TYPE:-mac2.metal}"
image_name="${CRABBOX_MACOS_IMAGE_NAME:-crabbox-macos-arm64-$(date -u +%Y%m%d-%H%M)}"
ttl="${CRABBOX_MACOS_TTL:-2h}"
idle_timeout="${CRABBOX_MACOS_IDLE_TIMEOUT:-30m}"
image_wait_timeout="${CRABBOX_MACOS_IMAGE_WAIT_TIMEOUT:-60m}"
host_wait_timeout="${CRABBOX_MACOS_HOST_WAIT_TIMEOUT:-5h}"
host_wait_interval="${CRABBOX_MACOS_HOST_WAIT_INTERVAL:-2m}"
host_available_stable_count="${CRABBOX_MACOS_HOST_AVAILABLE_STABLE_COUNT:-2}"
webvnc_wait_timeout="${CRABBOX_MACOS_WEBVNC_WAIT_TIMEOUT:-2m}"
webvnc_wait_interval="${CRABBOX_MACOS_WEBVNC_WAIT_INTERVAL:-5s}"
webvnc_start_grace="${CRABBOX_MACOS_WEBVNC_START_GRACE:-3s}"
allocate="${CRABBOX_MACOS_ALLOCATE:-0}"
run_existing="${CRABBOX_MACOS_RUN:-0}"
create_image="${CRABBOX_MACOS_CREATE_IMAGE:-1}"
checkpoint="${CRABBOX_MACOS_CHECKPOINT:-$create_image}"
promote="${CRABBOX_MACOS_PROMOTE:-0}"
open_webvnc="${CRABBOX_MACOS_OPEN_WEBVNC:-0}"
keep_lease="${CRABBOX_MACOS_KEEP_LEASE:-0}"
keep_checkpoint="${CRABBOX_MACOS_KEEP_CHECKPOINT:-0}"
release_host="${CRABBOX_MACOS_RELEASE_HOST:-0}"
if [[ -n "${CRABBOX_MACOS_REQUIRED_MAJOR:-}" ]]; then
  required_macos_major="$CRABBOX_MACOS_REQUIRED_MAJOR"
elif [[ "$instance_type" == mac-m* ]]; then
  required_macos_major="15"
else
  required_macos_major="14"
fi
if [[ -n "${CRABBOX_MACOS_REQUIRED_SWIFT_TOOLS:-}" ]]; then
  required_swift_tools="$CRABBOX_MACOS_REQUIRED_SWIFT_TOOLS"
elif [[ "$instance_type" == mac-m* ]]; then
  required_swift_tools="6.2"
else
  required_swift_tools="6.0"
fi
require_xcode="${CRABBOX_MACOS_REQUIRE_XCODE:-0}"
source_prep_script="${CRABBOX_MACOS_SOURCE_PREP_SCRIPT:-}"
artifact_root="${CRABBOX_MACOS_ARTIFACT_DIR:-$ROOT/.crabbox/macos-image-smoke/$image_name}"
summary_file="$artifact_root/summary.json"
evidence_dir="$artifact_root/evidence"

source_lease=""
checkpoint_fork_lease=""
candidate_lease=""
promoted_lease=""
source_lease_id=""
checkpoint_fork_lease_id=""
candidate_lease_id=""
promoted_lease_id=""
allocated_host=""
host_id=""
host_allocated_by_script=0
host_released=0
ami_id=""
checkpoint_id=""
checkpoint_deleted=0
summary_result=""
summary_phase="init"
blocker_message=""
blocker_remediation=""
blocker_commands=""
provider_identity_log=""
aws_policy_log=""
mac_host_policy_log=""
macos_image_policy_log=""
region_preflight_log=""
offerings_log=""
hosts_log=""
dry_log=""
quota_log=""
allocate_log=""
image_create_log=""
image_promote_log=""
checkpoint_create_log=""
checkpoint_fork_log=""
checkpoint_delete_log=""
source_host_wait_log=""
checkpoint_host_wait_log=""
candidate_host_wait_log=""
promoted_host_wait_log=""
source_prep_log=""
source_warmup_log=""
candidate_warmup_log=""
promoted_warmup_log=""
source_webvnc_status_log=""
checkpoint_webvnc_status_log=""
candidate_webvnc_status_log=""
promoted_webvnc_status_log=""
source_webvnc_daemon_log=""
checkpoint_webvnc_daemon_log=""
candidate_webvnc_daemon_log=""
promoted_webvnc_daemon_log=""

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  "$@"
}

run_tee() {
  local out="$1"
  shift
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  "$@" | tee "$out"
}

run_tee_combined() {
  local out="$1"
  shift
  local errexit_set=0
  local status
  [[ "$-" == *e* ]] && errexit_set=1
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  set +e
  "$@" 2>&1 | tee "$out"
  status="${PIPESTATUS[0]}"
  if [[ "$errexit_set" -eq 1 ]]; then
    set -e
  fi
  return "$status"
}

preflight_blocker_from_stderr() {
  local label="$1"
  local err="$2"
  local detail=""
  if [[ -s "$err" ]]; then
    detail="$(head -c 500 "$err" | tr '\n' ' ' | tr -s ' ')"
  fi
  if grep -q -E 'http 404|not_found' "$err"; then
    blocker_message="coordinator does not expose provider-neutral host lifecycle admin endpoints; deploy a coordinator with admin hosts support before Mac image validation"
    blocker_remediation="Deploy a coordinator that exposes /v1/admin/hosts with the legacy /v1/admin/mac-hosts compatibility route, then rerun the no-spend Mac host preflight."
    return 0
  fi
  blocker_message="$label failed"
  if [[ -n "$detail" ]]; then
    blocker_message="$blocker_message: $detail"
  fi
}

set_host_iam_remediation() {
  blocker_remediation="Apply the EC2 Mac host lifecycle policy to the coordinator AWS identity, verify the baseline AWS provider policy before paid image validation, then rerun the no-spend Mac host dry-run."
  blocker_commands="$(printf '%s\n' \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region" \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region --json > provider-identity.json" \
    "$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
    "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto" \
    "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply" \
    "$CRABBOX_REMEDIATION_BIN admin hosts allocate --provider aws --target macos --region $region --type $instance_type --dry-run --json")"
}

set_quota_iam_remediation() {
  blocker_remediation="Apply the combined provider plus macOS host lifecycle policy printed by crabbox admin providers policy --provider aws --target macos; it includes servicequotas:ListServiceQuotas for the quota preflight."
  blocker_commands="$(printf '%s\n' \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region" \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region --json > provider-identity.json" \
    "$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
    "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto" \
    "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply" \
    "$CRABBOX_REMEDIATION_BIN admin hosts quota --provider aws --target macos --region $region --type $instance_type --json")"
}

set_host_and_quota_iam_remediation() {
  blocker_remediation="Apply the combined provider plus macOS host lifecycle policy printed by crabbox admin providers policy --provider aws --target macos, then rerun both the Mac host quota preflight and no-spend Mac host dry-run."
  blocker_commands="$(printf '%s\n' \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region" \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region --json > provider-identity.json" \
    "$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
    "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto" \
    "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply" \
    "$CRABBOX_REMEDIATION_BIN admin hosts quota --provider aws --target macos --region $region --type $instance_type --json" \
    "$CRABBOX_REMEDIATION_BIN admin hosts allocate --provider aws --target macos --region $region --type $instance_type --dry-run --json")"
}

capture_preflight_command() {
  local phase="$1"
  local label="$2"
  local out="$3"
  shift 3
  local err="${out}.stderr"
  printf '+' >&2
  printf ' %q' "$@" >&2
  printf '\n' >&2
  set +e
  "$@" >"$out" 2>"$err"
  local status=$?
  cat "$out"
  cat "$err" >&2
  if [[ "$status" -ne 0 ]]; then
    jq -n \
      --arg phase "$phase" \
      --arg label "$label" \
      --arg stderr "$(head -c 2000 "$err")" \
      '{ok:false, phase:$phase, label:$label, stderr:$stderr}' >"$out" || true
  fi
  return "$status"
}

preflight_command() {
  local phase="$1"
  local label="$2"
  local out="$3"
  shift 3
  local err="${out}.stderr"
  set +e
  capture_preflight_command "$phase" "$label" "$out" "$@"
  local status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    preflight_blocker_from_stderr "$label" "$err"
    if grep -q 'servicequotas:ListServiceQuotas' "$err"; then
      set_quota_iam_remediation
    fi
    printf 'macOS lifecycle blocked before paid work: %s\n' "$blocker_message" >&2
    write_summary blocked "$phase"
    exit 1
  fi
}

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'missing required command: %s\n' "$1" >&2
    exit 2
  fi
}

stop_lease() {
  local lease="$1"
  [[ -n "$lease" ]] || return 0
  run "$CRABBOX_BIN" webvnc daemon stop --id "$lease" || true
  run "$CRABBOX_BIN" stop --provider aws --target macos "$lease" || true
}

delete_checkpoint() {
  [[ -n "$checkpoint_id" ]] || return 0
  [[ "$checkpoint_deleted" != "1" ]] || return 0
  [[ "$keep_checkpoint" != "1" ]] || return 0
  if [[ -z "$checkpoint_delete_log" ]]; then
    checkpoint_delete_log="$evidence_dir/checkpoint-delete.log"
  fi
  if run_tee "$checkpoint_delete_log" "$CRABBOX_BIN" checkpoint delete "$checkpoint_id"; then
    checkpoint_deleted=1
  fi
}

cleanup() {
  if [[ "$keep_lease" != "1" ]]; then
    stop_lease "$promoted_lease"
    stop_lease "$candidate_lease"
    stop_lease "$checkpoint_fork_lease"
    stop_lease "$source_lease"
  fi
  delete_checkpoint || true
  release_host_if_requested cleanup || true
}

write_summary() {
  local result="$1"
  local phase="$2"
  local provider_identity_log_path aws_policy_log_path mac_host_policy_log_path macos_image_policy_log_path offerings_log_path hosts_log_path
  local region_preflight_log_path
  local dry_log_path allocate_log_path image_create_log_path image_promote_log_path
  local checkpoint_create_log_path checkpoint_fork_log_path checkpoint_delete_log_path
  local quota_log_path
  local source_prep_log_path
  local source_artifact_dir_path checkpoint_artifact_dir_path candidate_artifact_dir_path promoted_artifact_dir_path
  local source_host_wait_log_path checkpoint_host_wait_log_path candidate_host_wait_log_path promoted_host_wait_log_path
  local source_warmup_log_path candidate_warmup_log_path promoted_warmup_log_path
  local source_webvnc_status_log_path checkpoint_webvnc_status_log_path candidate_webvnc_status_log_path promoted_webvnc_status_log_path
  local source_webvnc_daemon_log_path checkpoint_webvnc_daemon_log_path candidate_webvnc_daemon_log_path promoted_webvnc_daemon_log_path
  summary_result="$result"
  summary_phase="$phase"
  mkdir -p "$artifact_root"
  provider_identity_log_path="$(existing_file_or_empty "$provider_identity_log")"
  aws_policy_log_path="$(existing_file_or_empty "$aws_policy_log")"
  mac_host_policy_log_path="$(existing_file_or_empty "$mac_host_policy_log")"
  macos_image_policy_log_path="$(existing_file_or_empty "$macos_image_policy_log")"
  region_preflight_log_path="$(existing_file_or_empty "$region_preflight_log")"
  offerings_log_path="$(existing_file_or_empty "$offerings_log")"
  hosts_log_path="$(existing_file_or_empty "$hosts_log")"
  dry_log_path="$(existing_file_or_empty "$dry_log")"
  quota_log_path="$(existing_file_or_empty "$quota_log")"
  allocate_log_path="$(existing_file_or_empty "$allocate_log")"
  image_create_log_path="$(existing_file_or_empty "$image_create_log")"
  image_promote_log_path="$(existing_file_or_empty "$image_promote_log")"
  checkpoint_create_log_path="$(existing_file_or_empty "$checkpoint_create_log")"
  checkpoint_fork_log_path="$(existing_file_or_empty "$checkpoint_fork_log")"
  checkpoint_delete_log_path="$(existing_file_or_empty "$checkpoint_delete_log")"
  source_prep_log_path="$(existing_file_or_empty "$source_prep_log")"
  source_artifact_dir_path="$(existing_dir_or_empty "$artifact_root/source")"
  checkpoint_artifact_dir_path="$(existing_dir_or_empty "$artifact_root/checkpoint")"
  candidate_artifact_dir_path="$(existing_dir_or_empty "$artifact_root/candidate")"
  promoted_artifact_dir_path="$(existing_dir_or_empty "$artifact_root/promoted")"
  source_host_wait_log_path="$(existing_file_or_empty "$source_host_wait_log")"
  checkpoint_host_wait_log_path="$(existing_file_or_empty "$checkpoint_host_wait_log")"
  candidate_host_wait_log_path="$(existing_file_or_empty "$candidate_host_wait_log")"
  promoted_host_wait_log_path="$(existing_file_or_empty "$promoted_host_wait_log")"
  source_warmup_log_path="$(existing_file_or_empty "$source_warmup_log")"
  candidate_warmup_log_path="$(existing_file_or_empty "$candidate_warmup_log")"
  promoted_warmup_log_path="$(existing_file_or_empty "$promoted_warmup_log")"
  source_webvnc_status_log_path="$(existing_file_or_empty "$source_webvnc_status_log")"
  checkpoint_webvnc_status_log_path="$(existing_file_or_empty "$checkpoint_webvnc_status_log")"
  candidate_webvnc_status_log_path="$(existing_file_or_empty "$candidate_webvnc_status_log")"
  promoted_webvnc_status_log_path="$(existing_file_or_empty "$promoted_webvnc_status_log")"
  source_webvnc_daemon_log_path="$(existing_file_or_empty "$source_webvnc_daemon_log")"
  checkpoint_webvnc_daemon_log_path="$(existing_file_or_empty "$checkpoint_webvnc_daemon_log")"
  candidate_webvnc_daemon_log_path="$(existing_file_or_empty "$candidate_webvnc_daemon_log")"
  promoted_webvnc_daemon_log_path="$(existing_file_or_empty "$promoted_webvnc_daemon_log")"
  jq -n \
    --arg generatedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg result "$result" \
    --arg phase "$phase" \
    --arg region "$region" \
    --arg instanceType "$instance_type" \
    --arg imageName "$image_name" \
    --arg artifactRoot "." \
    --arg sourceLease "$source_lease_id" \
    --arg checkpointForkLease "$checkpoint_fork_lease_id" \
    --arg candidateLease "$candidate_lease_id" \
    --arg promotedLease "$promoted_lease_id" \
    --arg hostID "$host_id" \
    --arg hostAllocatedByScript "$host_allocated_by_script" \
    --arg hostReleaseRequested "$release_host" \
    --arg hostReleased "$host_released" \
    --arg keepLease "$keep_lease" \
    --arg createImage "$create_image" \
    --arg checkpoint "$checkpoint" \
    --arg keepCheckpoint "$keep_checkpoint" \
    --arg checkpointID "$checkpoint_id" \
    --arg checkpointDeleted "$checkpoint_deleted" \
    --arg promote "$promote" \
    --arg amiID "$ami_id" \
    --arg blockerMessage "$blocker_message" \
    --arg blockerRemediation "$blocker_remediation" \
    --arg blockerCommands "$blocker_commands" \
    --arg providerIdentityLog "$provider_identity_log_path" \
    --arg awsPolicyLog "$aws_policy_log_path" \
    --arg macHostPolicyLog "$mac_host_policy_log_path" \
    --arg macosImagePolicyLog "$macos_image_policy_log_path" \
    --arg regionPreflightLog "$region_preflight_log_path" \
    --arg offeringsLog "$offerings_log_path" \
    --arg hostsLog "$hosts_log_path" \
    --arg dryLog "$dry_log_path" \
    --arg quotaLog "$quota_log_path" \
    --arg allocateLog "$allocate_log_path" \
    --arg imageCreateLog "$image_create_log_path" \
    --arg imagePromoteLog "$image_promote_log_path" \
    --arg checkpointCreateLog "$checkpoint_create_log_path" \
    --arg checkpointForkLog "$checkpoint_fork_log_path" \
    --arg checkpointDeleteLog "$checkpoint_delete_log_path" \
    --arg sourcePrepLog "$source_prep_log_path" \
    --arg sourceArtifactDir "$source_artifact_dir_path" \
    --arg checkpointArtifactDir "$checkpoint_artifact_dir_path" \
    --arg candidateArtifactDir "$candidate_artifact_dir_path" \
    --arg promotedArtifactDir "$promoted_artifact_dir_path" \
    --arg sourceHostWaitLog "$source_host_wait_log_path" \
    --arg checkpointHostWaitLog "$checkpoint_host_wait_log_path" \
    --arg candidateHostWaitLog "$candidate_host_wait_log_path" \
    --arg promotedHostWaitLog "$promoted_host_wait_log_path" \
    --arg sourceWarmupLog "$source_warmup_log_path" \
    --arg candidateWarmupLog "$candidate_warmup_log_path" \
    --arg promotedWarmupLog "$promoted_warmup_log_path" \
    --arg sourceWebVNCStatusLog "$source_webvnc_status_log_path" \
    --arg checkpointWebVNCStatusLog "$checkpoint_webvnc_status_log_path" \
    --arg candidateWebVNCStatusLog "$candidate_webvnc_status_log_path" \
    --arg promotedWebVNCStatusLog "$promoted_webvnc_status_log_path" \
    --arg sourceWebVNCDaemonLog "$source_webvnc_daemon_log_path" \
    --arg checkpointWebVNCDaemonLog "$checkpoint_webvnc_daemon_log_path" \
    --arg candidateWebVNCDaemonLog "$candidate_webvnc_daemon_log_path" \
    --arg promotedWebVNCDaemonLog "$promoted_webvnc_daemon_log_path" \
    'def maybe_path($path): if $path == "" then null else $path end;
    {
      generatedAt: $generatedAt,
      result: $result,
      phase: $phase,
      region: $region,
      instanceType: $instanceType,
      imageName: $imageName,
      artifactRoot: $artifactRoot,
      host: {
        id: $hostID,
        allocatedByScript: ($hostAllocatedByScript == "1"),
        releaseRequested: ($hostReleaseRequested == "1"),
        released: ($hostReleased == "1")
      },
      leases: {
        source: $sourceLease,
        checkpointFork: $checkpointForkLease,
        candidate: $candidateLease,
        promoted: $promotedLease,
        keepRequested: ($keepLease == "1")
      },
      image: {
        createRequested: ($createImage == "1"),
        promoteRequested: ($promote == "1"),
        amiId: $amiID
      },
      checkpoint: {
        requested: ($checkpoint == "1"),
        keepRequested: ($keepCheckpoint == "1"),
        id: $checkpointID,
        deleted: ($checkpointDeleted == "1")
      },
      blocker: {
        reason: $blockerMessage,
        message: $blockerMessage,
        remediation: $blockerRemediation,
        commands: ($blockerCommands | split("\n") | map(select(length > 0)))
      },
      artifacts: {
        source: maybe_path($sourceArtifactDir),
        checkpointFork: maybe_path($checkpointArtifactDir),
        candidate: maybe_path($candidateArtifactDir),
        promoted: maybe_path($promotedArtifactDir)
      },
      evidence: {
        providerIdentity: maybe_path($providerIdentityLog),
        awsProviderPolicy: maybe_path($awsPolicyLog),
        macHostPolicy: maybe_path($macHostPolicyLog),
        macosImagePolicy: maybe_path($macosImagePolicyLog),
        regionPreflight: maybe_path($regionPreflightLog),
        hostOfferings: maybe_path($offeringsLog),
        hostList: maybe_path($hostsLog),
        hostDryRun: maybe_path($dryLog),
        hostQuota: maybe_path($quotaLog),
        hostAllocate: maybe_path($allocateLog),
        imageCreate: maybe_path($imageCreateLog),
        imagePromote: maybe_path($imagePromoteLog),
        checkpointCreate: maybe_path($checkpointCreateLog),
        checkpointFork: maybe_path($checkpointForkLog),
        checkpointDelete: maybe_path($checkpointDeleteLog),
        sourcePrep: maybe_path($sourcePrepLog),
        hostWait: {
          source: maybe_path($sourceHostWaitLog),
          checkpointFork: maybe_path($checkpointHostWaitLog),
          candidate: maybe_path($candidateHostWaitLog),
          promoted: maybe_path($promotedHostWaitLog)
        },
        warmup: {
          source: maybe_path($sourceWarmupLog),
          candidate: maybe_path($candidateWarmupLog),
          promoted: maybe_path($promotedWarmupLog)
        },
        webvncStatus: {
          source: maybe_path($sourceWebVNCStatusLog),
          checkpointFork: maybe_path($checkpointWebVNCStatusLog),
          candidate: maybe_path($candidateWebVNCStatusLog),
          promoted: maybe_path($promotedWebVNCStatusLog)
        },
        webvncDaemon: {
          source: maybe_path($sourceWebVNCDaemonLog),
          checkpointFork: maybe_path($checkpointWebVNCDaemonLog),
          candidate: maybe_path($candidateWebVNCDaemonLog),
          promoted: maybe_path($promotedWebVNCDaemonLog)
        }
      }
    }' >"$summary_file"
  printf 'macOS lifecycle summary: %s\n' "$summary_file"
}

existing_file_or_empty() {
  local path="${1:-}"
  if [[ -n "$path" && -f "$path" ]]; then
    summary_path "$path"
  fi
}

existing_dir_or_empty() {
  local path="${1:-}"
  if [[ -n "$path" && -d "$path" ]]; then
    summary_path "$path"
  fi
}

summary_path() {
  local path="$1"
  if [[ "$path" == "$artifact_root" ]]; then
    printf '.'
  elif [[ "$path" == "$artifact_root/"* ]]; then
    printf '%s' "${path#"$artifact_root"/}"
  else
    printf '%s' "$(basename "$path")"
  fi
}

on_exit() {
  local status=$?
  if [[ "$status" -ne 0 ]]; then
    cleanup || true
  fi
  if [[ "$status" -ne 0 && "$summary_result" != "blocked" ]]; then
    write_summary failed "$summary_phase" || true
  fi
  if [[ "$status" -eq 0 ]]; then
    cleanup || true
  fi
  exit "$status"
}
trap on_exit EXIT

release_host_if_requested() {
  local label="$1"
  [[ "$release_host" == "1" && -n "$allocated_host" ]] || return 0
  if [[ "$host_allocated_by_script" != "1" && "${CRABBOX_MACOS_RELEASE_EXISTING_HOST:-0}" != "1" ]]; then
    printf 'refusing to release pre-existing EC2 Mac Dedicated Host %s; set CRABBOX_MACOS_RELEASE_EXISTING_HOST=1 to confirm.\n' "$allocated_host" >&2
    return 1
  fi
  wait_for_host_available "$allocated_host" "$label"
  run "$CRABBOX_BIN" admin hosts release "$allocated_host" --provider aws --target macos --region "$region" --force
  host_released=1
  allocated_host=""
}

lease_from_log() {
  node -e '
const fs = require("fs");
const text = fs.readFileSync(process.argv[1], "utf8");
for (const line of text.trim().split(/\n/).reverse()) {
  try {
    const json = JSON.parse(line);
    if (json.leaseId) {
      console.log(json.leaseId);
      process.exit(0);
    }
  } catch {}
}
process.exit(1);
' "$1"
}

checkpoint_id_from_log() {
  node -e '
const fs = require("fs");
const text = fs.readFileSync(process.argv[1], "utf8");
const match = text.match(/\bid=(chk_[A-Za-z0-9_-]+)/);
if (!match) process.exit(1);
console.log(match[1]);
' "$1"
}

checkpoint_fork_lease_from_log() {
  node -e '
const fs = require("fs");
const text = fs.readFileSync(process.argv[1], "utf8");
const match = text.match(/\blease=([A-Za-z0-9_-]+)/);
if (!match) process.exit(1);
console.log(match[1]);
' "$1"
}

duration_seconds() {
  local value="$1"
  local number
  case "$value" in
    *h) number="${value%h}";;
    *m) number="${value%m}";;
    *s) number="${value%s}";;
    *) number="$value";;
  esac
  if [[ ! "$number" =~ ^[0-9]+$ ]]; then
    printf 'invalid duration: %s\n' "$value" >&2
    exit 2
  fi
  case "$value" in
    *h) printf '%s\n' "$((number * 3600))";;
    *m) printf '%s\n' "$((number * 60))";;
    *s | *) printf '%s\n' "$number";;
  esac
}

log_for_label() {
  local category="$1"
  local label="$2"
  case "$label" in
    source | checkpoint | candidate | promoted) ;;
    *)
      printf 'invalid lifecycle label: %s\n' "$label" >&2
      exit 2
      ;;
  esac
  printf '%s/%s-%s.log\n' "$evidence_dir" "$category" "$label"
}

log_line() {
  local log="$1"
  shift
  printf '%s\n' "$*" | tee -a "$log"
}

set_evidence_paths() {
  provider_identity_log="$evidence_dir/provider-identity.json"
  aws_policy_log="$evidence_dir/aws-provider-policy.json"
  mac_host_policy_log="$evidence_dir/mac-host-policy.json"
  macos_image_policy_log="$evidence_dir/macos-image-policy.json"
  region_preflight_log="$evidence_dir/mac-host-region-preflight.json"
  source_host_wait_log="$(log_for_label host-wait source)"
  checkpoint_host_wait_log="$(log_for_label host-wait checkpoint)"
  candidate_host_wait_log="$(log_for_label host-wait candidate)"
  promoted_host_wait_log="$(log_for_label host-wait promoted)"
  source_warmup_log="$(log_for_label warmup source)"
  candidate_warmup_log="$(log_for_label warmup candidate)"
  promoted_warmup_log="$(log_for_label warmup promoted)"
  source_webvnc_status_log="$(log_for_label webvnc-status source)"
  checkpoint_webvnc_status_log="$(log_for_label webvnc-status checkpoint)"
  candidate_webvnc_status_log="$(log_for_label webvnc-status candidate)"
  promoted_webvnc_status_log="$(log_for_label webvnc-status promoted)"
  source_webvnc_daemon_log="$(log_for_label webvnc-daemon source)"
  checkpoint_webvnc_daemon_log="$(log_for_label webvnc-daemon checkpoint)"
  candidate_webvnc_daemon_log="$(log_for_label webvnc-daemon candidate)"
  promoted_webvnc_daemon_log="$(log_for_label webvnc-daemon promoted)"
}

should_run_region_preflight() {
  case "$region_preflight" in
    1 | true | yes) return 0 ;;
    0 | false | no) return 1 ;;
    auto)
      [[ -z "${CRABBOX_MACOS_REGION:-}" && -n "$regions_raw" ]]
      return
      ;;
    *)
      printf 'invalid CRABBOX_MACOS_REGION_PREFLIGHT: %s\n' "$region_preflight" >&2
      exit 2
      ;;
  esac
}

select_region_from_preflight() {
  should_run_region_preflight || return 0
  if [[ ! -x "$region_preflight_script" ]]; then
    printf 'CRABBOX_MACOS_REGION_PREFLIGHT_SCRIPT is not executable: %s\n' "$region_preflight_script" >&2
    exit 2
  fi

  summary_phase="region-preflight"
  set +e
  if [[ -n "${CRABBOX_MACOS_TYPE:-}" ]]; then
    env "CRABBOX_BIN=$CRABBOX_BIN" "CRABBOX_MACOS_TYPE=$instance_type" \
      "$region_preflight_script" >"$region_preflight_log" 2>"${region_preflight_log}.stderr"
  else
    env "CRABBOX_BIN=$CRABBOX_BIN" \
      "$region_preflight_script" >"$region_preflight_log" 2>"${region_preflight_log}.stderr"
  fi
  local status=$?
  set -e
  cat "$region_preflight_log"
  cat "${region_preflight_log}.stderr" >&2

  if [[ "$status" -ne 0 ]]; then
    blocker_message="$(jq -r '.blocker.message // "no configured macOS region is ready"' "$region_preflight_log" 2>/dev/null || printf 'no configured macOS region is ready')"
    blocker_remediation="$(jq -r '.blocker.remediation // "Rerun the macOS region preflight after IAM, quota, or host availability changes."' "$region_preflight_log" 2>/dev/null || printf 'Rerun the macOS region preflight after IAM, quota, or host availability changes.')"
    blocker_commands="$(jq -r '.blocker.commands[]? // empty' "$region_preflight_log" 2>/dev/null || true)"
    if [[ -z "$blocker_commands" ]]; then
      blocker_commands="$(printf '%s\n' \
        "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region" \
        "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region --json > provider-identity.json" \
        "$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
        "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto" \
        "$iam_apply_command --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply" \
        "$region_preflight_command")"
    fi
    printf 'macOS lifecycle blocked before paid work: %s\n' "$blocker_message" >&2
    write_summary blocked region-preflight
    exit 1
  fi

  local selected_region
  selected_region="$(jq -r '.selectedRegion // empty' "$region_preflight_log")"
  if [[ -z "$selected_region" ]]; then
    blocker_message="macOS region preflight succeeded but did not return selectedRegion"
    printf 'macOS lifecycle blocked before paid work: %s\n' "$blocker_message" >&2
    write_summary blocked region-preflight
    exit 1
  fi
  region="$selected_region"

  local selected_type
  selected_type="$(jq -r '.selectedInstanceType // .instanceType // empty' "$region_preflight_log")"
  if [[ -n "$selected_type" ]]; then
    instance_type="$selected_type"
  fi
}

mac_host_state() {
  local host="$1"
  "$CRABBOX_BIN" admin hosts list --provider aws --target macos --region "$region" --type "$instance_type" --json |
    jq -r --arg host "$host" '[.[] | select(.id == $host) | .state][0] // empty'
}

wait_for_host_available() {
  local host="$1"
  local label="$2"
  [[ -n "$host" ]] || return 0
  local available_count available_needed timeout_seconds interval_seconds deadline state log
  log="$(log_for_label host-wait "$label")"
  : >"$log"
  timeout_seconds="$(duration_seconds "$host_wait_timeout")"
  interval_seconds="$(duration_seconds "$host_wait_interval")"
  available_needed="$host_available_stable_count"
  if ! [[ "$available_needed" =~ ^[0-9]+$ ]] || [[ "$available_needed" -lt 1 ]]; then
    available_needed=1
  fi
  available_count=0
  deadline="$(($(date +%s) + timeout_seconds))"
  log_line "$log" "waiting for EC2 Mac Dedicated Host $host to become stably available after $label lease stop; timeout=$host_wait_timeout interval=$host_wait_interval stable_count=$available_needed"
  while true; do
    state="$(mac_host_state "$host")"
    if [[ "$state" == "available" ]]; then
      available_count="$((available_count + 1))"
      log_line "$log" "host $host is available ($available_count/$available_needed)"
      if [[ "$available_count" -ge "$available_needed" ]]; then
        return 0
      fi
    else
      available_count=0
    fi
    if [[ "$(date +%s)" -ge "$deadline" ]]; then
      log_line "$log" "timed out waiting for EC2 Mac Dedicated Host $host to become available; last state=${state:-missing}" >&2
      return 1
    fi
    log_line "$log" "host $host state=${state:-missing}; sleeping ${interval_seconds}s"
    sleep "$interval_seconds"
  done
}

require_webvnc_connected() {
  local lease="$1"
  local label="$2"
  local timeout_seconds interval_seconds deadline log
  log="$(log_for_label webvnc-status "$label")"
  : >"$log"
  timeout_seconds="$(duration_seconds "$webvnc_wait_timeout")"
  interval_seconds="$(duration_seconds "$webvnc_wait_interval")"
  deadline="$(($(date +%s) + timeout_seconds))"
  printf 'waiting for WebVNC portal bridge for lease %s; timeout=%s interval=%s\n' "$lease" "$webvnc_wait_timeout" "$webvnc_wait_interval"
  while true; do
    run "$CRABBOX_BIN" webvnc status --provider aws --target macos --id "$lease" | tee -a "$log"
    if grep -q '^portal bridge: connected=true' "$log"; then
      printf 'WebVNC portal bridge connected for lease %s\n' "$lease"
      return 0
    fi
    if [[ "$(date +%s)" -ge "$deadline" ]]; then
      printf 'timed out waiting for WebVNC portal bridge for lease %s\n' "$lease" >&2
      return 1
    fi
    printf 'WebVNC portal bridge is not connected for lease %s; sleeping %ss\n' "$lease" "$interval_seconds"
    sleep "$interval_seconds"
  done
}

warmup_macos() {
  local label="$1"
  shift
  local log status
  log="$(log_for_label warmup "$label")"
  : >"$log"
  printf 'warming macOS lease: %s\n' "$label" >&2
  set +e
  (
    CRABBOX_AWS_REGION="$region" AWS_REGION="$region" "$CRABBOX_BIN" warmup \
      --provider aws \
      --target macos \
      --type "$instance_type" \
      --market on-demand \
      --desktop \
      --ttl "$ttl" \
      --idle-timeout "$idle_timeout" \
      --timing-json \
      "$@"
  ) 2>&1 | tee -a "$log" >&2
  status="${PIPESTATUS[0]}"
  set -e
  if [[ "$status" -ne 0 ]]; then
    return "$status"
  fi
  lease_from_log "$log"
}

run_source_prep() {
  local lease="$1"
  [[ -n "$source_prep_script" ]] || return 0
  if [[ ! -f "$source_prep_script" ]]; then
    blocker_message="macOS source prep script not found: $source_prep_script"
    printf '%s\n' "$blocker_message" >&2
    exit 1
  fi
  source_prep_log="$evidence_dir/source-prep.log"
  run_tee_combined "$source_prep_log" "$CRABBOX_BIN" run \
    --provider aws \
    --target macos \
    --id "$lease" \
    --no-sync \
    --script "$source_prep_script"
}

smoke_macos_lease() {
  local lease="$1"
  local label="$2"
  local out_dir="$artifact_root/$label"
  local daemon_log webvnc_grace_seconds remote probe
  printf -v remote 'set -euo pipefail\nrequired_macos_major=%q\nrequired_swift_tools=%q\nrequire_xcode=%q\n' \
    "$required_macos_major" \
    "$required_swift_tools" \
    "$require_xcode"
  IFS= read -r -d '' probe <<'REMOTE' || true
echo macos-smoke-ok
product_version="$(sw_vers -productVersion)"
product_major="${product_version%%.*}"
sw_vers
case "$product_major" in
  ""|*[!0-9]*)
    echo "could not parse macOS major version from $product_version" >&2
    exit 1
    ;;
esac
if (( product_major < required_macos_major )); then
  echo "macOS ${required_macos_major}+ required, got $product_version" >&2
  exit 1
fi
developer_dir="$(xcode-select -p 2>/dev/null || true)"
echo "developer_dir=$developer_dir"
if [[ -z "$developer_dir" ]]; then
  echo "xcode-select has no active developer directory" >&2
  exit 1
fi
if [[ "$require_xcode" == "1" ]]; then
  case "$developer_dir" in
    *CommandLineTools*)
      echo "full Xcode developer directory required, got Command Line Tools: $developer_dir" >&2
      exit 1
      ;;
  esac
  command -v xcodebuild
  xcodebuild -version
  test -d "$developer_dir/Platforms/MacOSX.platform/Developer/SDKs"
fi
command -v xcrun
xcrun --sdk macosx --show-sdk-path
xcrun --find clang
xcrun --find swift
command -v clang
command -v swift
swift --version
swift_version="$(swift --version | awk '{ for (i = 1; i < NF; i++) if ($i == "version") { print $(i + 1); exit } }')"
if [[ -z "$swift_version" ]]; then
  echo "could not parse Swift version" >&2
  exit 1
fi
awk -v have="$swift_version" -v want="$required_swift_tools" '
  BEGIN {
    haveParts = split(have, h, ".")
    wantParts = split(want, w, ".")
    haveMajor = h[1] + 0
    haveMinor = haveParts > 1 ? h[2] + 0 : 0
    wantMajor = w[1] + 0
    wantMinor = wantParts > 1 ? w[2] + 0 : 0
    if (haveMajor < wantMajor || (haveMajor == wantMajor && haveMinor < wantMinor)) {
      printf("Swift tools %s+ required, got %s\n", want, have) > "/dev/stderr"
      exit 1
    }
  }'
command -v ssh
command -v git
command -v rsync
command -v curl
command -v nc
command -v brew
brew --version
brew --prefix
command -v node
node --version
command -v npm
npm --version
command -v corepack
corepack --version
command -v pnpm
pnpm --version
command -v python3
python3 --version
test -d "$HOME/crabbox"
test -w "$HOME/crabbox"
sudo test -s /var/db/crabbox/vnc.password
nc -z 127.0.0.1 5900
REMOTE
  remote+="$probe"
  run "$CRABBOX_BIN" run \
    --provider aws \
    --target macos \
    --id "$lease" \
    --no-sync \
    --shell -- \
    "$remote"

  daemon_log="$(log_for_label webvnc-daemon "$label")"
  if [[ "$open_webvnc" == "1" ]]; then
    run_tee "$daemon_log" "$CRABBOX_BIN" webvnc daemon start --provider aws --target macos --id "$lease" --open
  else
    run_tee "$daemon_log" "$CRABBOX_BIN" webvnc daemon start --provider aws --target macos --id "$lease"
  fi
  webvnc_grace_seconds="$(duration_seconds "$webvnc_start_grace")"
  if [[ "$webvnc_grace_seconds" -gt 0 ]]; then
    sleep "$webvnc_grace_seconds"
  fi
  require_webvnc_connected "$lease" "$label"
  run "$CRABBOX_BIN" artifacts collect \
    --provider aws \
    --target macos \
    --id "$lease" \
    --output "$out_dir" \
    --screenshot \
    --doctor \
    --webvnc-status \
    --json
}

create_checkpoint_from_source() {
  [[ "$checkpoint" == "1" ]] || return 0
  summary_phase="checkpoint-create"
  checkpoint_create_log="$evidence_dir/checkpoint-create.log"
  run_tee "$checkpoint_create_log" "$CRABBOX_BIN" checkpoint create \
    --id "$source_lease" \
    --name "$image_name-checkpoint" \
    --mode native \
    --strategy image \
    --wait \
    --wait-timeout "$image_wait_timeout"
  checkpoint_id="$(checkpoint_id_from_log "$checkpoint_create_log")"
  if [[ -z "$checkpoint_id" ]]; then
    blocker_message="checkpoint create did not return a checkpoint id"
    printf 'checkpoint create did not return a checkpoint id\n' >&2
    exit 1
  fi
}

smoke_checkpoint_fork() {
  [[ "$checkpoint" == "1" ]] || return 0
  [[ -n "$checkpoint_id" ]] || return 0
  local attempt max_attempts status
  max_attempts="${CRABBOX_MACOS_CHECKPOINT_FORK_ATTEMPTS:-2}"
  if ! [[ "$max_attempts" =~ ^[0-9]+$ ]] || [[ "$max_attempts" -lt 1 ]]; then
    max_attempts=1
  fi
  summary_phase="checkpoint-fork"
  checkpoint_fork_log="$evidence_dir/checkpoint-fork.log"
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    if [[ "$attempt" -gt 1 ]]; then
      printf 'retrying checkpoint fork attempt %s/%s after host wait\n' "$attempt" "$max_attempts"
      wait_for_host_available "$allocated_host" checkpoint
    fi
    set +e
    run_tee_combined "$checkpoint_fork_log" "$CRABBOX_BIN" checkpoint fork "$checkpoint_id" --desktop
    status=$?
    set -e
    if [[ "$status" -eq 0 ]]; then
      break
    fi
    if [[ "$attempt" -eq "$max_attempts" ]]; then
      blocker_message="checkpoint fork failed after $max_attempts attempt(s)"
      return "$status"
    fi
  done
  checkpoint_fork_lease="$(checkpoint_fork_lease_from_log "$checkpoint_fork_log")"
  checkpoint_fork_lease_id="$checkpoint_fork_lease"
  if [[ -z "$checkpoint_fork_lease" ]]; then
    blocker_message="checkpoint fork did not return a lease id"
    printf 'checkpoint fork did not return a lease id\n' >&2
    exit 1
  fi
  write_summary running checkpoint-smoke
  smoke_macos_lease "$checkpoint_fork_lease" checkpoint
  stop_lease "$checkpoint_fork_lease"
  checkpoint_fork_lease=""
  wait_for_host_available "$allocated_host" checkpoint
  delete_checkpoint
}

need node
need jq
if [[ ! -x "$CRABBOX_BIN" ]]; then
  printf 'CRABBOX_BIN is not executable: %s\n' "$CRABBOX_BIN" >&2
  exit 2
fi

mkdir -p "$evidence_dir"
set_evidence_paths
select_region_from_preflight
write_summary running preflight
printf 'macOS lifecycle smoke region=%s type=%s image=%s host-wait=%s\n' "$region" "$instance_type" "$image_name" "$host_wait_timeout"
preflight_command provider-identity "provider identity" "$provider_identity_log" "$CRABBOX_BIN" admin providers identity --provider aws --region "$region" --json
preflight_command provider-policy "aws provider policy" "$aws_policy_log" "$CRABBOX_BIN" admin providers policy --provider aws
preflight_command mac-host-policy "mac host policy" "$mac_host_policy_log" "$CRABBOX_BIN" admin hosts policy --provider aws --target macos
preflight_command macos-image-policy "macOS image policy" "$macos_image_policy_log" "$CRABBOX_BIN" admin providers policy --provider aws --target macos
offerings_log="$evidence_dir/mac-host-offerings.txt"
hosts_log="$evidence_dir/mac-host-list.json"
preflight_command host-offerings "mac host offerings" "$offerings_log" "$CRABBOX_BIN" admin hosts offerings --provider aws --target macos --region "$region" --type "$instance_type"
preflight_command host-list "mac host list" "$hosts_log" "$CRABBOX_BIN" admin hosts list --provider aws --target macos --region "$region" --type "$instance_type" --json
hosts_json="$(cat "$hosts_log")"
printf '%s\n' "$hosts_json" | jq .

existing_host="$(
  printf '%s\n' "$hosts_json" |
    jq -r --arg type "$instance_type" '[.[] | select(.instanceType == $type and .state == "available") | .id][0] // empty'
)"

if [[ -n "$existing_host" ]]; then
  if [[ "$run_existing" != "1" && "$allocate" != "1" ]]; then
    printf 'available EC2 Mac Dedicated Host found: %s\n' "$existing_host"
    printf 'set CRABBOX_MACOS_RUN=1 to use the existing host and continue.\n'
    allocated_host="$existing_host"
    host_id="$existing_host"
    write_summary ready existing-host
    exit 0
  fi
  printf 'using existing EC2 Mac Dedicated Host: %s\n' "$existing_host"
  allocated_host="$existing_host"
  host_id="$existing_host"
else
  summary_phase="host-quota"
  quota_log="$evidence_dir/mac-host-quota.json"
  set +e
  capture_preflight_command host-quota "mac host quota" "$quota_log" "$CRABBOX_BIN" admin hosts quota --provider aws --target macos --region "$region" --type "$instance_type" --json
  quota_status=$?
  set -e

  summary_phase="host-dry-run"
  dry_log="$evidence_dir/mac-host-dry-run.json"
  preflight_command host-dry-run "mac host dry-run" "$dry_log" "$CRABBOX_BIN" admin hosts allocate --provider aws --target macos --region "$region" --type "$instance_type" --dry-run --json
  if ! jq -e 'any(.[]; .ok == true)' "$dry_log" >/dev/null; then
    blocker_message="$(jq -r '[.[] | select(.ok != true) | .message] | unique | join("; ")' "$dry_log")"
    if grep -q 'UnauthorizedOperation' "$dry_log"; then
      if [[ "$quota_status" -ne 0 ]]; then
        set_host_and_quota_iam_remediation
      else
        set_host_iam_remediation
      fi
    fi
    if [[ "$quota_status" -ne 0 ]]; then
      blocker_message="$blocker_message; mac host quota preflight also failed"
      if [[ -z "$blocker_remediation" ]]; then
        set_quota_iam_remediation
      elif [[ "$blocker_remediation" != *"servicequotas:ListServiceQuotas"* ]]; then
        blocker_remediation="$blocker_remediation Also verify servicequotas:ListServiceQuotas from the combined AWS policy."
      fi
    fi
    printf 'macOS lifecycle blocked before paid work: EC2 Mac host dry-run did not succeed.\n' >&2
    write_summary blocked host-dry-run
    exit 1
  fi

  if [[ "$quota_status" -ne 0 ]]; then
    preflight_blocker_from_stderr "mac host quota" "${quota_log}.stderr"
    if grep -q 'servicequotas:ListServiceQuotas' "${quota_log}.stderr"; then
      set_quota_iam_remediation
    fi
    printf 'macOS lifecycle blocked before paid work: %s\n' "$blocker_message" >&2
    write_summary blocked host-quota
    exit 1
  fi

  if ! jq -e 'length > 0' "$quota_log" >/dev/null; then
    blocker_message="no EC2 Mac Dedicated Host service quota was visible for $instance_type in $region"
    blocker_remediation="Check AWS Service Quotas for the selected EC2 Mac host family in $region, then rerun the no-spend Mac host preflight."
    printf 'macOS lifecycle blocked before paid work: %s\n' "$blocker_message" >&2
    write_summary blocked host-quota
    exit 1
  fi
  if ! jq -e 'any(.[]; (.value // 0) >= 1)' "$quota_log" >/dev/null; then
    blocker_message="EC2 Mac Dedicated Host quota is below 1 for $instance_type in $region"
    blocker_remediation="Request or raise the AWS EC2 Mac Dedicated Host quota for the selected host family in $region, then rerun the no-spend Mac host preflight."
    blocker_commands="$(printf '%s\n' \
      "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region --json > provider-identity.json" \
      "$CRABBOX_REMEDIATION_BIN admin hosts quota --provider aws --target macos --region $region --type $instance_type --json > mac-host-quota.json" \
      "$quota_request_command --identity provider-identity.json --quota mac-host-quota.json --region $region --profile auto" \
      "$quota_request_command --identity provider-identity.json --quota mac-host-quota.json --region $region --profile auto --apply" \
      "$region_preflight_command")"
    printf 'macOS lifecycle blocked before paid work: %s\n' "$blocker_message" >&2
    write_summary blocked host-quota
    exit 1
  fi

  if [[ "$allocate" != "1" ]]; then
    printf 'dry-run passed; set CRABBOX_MACOS_ALLOCATE=1 to allocate a paid EC2 Mac Dedicated Host and continue.\n'
    write_summary ready allocation
    exit 0
  fi

  summary_phase="host-allocation"
  allocate_log="$evidence_dir/mac-host-allocate.json"
  set +e
  capture_preflight_command host-allocation "mac host allocation" "$allocate_log" "$CRABBOX_BIN" admin hosts allocate --provider aws --target macos --region "$region" --type "$instance_type" --force --json
  allocate_status=$?
  set -e
  if [[ "$allocate_status" -ne 0 ]]; then
    preflight_blocker_from_stderr "mac host allocation" "${allocate_log}.stderr"
    blocker_remediation="Retry the allocation if AWS reports a transient service error. If dry-run still succeeds but paid allocation keeps failing, use an existing available EC2 Mac Dedicated Host or try another host family/region with non-zero quota."
    printf 'macOS lifecycle blocked during paid work: %s\n' "$blocker_message" >&2
    write_summary blocked host-allocation
    exit 1
  fi
  allocated_host="$(jq -r '.[0].id // empty' "$allocate_log")"
  if [[ -z "$allocated_host" ]]; then
    blocker_message="mac host allocation did not return a host id"
    printf 'macOS lifecycle blocked after allocation: could not determine allocated EC2 Mac Dedicated Host id.\n' >&2
    exit 1
  fi
  host_allocated_by_script=1
  host_id="$allocated_host"
fi

if [[ -n "$allocated_host" ]]; then
  printf 'pinning macOS leases to EC2 Mac Dedicated Host: %s\n' "$allocated_host"
  export CRABBOX_HOST_ID="$allocated_host"
fi

if [[ "$release_host" == "1" && -n "$allocated_host" && "$host_allocated_by_script" != "1" && "${CRABBOX_MACOS_RELEASE_EXISTING_HOST:-0}" != "1" ]]; then
  printf 'refusing to release pre-existing EC2 Mac Dedicated Host %s; set CRABBOX_MACOS_RELEASE_EXISTING_HOST=1 to confirm.\n' "$allocated_host" >&2
  exit 1
fi

write_summary running source-warmup
source_lease="$(warmup_macos source)"
source_lease_id="$source_lease"
if [[ -n "$source_prep_script" ]]; then
  write_summary running source-prep
  run_source_prep "$source_lease"
fi
write_summary running source-smoke
smoke_macos_lease "$source_lease" source
create_checkpoint_from_source

if [[ "$create_image" != "1" ]]; then
  if [[ "$release_host" == "1" || "$keep_lease" != "1" ]]; then
    stop_lease "$source_lease"
    source_lease=""
  fi
  release_host_if_requested source
  printf 'source lease smoke passed; set CRABBOX_MACOS_CREATE_IMAGE=1 to create the AMI.\n'
  write_summary passed source
  exit 0
fi

summary_phase="image-create"
image_create_log="$evidence_dir/image-create.json"
image_json="$("$CRABBOX_BIN" image create --id "$source_lease" --name "$image_name" --no-reboot=false --wait --wait-timeout "$image_wait_timeout" --json | tee "$image_create_log")"
printf '%s\n' "$image_json" | jq .
ami_id="$(printf '%s\n' "$image_json" | jq -r '.id // .image.id // empty')"
if [[ -z "$ami_id" ]]; then
  blocker_message="image create did not return an AMI id"
  printf 'image create did not return an AMI id\n' >&2
  exit 1
fi

stop_lease "$source_lease"
source_lease=""
wait_for_host_available "$allocated_host" source
smoke_checkpoint_fork

write_summary running candidate-warmup
candidate_lease="$(CRABBOX_AWS_AMI="$ami_id" warmup_macos candidate)"
candidate_lease_id="$candidate_lease"
write_summary running candidate-smoke
smoke_macos_lease "$candidate_lease" candidate

if [[ "$promote" != "1" ]]; then
  if [[ "$release_host" == "1" || "$keep_lease" != "1" ]]; then
    stop_lease "$candidate_lease"
    candidate_lease=""
  fi
  release_host_if_requested candidate
  printf 'candidate AMI smoke passed: %s\n' "$ami_id"
  printf 'set CRABBOX_MACOS_PROMOTE=1 to promote it and run the promoted-image smoke.\n'
  write_summary passed candidate
  exit 0
fi

summary_phase="image-promote"
image_promote_log="$evidence_dir/image-promote.json"
run_tee "$image_promote_log" "$CRABBOX_BIN" image promote "$ami_id" --target macos --region "$region" --json
stop_lease "$candidate_lease"
candidate_lease=""
wait_for_host_available "$allocated_host" candidate

write_summary running promoted-warmup
promoted_lease="$(warmup_macos promoted)"
promoted_lease_id="$promoted_lease"
write_summary running promoted-smoke
smoke_macos_lease "$promoted_lease" promoted
printf 'promoted macOS image lifecycle passed: %s\n' "$ami_id"

if [[ "$release_host" == "1" ]]; then
  stop_lease "$promoted_lease"
  promoted_lease=""
  release_host_if_requested promoted
fi

write_summary passed promoted
