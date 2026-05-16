#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"

instance_type="${CRABBOX_MACOS_TYPE:-mac2.metal}"
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

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

for region in "${regions[@]}"; do
  list_status=0
  list_output="$("$CRABBOX_BIN" admin mac-hosts list --region "$region" --type "$instance_type" --json 2>&1)" || list_status=$?
  existing_host=""
  if [[ "$list_status" -eq 0 ]]; then
    existing_host="$(printf '%s\n' "$list_output" | jq -r --arg type "$instance_type" '[.[] | select(.instanceType == $type and .state == "available") | .id][0] // empty' 2>/dev/null || true)"
  fi

  dry_status=0
  dry_output="$("$CRABBOX_BIN" admin mac-hosts allocate --region "$region" --type "$instance_type" --dry-run --json 2>&1)" || dry_status=$?
  dry_ok=false
  if [[ "$dry_status" -eq 0 ]] && printf '%s\n' "$dry_output" | jq -e 'any(.[]; .ok == true)' >/dev/null 2>&1; then
    dry_ok=true
  fi

  quota_status=0
  quota_output="$("$CRABBOX_BIN" admin mac-hosts quota --region "$region" --type "$instance_type" --json 2>&1)" || quota_status=$?
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

summary="$(
  jq -s --arg instanceType "$instance_type" '
    def first_existing: [ .[] | select(.existingHost != null) ][0] // null;
    def first_ready_allocation: [ .[] | select(.dryRun.ok == true and .quota.ok == true) ][0] // null;
    . as $regions |
    (first_existing) as $existing |
    (first_ready_allocation) as $ready |
    {
      result:
        (if $existing != null then "ready-existing-host"
         elif $ready != null then "ready-allocation"
         else "blocked" end),
      instanceType: $instanceType,
      selectedRegion:
        (if $existing != null then $existing.region
         elif $ready != null then $ready.region
         else null end),
      existingHost:
        (if $existing != null then $existing.existingHost else null end),
      blocker:
        (if $existing == null and $ready == null then {
          message: "no configured region has an available EC2 Mac Dedicated Host or quota-backed no-spend allocation dry-run",
          remediation: "Apply crabbox admin aws-policy --mac-hosts to the coordinator AWS identity, verify regional EC2 Mac Dedicated Host quota, then rerun this preflight before paid allocation."
        } else null end),
      regions: $regions
    }' "$tmp"
)"
printf '%s\n' "$summary"

result="$(printf '%s\n' "$summary" | jq -r '.result')"
if [[ "$result" == "blocked" ]]; then
  exit 1
fi
