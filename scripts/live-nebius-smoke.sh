#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "nebius" ]]; then
      return 0
    fi
  done
  return 1
}

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *"limit exceeded"* || "$lower" == *"not enough"* || "$lower" == *"insufficient"* || "$lower" == *"resource exhausted"* ]]; then
    classification="quota_blocked"
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
    classify_blocker "$command" "$status" "$output"
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
        if isinstance(labels, dict) and (labels.get("slug") == slug or labels.get("crabbox_slug") == slug):
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

validate_list_json_absent_slug() {
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
        if isinstance(labels, dict) and (labels.get("slug") == slug or labels.get("crabbox_slug") == slug):
            return True
        if value.get("slug") == slug or value.get("name") == slug or value.get("id") == slug or value.get("leaseId") == slug:
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

if has_slug(payload):
    print(f"list JSON still includes slug {slug}", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

validate_echo_ok() {
  local command="$1"
  local output="$2"
  if [[ "$output" != *"ok"* ]]; then
    classify_validation_failure "$command" 1 "remote command output did not include ok"
    exit 1
  fi
}

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_LIVE_not_enabled\n'
  exit 0
fi

if ! provider_enabled; then
  printf 'classification=environment_blocked reason=nebius_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
if [[ ! -x "$crabbox_bin" ]]; then
  mkdir -p bin
  go build -trimpath -o bin/crabbox ./cmd/crabbox
  crabbox_bin="bin/crabbox"
fi

nonce="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
slug="nebius-smoke-$(date +%s)-$$-$nonce"
cleanup_armed=0
provider_args=(--provider nebius)

cleanup() {
  local status=$?
  if [[ "$cleanup_armed" -eq 1 ]]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    for attempt in 1 2 3; do
      set +e
      cleanup_output="$("$crabbox_bin" stop "${provider_args[@]}" "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [[ "$cleanup_status" -eq 0 ]]; then
        cleanup_armed=0
        break
      fi
      sleep 2
    done
    if [[ "$cleanup_status" -ne 0 ]]; then
      printf 'classification=environment_blocked reason=cleanup_failed command=%q exit=%s slug=%s\n' "stop --provider nebius $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" >&2
      if [[ "$status" -eq 0 ]]; then
        status="$cleanup_status"
      fi
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

run_capture "doctor --provider nebius" "$crabbox_bin" doctor "${provider_args[@]}" >/dev/null
run_capture "list --provider nebius --json" "$crabbox_bin" list "${provider_args[@]}" --json >/dev/null

cleanup_armed=1
run_capture "warmup --provider nebius --slug $slug --keep" \
  "$crabbox_bin" warmup "${provider_args[@]}" --slug "$slug" --keep --ttl 20m --idle-timeout 5m >/dev/null

run_capture "status --provider nebius --id $slug --wait" \
  "$crabbox_bin" status "${provider_args[@]}" --id "$slug" --wait --wait-timeout 300s >/dev/null

run_output="$(run_capture "run --provider nebius --id $slug --no-sync -- echo ok" \
  "$crabbox_bin" run "${provider_args[@]}" --id "$slug" --no-sync -- echo ok)"
validate_echo_ok "run --provider nebius --id $slug --no-sync -- echo ok" "$run_output"

list_output="$(run_capture "list --provider nebius --json" "$crabbox_bin" list "${provider_args[@]}" --json)"
validate_list_json_contains_slug "list --provider nebius --json" "$list_output"

run_capture "stop --provider nebius $slug" "$crabbox_bin" stop "${provider_args[@]}" "$slug" >/dev/null
cleanup_armed=0

run_capture "cleanup --provider nebius --dry-run" "$crabbox_bin" cleanup "${provider_args[@]}" --dry-run >/dev/null
final_list_output="$(run_capture "list --provider nebius --json" "$crabbox_bin" list "${provider_args[@]}" --json)"
validate_list_json_absent_slug "list --provider nebius --json" "$final_list_output"

echo "classification=live_nebius_smoke_passed slug=$slug"
