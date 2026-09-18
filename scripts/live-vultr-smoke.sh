#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/live-smoke-common.sh"

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-vultr}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "vultr" ]]; then
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
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *"insufficient funds"* || "$lower" == *"account limit"* ]]; then
    classification="quota_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

raw_vultr_has_slug() {
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys
import urllib.request

slug = os.environ["CRABBOX_SMOKE_SLUG"]
token = os.environ["VULTR_API_KEY"]
slug_tag = f"crabbox:slug:{slug}".lower()
name = f"crabbox-{slug}".lower()
url = "https://api.vultr.com/v2/instances?per_page=500"

try:
    while url:
        request = urllib.request.Request(
            url,
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/json",
            },
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            payload = json.load(response)
        for item in payload.get("instances", []):
            tags = [str(tag).lower() for tag in item.get("tags", [])]
            label = str(item.get("label", "") or item.get("hostname", "")).lower()
            if slug_tag in tags or label == name:
                sys.exit(0)
        url = payload.get("meta", {}).get("links", {}).get("next", "")
except Exception:
    sys.exit(2)

sys.exit(1)
'
}

cleanup_armed=0
slug="vultr-smoke-$(date +%Y%m%d%H%M%S)-$$"
config_file=""
initial_local_key_snapshot=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    local cleanup_attempts=65
    local cleanup_poll_seconds=2
    for ((attempt = 1; attempt <= cleanup_attempts; attempt++)); do
      set +e
      cleanup_output="$(bin/crabbox stop --provider vultr "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      local lower_cleanup_output
      lower_cleanup_output="$(printf '%s' "$cleanup_output" | tr '[:upper:]' '[:lower:]')"
      if [ "$cleanup_status" -ne 4 ] || [[ "$lower_cleanup_output" != *"lease/instance not found:"* ]]; then
        if [ "$attempt" -lt "$cleanup_attempts" ]; then
          sleep "$cleanup_poll_seconds"
        fi
        continue
      fi
      local slug_status=2
      set +e
      raw_vultr_has_slug >/dev/null 2>&1
      slug_status=$?
      set -e
      if [ "$slug_status" -eq 1 ]; then
        local current_local_key_snapshot
        current_local_key_snapshot="$(local_testbox_key_snapshot)"
        if [ "$current_local_key_snapshot" = "$initial_local_key_snapshot" ]; then
          cleanup_status=0
          cleanup_armed=0
          break
        fi
      fi
      if [ "$attempt" -lt "$cleanup_attempts" ]; then
        sleep "$cleanup_poll_seconds"
      fi
    done
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider vultr $slug" "$cleanup_status" "$slug" >&2
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
  printf 'classification=environment_blocked reason=vultr_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "${VULTR_API_KEY:-}" ]]; then
  printf 'classification=environment_blocked reason=VULTR_API_KEY_missing\n'
  exit 0
fi

mkdir -p bin
go build -trimpath -o bin/crabbox ./cmd/crabbox

config_file="$(mktemp)"
cat >"$config_file" <<'YAML'
provider: vultr
target: linux
vultr:
  region: ewr
  userScheme: root
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export VULTR_API_KEY

doctor_output="$(run_capture "bin/crabbox doctor --provider vultr" bin/crabbox doctor --provider vultr)"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "bin/crabbox list --provider vultr --json" bin/crabbox list --provider vultr --json)"
validate_list_json_empty "bin/crabbox list --provider vultr --json" "$initial_list_output" "Vultr"
initial_local_key_snapshot="$(local_testbox_key_snapshot)"
cleanup_armed=1
run_capture "bin/crabbox warmup --provider vultr --slug $slug --keep --type vc2-1c-1gb --ttl 20m --idle-timeout 5m" bin/crabbox warmup --provider vultr --slug "$slug" --keep --type vc2-1c-1gb --ttl 20m --idle-timeout 5m >/dev/null
run_capture "bin/crabbox status --provider vultr --id $slug --wait --wait-timeout 300s" bin/crabbox status --provider vultr --id "$slug" --wait --wait-timeout 300s >/dev/null
run_capture "bin/crabbox run --provider vultr --id $slug --no-sync -- echo ok" bin/crabbox run --provider vultr --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "bin/crabbox list --provider vultr --json" bin/crabbox list --provider vultr --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider vultr --json" "$list_output"
run_capture "bin/crabbox stop --provider vultr $slug" bin/crabbox stop --provider vultr "$slug" >/dev/null
cleanup_armed=0
cleanup_output="$(run_capture "bin/crabbox cleanup --provider vultr --dry-run" bin/crabbox cleanup --provider vultr --dry-run)"
post_list_output="$(run_capture "bin/crabbox list --provider vultr --json" bin/crabbox list --provider vultr --json)"
validate_list_json_empty "bin/crabbox list --provider vultr --json" "$post_list_output" "Vultr"
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_vultr_smoke_passed slug=%s cleanup=complete\n' "$slug"
