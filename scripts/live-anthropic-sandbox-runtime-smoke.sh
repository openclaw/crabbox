#!/usr/bin/env bash
set -euo pipefail

tmp_root=""

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=environment_blocked command=%q exit=%s\n' "$command" "$status" >&2
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

expect_failure() {
  local command="$1"
  local required="$2"
  shift 2
  local output
  set +e
  output="$("$@" 2>&1)"
  local status=$?
  set -e
  if [ "$status" -eq 0 ]; then
    classify_validation_failure "$command" "$status" "command unexpectedly succeeded"
    exit 1
  fi
  if [ -n "$required" ] && [[ "$output" != *"$required"* ]]; then
    classify_validation_failure "$command" "$status" "expected output to contain ${required}; got: ${output}"
    exit 1
  fi
  printf '%s\n' "$output" >&2
}

cleanup() {
  if [ -n "$tmp_root" ]; then
    rm -rf "$tmp_root"
  fi
}
trap cleanup EXIT

mkdir -p bin
rm -f bin/crabbox
go build -trimpath -o bin/crabbox ./cmd/crabbox

srt_cli="${CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI:-srt}"
if ! command -v "$srt_cli" >/dev/null 2>&1; then
  classify_blocker "command -v $srt_cli" 127 "srt not found at configured path ${srt_cli}; install @anthropic-ai/sandbox-runtime or set CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI"
  exit 127
fi
if ! command -v curl >/dev/null 2>&1; then
  classify_blocker "command -v curl" 127 "curl not found on PATH; required for denied-network proof"
  exit 127
fi

doctor_output="$(run_capture "bin/crabbox doctor --provider anthropic-sandbox-runtime" bin/crabbox doctor --provider anthropic-sandbox-runtime)"
printf '%s\n' "$doctor_output"

echo_output="$(run_capture "bin/crabbox run --provider anthropic-sandbox-runtime -- echo ok" bin/crabbox run --provider anthropic-sandbox-runtime -- echo ok)"
printf '%s\n' "$echo_output"
if [[ "$echo_output" != *ok* ]]; then
  classify_validation_failure "bin/crabbox run --provider anthropic-sandbox-runtime -- echo ok" 0 "echo smoke did not print ok"
  exit 1
fi

tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-srt-smoke.XXXXXX")"
settings="$tmp_root/srt-settings.json"
secret="$tmp_root/secret.txt"
allowed_file="$tmp_root/crabbox-srt-allowed.txt"
printf 'secret\n' >"$secret"
cat >"$settings" <<JSON
{
  "network": {
    "allowedDomains": [],
    "deniedDomains": []
  },
  "filesystem": {
    "denyRead": ["$secret"],
    "allowRead": [],
    "allowWrite": ["$tmp_root"],
    "denyWrite": []
  }
}
JSON

allowed_output="$(run_capture "bin/crabbox run --provider anthropic-sandbox-runtime --anthropic-sandbox-runtime-settings <settings> -- sh -lc <allowed-write-read>" \
  bin/crabbox run --provider anthropic-sandbox-runtime --anthropic-sandbox-runtime-settings "$settings" -- sh -lc "printf ok > '$allowed_file' && cat '$allowed_file'")"
printf '%s\n' "$allowed_output"
if [[ "$allowed_output" != *ok* ]]; then
  classify_validation_failure "allowed write/read" 0 "allowed write/read did not print ok"
  exit 1
fi

expect_failure "bin/crabbox run --provider anthropic-sandbox-runtime --anthropic-sandbox-runtime-settings <settings> -- cat <denied-secret>" \
  "" \
  bin/crabbox run --provider anthropic-sandbox-runtime --anthropic-sandbox-runtime-settings "$settings" -- cat "$secret"

expect_failure "bin/crabbox run --provider anthropic-sandbox-runtime --anthropic-sandbox-runtime-settings <settings> -- curl https://example.com" \
  "" \
  bin/crabbox run --provider anthropic-sandbox-runtime --anthropic-sandbox-runtime-settings "$settings" -- curl -sS --max-time 3 https://example.com

printf 'classification=live_anthropic_sandbox_runtime_smoke_passed cleanup=complete\n'
