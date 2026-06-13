#!/usr/bin/env bash
set -euo pipefail

if [[ "${CRABBOX_NVIDIA_BREV_LIVE:-}" != "1" ]]; then
  echo "classification=environment_blocked reason=CRABBOX_NVIDIA_BREV_LIVE_not_enabled"
  exit 0
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

classify_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *"too many requests"* || "$lower" == *"limit exceeded"* ]]; then
    classification="provider_quota_blocked"
  elif [[ "$lower" == *capacity* || "$lower" == *"not available"* || "$lower" == *"no available"* || "$lower" == *"sold out"* || "$lower" == *"gpu unavailable"* ]]; then
    classification="capacity_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

run_capture() {
  local command="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_failure "$command" "$status" "$output"
    exit "$status"
  fi
  printf '%s\n' "$output"
}

validate_list_json_contains_slug() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local status=0
  set +e
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

def has_slug(value):
    if isinstance(value, dict):
        labels = value.get("labels")
        if isinstance(labels, dict) and labels.get("slug") == slug:
            return True
        if value.get("slug") == slug or value.get("name") == slug or value.get("id") == slug or value.get("leaseId") == slug:
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

if not has_slug(payload):
    print(f"list JSON did not include slug {slug}", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

validate_nvidia_smi_output() {
  local command="$1"
  local output="$2"
  if [[ "$output" != *"NVIDIA-SMI"* && "$output" != *"nvidia-smi"* ]]; then
    classify_validation_failure "$command" 1 "nvidia-smi output marker not found"
    exit 1
  fi
}

crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
if [[ ! -x "$crabbox_bin" ]]; then
  mkdir -p bin
  go build -trimpath -o bin/crabbox ./cmd/crabbox
  crabbox_bin="bin/crabbox"
fi

nonce="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
slug="nbrev-smoke-$(date +%s)-$$-$nonce"
cleanup_armed=0
brev_bin="${CRABBOX_NVIDIA_BREV_CLI:-brev}"
provider_args=(--provider nvidia-brev --nvidia-brev-cli "$brev_bin")
common_args=("${provider_args[@]}" --nvidia-brev-release-action delete)

cleanup_unclaimed_workspaces() {
  local prefix="crabbox-$slug-"
  local attempts="${CRABBOX_NVIDIA_BREV_CLEANUP_ATTEMPTS:-6}"
  local poll_seconds="${CRABBOX_NVIDIA_BREV_CLEANUP_POLL_SECONDS:-2}"
  if [[ ! "$attempts" =~ ^[1-9][0-9]*$ ]]; then
    attempts=6
  fi
  if [[ ! "$poll_seconds" =~ ^[0-9]+$ ]]; then
    poll_seconds=2
  fi
  local attempt=1
  local matches=""
  while [[ "$attempt" -le "$attempts" ]]; do
    local inventory=""
    local inventory_status=0
    set +e
    inventory="$("$brev_bin" ls --json --all 2>&1)"
    inventory_status=$?
    set -e
    if [[ "$inventory_status" -ne 0 ]]; then
      printf 'classification=environment_blocked reason=cleanup_failed command=%q exit=%s\n' "$brev_bin ls --json --all" "$inventory_status" >&2
      printf '%s\n' "$inventory" >&2
      return 1
    fi

    local matches_status=0
    set +e
    matches="$(CRABBOX_SMOKE_PREFIX="$prefix" python3 -c '
import json
import os
import sys

prefix = os.environ["CRABBOX_SMOKE_PREFIX"]
try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

items = payload if isinstance(payload, list) else payload.get("workspaces")
if items is None:
    items = []
if not isinstance(items, list):
    print("workspaces is not an array", file=sys.stderr)
    sys.exit(1)
matches = []
for item in items:
    if not isinstance(item, dict):
        continue
    name = item.get("name")
    if isinstance(name, str) and name.startswith(prefix) and "\n" not in name:
        matches.append(name)
if len(matches) > 1:
    print(f"multiple workspaces match cleanup prefix {prefix}", file=sys.stderr)
    sys.exit(1)
if matches:
    print(matches[0])
' <<<"$inventory" 2>&1)"
    matches_status=$?
    set -e
    if [[ "$matches_status" -ne 0 ]]; then
      printf 'classification=validation_failed reason=cleanup_inventory_invalid exit=%s\n' "$matches_status" >&2
      printf '%s\n' "$matches" >&2
      return 1
    fi
    if [[ -n "$matches" || "$attempt" -eq "$attempts" ]]; then
      break
    fi
    sleep "$poll_seconds"
    attempt=$((attempt + 1))
  done

  local failed=0
  while IFS= read -r workspace_name; do
    [[ -z "$workspace_name" ]] && continue
    local delete_output=""
    local delete_status=0
    set +e
    delete_output="$("$brev_bin" delete "$workspace_name" 2>&1)"
    delete_status=$?
    set -e
    if [[ "$delete_status" -ne 0 ]]; then
      printf 'classification=environment_blocked reason=cleanup_failed command=%q exit=%s\n' "$brev_bin delete $workspace_name" "$delete_status" >&2
      printf '%s\n' "$delete_output" >&2
      failed=1
    fi
  done <<<"$matches"
  return "$failed"
}

cleanup() {
  local status=$?
  if [[ "$cleanup_armed" -eq 1 ]]; then
    local cleanup_output=""
    local cleanup_status=0
    set +e
    cleanup_output="$("$crabbox_bin" stop "${common_args[@]}" "$slug" 2>&1)"
    cleanup_status=$?
    set -e
    if [[ "$cleanup_status" -ne 0 ]]; then
      local cleanup_lower
      cleanup_lower="$(printf '%s' "$cleanup_output" | tr '[:upper:]' '[:lower:]')"
      local cleanup_absent=0
      if [[ "$cleanup_status" -eq 4 && "$cleanup_lower" == *"nvidia-brev workspace not found:"* ]]; then
        cleanup_absent=1
      elif [[ "$cleanup_status" -eq 2 && "$cleanup_lower" == *"without a local crabbox claim"* ]]; then
        cleanup_absent=1
      fi
      if [[ "$cleanup_absent" -eq 1 ]]; then
        cleanup_unclaimed_workspaces || true
      else
        printf 'classification=environment_blocked reason=cleanup_failed command=%q exit=%s\n' "stop --provider nvidia-brev $slug" "$cleanup_status" >&2
        printf '%s\n' "$cleanup_output" >&2
      fi
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

run_capture "doctor --provider nvidia-brev" "$crabbox_bin" doctor "${provider_args[@]}" >/dev/null
run_capture "list --provider nvidia-brev --json" "$crabbox_bin" list "${provider_args[@]}" --json >/dev/null

cleanup_armed=1
run_capture "warmup --provider nvidia-brev --slug $slug --keep=false" \
  "$crabbox_bin" warmup "${common_args[@]}" --slug "$slug" --keep=false --ttl 20m --idle-timeout 5m >/dev/null

run_capture "status --provider nvidia-brev --id $slug --wait" \
  "$crabbox_bin" status "${common_args[@]}" --id "$slug" --wait --wait-timeout 300s >/dev/null

run_output="$(run_capture "run --provider nvidia-brev --id $slug --no-sync -- nvidia-smi" \
  "$crabbox_bin" run "${common_args[@]}" --id "$slug" --no-sync -- nvidia-smi)"
validate_nvidia_smi_output "run --provider nvidia-brev --id $slug --no-sync -- nvidia-smi" "$run_output"

list_output="$(run_capture "list --provider nvidia-brev --json" "$crabbox_bin" list "${provider_args[@]}" --json)"
validate_list_json_contains_slug "list --provider nvidia-brev --json" "$list_output"

run_capture "stop --provider nvidia-brev $slug" "$crabbox_bin" stop "${common_args[@]}" "$slug" >/dev/null
cleanup_armed=0

echo "classification=live_nvidia_brev_smoke_passed slug=$slug"
