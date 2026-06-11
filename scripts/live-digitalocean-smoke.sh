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

validate_list_json_absent_slug() {
  local command="$1"
  local output="$2"
  set +e
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
payload = json.load(sys.stdin)

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

sys.exit(1 if has_slug(payload) else 0)
' <<<"$output" >/dev/null 2>&1
  local status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "list JSON still included slug $slug after stop"
    exit "$status"
  fi
}

cleanup_armed=0
slug="digitalocean-smoke-$(date +%Y%m%d%H%M%S)-$$"
config_file=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    bin/crabbox stop --provider digitalocean "$slug" >/dev/null 2>&1 || true
    bin/crabbox cleanup --provider digitalocean >/dev/null 2>&1 || true
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
  pat_file="${HOME}/Desktop/digitalocean-crabbox/PAT.md"
  if [[ -f "$pat_file" ]]; then
    DIGITALOCEAN_TOKEN="$(tr -d '\r\n' < "$pat_file")"
    export DIGITALOCEAN_TOKEN
  fi
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
cleanup_armed=1
run_capture "bin/crabbox warmup --provider digitalocean --slug $slug --keep --type s-1vcpu-1gb --ttl 20m --idle-timeout 5m" bin/crabbox warmup --provider digitalocean --slug "$slug" --keep --type s-1vcpu-1gb --ttl 20m --idle-timeout 5m >/dev/null
run_capture "bin/crabbox run --provider digitalocean --id $slug --no-sync -- echo ok" bin/crabbox run --provider digitalocean --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "bin/crabbox list --provider digitalocean --json" bin/crabbox list --provider digitalocean --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider digitalocean --json" "$list_output"
run_capture "bin/crabbox stop --provider digitalocean $slug" bin/crabbox stop --provider digitalocean "$slug" >/dev/null
cleanup_armed=0
cleanup_output="$(run_capture "bin/crabbox cleanup --provider digitalocean --dry-run" bin/crabbox cleanup --provider digitalocean --dry-run)"
post_list_output="$(run_capture "bin/crabbox list --provider digitalocean --json" bin/crabbox list --provider digitalocean --json)"
validate_list_json_absent_slug "bin/crabbox list --provider digitalocean --json" "$post_list_output"
printf '%s\n' "$cleanup_output"
printf 'classification=live_digitalocean_smoke_passed slug=%s cleanup=complete\n' "$slug"
