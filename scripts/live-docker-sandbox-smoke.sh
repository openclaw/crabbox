#!/usr/bin/env bash
set -euo pipefail

slug="docker-sandbox-smoke-$(date +%Y%m%d%H%M%S)-$$"
created=0

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=environment_blocked command=%q exit=%s\n' "$command" "$status" >&2
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

go build -trimpath -o bin/crabbox ./cmd/crabbox

run_capture "bin/crabbox doctor --provider docker-sandbox" bin/crabbox doctor --provider docker-sandbox
run_capture "bin/crabbox warmup --provider docker-sandbox --slug $slug --keep" bin/crabbox warmup --provider docker-sandbox --slug "$slug" --keep >/dev/null
created=1
run_capture "bin/crabbox run --provider docker-sandbox --id $slug -- echo ok" bin/crabbox run --provider docker-sandbox --id "$slug" -- echo ok >/dev/null
run_capture "bin/crabbox run --provider docker-sandbox --id $slug -- pwd" bin/crabbox run --provider docker-sandbox --id "$slug" -- pwd >/dev/null
run_capture "bin/crabbox list --provider docker-sandbox --json" bin/crabbox list --provider docker-sandbox --json
run_capture "bin/crabbox stop --provider docker-sandbox $slug" bin/crabbox stop --provider docker-sandbox "$slug" >/dev/null
created=0
printf 'classification=live_sbx_smoke_passed slug=%s cleanup=complete\n' "$slug"
