#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-lambda}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "lambda" ]]; then
      return 0
    fi
  done
  return 1
}

redact_output() {
  LAMBDA_SMOKE_REDACT_TOKEN="${LAMBDA_API_KEY:-}" python3 -c '
import os
import re
import sys

body = sys.stdin.read()
token = os.environ.get("LAMBDA_SMOKE_REDACT_TOKEN", "")
if token:
    body = body.replace(token, "<redacted>")
for field in ("token", "api_key", "user_data", "private_key", "privateKey", "jupyter_token", "jupyterToken", "jupyter_url", "jupyterUrl"):
    body = re.sub(rf"(\"{re.escape(field)}\"\s*:\s*\")[^\"]*(\")", rf"\1<redacted>\2", body, flags=re.IGNORECASE)
    body = re.sub(rf"({re.escape(field)}\s*[=: ]\s*)[^\",\s]+", rf"\1<redacted>", body, flags=re.IGNORECASE)
body = re.sub(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----", "<redacted>", body)
body = re.sub(r"""https?://[^\s"]*token=[^\s"]+""", "<redacted>", body, flags=re.IGNORECASE)
sys.stdout.write(body)
'
}

classify_known_external_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification=""
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *billing* || "$lower" == *"invalid-address"* || "$lower" == *"invalid billing"* || "$lower" == *"account-inactive"* || "$lower" == *"account inactive"* || "$lower" == *"inactive account"* ]]; then
    classification="billing_blocked"
  elif [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *"account limit"* ]]; then
    classification="quota_blocked"
  elif [[ "$lower" == *capacity* || "$lower" == *"insufficient-capacity"* || "$lower" == *"insufficient capacity"* ]]; then
    classification="capacity_blocked"
  elif [[ "$lower" == *"invalid-api-key"* || "$lower" == *unauthorized* || "$lower" == *forbidden* || "$lower" == *"permission denied"* || "$lower" == *"missing_token"* ]]; then
    classification="environment_blocked"
  else
    return 1
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  printf '%s\n' "$output" | redact_output >&2
  return 0
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  printf '%s\n' "$output" | redact_output >&2
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
    if classify_known_external_blocker "$command" "$status" "$output"; then
      exit 0
    fi
    classify_validation_failure "$command" "$status" "$output"
    exit "$status"
  fi
  CAPTURED_OUTPUT="$(printf '%s\n' "$output" | redact_output)"
}

