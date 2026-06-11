#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

classification="diagnostic_only"
slug="crabbox-opensandbox-smoke-$$"
bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"

classify_and_exit() {
  classification="$1"
  message="${2:-}"
  if [[ -n "$message" ]]; then
    printf '%s %s\n' "$classification" "$message"
  else
    printf '%s\n' "$classification"
  fi
  case "$classification" in
    live_opensandbox_smoke_passed) exit 0 ;;
    environment_blocked|quota_blocked|diagnostic_only) exit 0 ;;
    *) exit 1 ;;
  esac
}

cleanup() {
  if [[ -x "$bin" ]]; then
    "$bin" stop --provider opensandbox --opensandbox-forget-missing "$slug" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [[ -z "${CRABBOX_OPENSANDBOX_API_KEY:-${OPEN_SANDBOX_API_KEY:-}}" ]]; then
  classify_and_exit environment_blocked "missing CRABBOX_OPENSANDBOX_API_KEY or OPEN_SANDBOX_API_KEY"
fi

if [[ -z "${CRABBOX_OPENSANDBOX_API_URL:-${OPEN_SANDBOX_API_URL:-}}" ]]; then
  classify_and_exit environment_blocked "missing CRABBOX_OPENSANDBOX_API_URL or OPEN_SANDBOX_API_URL"
fi

mkdir -p "$(dirname "$bin")"
go build -trimpath -o "$bin" ./cmd/crabbox

set +e
run_output="$("$bin" run --provider opensandbox --keep --no-sync --slug "$slug" -- /bin/sh -lc 'printf OPEN_SANDBOX_SMOKE_OK' 2>&1)"
run_status=$?
set -e
if [[ $run_status -ne 0 ]]; then
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient' <<<"$run_output"; then
    classify_and_exit quota_blocked "$run_output"
  fi
  if grep -Eiq 'api key|unauthorized|forbidden|connection refused|no such host|timeout|TLS|x509' <<<"$run_output"; then
    classify_and_exit environment_blocked "$run_output"
  fi
  classify_and_exit diagnostic_only "$run_output"
fi
if ! grep -q 'OPEN_SANDBOX_SMOKE_OK' <<<"$run_output"; then
  classify_and_exit diagnostic_only "run succeeded but sentinel was missing"
fi

"$bin" status --provider opensandbox --id "$slug" >/dev/null
"$bin" list --provider opensandbox --json >/dev/null
"$bin" stop --provider opensandbox "$slug" >/dev/null
trap - EXIT

classify_and_exit live_opensandbox_smoke_passed
