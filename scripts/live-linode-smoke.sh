#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/live-smoke-common.sh"

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-linode}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "linode" ]]; then
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

raw_linode_has_slug() {
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys
import urllib.request

slug = os.environ["CRABBOX_SMOKE_SLUG"]
token = os.environ["LINODE_TOKEN"]
slug_tag = f"crabbox:slug:{slug}".lower()
name = f"crabbox-{slug}".lower()
url = "https://api.linode.com/v4/linode/instances?page=1&page_size=500"

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
        for item in payload.get("data", []):
            tags = [str(tag).lower() for tag in item.get("tags", [])]
            label = str(item.get("label", "")).lower()
            if slug_tag in tags or label == name:
                sys.exit(0)
        page = int(payload.get("page") or 1)
        pages = int(payload.get("pages") or page)
        url = f"https://api.linode.com/v4/linode/instances?page={page + 1}&page_size=500" if page < pages else ""
except Exception:
    sys.exit(2)

sys.exit(1)
'
}

cleanup_armed=0
slug="linode-smoke-$(date +%Y%m%d%H%M%S)-$$"
crabbox_bin=""
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
      cleanup_output="$("$crabbox_bin" stop --provider linode "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      local lower_cleanup_output
      lower_cleanup_output="$(printf '%s' "$cleanup_output" | tr '[:upper:]' '[:lower:]')"
      if [ "$cleanup_status" -ne 4 ] || [[ "$lower_cleanup_output" != *"lease/linode not found:"* ]]; then
        if [ "$attempt" -lt "$cleanup_attempts" ]; then
          sleep "$cleanup_poll_seconds"
        fi
        continue
      fi
      local slug_status=2
      set +e
      raw_linode_has_slug >/dev/null 2>&1
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
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider linode $slug" "$cleanup_status" "$slug" >&2
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
  printf 'classification=environment_blocked reason=linode_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "${LINODE_TOKEN:-}" ]]; then
  printf 'classification=environment_blocked reason=LINODE_TOKEN_missing\n'
  exit 0
fi

crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$crabbox_bin")"
  go build -trimpath -o "$crabbox_bin" ./cmd/crabbox
fi

config_file="$(mktemp)"
cat >"$config_file" <<'YAML'
provider: linode
target: linux
linode:
  region: us-ord
  image: linode/ubuntu24.04
  type: g6-standard-1
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export LINODE_TOKEN

doctor_output="$(run_capture "$crabbox_bin doctor --provider linode" "$crabbox_bin" doctor --provider linode)"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "$crabbox_bin list --provider linode --json" "$crabbox_bin" list --provider linode --json)"
validate_list_json_empty "$crabbox_bin list --provider linode --json" "$initial_list_output" "Linode"
initial_local_key_snapshot="$(local_testbox_key_snapshot)"
cleanup_armed=1
run_capture "$crabbox_bin warmup --provider linode --slug $slug --keep --type g6-standard-1 --ttl 20m --idle-timeout 5m" "$crabbox_bin" warmup --provider linode --slug "$slug" --keep --type g6-standard-1 --ttl 20m --idle-timeout 5m >/dev/null
run_capture "$crabbox_bin status --provider linode --id $slug --wait --wait-timeout 300s" "$crabbox_bin" status --provider linode --id "$slug" --wait --wait-timeout 300s >/dev/null
run_capture "$crabbox_bin run --provider linode --id $slug --no-sync -- echo ok" "$crabbox_bin" run --provider linode --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "$crabbox_bin list --provider linode --json" "$crabbox_bin" list --provider linode --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "$crabbox_bin list --provider linode --json" "$list_output"
run_capture "$crabbox_bin stop --provider linode $slug" "$crabbox_bin" stop --provider linode "$slug" >/dev/null
cleanup_armed=0
cleanup_output="$(run_capture "$crabbox_bin cleanup --provider linode --dry-run" "$crabbox_bin" cleanup --provider linode --dry-run)"
post_list_output="$(run_capture "$crabbox_bin list --provider linode --json" "$crabbox_bin" list --provider linode --json)"
validate_list_json_empty "$crabbox_bin list --provider linode --json" "$post_list_output" "Linode"
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_linode_smoke_passed slug=%s cleanup=complete\n' "$slug"
