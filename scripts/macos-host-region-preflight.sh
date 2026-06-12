#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
CRABBOX_REMEDIATION_BIN="${CRABBOX_REMEDIATION_BIN:-crabbox}"
CRABBOX_REGION_PREFLIGHT_COMMAND="${CRABBOX_REGION_PREFLIGHT_COMMAND:-scripts/macos-host-region-preflight.sh}"
CRABBOX_MACOS_IAM_APPLY_COMMAND="${CRABBOX_MACOS_IAM_APPLY_COMMAND:-scripts/apply-macos-image-iam-policy.sh}"
CRABBOX_MACOS_QUOTA_REQUEST_COMMAND="${CRABBOX_MACOS_QUOTA_REQUEST_COMMAND:-scripts/request-macos-host-quota.sh}"
CRABBOX_MACOS_KNOWN_TYPES="mac2.metal,mac2-m2.metal,mac2-m2pro.metal,mac-m4.metal,mac-m4pro.metal,mac-m4max.metal,mac2-m1ultra.metal,mac-m3ultra.metal,mac1.metal"

if [[ -n "${CRABBOX_MACOS_TYPE:-}" ]]; then
  types_raw="$CRABBOX_MACOS_TYPE"
else
  types_raw="${CRABBOX_MACOS_TYPES:-mac2.metal,mac1.metal}"
fi
if [[ "$(printf '%s' "$types_raw" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')" == "all" ]]; then
  types_raw="$CRABBOX_MACOS_KNOWN_TYPES"
fi
regions_raw="${CRABBOX_MACOS_REGIONS:-${CRABBOX_CAPACITY_REGIONS:-${CRABBOX_MACOS_REGION:-eu-west-1}}}"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'missing required command: %s\n' "$1" >&2
    exit 2
  fi
}

need jq
if [[ ! -x "$CRABBOX_BIN" ]]; then
  printf 'CRABBOX_BIN is not executable: %s\n' "$CRABBOX_BIN" >&2
  exit 2
fi

regions=()
while IFS= read -r candidate_region; do
  regions+=("$candidate_region")
done < <(printf '%s\n' "$regions_raw" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | awk 'NF && !seen[$0]++')
if [[ "${#regions[@]}" -eq 0 ]]; then
  printf 'no macOS regions configured\n' >&2
  exit 2
fi

instance_types=()
while IFS= read -r candidate_type; do
  instance_types+=("$candidate_type")
done < <(printf '%s\n' "$types_raw" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | awk 'NF && !seen[$0]++')
if [[ "${#instance_types[@]}" -eq 0 ]]; then
  printf 'no macOS instance types configured\n' >&2
  exit 2
fi

tmp_dir="$(mktemp -d)"
tmp="$tmp_dir/results.jsonl"
trap 'rm -rf "$tmp_dir"' EXIT

capture_command() {
  local prefix="$1"
  shift
  local stdout_file="$tmp_dir/$prefix.stdout"
  local stderr_file="$tmp_dir/$prefix.stderr"
  local status=0
  set +e
  "$@" >"$stdout_file" 2>"$stderr_file"
  status=$?
  set -e
  cat "$stdout_file"
  printf '\0'
  cat "$stderr_file"
  printf '\0%s\n' "$status"
}

join_command_output() {
  local stdout="$1"
  local stderr="$2"
  if [[ -z "$stderr" ]]; then
    printf '%s' "$stdout"
  elif [[ -z "$stdout" ]]; then
    printf '%s' "$stderr"
  else
    printf '%s\n%s' "$stdout" "$stderr"
  fi
}

