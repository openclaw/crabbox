#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-fal}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "fal" || "$item" == "fal-ai" ]]; then
      return 0
    fi
  done
  return 1
}

redact_output() {
  FAL_SMOKE_REDACT_PRIMARY="${CRABBOX_FAL_KEY:-}" FAL_SMOKE_REDACT_FALLBACK="${FAL_KEY:-}" python3 -c '
import os
import re
import sys

body = sys.stdin.read()
for token in (os.environ.get("FAL_SMOKE_REDACT_PRIMARY", ""), os.environ.get("FAL_SMOKE_REDACT_FALLBACK", "")):
    if token:
        body = body.replace(token, "<redacted>")
for field in ("token", "api_key", "apiKey", "fal_key", "falKey", "ssh_key", "sshKey", "public_key", "publicKey", "private_key", "privateKey"):
    body = re.sub(rf"(\"{re.escape(field)}\"\s*:\s*\")[^\"]*(\")", rf"\1<redacted>\2", body, flags=re.IGNORECASE)
    body = re.sub(rf"({re.escape(field)}\s*[=: ]\s*)[^\",\s]+", rf"\1<redacted>", body, flags=re.IGNORECASE)
body = re.sub(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----", "<redacted>", body)
body = re.sub(r"""https?://[^\s"]*(?:token|api_key|key)=[^\s"]+""", "<redacted>", body, flags=re.IGNORECASE)
sys.stdout.write(body)
'
}

classify_known_external_blocker() {
  local command="$1"
  local exit_status="$2"
  local output="$3"
  local classification=""
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *billing* || "$lower" == *"payment"* || "$lower" == *"account-inactive"* || "$lower" == *"account inactive"* || "$lower" == *"inactive account"* ]]; then
    classification="billing_blocked"
  elif [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *"too many requests"* || "$lower" == *"account limit"* || "$lower" == *"resource exhausted"* ]]; then
    classification="quota_blocked"
  elif [[ "$lower" == *capacity* || "$lower" == *"insufficient capacity"* || "$lower" == *"unavailable"* || "$lower" == *"no instances available"* ]]; then
    classification="capacity_blocked"
  elif [[ "$lower" == *"invalid key"* || "$lower" == *"invalid-api-key"* || "$lower" == *unauthorized* || "$lower" == *forbidden* || "$lower" == *"permission denied"* || "$lower" == *"missing"* || "$lower" == *"requires fal credentials"* ]]; then
    classification="environment_blocked"
  else
    return 1
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$exit_status" >&2
  printf '%s\n' "$output" | redact_output >&2
  return 0
}

classify_validation_failure() {
  local command="$1"
  local exit_status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$exit_status" >&2
  printf '%s\n' "$output" | redact_output >&2
}

run_capture() {
  local command="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local exit_status=$?
  set -e
  if [ "$exit_status" -ne 0 ]; then
    if classify_known_external_blocker "$command" "$exit_status" "$output"; then
      exit 0
    fi
    classify_validation_failure "$command" "$exit_status" "$output"
    exit "$exit_status"
  fi
  CAPTURED_OUTPUT="$(printf '%s\n' "$output" | redact_output)"
}

run_capture_validation() {
  local command="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local exit_status=$?
  set -e
  if [ "$exit_status" -ne 0 ]; then
    classify_validation_failure "$command" "$exit_status" "$output"
    exit "$exit_status"
  fi
  CAPTURED_OUTPUT="$(printf '%s\n' "$output" | redact_output)"
}

validate_list_json_contains_slug() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local exit_status=0
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
  exit_status=$?
  set -e
  if [ "$exit_status" -ne 0 ]; then
    classify_validation_failure "$command" "$exit_status" "$validation_output"
    exit "$exit_status"
  fi
}

validate_list_json_empty() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local exit_status=0
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
    print("fal Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  exit_status=$?
  set -e
  if [ "$exit_status" -ne 0 ]; then
    classify_validation_failure "$command" "$exit_status" "$validation_output"
    exit "$exit_status"
  fi
}

is_fal_not_found_output() {
  local output="$1"
  [[ "$output" == *"lease/fal instance not found"* || "$output" == *"no local claim for fal lease"* || "$output" == *"not locally claimed"* ]]
}

