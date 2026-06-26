#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-tencentcloud}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    case "$item" in
      tencentcloud|tencent|tencent-cvm|cvm) return 0 ;;
    esac
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
  if [[ "$lower" == *quota* || "$lower" == *"insufficient"* || "$lower" == *capacity* || "$lower" == *limit* || "$lower" == *"not enough"* ]]; then
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
    print("Tencent Cloud Crabbox inventory is not empty", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
	fi
}

list_json_empty() {
	local output="$1"
	python3 -c '
import json
import sys

try:
    payload = json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if payload == [] else 1)
' <<<"$output" >/dev/null 2>&1
}

wait_list_json_empty() {
	local command="$1"
	shift
	local output=""
	for _ in {1..18}; do
		output="$(run_capture "$command" "$@")"
		if list_json_empty "$output"; then
			printf '%s\n' "$output"
			return 0
		fi
		sleep 10
	done
	classify_validation_failure "$command" 1 "Tencent Cloud Crabbox inventory is not empty after waiting"
	exit 1
}

config_image() {
	"$crabbox_bin" config show --provider tencentcloud --json | python3 -c '
import json
import sys

payload = json.load(sys.stdin)
print(((payload.get("tencentcloud") or {}).get("image") or "").strip())
'
}

cleanup_armed=0
slug="tencentcloud-smoke-$(date +%Y%m%d%H%M%S)-$$"
crabbox_bin=""

cleanup() {
  local status=$?
  if [ "$cleanup_armed" -eq 1 ]; then
    set +e
    local cleanup_output
    cleanup_output="$("$crabbox_bin" stop --provider tencentcloud "$slug" 2>&1)"
    local cleanup_status=$?
    set -e
    if [ "$cleanup_status" -ne 0 ]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider tencentcloud $slug" "$cleanup_status" "$slug" >&2
      printf '%s\n' "$cleanup_output" >&2
      if [ "$status" -eq 0 ]; then
        status="$cleanup_status"
      fi
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_LIVE_not_enabled\n'
  exit 0
fi

if ! provider_enabled; then
  printf 'classification=environment_blocked reason=tencentcloud_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ -z "${TENCENTCLOUD_SECRET_ID:-}" || -z "${TENCENTCLOUD_SECRET_KEY:-}" ]]; then
  printf 'classification=environment_blocked reason=TENCENTCLOUD_SECRET_ID_or_SECRET_KEY_missing\n'
  exit 0
fi

crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$crabbox_bin")"
  go build -trimpath -o "$crabbox_bin" ./cmd/crabbox
fi

image="${CRABBOX_LIVE_TENCENTCLOUD_IMAGE:-${CRABBOX_TENCENTCLOUD_IMAGE:-}}"
if [[ -z "$image" ]]; then
  image="$(config_image)"
fi
if [[ -z "$image" ]]; then
  printf 'classification=environment_blocked reason=tencentcloud_image_missing\n'
  exit 0
fi

type_arg="${CRABBOX_LIVE_TENCENTCLOUD_TYPE:-${CRABBOX_TENCENTCLOUD_TYPE:-SA5.MEDIUM2}}"

doctor_output="$(run_capture "$crabbox_bin doctor --provider tencentcloud" "$crabbox_bin" doctor --provider tencentcloud)"
printf '%s\n' "$doctor_output"

list_output="$(run_capture "$crabbox_bin list --provider tencentcloud --json" "$crabbox_bin" list --provider tencentcloud --json)"
validate_list_json_empty "$crabbox_bin list --provider tencentcloud --json" "$list_output"

cleanup_armed=1
warmup_output="$(run_capture "$crabbox_bin warmup --provider tencentcloud --slug $slug --keep --tencentcloud-image $image --tencentcloud-type $type_arg --ttl 20m --idle-timeout 5m" "$crabbox_bin" warmup --provider tencentcloud --slug "$slug" --keep --tencentcloud-image "$image" --tencentcloud-type "$type_arg" --ttl 20m --idle-timeout 5m)"
printf '%s\n' "$warmup_output"

run_capture "$crabbox_bin status --provider tencentcloud --id $slug --wait --wait-timeout 300s" "$crabbox_bin" status --provider tencentcloud --id "$slug" --wait --wait-timeout 300s >/dev/null
run_output="$(run_capture "$crabbox_bin run --provider tencentcloud --id $slug --no-sync -- echo ok" "$crabbox_bin" run --provider tencentcloud --id "$slug" --no-sync -- echo ok)"
printf '%s\n' "$run_output"
if [[ "$run_output" != *ok* ]]; then
  classify_validation_failure "$crabbox_bin run --provider tencentcloud --id $slug --no-sync -- echo ok" 1 "$run_output"
  exit 1
fi

list_output="$(run_capture "$crabbox_bin list --provider tencentcloud --json" "$crabbox_bin" list --provider tencentcloud --json)"
validate_list_json_contains_slug "$crabbox_bin list --provider tencentcloud --json" "$list_output"

stop_output="$(run_capture "$crabbox_bin stop --provider tencentcloud $slug" "$crabbox_bin" stop --provider tencentcloud "$slug")"
printf '%s\n' "$stop_output"
cleanup_armed=0

wait_list_json_empty "$crabbox_bin list --provider tencentcloud --json" "$crabbox_bin" list --provider tencentcloud --json >/dev/null

cleanup_output="$(run_capture "$crabbox_bin cleanup --provider tencentcloud --dry-run" "$crabbox_bin" cleanup --provider tencentcloud --dry-run)"
printf '%s\n' "$cleanup_output"

list_output="$(run_capture "$crabbox_bin list --provider tencentcloud --json" "$crabbox_bin" list --provider tencentcloud --json)"
validate_list_json_empty "$crabbox_bin list --provider tencentcloud --json" "$list_output"

printf 'classification=live_tencentcloud_smoke_passed slug=%s image=%s type=%s\n' "$slug" "$image" "$type_arg"
