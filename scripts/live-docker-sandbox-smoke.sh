#!/usr/bin/env bash
set -euo pipefail

slug="docker-sandbox-smoke-$(date +%Y%m%d%H%M%S)-$$"
created=0

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* ]]; then
    classification="quota_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

classify_diagnostic() {
  local command="$1"
  local output="$2"
  printf 'classification=diagnostic_only command=%q exit=0\n' "$command" >&2
  printf '%s\n' "$output" >&2
}

classify_clone_guard() {
  local command="$1"
  local output="$2"
  printf 'classification=diagnostic_only clone_guard=manual command=%q exit=0\n' "$command" >&2
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
  if [ "$created" -eq 1 ]; then
    bin/crabbox stop --provider docker-sandbox "$slug" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

mkdir -p bin
rm -f bin/crabbox
go build -trimpath -o bin/crabbox ./cmd/crabbox

doctor_output="$(run_capture "bin/crabbox doctor --provider docker-sandbox" bin/crabbox doctor --provider docker-sandbox)"
printf '%s\n' "$doctor_output"
if [[ "$doctor_output" != *sbx_version* ]]; then
  classify_diagnostic "bin/crabbox doctor --provider docker-sandbox" "$doctor_output"
  exit 1
fi
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  classify_clone_guard "git rev-parse --is-inside-work-tree" "clone-mode live proof skipped outside a Git repository workspace"
fi
run_capture "bin/crabbox warmup --provider docker-sandbox --slug $slug --keep" bin/crabbox warmup --provider docker-sandbox --slug "$slug" --keep >/dev/null
created=1
run_capture "bin/crabbox run --provider docker-sandbox --id $slug -- echo ok" bin/crabbox run --provider docker-sandbox --id "$slug" -- echo ok >/dev/null
run_capture "bin/crabbox run --provider docker-sandbox --id $slug -- pwd" bin/crabbox run --provider docker-sandbox --id "$slug" -- pwd >/dev/null
run_capture "bin/crabbox list --provider docker-sandbox --json" bin/crabbox list --provider docker-sandbox --json
run_capture "bin/crabbox stop --provider docker-sandbox $slug" bin/crabbox stop --provider docker-sandbox "$slug" >/dev/null
created=0
printf 'classification=live_sbx_smoke_passed slug=%s cleanup=complete\n' "$slug"
