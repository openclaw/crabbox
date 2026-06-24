#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "vast" || "$item" == "vast-ai" || "$item" == "vastai" ]]; then
      return 0
    fi
  done
  return 1
}

redact_output() {
  VAST_SMOKE_REDACT_TOKEN_1="${CRABBOX_VAST_API_KEY:-}" \
    VAST_SMOKE_REDACT_TOKEN_2="${VAST_API_KEY:-}" python3 -c '
import os
import re
import sys

body = sys.stdin.read()
for token in (os.environ.get("VAST_SMOKE_REDACT_TOKEN_1", ""), os.environ.get("VAST_SMOKE_REDACT_TOKEN_2", "")):
    if token:
        body = body.replace(token, "<redacted>")
fields = (
    "api_key",
    "instance_api_key",
    "instanceApiKey",
    "jupyter_token",
    "jupyterToken",
    "jupyter_url",
    "jupyterUrl",
    "user_data",
    "userData",
    "private_key",
    "privateKey",
    "ssh_key",
)
for field in fields:
    body = re.sub(rf"(\"{re.escape(field)}\"\s*:\s*\")[^\"]*(\")", rf"\1<redacted>\2", body, flags=re.IGNORECASE)
    body = re.sub(rf"({re.escape(field)}\s*[=:]\s*)[^\",\s]+", rf"\1<redacted>", body, flags=re.IGNORECASE)
body = re.sub(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----", "<redacted>", body)
body = re.sub(r"https?://[^\s\"'\''<>]*(?:token|api_key|apikey|auth|signature|sig|access_token)=[^\s\"'\''<>]+", "<redacted-url>", body, flags=re.IGNORECASE)
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
  if [[ "$lower" == *billing* || "$lower" == *fund* || "$lower" == *payment* || "$lower" == *balance* || "$lower" == *credit* ]]; then
    classification="billing_blocked"
  elif [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *"account limit"* || "$lower" == *"limit exceeded"* ]]; then
    classification="quota_blocked"
  elif [[ "$lower" == *capacity* || "$lower" == *"no eligible offers"* || "$lower" == *"no offers"* || "$lower" == *"no matching"* || "$lower" == *"not enough"* || "$lower" == *"insufficient"* || "$lower" == *"resource exhausted"* || "$lower" == *"found no eligible offers"* ]]; then
    classification="capacity_blocked"
  elif [[ "$lower" == *unauthorized* || "$lower" == *forbidden* || "$lower" == *"permission denied"* || "$lower" == *"invalid api"* || "$lower" == *"invalid-api"* || "$lower" == *"missing_token"* || "$lower" == *"api key"* ]]; then
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
  if [[ "$status" -ne 0 ]]; then
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
  if [[ "$status" -ne 0 ]]; then
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
        label = str(value.get("label", ""))
        if f"|{slug}|" in label or value.get("slug") == slug or value.get("name") == slug or value.get("id") == slug or value.get("leaseId") == slug:
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
  if [[ "$status" -ne 0 ]]; then
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
    print("Vast Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

validate_nvidia_smi() {
  local command="$1"
  local output="$2"
  if [[ "$output" != *"NVIDIA-SMI"* && "$output" != *"NVIDIA"* ]]; then
    classify_validation_failure "$command" 1 "remote command output did not include NVIDIA-SMI output"
    exit 1
  fi
}

cleanup_armed=0
slug=""
config_file=""

cleanup() {
  local status=$?
  if [[ "$cleanup_armed" -eq 1 && -n "$slug" ]]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    for attempt in 1 2 3; do
      set +e
      cleanup_output="$(bin/crabbox stop --provider vast "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [[ "$cleanup_status" -eq 0 ]]; then
        cleanup_armed=0
        break
      fi
      sleep 2
    done
    if [[ "$cleanup_status" -ne 0 ]]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider vast $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" | redact_output >&2
      if [[ "$status" -eq 0 ]]; then
        status="$cleanup_status"
      fi
    fi
  fi
  if [[ -n "$config_file" ]]; then
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
  printf 'classification=environment_blocked reason=vast_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "${CRABBOX_VAST_API_KEY:-}${VAST_API_KEY:-}" ]]; then
  printf 'classification=environment_blocked reason=VAST_API_KEY_missing\n'
  exit 0
fi

mkdir -p bin
go build -trimpath -o bin/crabbox ./cmd/crabbox

slug="vast-smoke-$(date +%Y%m%d%H%M%S)-$$"
vast_gpu_name="${CRABBOX_LIVE_VAST_GPU_NAME:-}"
vast_gpu_count="${CRABBOX_LIVE_VAST_GPU_COUNT:-1}"
vast_max_dph_total="${CRABBOX_LIVE_VAST_MAX_DPH_TOTAL:-0}"
vast_instance_type="${CRABBOX_LIVE_VAST_INSTANCE_TYPE:-ondemand}"
vast_image="${CRABBOX_LIVE_VAST_IMAGE:-nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04}"
vast_release_action="${CRABBOX_LIVE_VAST_RELEASE_ACTION:-destroy}"

config_file="$(mktemp)"
cat >"$config_file" <<YAML
provider: vast
target: linux
vast:
  instanceType: $vast_instance_type
  gpuName: "$vast_gpu_name"
  gpuCount: $vast_gpu_count
  image: "$vast_image"
  runtype: ssh_direct
  maxDphTotal: $vast_max_dph_total
  releaseAction: $vast_release_action
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export CRABBOX_VAST_API_KEY="${CRABBOX_VAST_API_KEY:-${VAST_API_KEY:-}}"

run_capture "bin/crabbox doctor --provider vast" bin/crabbox doctor --provider vast
doctor_output="$CAPTURED_OUTPUT"
printf '%s\n' "$doctor_output"

run_capture "bin/crabbox list --provider vast --json" bin/crabbox list --provider vast --json
initial_list_output="$CAPTURED_OUTPUT"
validate_list_json_empty "bin/crabbox list --provider vast --json" "$initial_list_output"

cleanup_armed=1
run_capture "bin/crabbox warmup --provider vast --slug $slug --keep --ttl 20m --idle-timeout 5m" \
  bin/crabbox warmup --provider vast --slug "$slug" --keep --ttl 20m --idle-timeout 5m

run_capture_validation "bin/crabbox status --provider vast --id $slug --wait --wait-timeout 600s" \
  bin/crabbox status --provider vast --id "$slug" --wait --wait-timeout 600s

run_capture_validation "bin/crabbox run --provider vast --id $slug --no-sync -- nvidia-smi" \
  bin/crabbox run --provider vast --id "$slug" --no-sync -- nvidia-smi
nvidia_output="$CAPTURED_OUTPUT"
validate_nvidia_smi "bin/crabbox run --provider vast --id $slug --no-sync -- nvidia-smi" "$nvidia_output"

run_capture_validation "bin/crabbox list --provider vast --json" bin/crabbox list --provider vast --json
list_output="$CAPTURED_OUTPUT"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider vast --json" "$list_output"

run_capture_validation "bin/crabbox stop --provider vast $slug" bin/crabbox stop --provider vast "$slug"
cleanup_armed=0

run_capture_validation "bin/crabbox cleanup --provider vast --dry-run" bin/crabbox cleanup --provider vast --dry-run
cleanup_output="$CAPTURED_OUTPUT"
run_capture_validation "bin/crabbox list --provider vast --json" bin/crabbox list --provider vast --json
post_list_output="$CAPTURED_OUTPUT"
validate_list_json_empty "bin/crabbox list --provider vast --json" "$post_list_output"

printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_vast_smoke_passed slug=%s cleanup=complete\n' "$slug"
