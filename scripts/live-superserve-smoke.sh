#!/usr/bin/env bash
set -Eeuo pipefail

classification="diagnostic_only"
classification_emitted=0
invocation_dir="$PWD"
repo_root=""
bin=""
smoke_root=""
smoke_repo=""
slug="ss-live-$(date -u +%Y%m%d)-$(printf '%06x%06x' "$$" "$RANDOM")"
cleanup_needed=0
cleanup_retry_delay="${CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS:-2}"

classify_and_exit() {
  trap - ERR
  if [[ $classification_emitted -ne 0 ]]; then
    exit 1
  fi
  classification_emitted=1
  classification="$1"
  reason="${2:-}"
  if [[ -n "$reason" ]]; then
    printf 'classification=%s reason=%s\n' "$classification" "$reason"
  else
    printf 'classification=%s\n' "$classification"
  fi
  case "$classification" in
    live_superserve_smoke_passed|environment_blocked|quota_blocked) exit 0 ;;
    *) exit 1 ;;
  esac
}

classify_failure() {
  local output="$1"
  local reason="$2"
  print_debug_detail "$output"
  if grep -Eiq 'quota|capacity|admission|rate limit|too many requests|429|insufficient|limit exceeded' <<<"$output"; then
    classify_and_exit quota_blocked "$reason"
  fi
  if grep -Eiq 'api key|unauthorized|forbidden|connection refused|no such host|timeout|timed out|TLS|x509|certificate|authentication|permission denied' <<<"$output"; then
    classify_and_exit environment_blocked "$reason"
  fi
  classify_and_exit diagnostic_only "$reason"
}

print_debug_detail() {
  if [[ "${CRABBOX_SUPERSERVE_SMOKE_DEBUG:-0}" != "1" ]]; then
    return 0
  fi
  local detail="$1"
  local secret
  for secret in "${CRABBOX_SUPERSERVE_API_KEY:-}" "${SUPERSERVE_API_KEY:-}"; do
    if [[ -n "$secret" ]]; then
      detail="${detail//$secret/[redacted]}"
    fi
  done
  detail="$(printf '%s' "$detail" |
    perl -0pe 's/"(access_token|token)"\s*:\s*"[^"]*"/"$1":"[redacted]"/g; s/[[:space:]]+/ /g' |
    cut -c 1-800)"
  if [[ -n "$detail" ]]; then
    printf 'debug_detail=%s\n' "$detail" >&2
  fi
}

unexpected_failure() {
  classify_and_exit diagnostic_only "unexpected_failure_line_$1"
}

need_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    classify_and_exit environment_blocked "missing_required_tool_$1"
  fi
}

inventory_has_slug() {
  local inventory
  if ! inventory="$("$bin" list --provider superserve --json 2>/dev/null)"; then
    return 2
  fi
  if jq -e --arg slug "$slug" 'any(.[]; ((.slug // .Slug // .labels.slug // .Labels.slug // "") == $slug))' <<<"$inventory" >/dev/null 2>&1; then
    return 0
  fi
  if jq -e 'type == "array"' <<<"$inventory" >/dev/null 2>&1; then
    return 1
  fi
  return 2
}

stop_and_confirm() {
  local attempt
  local inventory_status
  for attempt in 1 2 3; do
    if inventory_has_slug; then
      inventory_status=0
    else
      inventory_status=$?
    fi
    if [[ $inventory_status -eq 1 ]]; then
      return 0
    fi
    "$bin" stop --provider superserve "$slug" >/dev/null 2>&1 || true
    if [[ $attempt -lt 3 ]]; then
      sleep "$cleanup_retry_delay"
    fi
  done
  if inventory_has_slug; then
    inventory_status=0
  else
    inventory_status=$?
  fi
  [[ $inventory_status -eq 1 ]]
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ $cleanup_needed -eq 1 && -n "$bin" && -x "$bin" ]]; then
    if ! stop_and_confirm; then
      printf 'cleanup=failed provider=superserve slug=%s attempts=3\n' "$slug" >&2
      status=1
    fi
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'unexpected_failure "$LINENO"' ERR

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ "${CRABBOX_LIVE:-0}" != "1" ]]; then
  classify_and_exit environment_blocked "set_CRABBOX_LIVE=1"
