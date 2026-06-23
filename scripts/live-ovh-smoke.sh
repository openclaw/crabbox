#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-ovh}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "ovh" ]]; then
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
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *"insufficient funds"* || "$lower" == *"not enough"* ]]; then
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
  if [ "$status" -ne 0 ]; then
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
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

validate_list_json_empty() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local status=0
  set +e
  validation_output="$(python3 -c '
import json
import sys

try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

if payload != []:
    print("OVH Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

doctor_inventory_empty() {
  grep -Eq '(^|[[:space:]])leases=0($|[[:space:]])' <<<"$1"
}

validate_doctor_inventory_empty() {
  local command="$1"
  local output="$2"
  if ! doctor_inventory_empty "$output"; then
    classify_validation_failure "$command" 1 "OVH cloud inventory is not empty or did not report leases=0"
    exit 1
  fi
}

cleanup_armed=0
slug="ovh-smoke-$(date +%Y%m%d%H%M%S)-$$"
crabbox_bin=""
config_file=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    local cleanup_attempts="${CRABBOX_OVH_CLEANUP_ATTEMPTS:-65}"
    for ((attempt = 1; attempt <= cleanup_attempts; attempt++)); do
      set +e
      cleanup_output="$("$crabbox_bin" stop --provider ovh "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      if [ "$attempt" -lt "$cleanup_attempts" ]; then
        sleep 2
      fi
    done
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider ovh $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" >&2
      if [ "$status" -eq 0 ]; then
        status="$cleanup_status"
      fi
    fi
  fi
  if [ -n "$config_file" ]; then
    rm -f "$config_file"
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_LIVE_not_enabled\n'
  exit 0
fi

if ! provider_enabled; then
  printf 'classification=environment_blocked reason=ovh_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

for required in OVH_APPLICATION_KEY OVH_APPLICATION_SECRET OVH_CONSUMER_KEY; do
  if [[ -z "${!required:-}" ]]; then
    printf 'classification=environment_blocked reason=%s_missing\n' "$required"
    exit 0
  fi
done

ovh_endpoint="${CRABBOX_LIVE_OVH_ENDPOINT:-${OVH_ENDPOINT:-https://api.us.ovhcloud.com/1.0}}"
ovh_project_id="${CRABBOX_LIVE_OVH_PROJECT_ID:-${CRABBOX_OVH_PROJECT_ID:-}}"
ovh_region="${CRABBOX_LIVE_OVH_REGION:-${CRABBOX_OVH_REGION:-}}"
ovh_image="${CRABBOX_LIVE_OVH_IMAGE:-${CRABBOX_OVH_IMAGE:-Ubuntu 24.04}}"
ovh_flavor="${CRABBOX_LIVE_OVH_FLAVOR:-${CRABBOX_OVH_FLAVOR:-b3-8}}"

if [[ -z "$ovh_project_id" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_OVH_PROJECT_ID_missing\n'
  exit 0
fi

if [[ -z "$ovh_region" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_OVH_REGION_missing\n'
  exit 0
fi

mkdir -p bin
crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$crabbox_bin")"
  go build -trimpath -o "$crabbox_bin" ./cmd/crabbox
fi

config_file="$(mktemp)"
cat >"$config_file" <<YAML
provider: ovh
target: linux
ovh:
  endpoint: "$ovh_endpoint"
  projectId: "$ovh_project_id"
  region: "$ovh_region"
  image: "$ovh_image"
  flavor: "$ovh_flavor"
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export OVH_APPLICATION_KEY
export OVH_APPLICATION_SECRET
export OVH_CONSUMER_KEY

doctor_output="$(run_capture "$crabbox_bin doctor --provider ovh" "$crabbox_bin" doctor --provider ovh)"
validate_doctor_inventory_empty "$crabbox_bin doctor --provider ovh" "$doctor_output"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "$crabbox_bin list --provider ovh --json" "$crabbox_bin" list --provider ovh --json)"
validate_list_json_empty "$crabbox_bin list --provider ovh --json" "$initial_list_output"
cleanup_armed=1
run_capture "$crabbox_bin warmup --provider ovh --slug $slug --keep --type $ovh_flavor --ttl 20m --idle-timeout 5m" "$crabbox_bin" warmup --provider ovh --slug "$slug" --keep --type "$ovh_flavor" --ttl 20m --idle-timeout 5m >/dev/null
run_capture "$crabbox_bin status --provider ovh --id $slug --wait --wait-timeout 300s" "$crabbox_bin" status --provider ovh --id "$slug" --wait --wait-timeout 300s >/dev/null
run_capture "$crabbox_bin run --provider ovh --id $slug --no-sync -- echo ok" "$crabbox_bin" run --provider ovh --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "$crabbox_bin list --provider ovh --json" "$crabbox_bin" list --provider ovh --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "$crabbox_bin list --provider ovh --json" "$list_output"
run_capture "$crabbox_bin stop --provider ovh $slug" "$crabbox_bin" stop --provider ovh "$slug" >/dev/null
cleanup_armed=0
post_doctor_output=""
for attempt in {1..30}; do
  post_doctor_output="$(run_capture "$crabbox_bin doctor --provider ovh" "$crabbox_bin" doctor --provider ovh)"
  if doctor_inventory_empty "$post_doctor_output"; then
    break
  fi
  if [ "$attempt" -lt 30 ]; then
    sleep 2
  fi
done
validate_doctor_inventory_empty "$crabbox_bin doctor --provider ovh" "$post_doctor_output"
cleanup_output="$(run_capture "$crabbox_bin cleanup --provider ovh --dry-run" "$crabbox_bin" cleanup --provider ovh --dry-run)"
post_list_output="$(run_capture "$crabbox_bin list --provider ovh --json" "$crabbox_bin" list --provider ovh --json)"
validate_list_json_empty "$crabbox_bin list --provider ovh --json" "$post_list_output"
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_doctor_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_ovh_smoke_passed slug=%s cleanup=complete\n' "$slug"
