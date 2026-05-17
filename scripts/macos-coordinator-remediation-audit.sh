#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
CRABBOX_REMEDIATION_BIN="${CRABBOX_REMEDIATION_BIN:-crabbox}"
CRABBOX_MACOS_IAM_APPLY_COMMAND="${CRABBOX_MACOS_IAM_APPLY_COMMAND:-$ROOT/scripts/apply-macos-image-iam-policy.sh}"
CRABBOX_MACOS_QUOTA_REQUEST_COMMAND="${CRABBOX_MACOS_QUOTA_REQUEST_COMMAND:-$ROOT/scripts/request-macos-host-quota.sh}"
CRABBOX_MACOS_IAM_APPLY_DISPLAY_COMMAND="${CRABBOX_MACOS_IAM_APPLY_DISPLAY_COMMAND:-scripts/apply-macos-image-iam-policy.sh}"
CRABBOX_MACOS_QUOTA_REQUEST_DISPLAY_COMMAND="${CRABBOX_MACOS_QUOTA_REQUEST_DISPLAY_COMMAND:-scripts/request-macos-host-quota.sh}"

usage() {
  cat <<'USAGE'
Usage: scripts/macos-coordinator-remediation-audit.sh [--region <aws-region>] [--type <mac-host-type>] [--artifact-dir <dir>] [--profile <aws-profile|auto>]

Collect a no-spend AWS macOS coordinator remediation bundle:
provider identity, combined IAM policy, EC2 Mac Dedicated Host quota,
allocation dry-run, guarded IAM apply dry-run, guarded quota request dry-run,
and a summary.json with explicit blockers.

Options:
  --region <region>       AWS region to inspect; default CRABBOX_MACOS_REGION or eu-west-1
  --type <instance-type>  EC2 Mac host type; default CRABBOX_MACOS_TYPE or mac2.metal
  --artifact-dir <dir>    output directory for evidence
  --profile <name|auto>   AWS profile for guarded remediation helper dry-runs; default auto
  -h, --help              show this help
USAGE
}

region="${CRABBOX_MACOS_REGION:-eu-west-1}"
instance_type="${CRABBOX_MACOS_TYPE:-mac2.metal}"
artifact_dir="${CRABBOX_MACOS_REMEDIATION_AUDIT_DIR:-}"
profile="${CRABBOX_MACOS_REMEDIATION_PROFILE:-auto}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --region)
      region="${2:?missing value for --region}"
      shift 2
      ;;
    --type)
      instance_type="${2:?missing value for --type}"
      shift 2
      ;;
    --artifact-dir)
      artifact_dir="${2:?missing value for --artifact-dir}"
      shift 2
      ;;
    --profile)
      profile="${2:?missing value for --profile}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'missing required command: %s\n' "$1" >&2
    exit 127
  fi
}

need jq
if [[ ! -x "$CRABBOX_BIN" ]]; then
  printf 'CRABBOX_BIN is not executable: %s\n' "$CRABBOX_BIN" >&2
  exit 2
fi
if [[ ! -x "$CRABBOX_MACOS_IAM_APPLY_COMMAND" ]]; then
  printf 'CRABBOX_MACOS_IAM_APPLY_COMMAND is not executable: %s\n' "$CRABBOX_MACOS_IAM_APPLY_COMMAND" >&2
  exit 2
fi
if [[ ! -x "$CRABBOX_MACOS_QUOTA_REQUEST_COMMAND" ]]; then
  printf 'CRABBOX_MACOS_QUOTA_REQUEST_COMMAND is not executable: %s\n' "$CRABBOX_MACOS_QUOTA_REQUEST_COMMAND" >&2
  exit 2
fi

if [[ -z "$artifact_dir" ]]; then
  artifact_dir="$ROOT/.crabbox/macos-remediation-audit/$(date -u +%Y%m%d-%H%M%S)"
fi
mkdir -p "$artifact_dir/evidence"

run_capture() {
  local name="$1"
  shift
  local status=0
  "$@" >"$artifact_dir/evidence/$name.out" 2>"$artifact_dir/evidence/$name.err" || status=$?
  printf '%s\n' "$status" >"$artifact_dir/evidence/$name.status"
}

