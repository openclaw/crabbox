#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
CRABBOX_REMEDIATION_BIN="${CRABBOX_REMEDIATION_BIN:-crabbox}"
CRABBOX_REGION_PREFLIGHT_COMMAND="${CRABBOX_REGION_PREFLIGHT_COMMAND:-scripts/macos-host-region-preflight.sh}"
CRABBOX_MACOS_IAM_APPLY_COMMAND="${CRABBOX_MACOS_IAM_APPLY_COMMAND:-scripts/apply-macos-image-iam-policy.sh}"
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

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

for instance_type in "${instance_types[@]}"; do
for region in "${regions[@]}"; do
  list_status=0
  list_output="$("$CRABBOX_BIN" admin hosts list --provider aws --target macos --region "$region" --type "$instance_type" --json 2>&1)" || list_status=$?
  existing_host=""
  if [[ "$list_status" -eq 0 ]]; then
    existing_host="$(printf '%s\n' "$list_output" | jq -r --arg type "$instance_type" '[.[] | select(.instanceType == $type and .state == "available") | .id][0] // empty' 2>/dev/null || true)"
  fi

  dry_status=0
  dry_output="$("$CRABBOX_BIN" admin hosts allocate --provider aws --target macos --region "$region" --type "$instance_type" --dry-run --json 2>&1)" || dry_status=$?
  dry_ok=false
  if [[ "$dry_status" -eq 0 ]] && printf '%s\n' "$dry_output" | jq -e 'any(.[]; .ok == true)' >/dev/null 2>&1; then
    dry_ok=true
  fi

  quota_status=0
  quota_output="$("$CRABBOX_BIN" admin hosts quota --provider aws --target macos --region "$region" --type "$instance_type" --json 2>&1)" || quota_status=$?
  quota_ok=false
  if [[ "$quota_status" -eq 0 ]] && printf '%s\n' "$quota_output" | jq -e 'any(.[]; (.value // 0) >= 1)' >/dev/null 2>&1; then
    quota_ok=true
  fi

  jq -n \
    --arg region "$region" \
    --arg instanceType "$instance_type" \
    --arg existingHost "$existing_host" \
    --argjson listStatus "$list_status" \
    --arg listOutput "$list_output" \
    --argjson dryRunStatus "$dry_status" \
    --arg dryRunOutput "$dry_output" \
    --argjson dryRunOK "$dry_ok" \
    --argjson quotaStatus "$quota_status" \
    --arg quotaOutput "$quota_output" \
    --argjson quotaOK "$quota_ok" \
    '{
      region: $region,
      instanceType: $instanceType,
      existingHost: (if $existingHost == "" then null else $existingHost end),
      list: {
        ok: ($listStatus == 0),
        status: $listStatus,
        output: $listOutput
      },
      dryRun: {
        ok: $dryRunOK,
        status: $dryRunStatus,
        output: $dryRunOutput
      },
      quota: {
        ok: $quotaOK,
        status: $quotaStatus,
        output: $quotaOutput
      }
    }' >>"$tmp"
done
done

instance_types_json="$(printf '%s\n' "${instance_types[@]}" | jq -R . | jq -s .)"
remediation_region="${regions[0]}"
blocker_commands_json="$(
  printf '%s\n' \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $remediation_region" \
    "$CRABBOX_REMEDIATION_BIN admin providers identity --provider aws --region $remediation_region --json > provider-identity.json" \
    "$CRABBOX_REMEDIATION_BIN admin providers policy --provider aws --target macos > macos-image-policy.json" \
    "$CRABBOX_MACOS_IAM_APPLY_COMMAND --identity provider-identity.json --policy macos-image-policy.json --profile auto" \
    "$CRABBOX_MACOS_IAM_APPLY_COMMAND --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply" \
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
