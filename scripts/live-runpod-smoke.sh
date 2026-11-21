#!/usr/bin/env bash
set -euo pipefail

json_matcher="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/live-smoke-json-match.py"

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
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" CRABBOX_SMOKE_FAILURE="list JSON did not include slug $slug" python3 "$json_matcher" runpod contains <<<"$output" 2>&1)"
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
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" CRABBOX_SMOKE_FAILURE="RunPod Crabbox inventory is not empty" python3 "$json_matcher" standard empty <<<"$output" 2>&1)"
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
base_url = os.environ.get("CRABBOX_RUNPOD_API_URL") or os.environ.get("RUNPOD_API_URL") or "https://api.runpod.io/v2"
base_url = base_url.strip().rstrip("/")
parsed = urllib.parse.urlsplit(base_url)
if parsed.hostname in ("rest.runpod.io", "api.runpod.io") and parsed.path == "/v1":
    sys.exit("RunPod REST v1 retires on 2026-11-15; remove the API URL override or use https://api.runpod.io/v2")
if parsed.scheme != "https" and not (parsed.scheme == "http" and parsed.hostname in ("localhost", "127.0.0.1", "::1")):
    sys.exit("RunPod cleanup URL must use HTTPS or loopback HTTP")
url = base_url + "/pods"
prefix = f"crabbox-{slug}-"

class SameOriginRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        destination = urllib.parse.urlsplit(newurl)
        if (destination.scheme, destination.netloc) != (parsed.scheme, parsed.netloc):
            raise RuntimeError("RunPod cleanup refused cross-origin redirect")
        return super().redirect_request(req, fp, code, msg, headers, newurl)

opener = urllib.request.build_opener(SameOriginRedirect())

def request(method="GET", target=url):
    req = urllib.request.Request(
        target,
        method=method,
        headers={"Authorization": f"Bearer {token}", "Accept": "application/json"},
    )
    with opener.open(req, timeout=30) as response:
        if method == "GET":
            return json.load(response)
        return None

def inventory():
    pods = []
    cursor = None
    seen = set()
    while True:
        query = {"limit": 1000}
        if cursor is not None:
            query["cursor"] = cursor
        page = request(target=url + "?" + urllib.parse.urlencode(query))
        if not isinstance(page, dict) or not isinstance(page.get("pods"), list):
            raise ValueError("RunPod cleanup requires a complete v2 pods inventory")
        pagination = page.get("pagination")
        if not isinstance(pagination, dict) or type(pagination.get("hasNextPage")) is not bool:
            raise ValueError("RunPod cleanup response is missing pagination")
        if any(not isinstance(pod, dict) for pod in page["pods"]):
            raise ValueError("RunPod cleanup response contains an invalid pod")
        pods.extend(page["pods"])
        if not pagination["hasNextPage"]:
            return pods
        cursor = pagination.get("nextCursor")
        if not isinstance(cursor, str) or not cursor or cursor in seen:
            raise ValueError("RunPod cleanup response has a missing or repeated cursor")
        seen.add(cursor)

try:
    pods = inventory()
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
    target = url + "/" + urllib.parse.quote(pod_id, safe="")
    current = request(target=target)
    if current.get("id") != pod_id or current.get("name") != matches[0].get("name"):
        raise ValueError("RunPod cleanup pod identity changed; refusing deletion")
    request("DELETE", target)
    for _ in range(30):
        pods = inventory()
        if not any(str(pod.get("name", "")).startswith(prefix) for pod in pods):
            sys.exit(0)
        time.sleep(2)
except urllib.error.HTTPError as exc:
    try:
        problem = json.load(exc)
        detail = ": ".join(str(problem.get(k, "")) for k in ("title", "detail"))
        detail += "; " + "; ".join(problem.get("errors", [])) if problem.get("errors") else ""
    except Exception:
        detail = "request failed"
    print(f"RunPod cleanup HTTP {exc.code}: {detail}".replace(token, "[REDACTED]"), file=sys.stderr)
except Exception as exc:
    print(f"RunPod cleanup failed: {exc}".replace(token, "[REDACTED]"), file=sys.stderr)
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