run_capture provider-identity "$CRABBOX_BIN" admin providers identity --provider aws --region "$region" --json
run_capture macos-image-policy "$CRABBOX_BIN" admin providers policy --provider aws --target macos
run_capture mac-host-quota "$CRABBOX_BIN" admin hosts quota --provider aws --target macos --region "$region" --type "$instance_type" --json
run_capture mac-host-dry-run "$CRABBOX_BIN" admin hosts allocate --provider aws --target macos --region "$region" --type "$instance_type" --dry-run --json

if [[ "$(cat "$artifact_dir/evidence/provider-identity.status")" == "0" && "$(cat "$artifact_dir/evidence/macos-image-policy.status")" == "0" ]]; then
  run_capture iam-apply-dry-run "$CRABBOX_MACOS_IAM_APPLY_COMMAND" --identity "$artifact_dir/evidence/provider-identity.out" --policy "$artifact_dir/evidence/macos-image-policy.out" --profile "$profile"
else
  printf 'skipped; provider identity or policy capture failed\n' >"$artifact_dir/evidence/iam-apply-dry-run.err"
  printf '1\n' >"$artifact_dir/evidence/iam-apply-dry-run.status"
  : >"$artifact_dir/evidence/iam-apply-dry-run.out"
fi

if [[ "$(cat "$artifact_dir/evidence/provider-identity.status")" == "0" && "$(cat "$artifact_dir/evidence/mac-host-quota.status")" == "0" ]]; then
  run_capture quota-request-dry-run "$CRABBOX_MACOS_QUOTA_REQUEST_COMMAND" --identity "$artifact_dir/evidence/provider-identity.out" --quota "$artifact_dir/evidence/mac-host-quota.out" --region "$region" --profile "$profile"
else
  printf 'skipped; provider identity or quota capture failed\n' >"$artifact_dir/evidence/quota-request-dry-run.err"
  printf '1\n' >"$artifact_dir/evidence/quota-request-dry-run.status"
  : >"$artifact_dir/evidence/quota-request-dry-run.out"
fi

status_json() {
	local name="$1"
	local status
	status="$(cat "$artifact_dir/evidence/$name.status")"
	jq -n \
		--argjson status "$status" \
		--arg out "evidence/$name.out" \
		--arg err "evidence/$name.err" \
		--arg stdout "$(cat "$artifact_dir/evidence/$name.out" 2>/dev/null || true)" \
		--arg stderr "$(cat "$artifact_dir/evidence/$name.err" 2>/dev/null || true)" \
		'{
        status: $status,
        stdout: $out,
        stderr: $err,
        stdoutText: $stdout,
        stderrText: $stderr
      }'
}

provider_identity_status="$(cat "$artifact_dir/evidence/provider-identity.status")"
policy_status="$(cat "$artifact_dir/evidence/macos-image-policy.status")"
quota_status="$(cat "$artifact_dir/evidence/mac-host-quota.status")"
dry_status="$(cat "$artifact_dir/evidence/mac-host-dry-run.status")"
iam_status="$(cat "$artifact_dir/evidence/iam-apply-dry-run.status")"
quota_request_status="$(cat "$artifact_dir/evidence/quota-request-dry-run.status")"

host_dry_ok=false
if [[ "$dry_status" == "0" ]] && jq -e 'any(.[]; .ok == true)' "$artifact_dir/evidence/mac-host-dry-run.out" >/dev/null 2>&1; then
  host_dry_ok=true
fi
quota_ok=false
if [[ "$quota_status" == "0" ]] && jq -e 'any(.[]; (.value // 0) >= 1)' "$artifact_dir/evidence/mac-host-quota.out" >/dev/null 2>&1; then
  quota_ok=true
fi

helper_profile_ok=true
if grep -Eq 'no local AWS profile matches coordinator account|local AWS account .* does not match coordinator account' "$artifact_dir/evidence/iam-apply-dry-run.err" "$artifact_dir/evidence/quota-request-dry-run.err" 2>/dev/null; then
  helper_profile_ok=false
fi

