#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo="${CRABBOX_LIVE_REPO:-$root}"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
if [[ "$cb" != /* ]]; then
  cb="$PWD/$cb"
fi

run_crabbox() {
  (cd "$repo" && "$cb" "$@")
}

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-scaleway}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "scaleway" ]]; then
      return 0
    fi
  done
  return 1
}

redact_output() {
  local output="$1"
  local secret
  for secret in "${SCW_ACCESS_KEY:-}" "${SCW_SECRET_KEY:-}"; do
    if [[ -n "$secret" ]]; then
      output="${output//"$secret"/[redacted]}"
    fi
  done
  printf '%s\n' "$output"
}

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *"insufficient funds"* || "$lower" == *"not enough"* || "$lower" == *"resource exhausted"* || "$lower" == *"limit exceeded"* ]]; then
    classification="quota_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  redact_output "$output" >&2
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  redact_output "$output" >&2
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
  redact_output "$output"
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
    print("Scaleway Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

local_testbox_key_snapshot() {
  local roots=(
    "${XDG_CONFIG_HOME:-$HOME/.config}/crabbox/testboxes"
    "$HOME/Library/Application Support/crabbox/testboxes"
  )
  local root
  for root in "${roots[@]}"; do
    if [ -d "$root" ]; then
      find "$root" -type f -print
    fi
  done | sort -u
}

cleanup_armed=0
slug="scaleway-smoke-$(date +%Y%m%d%H%M%S)-$$"
config_file=""
initial_local_key_snapshot=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    local cleanup_attempts="${CRABBOX_SCALEWAY_CLEANUP_ATTEMPTS:-65}"
    for ((attempt = 1; attempt <= cleanup_attempts; attempt++)); do
      set +e
      cleanup_output="$(run_crabbox stop --provider scaleway "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      local lower_cleanup_output
      lower_cleanup_output="$(printf '%s' "$cleanup_output" | tr '[:upper:]' '[:lower:]')"
      if [ "$cleanup_status" -eq 4 ] && [[ "$lower_cleanup_output" == *"lease/scaleway server not found:"* || "$lower_cleanup_output" == *"scaleway server not found"* ]]; then
        local current_local_key_snapshot
        current_local_key_snapshot="$(local_testbox_key_snapshot)"
        if [ "$current_local_key_snapshot" = "$initial_local_key_snapshot" ]; then
          cleanup_status=0
          cleanup_armed=0
          break
        fi
      fi
      if [ "$attempt" -lt "$cleanup_attempts" ]; then
        sleep 2
      fi
    done
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider scaleway $slug" "$cleanup_status" "$slug" >&2
      redact_output "$cleanup_output" >&2
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
  printf 'classification=environment_blocked reason=scaleway_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

for required in SCW_ACCESS_KEY SCW_SECRET_KEY; do
  if [[ -z "${!required:-}" ]]; then
    printf 'classification=environment_blocked reason=%s_missing\n' "$required"
    exit 0
  fi
done

scaleway_project_id="${CRABBOX_SCALEWAY_PROJECT_ID:-${SCW_DEFAULT_PROJECT_ID:-}}"
scaleway_organization_id="${CRABBOX_SCALEWAY_ORGANIZATION_ID:-${SCW_DEFAULT_ORGANIZATION_ID:-}}"
scaleway_region="${CRABBOX_SCALEWAY_REGION:-${SCW_DEFAULT_REGION:-}}"
scaleway_zone="${CRABBOX_SCALEWAY_ZONE:-${SCW_DEFAULT_ZONE:-}}"
scaleway_image="${CRABBOX_SCALEWAY_IMAGE:-ubuntu_noble}"
scaleway_type="${CRABBOX_SCALEWAY_TYPE:-DEV1-S}"

if [[ -z "$scaleway_organization_id" ]]; then
  printf 'classification=environment_blocked reason=SCW_DEFAULT_ORGANIZATION_ID_missing\n'
  exit 0
fi

if [[ -z "$scaleway_project_id" ]]; then
  printf 'classification=environment_blocked reason=SCW_DEFAULT_PROJECT_ID_missing\n'
  exit 0
fi

if [[ -z "$scaleway_region" ]]; then
  printf 'classification=environment_blocked reason=SCW_DEFAULT_REGION_missing\n'
  exit 0
fi

if [[ -z "$scaleway_zone" ]]; then
  printf 'classification=environment_blocked reason=SCW_DEFAULT_ZONE_missing\n'
  exit 0
fi

if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$root/bin"
  (cd "$root" && go build -trimpath -o "$cb" ./cmd/crabbox)
elif [[ ! -x "$cb" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_BIN_not_executable path=%q\n' "$cb"
  exit 0
fi

config_file="$(mktemp)"
cat >"$config_file" <<YAML
provider: scaleway
target: linux
scaleway:
  region: "$scaleway_region"
  zone: "$scaleway_zone"
  image: "$scaleway_image"
  type: "$scaleway_type"
  projectId: "$scaleway_project_id"
  organizationId: "$scaleway_organization_id"
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export SCW_ACCESS_KEY
export SCW_SECRET_KEY

doctor_output="$(run_capture "bin/crabbox doctor --provider scaleway" run_crabbox doctor --provider scaleway)"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "bin/crabbox list --provider scaleway --json" run_crabbox list --provider scaleway --json)"
validate_list_json_empty "bin/crabbox list --provider scaleway --json" "$initial_list_output"
initial_local_key_snapshot="$(local_testbox_key_snapshot)"
cleanup_armed=1
run_capture "bin/crabbox warmup --provider scaleway --slug $slug --keep --type $scaleway_type --ttl 20m --idle-timeout 5m" run_crabbox warmup --provider scaleway --slug "$slug" --keep --type "$scaleway_type" --ttl 20m --idle-timeout 5m >/dev/null
run_capture "bin/crabbox status --provider scaleway --id $slug --wait --wait-timeout 300s" run_crabbox status --provider scaleway --id "$slug" --wait --wait-timeout 300s >/dev/null
run_capture "bin/crabbox run --provider scaleway --id $slug --no-sync -- echo ok" run_crabbox run --provider scaleway --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "bin/crabbox list --provider scaleway --json" run_crabbox list --provider scaleway --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider scaleway --json" "$list_output"
run_capture "bin/crabbox stop --provider scaleway $slug" run_crabbox stop --provider scaleway "$slug" >/dev/null
cleanup_output="$(run_capture "bin/crabbox cleanup --provider scaleway --dry-run" run_crabbox cleanup --provider scaleway --dry-run)"
post_list_output="$(run_capture "bin/crabbox list --provider scaleway --json" run_crabbox list --provider scaleway --json)"
validate_list_json_empty "bin/crabbox list --provider scaleway --json" "$post_list_output"
cleanup_armed=0
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_scaleway_smoke_passed slug=%s cleanup=complete\n' "$slug"
