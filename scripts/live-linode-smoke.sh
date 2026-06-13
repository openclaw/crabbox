#!/usr/bin/env bash
set -euo pipefail

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
    print("Linode Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
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
slug="linode-smoke-$(date +%Y%m%d%H%M%S)-$$"
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
      cleanup_output="$(bin/crabbox stop --provider linode "$slug" 2>&1)"
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
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider linode $slug" "$cleanup_status" "$slug" >&2
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

mkdir -p bin
go build -trimpath -o bin/crabbox ./cmd/crabbox

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

doctor_output="$(run_capture "bin/crabbox doctor --provider linode" bin/crabbox doctor --provider linode)"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "bin/crabbox list --provider linode --json" bin/crabbox list --provider linode --json)"
validate_list_json_empty "bin/crabbox list --provider linode --json" "$initial_list_output"
initial_local_key_snapshot="$(local_testbox_key_snapshot)"
cleanup_armed=1
run_capture "bin/crabbox warmup --provider linode --slug $slug --keep --type g6-standard-1 --ttl 20m --idle-timeout 5m" bin/crabbox warmup --provider linode --slug "$slug" --keep --type g6-standard-1 --ttl 20m --idle-timeout 5m >/dev/null
run_capture "bin/crabbox status --provider linode --id $slug --wait --wait-timeout 300s" bin/crabbox status --provider linode --id "$slug" --wait --wait-timeout 300s >/dev/null
run_capture "bin/crabbox run --provider linode --id $slug --no-sync -- echo ok" bin/crabbox run --provider linode --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "bin/crabbox list --provider linode --json" bin/crabbox list --provider linode --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider linode --json" "$list_output"
run_capture "bin/crabbox stop --provider linode $slug" bin/crabbox stop --provider linode "$slug" >/dev/null
cleanup_armed=0
cleanup_output="$(run_capture "bin/crabbox cleanup --provider linode --dry-run" bin/crabbox cleanup --provider linode --dry-run)"
post_list_output="$(run_capture "bin/crabbox list --provider linode --json" bin/crabbox list --provider linode --json)"
validate_list_json_empty "bin/crabbox list --provider linode --json" "$post_list_output"
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_linode_smoke_passed slug=%s cleanup=complete\n' "$slug"
