#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-digitalocean}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "digitalocean" || "$item" == "do" ]]; then
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
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *"insufficient funds"* ]]; then
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
    print("DigitalOcean Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
	fi
}

raw_digitalocean_has_slug() {
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys
import urllib.request

slug = os.environ["CRABBOX_SMOKE_SLUG"]
token = os.environ["DIGITALOCEAN_TOKEN"]
slug_tag = f"crabbox:slug:{slug}".lower()
name_prefix = f"crabbox-{slug}-".lower()
url = "https://api.digitalocean.com/v2/droplets?per_page=200&page=1"

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
        for droplet in payload.get("droplets", []):
            tags = [str(tag).lower() for tag in droplet.get("tags", [])]
            name = str(droplet.get("name", "")).lower()
            if slug_tag in tags or name.startswith(name_prefix):
                sys.exit(0)
        url = payload.get("links", {}).get("pages", {}).get("next", "")
except Exception:
    sys.exit(2)

sys.exit(1)
'
}

raw_digitalocean_managed_key_snapshot() {
  python3 -c '
import json
import os
import sys
import urllib.request

token = os.environ["DIGITALOCEAN_TOKEN"]
url = "https://api.digitalocean.com/v2/account/keys?per_page=200&page=1"
keys = []

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
        for key in payload.get("ssh_keys", []):
            name = str(key.get("name", ""))
            if name.startswith("crabbox-cbx-"):
                keys.append({
                    "id": key.get("id"),
                    "name": name,
                    "fingerprint": key.get("fingerprint"),
                })
        url = payload.get("links", {}).get("pages", {}).get("next", "")
except Exception:
    sys.exit(2)

print(json.dumps(sorted(keys, key=lambda key: (key["name"], str(key["id"]))), separators=(",", ":")))
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
slug="digitalocean-smoke-$(date +%Y%m%d%H%M%S)-$$"
config_file=""
initial_managed_key_snapshot=""
initial_local_key_snapshot=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    for attempt in 1 2 3; do
      set +e
      cleanup_output="$(bin/crabbox stop --provider digitalocean "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      local lower_cleanup_output
      lower_cleanup_output="$(printf '%s' "$cleanup_output" | tr '[:upper:]' '[:lower:]')"
      if [ "$cleanup_status" -ne 4 ] || [[ "$lower_cleanup_output" != *"lease/droplet not found:"* ]]; then
        sleep 2
        continue
      fi
      local slug_status=2
      set +e
      raw_digitalocean_has_slug >/dev/null 2>&1
      slug_status=$?
      set -e
      if [ "$slug_status" -eq 1 ]; then
        local current_managed_key_snapshot=""
        local key_snapshot_status=1
        set +e
        current_managed_key_snapshot="$(raw_digitalocean_managed_key_snapshot 2>/dev/null)"
        key_snapshot_status=$?
        set -e
        local current_local_key_snapshot
        current_local_key_snapshot="$(local_testbox_key_snapshot)"
        if [ "$key_snapshot_status" -eq 0 ] &&
          [ "$current_managed_key_snapshot" = "$initial_managed_key_snapshot" ] &&
          [ "$current_local_key_snapshot" = "$initial_local_key_snapshot" ]; then
          cleanup_status=0
          cleanup_armed=0
          break
        fi
      fi
      sleep 2
    done
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider digitalocean $slug" "$cleanup_status" "$slug" >&2
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
  printf 'classification=environment_blocked reason=digitalocean_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "${DIGITALOCEAN_TOKEN:-}" ]]; then
  printf 'classification=environment_blocked reason=DIGITALOCEAN_TOKEN_missing\n'
  exit 0
fi

mkdir -p bin
go build -trimpath -o bin/crabbox ./cmd/crabbox

config_file="$(mktemp)"
cat >"$config_file" <<'YAML'
provider: digitalocean
target: linux
digitalocean:
  region: nyc3
  image: ubuntu-24-04-x64
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export DIGITALOCEAN_TOKEN

doctor_output="$(run_capture "bin/crabbox doctor --provider digitalocean" bin/crabbox doctor --provider digitalocean)"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "bin/crabbox list --provider digitalocean --json" bin/crabbox list --provider digitalocean --json)"
validate_list_json_empty "bin/crabbox list --provider digitalocean --json" "$initial_list_output"
initial_managed_key_snapshot="$(run_capture "digitalocean managed SSH key snapshot" raw_digitalocean_managed_key_snapshot)"
initial_local_key_snapshot="$(local_testbox_key_snapshot)"
cleanup_armed=1
run_capture "bin/crabbox warmup --provider digitalocean --slug $slug --keep --type s-1vcpu-1gb --ttl 20m --idle-timeout 5m" bin/crabbox warmup --provider digitalocean --slug "$slug" --keep --type s-1vcpu-1gb --ttl 20m --idle-timeout 5m >/dev/null
run_capture "bin/crabbox status --provider digitalocean --id $slug --wait --wait-timeout 300s" bin/crabbox status --provider digitalocean --id "$slug" --wait --wait-timeout 300s >/dev/null
run_capture "bin/crabbox run --provider digitalocean --id $slug --no-sync -- echo ok" bin/crabbox run --provider digitalocean --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "bin/crabbox list --provider digitalocean --json" bin/crabbox list --provider digitalocean --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider digitalocean --json" "$list_output"
run_capture "bin/crabbox stop --provider digitalocean $slug" bin/crabbox stop --provider digitalocean "$slug" >/dev/null
cleanup_armed=0
cleanup_output="$(run_capture "bin/crabbox cleanup --provider digitalocean --dry-run" bin/crabbox cleanup --provider digitalocean --dry-run)"
post_list_output="$(run_capture "bin/crabbox list --provider digitalocean --json" bin/crabbox list --provider digitalocean --json)"
validate_list_json_empty "bin/crabbox list --provider digitalocean --json" "$post_list_output"
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_digitalocean_smoke_passed slug=%s cleanup=complete\n' "$slug"