for instance_type in "${instance_types[@]}"; do
for region in "${regions[@]}"; do
  prefix="${region//[^A-Za-z0-9_.-]/_}-${instance_type//[^A-Za-z0-9_.-]/_}"
  list_status=0
  list_stdout=""
  list_stderr=""
  {
    IFS= read -r -d '' list_stdout
    IFS= read -r -d '' list_stderr
    IFS= read -r list_status
  } < <(capture_command "$prefix-list" "$CRABBOX_BIN" admin hosts list --provider aws --target macos --region "$region" --type "$instance_type" --json)
  list_output="$(join_command_output "$list_stdout" "$list_stderr")"
  list_ok=false
  list_parse_error=""
  existing_host=""
  if [[ "$list_status" -eq 0 ]]; then
    list_jq_error="$tmp_dir/$prefix-list-jq.stderr"
    set +e
    existing_host="$(printf '%s\n' "$list_stdout" | jq -r --arg type "$instance_type" '[.[] | select(.instanceType == $type and .state == "available") | .id][0] // empty' 2>"$list_jq_error")"
    list_jq_status=$?
    set -e
    if [[ "$list_jq_status" -eq 0 ]]; then
      list_ok=true
    else
      list_parse_error="$(cat "$list_jq_error")"
    fi
  fi

  dry_status=0
  dry_stdout=""
  dry_stderr=""
  {
    IFS= read -r -d '' dry_stdout
    IFS= read -r -d '' dry_stderr
    IFS= read -r dry_status
  } < <(capture_command "$prefix-dry-run" "$CRABBOX_BIN" admin hosts allocate --provider aws --target macos --region "$region" --type "$instance_type" --dry-run --json)
  dry_output="$(join_command_output "$dry_stdout" "$dry_stderr")"
  dry_ok=false
  dry_parse_error=""
  if [[ "$dry_status" -eq 0 ]]; then
    dry_jq_error="$tmp_dir/$prefix-dry-run-jq.stderr"
    set +e
    printf '%s\n' "$dry_stdout" | jq -e 'any(.[]; .ok == true)' >/dev/null 2>"$dry_jq_error"
    dry_jq_status=$?
    set -e
    if [[ "$dry_jq_status" -eq 0 ]]; then
      dry_ok=true
    elif [[ "$dry_jq_status" -gt 1 ]]; then
      dry_parse_error="$(cat "$dry_jq_error")"
    fi
  fi

  quota_status=0
  quota_stdout=""
  quota_stderr=""
  {
    IFS= read -r -d '' quota_stdout
    IFS= read -r -d '' quota_stderr
    IFS= read -r quota_status
  } < <(capture_command "$prefix-quota" "$CRABBOX_BIN" admin hosts quota --provider aws --target macos --region "$region" --type "$instance_type" --json)
  quota_output="$(join_command_output "$quota_stdout" "$quota_stderr")"
  quota_ok=false
  quota_parse_error=""
  if [[ "$quota_status" -eq 0 ]]; then
    quota_jq_error="$tmp_dir/$prefix-quota-jq.stderr"
    set +e
    printf '%s\n' "$quota_stdout" | jq -e 'any(.[]; (.value // 0) >= 1)' >/dev/null 2>"$quota_jq_error"
    quota_jq_status=$?
    set -e
    if [[ "$quota_jq_status" -eq 0 ]]; then
      quota_ok=true
    elif [[ "$quota_jq_status" -gt 1 ]]; then
      quota_parse_error="$(cat "$quota_jq_error")"
    fi
  fi

  jq -n \
    --arg region "$region" \
    --arg instanceType "$instance_type" \
    --arg existingHost "$existing_host" \
    --argjson listStatus "$list_status" \
    --argjson listOK "$list_ok" \
    --arg listOutput "$list_output" \
    --arg listStdout "$list_stdout" \
    --arg listStderr "$list_stderr" \
    --arg listParseError "$list_parse_error" \
    --argjson dryRunStatus "$dry_status" \
    --arg dryRunOutput "$dry_output" \
    --arg dryRunStdout "$dry_stdout" \
    --arg dryRunStderr "$dry_stderr" \
    --arg dryRunParseError "$dry_parse_error" \
    --argjson dryRunOK "$dry_ok" \
    --argjson quotaStatus "$quota_status" \
    --arg quotaOutput "$quota_output" \
    --arg quotaStdout "$quota_stdout" \
    --arg quotaStderr "$quota_stderr" \
    --arg quotaParseError "$quota_parse_error" \
    --argjson quotaOK "$quota_ok" \
    '{
      region: $region,
      instanceType: $instanceType,
      existingHost: (if $existingHost == "" then null else $existingHost end),
      list: {
        ok: $listOK,
        status: $listStatus,
        output: $listOutput,
        stdout: $listStdout,
        stderr: (if $listStderr == "" then null else $listStderr end),
        parseError: (if $listParseError == "" then null else $listParseError end)
      },
      dryRun: {
        ok: $dryRunOK,
        status: $dryRunStatus,
        output: $dryRunOutput,
        stdout: $dryRunStdout,
        stderr: (if $dryRunStderr == "" then null else $dryRunStderr end),
        parseError: (if $dryRunParseError == "" then null else $dryRunParseError end)
      },
      quota: {
        ok: $quotaOK,
        status: $quotaStatus,
        output: $quotaOutput,
        stdout: $quotaStdout,
        stderr: (if $quotaStderr == "" then null else $quotaStderr end),
        parseError: (if $quotaParseError == "" then null else $quotaParseError end)
      }
    }' >>"$tmp"
