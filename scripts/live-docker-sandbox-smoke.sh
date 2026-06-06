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

if ! command -v sbx >/dev/null 2>&1; then
  classify_blocker "command -v sbx" 127 "sbx not found on PATH"
  exit 127
fi

run_capture "sbx version" sbx version >/dev/null
run_capture "sbx ls --json" sbx ls --json >/dev/null

go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox doctor --provider docker-sandbox
bin/crabbox warmup --provider docker-sandbox --slug "$slug" --keep
created=1
bin/crabbox run --provider docker-sandbox --id "$slug" -- echo ok
bin/crabbox run --provider docker-sandbox --id "$slug" -- pwd
bin/crabbox list --provider docker-sandbox --json
bin/crabbox stop --provider docker-sandbox "$slug"
created=0
printf 'classification=live_sbx_smoke_passed slug=%s cleanup=complete\n' "$slug"
