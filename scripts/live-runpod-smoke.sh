#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-runpod}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "runpod" || "$item" == "run-pod" || "$item" == "runpodio" ]]; then
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
    print("RunPod Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

raw_runpod_delete_slug() {
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

slug = os.environ["CRABBOX_SMOKE_SLUG"]
token = os.environ["RUNPOD_API_KEY"]
base_url = os.environ.get("CRABBOX_RUNPOD_API_URL") or os.environ.get("RUNPOD_API_URL") or "https://rest.runpod.io/v1"
url = base_url.rstrip("/") + "/pods"
prefix = f"crabbox-{slug}-"

def request(method="GET", target=url):
    req = urllib.request.Request(
        target,
        method=method,
        headers={"Authorization": f"Bearer {token}", "Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as response:
        if method == "GET":
            return json.load(response)
        return None

try:
    pods = request()
    matches = [pod for pod in pods if str(pod.get("name", "")).startswith(prefix)]
    if len(matches) > 1:
        print(f"multiple RunPod pods match cleanup slug {slug}", file=sys.stderr)
        sys.exit(3)
    if not matches:
        sys.exit(0)
    pod_id = str(matches[0].get("id", "")).strip()
    if not pod_id:
        print(f"RunPod cleanup match for {slug} has no id", file=sys.stderr)
        sys.exit(3)
    request("DELETE", url + "/" + urllib.parse.quote(pod_id, safe=""))
    for _ in range(30):
        pods = request()
        if not any(str(pod.get("name", "")).startswith(prefix) for pod in pods):
            sys.exit(0)
        time.sleep(2)
except urllib.error.HTTPError as exc:
    print(f"RunPod cleanup HTTP {exc.code}", file=sys.stderr)
except Exception as exc:
    print(f"RunPod cleanup failed: {exc}", file=sys.stderr)
sys.exit(1)
'
}

cleanup_armed=0
slug="runpod-smoke-$(date +%Y%m%d%H%M%S)-$$"
crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
config_file=""
state_root=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    local cleanup_output=""
    local cleanup_status=1
    set +e
    cleanup_output="$("$crabbox_bin" stop --provider runpod "$slug" 2>&1)"
    cleanup_status=$?
    set -e
    if [ "$cleanup_status" -ne 0 ]; then
      set +e
      cleanup_output="$(raw_runpod_delete_slug 2>&1)"
      cleanup_status=$?
      set -e
    fi
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider runpod $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" >&2
      if [ "$status" -eq 0 ]; then
        status="$cleanup_status"
      fi
    fi
  fi
  if [ -n "$config_file" ]; then
    rm -f "$config_file"
  fi
  if [ -n "$state_root" ]; then
    rm -rf "$state_root"
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_LIVE_not_enabled\n'
  exit 0
fi

if ! provider_enabled; then
  printf 'classification=environment_blocked reason=runpod_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

runpod_api_key="${CRABBOX_RUNPOD_API_KEY:-${RUNPOD_API_KEY:-}}"
if [[ -z "$runpod_api_key" ]]; then
  printf 'classification=environment_blocked reason=RUNPOD_API_KEY_missing\n'
  exit 0
fi
RUNPOD_API_KEY="$runpod_api_key"

if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$crabbox_bin")"
  go build -trimpath -o "$crabbox_bin" ./cmd/crabbox
fi

state_root="$(mktemp -d)"
config_file="$(mktemp)"
cat >"$config_file" <<'YAML'
provider: runpod
target: linux
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export XDG_CONFIG_HOME="$state_root/config"
export XDG_STATE_HOME="$state_root/state"
export RUNPOD_API_KEY

doctor_output="$(run_capture "$crabbox_bin doctor --provider runpod" "$crabbox_bin" doctor --provider runpod)"
printf '%s\n' "$doctor_output"
initial_list_output="$(run_capture "$crabbox_bin list --provider runpod --json" "$crabbox_bin" list --provider runpod --json)"
validate_list_json_empty "$crabbox_bin list --provider runpod --json" "$initial_list_output"
cleanup_armed=1
run_capture "$crabbox_bin warmup --provider runpod --slug $slug --keep --ttl 20m --idle-timeout 5m" "$crabbox_bin" warmup --provider runpod --slug "$slug" --keep --ttl 20m --idle-timeout 5m >/dev/null
run_capture "$crabbox_bin status --provider runpod --id $slug --wait --wait-timeout 600s" "$crabbox_bin" status --provider runpod --id "$slug" --wait --wait-timeout 600s >/dev/null
run_capture "$crabbox_bin run --provider runpod --id $slug --no-sync -- echo ok" "$crabbox_bin" run --provider runpod --id "$slug" --no-sync -- echo ok >/dev/null
list_output="$(run_capture "$crabbox_bin list --provider runpod --json" "$crabbox_bin" list --provider runpod --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "$crabbox_bin list --provider runpod --json" "$list_output"
run_capture "$crabbox_bin stop --provider runpod $slug" "$crabbox_bin" stop --provider runpod "$slug" >/dev/null
post_list_output="$(run_capture "$crabbox_bin list --provider runpod --json" "$crabbox_bin" list --provider runpod --json)"
validate_list_json_empty "$crabbox_bin list --provider runpod --json" "$post_list_output"
cleanup_armed=0
printf '%s\n' "$post_list_output"
printf 'classification=live_runpod_smoke_passed slug=%s cleanup=complete\n' "$slug"