commands_json="$(
	printf '%s\n' \
		"$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region" \
		"$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $region --json > provider-identity.json" \
		"$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
		"$CRABBOX_MACOS_IAM_APPLY_DISPLAY_COMMAND --identity provider-identity.json --policy macos-image-policy.json --profile $profile" \
		"$CRABBOX_MACOS_IAM_APPLY_DISPLAY_COMMAND --identity provider-identity.json --policy macos-image-policy.json --profile $profile --apply" \
		"$CRABBOX_REMEDIATION_BIN admin hosts quota --provider aws --target macos --region $region --type $instance_type --json > mac-host-quota.json" \
		"$CRABBOX_MACOS_QUOTA_REQUEST_DISPLAY_COMMAND --identity provider-identity.json --quota mac-host-quota.json --region $region --profile $profile" \
		"$CRABBOX_MACOS_QUOTA_REQUEST_DISPLAY_COMMAND --identity provider-identity.json --quota mac-host-quota.json --region $region --profile $profile --apply" |
    jq -R . |
    jq -s .
)"

blockers_json="$(
  jq -n \
    --argjson providerIdentityOK "$([[ "$provider_identity_status" == "0" ]] && echo true || echo false)" \
    --argjson policyOK "$([[ "$policy_status" == "0" ]] && echo true || echo false)" \
    --argjson dryOK "$host_dry_ok" \
    --argjson quotaOK "$quota_ok" \
    --argjson iamOK "$([[ "$iam_status" == "0" ]] && echo true || echo false)" \
    --argjson quotaRequestOK "$([[ "$quota_request_status" == "0" ]] && echo true || echo false)" \
    --argjson profileOK "$helper_profile_ok" \
    --arg dryText "$(cat "$artifact_dir/evidence/mac-host-dry-run.out" "$artifact_dir/evidence/mac-host-dry-run.err" 2>/dev/null || true)" \
    --arg quotaText "$(cat "$artifact_dir/evidence/mac-host-quota.out" "$artifact_dir/evidence/mac-host-quota.err" 2>/dev/null || true)" \
    '[
      (if $providerIdentityOK then empty else "provider-identity" end),
      (if $policyOK then empty else "provider-policy" end),
      (if $dryOK then empty
       elif ($dryText | contains("UnauthorizedOperation")) then "host-iam"
       else "host-dry-run" end),
      (if $quotaOK then empty
       elif ($quotaText | contains("\"value\":0") or contains("\"value\": 0")) then "host-quota"
       else "host-quota-visibility" end),
      (if $profileOK then empty else "local-coordinator-aws-profile" end),
      (if $iamOK then empty else "iam-apply-dry-run" end),
      (if $quotaRequestOK then empty else "quota-request-dry-run" end)
    ] | unique'
)"

result="blocked"
if [[ "$host_dry_ok" == "true" && "$quota_ok" == "true" ]] && jq -e 'length == 0' <<<"$blockers_json" >/dev/null; then
	result="ready-for-paid-smoke"
fi

jq -n \
  --arg generatedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg result "$result" \
  --arg region "$region" \
  --arg instanceType "$instance_type" \
  --arg artifactRoot "." \
  --argjson blockers "$blockers_json" \
  --argjson commands "$commands_json" \
  --argjson hostDryRunOK "$host_dry_ok" \
  --argjson quotaOK "$quota_ok" \
  --argjson helperProfileOK "$helper_profile_ok" \
  --argjson providerIdentity "$(status_json provider-identity)" \
  --argjson macosImagePolicy "$(status_json macos-image-policy)" \
  --argjson macHostQuota "$(status_json mac-host-quota)" \
  --argjson macHostDryRun "$(status_json mac-host-dry-run)" \
  --argjson iamApplyDryRun "$(status_json iam-apply-dry-run)" \
  --argjson quotaRequestDryRun "$(status_json quota-request-dry-run)" \
  '{
    generatedAt: $generatedAt,
    result: $result,
    region: $region,
    instanceType: $instanceType,
    artifactRoot: $artifactRoot,
    ready: {
      hostDryRun: $hostDryRunOK,
      hostQuota: $quotaOK,
      localCoordinatorAWSProfile: $helperProfileOK
    },
    blockers: $blockers,
    remediation: {
      commands: $commands
    },
    evidence: {
      providerIdentity: $providerIdentity,
      macosImagePolicy: $macosImagePolicy,
      macHostQuota: $macHostQuota,
      macHostDryRun: $macHostDryRun,
      iamApplyDryRun: $iamApplyDryRun,
      quotaRequestDryRun: $quotaRequestDryRun
    }
  }' >"$artifact_dir/summary.json"

printf 'macOS coordinator remediation audit: %s\n' "$artifact_dir/summary.json"
if [[ "$result" == "blocked" ]]; then
  exit 1
fi