fi

providers=",${CRABBOX_LIVE_PROVIDERS:-},"
if [[ "$providers" != *",superserve,"* ]]; then
  classify_and_exit environment_blocked "set_CRABBOX_LIVE_PROVIDERS=superserve"
fi

if [[ -z "${CRABBOX_SUPERSERVE_API_KEY:-${SUPERSERVE_API_KEY:-}}" ]]; then
  classify_and_exit environment_blocked "missing_superserve_api_key"
fi

need_tool git
need_tool jq

bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
if [[ "$bin" != /* ]]; then
  bin="$invocation_dir/$bin"
fi
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-superserve-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_SUPERSERVE_SMOKE_VALUE="forwarded-ok"

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Superserve Smoke"
cat >.crabbox.yaml <<EOF
provider: superserve
sync:
  delete: true
superserve:
  template: ${CRABBOX_SUPERSERVE_SMOKE_TEMPLATE:-superserve/base}
  workdir: /workspace/crabbox
EOF
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed Superserve smoke fixture"

trap - ERR
if doctor_output="$("$bin" doctor --provider superserve --json 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  classify_failure "$doctor_output" "doctor_failed"
fi

cleanup_needed=1
trap - ERR
if run_output="$("$bin" run --provider superserve --keep --slug "$slug" --timing-json \
  --allow-env CRABBOX_SUPERSERVE_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_SUPERSERVE_SMOKE_VALUE" = forwarded-ok && printf SUPERSERVE_SMOKE_STDOUT && printf SUPERSERVE_SMOKE_STDERR >&2 && printf SUPERSERVE_SMOKE_V1_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $run_status -ne 0 ]]; then
  classify_failure "$run_output" "initial_run_failed"
fi
if ! grep -q 'SUPERSERVE_SMOKE_V1_OK' <<<"$run_output" || ! grep -q 'SUPERSERVE_SMOKE_STDOUT' <<<"$run_output" || ! grep -q 'SUPERSERVE_SMOKE_STDERR' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial_run_proof_incomplete"
fi
if ! grep -q '"provider":"superserve"' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial_timing_provider_missing"
fi

"$bin" status --provider superserve --id "$slug" --wait --json >/dev/null
"$bin" list --provider superserve --json |
  jq -e --arg slug "$slug" 'any(.[]; ((.slug // .Slug // .labels.slug // .Labels.slug // "") == $slug))' >/dev/null

printf 'v2\n' >proof.txt
printf 'second\n' >second.txt
git add proof.txt second.txt
git rm -q stale.txt
git commit -qm "test: update Superserve smoke fixture"

trap - ERR
if reuse_output="$("$bin" run --provider superserve --id "$slug" --timing-json -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test -f second.txt && test ! -e stale.txt && printf SUPERSERVE_SMOKE_V2_OK' 2>&1)"; then
  reuse_status=0
else
  reuse_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $reuse_status -ne 0 ]]; then
  classify_failure "$reuse_output" "reuse_run_failed"
fi
if ! grep -q 'SUPERSERVE_SMOKE_V2_OK' <<<"$reuse_output" || ! grep -q '"provider":"superserve"' <<<"$reuse_output"; then
  classify_and_exit diagnostic_only "reuse_run_proof_incomplete"
fi

trap - ERR
if exit_output="$("$bin" run --provider superserve --id "$slug" --no-sync -- \
  /bin/sh -lc 'printf SUPERSERVE_SMOKE_EXIT_23; exit 23' 2>&1)"; then
  exit_status=0
else
  exit_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $exit_status -ne 23 ]] || ! grep -q 'SUPERSERVE_SMOKE_EXIT_23' <<<"$exit_output"; then
  classify_and_exit diagnostic_only "exit_propagation_failed"
fi

if ! stop_and_confirm; then
  classify_and_exit diagnostic_only "lease_cleanup_unconfirmed"
fi
cleanup_needed=0
printf 'cleanup=confirmed provider=superserve slug=%s\n' "$slug"

trap - EXIT
rm -rf -- "$smoke_root"
classify_and_exit live_superserve_smoke_passed