run_capture_validation() {
  local command="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$output"
    exit "$status"
  fi
  CAPTURED_OUTPUT="$(printf '%s\n' "$output" | redact_output)"
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
        labels = value.get("labels") or value.get("tags")
        if isinstance(labels, dict) and (labels.get("slug") == slug or labels.get("crabbox.slug") == slug):
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
    print("Lambda Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

raw_lambda_has_slug() {
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys
import urllib.request

slug = os.environ["CRABBOX_SMOKE_SLUG"]
token = os.environ["LAMBDA_API_KEY"]
url = "https://cloud.lambda.ai/api/v1/instances"

try:
    request = urllib.request.Request(
        url,
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/json",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        payload = json.load(response)
    data = payload.get("data", payload)
    for item in data if isinstance(data, list) else []:
        tags = item.get("tags") or {}
        name = str(item.get("name", ""))
        if tags.get("crabbox.slug") == slug or tags.get("slug") == slug or name == f"crabbox-{slug}":
            sys.exit(0)
except Exception:
    sys.exit(2)

sys.exit(1)
'
}

raw_lambda_managed_key_snapshot() {
  python3 -c '
import json
import os
import sys
import urllib.request

token = os.environ["LAMBDA_API_KEY"]
url = "https://cloud.lambda.ai/api/v1/ssh-keys"
keys = []

try:
    request = urllib.request.Request(
        url,
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/json",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        payload = json.load(response)
    data = payload.get("data", payload)
    for key in data if isinstance(data, list) else []:
        name = str(key.get("name", ""))
        if name.startswith("crabbox-cbx-"):
            keys.append({
                "id": key.get("id"),
                "name": name,
                "public_key": key.get("public_key"),
            })
except Exception:
    sys.exit(2)

print(json.dumps(sorted(keys, key=lambda key: (key["name"], str(key["id"]))), separators=(",", ":")))
'
}

is_lambda_not_found_output() {
  local output="$1"
  [[ "$output" == *"lease/lambda not found:"* || "$output" == *"lease/lambda instance not found:"* ]]
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
slug="lambda-smoke-$(date +%Y%m%d%H%M%S)-$$"
config_file=""
initial_managed_key_snapshot=""
initial_local_key_snapshot=""
lambda_type="${CRABBOX_LIVE_LAMBDA_TYPE:-gpu_1x_a10}"
lambda_region="${CRABBOX_LIVE_LAMBDA_REGION:-us-west-1}"

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
      cleanup_output="$(bin/crabbox stop --provider lambda "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      local lower_cleanup_output
      lower_cleanup_output="$(printf '%s' "$cleanup_output" | tr '[:upper:]' '[:lower:]')"
      if [ "$cleanup_status" -ne 4 ] || ! is_lambda_not_found_output "$lower_cleanup_output"; then
        if [ "$attempt" -lt "$cleanup_attempts" ]; then
          sleep "$cleanup_poll_seconds"
        fi
        continue
      fi
      local slug_status=2
      set +e
      raw_lambda_has_slug >/dev/null 2>&1
      slug_status=$?
      set -e
      if [ "$slug_status" -eq 1 ]; then
        local current_managed_key_snapshot=""
        local key_snapshot_status=1
        set +e
        current_managed_key_snapshot="$(raw_lambda_managed_key_snapshot 2>/dev/null)"
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
      if [ "$attempt" -lt "$cleanup_attempts" ]; then
        sleep "$cleanup_poll_seconds"
      fi
    done
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider lambda $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" | redact_output >&2
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
  printf 'classification=environment_blocked reason=lambda_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "${LAMBDA_API_KEY:-}" ]]; then
  printf 'classification=environment_blocked reason=LAMBDA_API_KEY_missing\n'
  exit 0
fi

mkdir -p bin
go build -trimpath -o bin/crabbox ./cmd/crabbox

config_file="$(mktemp)"
cat >"$config_file" <<YAML
provider: lambda
target: linux
lambda:
  region: $lambda_region
  imageFamily: lambda-stack-24-04
  type: $lambda_type
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export LAMBDA_API_KEY

run_capture "bin/crabbox doctor --provider lambda" bin/crabbox doctor --provider lambda
doctor_output="$CAPTURED_OUTPUT"
printf '%s\n' "$doctor_output"
run_capture "bin/crabbox list --provider lambda --json" bin/crabbox list --provider lambda --json
initial_list_output="$CAPTURED_OUTPUT"
validate_list_json_empty "bin/crabbox list --provider lambda --json" "$initial_list_output"
run_capture "lambda managed SSH key snapshot" raw_lambda_managed_key_snapshot
initial_managed_key_snapshot="$CAPTURED_OUTPUT"
initial_local_key_snapshot="$(local_testbox_key_snapshot)"
cleanup_armed=1
run_capture "bin/crabbox warmup --provider lambda --slug $slug --keep --type $lambda_type --ttl 20m --idle-timeout 5m" bin/crabbox warmup --provider lambda --slug "$slug" --keep --type "$lambda_type" --ttl 20m --idle-timeout 5m
run_capture_validation "bin/crabbox status --provider lambda --id $slug --wait --wait-timeout 600s" bin/crabbox status --provider lambda --id "$slug" --wait --wait-timeout 600s
run_capture_validation "bin/crabbox run --provider lambda --id $slug --no-sync -- echo ok" bin/crabbox run --provider lambda --id "$slug" --no-sync -- echo ok
run_capture_validation "bin/crabbox list --provider lambda --json" bin/crabbox list --provider lambda --json
list_output="$CAPTURED_OUTPUT"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider lambda --json" "$list_output"
run_capture_validation "bin/crabbox stop --provider lambda $slug" bin/crabbox stop --provider lambda "$slug"
run_capture_validation "bin/crabbox cleanup --provider lambda --dry-run" bin/crabbox cleanup --provider lambda --dry-run
cleanup_output="$CAPTURED_OUTPUT"
run_capture_validation "bin/crabbox list --provider lambda --json" bin/crabbox list --provider lambda --json
post_list_output="$CAPTURED_OUTPUT"
validate_list_json_empty "bin/crabbox list --provider lambda --json" "$post_list_output"
cleanup_armed=0
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_lambda_smoke_passed slug=%s cleanup=complete\n' "$slug"
