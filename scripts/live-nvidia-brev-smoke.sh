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

slug="nvidia-brev-smoke-$(date +%Y%m%d%H%M%S)-$$"
cleanup_armed=0
common_args=(--provider nvidia-brev --nvidia-brev-release-action delete)

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
      printf 'classification=environment_blocked reason=cleanup_failed command=%q exit=%s\n' "stop --provider nvidia-brev $slug" "$cleanup_status" >&2
      printf '%s\n' "$cleanup_output" >&2
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

run_capture "doctor --provider nvidia-brev" "$crabbox_bin" doctor --provider nvidia-brev >/dev/null
run_capture "list --provider nvidia-brev --json" "$crabbox_bin" list --provider nvidia-brev --json >/dev/null

cleanup_armed=1
run_capture "warmup --provider nvidia-brev --slug $slug --keep" \
  "$crabbox_bin" warmup "${common_args[@]}" --slug "$slug" --keep --ttl 20m --idle-timeout 5m >/dev/null

run_capture "status --provider nvidia-brev --id $slug --wait" \
  "$crabbox_bin" status "${common_args[@]}" --id "$slug" --wait --wait-timeout 300s >/dev/null

run_output="$(run_capture "run --provider nvidia-brev --id $slug --no-sync -- nvidia-smi" \
  "$crabbox_bin" run "${common_args[@]}" --id "$slug" --no-sync -- nvidia-smi)"
validate_nvidia_smi_output "run --provider nvidia-brev --id $slug --no-sync -- nvidia-smi" "$run_output"

list_output="$(run_capture "list --provider nvidia-brev --json" "$crabbox_bin" list --provider nvidia-brev --json)"
validate_list_json_contains_slug "list --provider nvidia-brev --json" "$list_output"

run_capture "stop --provider nvidia-brev $slug" "$crabbox_bin" stop "${common_args[@]}" "$slug" >/dev/null
cleanup_armed=0

echo "classification=live_nvidia_brev_smoke_passed slug=$slug"
