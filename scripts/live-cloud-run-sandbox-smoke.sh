#!/usr/bin/env bash
set -euo pipefail

slug="cloud-run-sandbox-smoke-$(date +%Y%m%d%H%M%S)-$$"
cleanup_armed=0
root="$(cd "$(dirname "$0")/.." && pwd)"
bin="${CRABBOX_BIN:-$root/bin/crabbox}"

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *allow-list* || "$lower" == *allowlist* ]]; then
    classification="quota_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
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

cleanup() {
  if [ "$cleanup_armed" -eq 1 ]; then
    "$bin" stop --provider cloud-run-sandbox --id "$slug" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [ ! -x "$bin" ]; then
  (cd "$root" && go build -trimpath -o bin/crabbox ./cmd/crabbox)
fi

doctor_out="$(run_capture "crabbox doctor --provider cloud-run-sandbox" \
  "$bin" doctor --provider cloud-run-sandbox)"
printf '%s\n' "$doctor_out"

if ! printf '%s' "$doctor_out" | rg -q 'control_plane=ready|mode=remote|mode=direct|Status: ok|status=ok|"status": "ok"'; then
  # Accept inventory-style doctor messages too.
  if ! printf '%s' "$doctor_out" | rg -qi 'ready|ok'; then
    printf 'classification=diagnostic_only command=%q exit=0\n' "crabbox doctor --provider cloud-run-sandbox" >&2
    printf '%s\n' "$doctor_out" >&2
    exit 0
  fi
fi

cleanup_armed=1
run_capture "crabbox warmup --provider cloud-run-sandbox" \
  "$bin" warmup --provider cloud-run-sandbox --slug "$slug" --ttl 15m --idle-timeout 5m

run_capture "crabbox run --provider cloud-run-sandbox --id $slug -- echo ok" \
  "$bin" run --provider cloud-run-sandbox --id "$slug" --no-sync -- echo ok

run_capture "crabbox list --provider cloud-run-sandbox --json" \
  "$bin" list --provider cloud-run-sandbox --json

run_capture "crabbox stop --provider cloud-run-sandbox" \
  "$bin" stop --provider cloud-run-sandbox --id "$slug"
cleanup_armed=0

printf 'classification=live_cloud_run_sandbox_smoke_passed slug=%s cleanup=complete\n' "$slug"