cleanup_armed=0
slug="fal-smoke-$(date +%Y%m%d%H%M%S)-$$"
config_file=""
fal_key="${CRABBOX_FAL_KEY:-${FAL_KEY:-}}"
fal_instance_type="${CRABBOX_LIVE_FAL_INSTANCE_TYPE:-gpu_1x_h100_sxm5}"
fal_sector="${CRABBOX_LIVE_FAL_SECTOR:-sector_1}"
fal_api_url="${CRABBOX_LIVE_FAL_API_URL:-https://api.fal.ai/v1}"

cleanup() {
  local exit_status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    local cleanup_output=""
    local cleanup_status=1
    local attempt
    local cleanup_attempts=65
    local cleanup_poll_seconds=2
    for ((attempt = 1; attempt <= cleanup_attempts; attempt++)); do
      set +e
      cleanup_output="$(bin/crabbox stop --provider fal "$slug" 2>&1)"
      cleanup_status=$?
      set -e
      if [ "$cleanup_status" -eq 0 ]; then
        cleanup_armed=0
        break
      fi
      if is_fal_not_found_output "$cleanup_output"; then
        cleanup_armed=0
        break
      fi
      if [ "$attempt" -lt "$cleanup_attempts" ]; then
        sleep "$cleanup_poll_seconds"
      fi
    done
    if [ "$cleanup_status" -ne 0 ] && [ "$cleanup_armed" -eq 1 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "bin/crabbox stop --provider fal $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" | redact_output >&2
      if [ "$exit_status" -eq 0 ]; then
        exit_status="$cleanup_status"
      fi
    fi
  fi
  if [ -n "$config_file" ]; then
    rm -f "$config_file"
  fi
  exit "$exit_status"
}
trap cleanup EXIT

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_LIVE_not_enabled\n'
  exit 0
fi

if ! provider_enabled; then
  printf 'classification=environment_blocked reason=fal_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "$fal_key" ]]; then
  printf 'classification=environment_blocked reason=FAL_KEY_missing\n'
  exit 0
fi

mkdir -p bin
go build -trimpath -o bin/crabbox ./cmd/crabbox

config_file="$(mktemp)"
cat >"$config_file" <<YAML
provider: fal
target: linux
fal:
  apiUrl: $fal_api_url
  instanceType: $fal_instance_type
  sector: $fal_sector
  user: root
  workRoot: /work/crabbox
YAML

export CRABBOX_CONFIG="$config_file"
export CRABBOX_COORDINATOR=
export CRABBOX_FAL_KEY="$fal_key"

run_capture "bin/crabbox doctor --provider fal" bin/crabbox doctor --provider fal
doctor_output="$CAPTURED_OUTPUT"
printf '%s\n' "$doctor_output"
run_capture "bin/crabbox list --provider fal --json" bin/crabbox list --provider fal --json
initial_list_output="$CAPTURED_OUTPUT"
validate_list_json_empty "bin/crabbox list --provider fal --json" "$initial_list_output"
cleanup_armed=1
run_capture "bin/crabbox warmup --provider fal --slug $slug --keep --fal-instance-type $fal_instance_type --fal-sector $fal_sector --ttl 20m --idle-timeout 5m" bin/crabbox warmup --provider fal --slug "$slug" --keep --fal-instance-type "$fal_instance_type" --fal-sector "$fal_sector" --ttl 20m --idle-timeout 5m
run_capture_validation "bin/crabbox status --provider fal --id $slug --wait --wait-timeout 600s" bin/crabbox status --provider fal --id "$slug" --wait --wait-timeout 600s
run_capture_validation "bin/crabbox run --provider fal --id $slug --no-sync -- echo ok" bin/crabbox run --provider fal --id "$slug" --no-sync -- echo ok
run_capture_validation "bin/crabbox list --provider fal --json" bin/crabbox list --provider fal --json
list_output="$CAPTURED_OUTPUT"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider fal --json" "$list_output"
run_capture_validation "bin/crabbox stop --provider fal $slug" bin/crabbox stop --provider fal "$slug"
cleanup_armed=0
run_capture_validation "bin/crabbox cleanup --provider fal --dry-run" bin/crabbox cleanup --provider fal --dry-run
cleanup_output="$CAPTURED_OUTPUT"
run_capture_validation "bin/crabbox list --provider fal --json" bin/crabbox list --provider fal --json
post_list_output="$CAPTURED_OUTPUT"
validate_list_json_empty "bin/crabbox list --provider fal --json" "$post_list_output"
printf '%s\n' "$cleanup_output"
printf '%s\n' "$post_list_output"
printf 'classification=live_fal_smoke_passed slug=%s cleanup=complete\n' "$slug"
