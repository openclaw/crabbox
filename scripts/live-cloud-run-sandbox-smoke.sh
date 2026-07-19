#!/usr/bin/env bash
set -euo pipefail

slug="cloud-run-sandbox-smoke-$(date +%Y%m%d%H%M%S)-$$"
cleanup_armed=0
root="$(cd "$(dirname "$0")/.." && pwd)"
bin="${CRABBOX_BIN:-$root/bin/crabbox}"
if [[ "$bin" != /* ]]; then
  bin="$root/$bin"
fi

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
  mkdir -p "$(dirname "$bin")"
  (cd "$root" && go build -trimpath -o "$bin" ./cmd/crabbox)
fi

doctor_out="$(run_capture "crabbox doctor --provider cloud-run-sandbox" \
  "$bin" doctor --provider cloud-run-sandbox)"
printf '%s\n' "$doctor_out"

if ! printf '%s' "$doctor_out" | grep -Eq 'control_plane=ready|Status: ok|status=ok|"status": "ok"'; then
  printf 'classification=unexpected_output command=%q exit=1\n' "crabbox doctor --provider cloud-run-sandbox" >&2
  printf '%s\n' "$doctor_out" >&2
  exit 1
fi

cleanup_armed=1
run_capture "crabbox warmup --provider cloud-run-sandbox" \
  "$bin" warmup --provider cloud-run-sandbox --slug "$slug" --ttl 15m --idle-timeout 5m

run_out="$(run_capture "crabbox run --provider cloud-run-sandbox --id $slug -- echo ok" \
  "$bin" run --provider cloud-run-sandbox --id "$slug" --no-sync -- echo ok)"
printf '%s\n' "$run_out"
if ! printf '%s\n' "$run_out" | grep -Fxq 'ok'; then
  printf 'classification=unexpected_output command=%q exit=1\n' "crabbox run --provider cloud-run-sandbox --id $slug -- echo ok" >&2
  exit 1
fi

list_out="$(run_capture "crabbox list --provider cloud-run-sandbox --json" \
  "$bin" list --provider cloud-run-sandbox --json)"
printf '%s\n' "$list_out"
if ! printf '%s' "$list_out" | grep -Fq "$slug"; then
  printf 'classification=unexpected_output command=%q exit=1 missing_slug=%q\n' "crabbox list --provider cloud-run-sandbox --json" "$slug" >&2
  exit 1
fi

run_capture "crabbox stop --provider cloud-run-sandbox" \
  "$bin" stop --provider cloud-run-sandbox --id "$slug"
cleanup_armed=0

printf 'classification=live_cloud_run_sandbox_smoke_passed slug=%s cleanup=complete\n' "$slug"