done
done

instance_types_json="$(printf '%s\n' "${instance_types[@]}" | jq -R . | jq -s .)"
read -r remediation_region remediation_type < <(
  jq -s -r '
    ([.[] | select(.dryRun.ok == true and .quota.ok == false)][0] // .[0])
    | [.region, .instanceType]
    | @tsv
  ' "$tmp"
)
blocker_commands_json="$(
  printf '%s\n' \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $remediation_region" \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $remediation_region --json > provider-identity.json" \
    "$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
    "$CRABBOX_MACOS_IAM_APPLY_COMMAND --identity provider-identity.json --policy macos-image-policy.json --profile auto" \
    "$CRABBOX_MACOS_IAM_APPLY_COMMAND --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply" \
    "$CRABBOX_REMEDIATION_BIN admin hosts quota --provider aws --target macos --region $remediation_region --type $remediation_type --json > mac-host-quota.json" \
    "$CRABBOX_MACOS_QUOTA_REQUEST_COMMAND --identity provider-identity.json --quota mac-host-quota.json --region $remediation_region --profile auto" \
    "$CRABBOX_MACOS_QUOTA_REQUEST_COMMAND --identity provider-identity.json --quota mac-host-quota.json --region $remediation_region --profile auto --apply" \
    "$CRABBOX_REGION_PREFLIGHT_COMMAND" |
    jq -R . |
    jq -s .
)"

summary="$(
  jq -s --argjson instanceTypes "$instance_types_json" --argjson blockerCommands "$blocker_commands_json" '
    def first_existing: [ .[] | select(.existingHost != null) ][0] // null;
    def first_ready_allocation: [ .[] | select(.dryRun.ok == true and .quota.ok == true) ][0] // null;
    . as $regions |
    (first_existing) as $existing |
    (first_ready_allocation) as $ready |
    (if $existing != null then $existing.instanceType
     elif $ready != null then $ready.instanceType
     else null end) as $selectedType |
    {
      result:
        (if $existing != null then "ready-existing-host"
         elif $ready != null then "ready-allocation"
         else "blocked" end),
      instanceType: ($selectedType // $instanceTypes[0]),
      selectedInstanceType: $selectedType,
      instanceTypes: $instanceTypes,
      selectedRegion:
        (if $existing != null then $existing.region
         elif $ready != null then $ready.region
         else null end),
      existingHost:
        (if $existing != null then $existing.existingHost else null end),
      blocker:
        (if $existing == null and $ready == null then {
          message: "no configured region/type has an available EC2 Mac Dedicated Host or quota-backed no-spend allocation dry-run",
          remediation: "Apply crabbox admin providers policy --provider aws --target macos to the coordinator AWS identity, verify regional EC2 Mac Dedicated Host quota, then rerun this preflight before paid allocation.",
          commands: $blockerCommands
        } else null end),
      regions: $regions
    }' "$tmp"
)"
printf '%s\n' "$summary"

result="$(printf '%s\n' "$summary" | jq -r '.result')"
if [[ "$result" == "blocked" ]]; then
  exit 1
fi
